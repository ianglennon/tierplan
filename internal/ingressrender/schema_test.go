// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

func TestRenderSchema(t *testing.T) {
	if got := Schemas(); !slices.Equal(got, []int{ingressbundle.Schema2}) {
		t.Fatalf("Schemas() = %v, want [2]", got)
	}
	got := Schemas()
	got[0] = 99
	if again := Schemas(); !slices.Equal(again, []int{ingressbundle.Schema2}) {
		t.Errorf("mutating the returned slice changed Schemas() to %v", again)
	}
	if Latest != ingressbundle.Schema2 {
		t.Errorf("Latest = %d, want %d", Latest, ingressbundle.Schema2)
	}

	in := goldenScenarios()["overlap"]
	want := mustRender(t, in)
	res, err := RenderSchema(ingressbundle.Schema2, in)
	if err != nil {
		t.Fatalf("RenderSchema(2): %v", err)
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("RenderSchema(2) differs from Render: ids %s and %s", res.Bundle.ID, want.Bundle.ID)
	}

	for _, schema := range []int{0, ingressbundle.Schema1, -1} { // schema 1 is the old schema this renderer no longer makes
		res, err := RenderSchema(schema, in)
		var unsupported *UnsupportedSchemaError
		if !errors.As(err, &unsupported) {
			t.Fatalf("RenderSchema(%d) error %T %v is not an *UnsupportedSchemaError", schema, err, err)
		}
		if !slices.Equal(unsupported.Offered, []int{schema}) || !slices.Equal(unsupported.Supported, []int{ingressbundle.Schema2}) {
			t.Errorf("RenderSchema(%d): offered %v supported %v, want [%d] and [2]", schema, unsupported.Offered, unsupported.Supported, schema)
		}
		if !strings.Contains(err.Error(), CodeSchemaUnsupported) {
			t.Errorf("error text %q lacks code %s", err, CodeSchemaUnsupported)
		}
		if res.Bundle.ID != "" || len(res.Bundle.Files) != 0 {
			t.Errorf("RenderSchema(%d) returned a bundle", schema)
		}
	}
}

func TestPickSchema(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offered []int
		want    int
	}{
		{"only 2", []int{2}, 2},
		{"1 and 2", []int{1, 2}, 2},
		{"unordered", []int{3, 2, 1}, 2},
		{"duplicates", []int{2, 2}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PickSchema(tc.offered)
			if err != nil || got != tc.want {
				t.Errorf("PickSchema(%v) = %d, %v; want %d", tc.offered, got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		offered []int
	}{
		{"only 1, the old schema", []int{ingressbundle.Schema1}},
		{"nil", nil},
		{"empty", []int{}},
		{"zero and negative", []int{0, -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PickSchema(tc.offered)
			var unsupported *UnsupportedSchemaError
			if !errors.As(err, &unsupported) {
				t.Fatalf("PickSchema(%v) = %d, %v; want an *UnsupportedSchemaError", tc.offered, got, err)
			}
			if !slices.Equal(unsupported.Supported, []int{ingressbundle.Schema2}) || len(unsupported.Offered) != len(tc.offered) {
				t.Errorf("error carries offered %v supported %v", unsupported.Offered, unsupported.Supported)
			}
			if got != 0 {
				t.Errorf("schema = %d with an error", got)
			}
		})
	}
}
