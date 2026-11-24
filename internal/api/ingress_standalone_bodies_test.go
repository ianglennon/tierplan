// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The bodies of a standalone environment's ingress routes (Phase 25, D-11, D-20, D-28). The fixture's standalone environment
// has one node whose agent is not connected, so nothing is read from a host: the test is about the shape of the bodies.

// bodyKeys decodes a JSON body into its top-level object.
func bodyKeys(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

// TestIngressStandaloneBodies: the enable preview of a standalone environment says plan.standalone is true, and one of a swarm
// says false; the status carries the keys objects.container and objects.volume as booleans, and no body carries a token.
func TestIngressStandaloneBodies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		standalone bool
	}{{"standalone", true}, {"swarm", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDeployFix(t, tc.standalone)
			base := "/api/env/" + f.envA + "/ingress"

			rec := f.credCall(t, f.adminA, http.MethodPost, base+"/enable/preview", `{}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
			}
			var prev struct {
				Creates struct {
					Standalone *bool `json:"standalone"`
				} `json:"creates"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &prev); err != nil {
				t.Fatalf("decode preview %s: %v", rec.Body.String(), err)
			}
			if prev.Creates.Standalone == nil || *prev.Creates.Standalone != tc.standalone {
				t.Errorf("creates.standalone = %v, want %v in %s", prev.Creates.Standalone, tc.standalone, rec.Body.String())
			}

			rec = f.credCall(t, f.adminA, http.MethodGet, base+"/status", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
			}
			var objects map[string]json.RawMessage
			if err := json.Unmarshal(bodyKeys(t, rec.Body.Bytes())["objects"], &objects); err != nil {
				t.Fatalf("decode objects of %s: %v", rec.Body.String(), err)
			}
			for _, key := range []string{"network", "service", "secret", "container", "volume"} {
				var v bool
				raw, ok := objects[key]
				if !ok {
					t.Errorf("status objects has no key %q: %s", key, rec.Body.String())
					continue
				}
				if err := json.Unmarshal(raw, &v); err != nil {
					t.Errorf("status objects.%s is not a boolean: %s", key, raw)
				}
			}
		})
	}
}
