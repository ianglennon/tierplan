// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresswire

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

func TestParseNodeMessageClosedSet(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		want    any
		wantErr error
	}{
		{"hello", `{"t":"hello","version":"0.2.0","schemas":[1,2],"appliedId":"abc"}`, Hello{T: TypeHello, Version: "0.2.0", Schemas: []int{1, 2}, AppliedID: "abc"}, nil},
		{"applied", `{"t":"applied","id":"abc"}`, Applied{T: TypeApplied, ID: "abc"}, nil},
		{"rejected", `{"t":"rejected","id":"abc","reason":"bad"}`, Rejected{T: TypeRejected, ID: "abc", Reason: "bad"}, nil},
		{"health", `{"t":"health","traefikOk":true}`, Health{T: TypeHealth, TraefikOK: true}, nil},
		{"hello with an unknown field", `{"t":"hello","version":"1","schemas":[1],"future":{"a":1}}`, Hello{T: TypeHello, Version: "1", Schemas: []int{1}}, nil},
		{"challenge", `{"t":"challenge"}`, nil, ErrUnknownMessage},
		{"welcome", `{"t":"welcome"}`, nil, ErrUnknownMessage},
		{"refuse", `{"t":"refuse"}`, nil, ErrUnknownMessage},
		{"bundle", `{"t":"bundle"}`, nil, ErrUnknownMessage},
		{"auth", `{"t":"auth"}`, nil, ErrUnknownMessage},
		{"a request for configuration", `{"t":"request_bundle"}`, nil, ErrUnknownMessage},
		{"a type in another case", `{"t":"Hello"}`, nil, ErrUnknownMessage},
		{"an empty type", `{"t":""}`, nil, ErrMalformed},
		{"no type", `{"version":"1"}`, nil, ErrMalformed},
		{"null", `null`, nil, ErrMalformed},
		{"an array", `[]`, nil, ErrMalformed},
		{"not JSON", `{"t":`, nil, ErrMalformed},
		{"empty", ``, nil, ErrMalformed},
		{"a known type with a wrong field type", `{"t":"hello","schemas":"one"}`, nil, ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseNodeMessage([]byte(c.line))
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				if got != nil {
					t.Fatalf("a refused message returned %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestParseNodeMessageErrorsAreDistinct(t *testing.T) {
	if errors.Is(ErrUnknownMessage, ErrMalformed) || errors.Is(ErrMalformed, ErrUnknownMessage) {
		t.Fatal("the two sentinels must be distinct")
	}
	if !strings.Contains(ErrUnknownMessage.Error(), "hello, applied, rejected and health") {
		t.Fatalf("ErrUnknownMessage text = %q", ErrUnknownMessage)
	}
}

func TestBundleEnvelopeNeverPrintsFileData(t *testing.T) {
	const marker = "SECRET-KEY-MARKER-do-not-log"
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: 1},
		[]ingressbundle.File{
			{Path: ingressbundle.DynamicFile, Data: []byte("http: {}\n")},
			{Path: ingressbundle.CertsDir + "/a.key", Data: []byte(marker)},
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := BundleEnvelope{T: TypeBundle, InstanceID: fixedInstanceID, Bundle: b}
	outputs := map[string]string{
		"%v":  fmt.Sprintf("%v", e),
		"%+v": fmt.Sprintf("%+v", e),
		"%#v": fmt.Sprintf("%#v", e),
		"%s":  fmt.Sprintf("%s", e), //nolint:staticcheck // S1025: the %s verb is what the test exercises
		"&%v": fmt.Sprintf("%v", &e),
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("envelope", "envelope", e)
	outputs["slog json"] = buf.String()
	buf.Reset()
	slog.New(slog.NewTextHandler(&buf, nil)).Info("envelope", "envelope", e)
	outputs["slog text"] = buf.String()
	for name, out := range outputs {
		if strings.Contains(out, marker) {
			t.Errorf("%s printed file data: %s", name, out)
		}
		if !strings.Contains(out, fixedInstanceID) {
			t.Errorf("%s lost the instance ID: %s", name, out)
		}
		if !strings.Contains(out, ingressbundle.CertsDir+"/a.key") {
			t.Errorf("%s lost the file path: %s", name, out)
		}
	}
}

func TestValidIdentity(t *testing.T) {
	for _, s := range []string{"", strings.Repeat("a", MaxIdentityLen+1), "a/b", "a{b}", "a b", "a\n", "é"} {
		if ValidIdentity(s) {
			t.Errorf("ValidIdentity(%q) = true", s)
		}
	}
	for _, s := range []string{"abcdefghijklmnopqrstuvwxy", "7XQ2:AAAA:BBBB:CCCC", "a_b.c-d", strings.Repeat("a", MaxIdentityLen)} {
		if !ValidIdentity(s) {
			t.Errorf("ValidIdentity(%q) = false", s)
		}
	}
}
