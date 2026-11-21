// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The review answers what a deploy does to ingress (plan 24-22, D-22, DEP-03): the routes it adds, keeps and releases,
// through the existing review route, read against the claims the stack holds.

// ingressReviewAnswer is the ingress part of a review answer.
type ingressReviewAnswer struct {
	Ingress *struct {
		Routes []struct {
			Change, Hostname, Path, Service, Scheme, Note string
			Port                                          int
		} `json:"routes"`
		Certificates []struct {
			Hostname, State, Reason string
		} `json:"certificates"`
		Network []struct {
			Service, Change string
			ReplacesTasks   bool `json:"replacesTasks"`
		} `json:"network"`
	} `json:"ingress"`
}

func TestStackReviewIngressBody(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "review-ingress")

	// The stack holds a and c; the draft keeps a (the same route), adds b and drops c.
	f.deployGood(t, admin, st.ID, ingressCompose("a.apps.example.com", "c.apps.example.com"))
	saveCompose(t, admin, st.ID, ingressCompose("a.apps.example.com", "b.apps.example.com"), "[]")

	rec := admin.do(http.MethodPost, "stacks/"+st.ID+"/review", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %.300s", rec.Code, rec.Body)
	}
	var got ingressReviewAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if got.Ingress == nil {
		t.Fatalf("answer has no ingress section: %.600s", rec.Body)
	}
	type row struct{ change, host, path, service, scheme string }
	var rows []row
	for _, r := range got.Ingress.Routes {
		if r.Port != 80 {
			t.Errorf("route %+v, want port 80", r)
		}
		rows = append(rows, row{r.Change, r.Hostname, r.Path, r.Service, r.Scheme})
	}
	want := []row{
		{"kept", "a.apps.example.com", "/", "weba", "http"},
		{"added", "b.apps.example.com", "/", "webb", "http"},
		{"released", "c.apps.example.com", "/", "webb", "http"},
	}
	if len(rows) != len(want) {
		t.Fatalf("routes = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("route %d = %+v, want %+v", i, rows[i], want[i])
		}
	}

	// A stack with no ingress label gets no section at all: the field is left out.
	plain := newStack(t, admin, "review-plain")
	saveCompose(t, admin, plain.ID, "services:\n  web:\n    image: nginx:1.27\n", "[]")
	rec = admin.do(http.MethodPost, "stacks/"+plain.ID+"/review", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %.300s", rec.Code, rec.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["ingress"]; ok {
		t.Errorf("answer has an ingress section %s for a stack with no route, want the field omitted", raw["ingress"])
	}
}
