// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/hostpath"
)

// vectorName is the S4 vector that holds the spec every validator accepted.
const vectorName = "tierplan-ingress proxy spec"

// vectorInput is the inputs of the S4 vector.
func vectorInput() ingressdeploy.SpecInput {
	return ingressdeploy.SpecInput{
		Image:      "ghcr.io/ianglennon/tierplan-ingress:edge",
		URL:        "wss://tierplan.example:7480/ingress/connect",
		Pin:        "sha256/AAAA",
		SecretName: "tierplan-ingress-token-g1-abc123def456-0123abcd",
	}
}

// asTree marshals v and decodes it again into maps, slices and numbers, so two values compare by
// content and not by field order.
func asTree(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// firstDiff returns the path of the first difference between two decoded JSON trees, or "" when they
// are equal. Map keys are visited in sorted order so the answer is the same on every run.
func firstDiff(path string, a, b any) string {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return path + " (type)"
		}
		keys := slices.Sorted(maps.Keys(av))
		for k := range bv {
			if _, ok := av[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range slices.Compact(keys) {
			x, inA := av[k]
			y, inB := bv[k]
			switch {
			case !inA:
				return fmt.Sprintf("%s.%s only in the vector", path, k)
			case !inB:
				return fmt.Sprintf("%s.%s only in the builder", path, k)
			}
			if d := firstDiff(path+"."+k, x, y); d != "" {
				return d
			}
		}
		return ""
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return fmt.Sprintf("%s (length or type: builder %v, vector %v)", path, a, b)
		}
		for i := range av {
			if d := firstDiff(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i]); d != "" {
				return d
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(a, b) {
			return fmt.Sprintf("%s: builder %v, vector %v", path, a, b)
		}
		return ""
	}
}

// proxySpec builds the spec for in.
func proxySpec(in ingressdeploy.SpecInput) swarm.ServiceSpec {
	return ingressdeploy.ProxyServiceSpec(in)
}

// TestProxySpecMatchesS4Vector is the tracer: the spec the deployer sends is the spec every validator
// (the server's policy, the agent's proxy, the RBAC walker) was shown and accepted.
func TestProxySpecMatchesS4Vector(t *testing.T) {
	var vector *hostpath.SpecVector
	for _, v := range hostpath.Vectors() {
		if v.Name == vectorName {
			vector = &v
			break
		}
	}
	if vector == nil {
		t.Fatalf("vector %q not found in proto/hostpath/testdata/service-specs.json", vectorName)
	}
	if vector.Escalates {
		t.Fatalf("vector %q escalates; the proxy spec must not", vectorName)
	}
	var want any
	if err := json.Unmarshal(vector.Spec, &want); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	got := asTree(t, proxySpec(vectorInput()))
	if d := firstDiff("$", got, want); d != "" {
		t.Errorf("the builder's spec differs from the S4 vector at %s", d)
	}
}
