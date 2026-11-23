// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/proto/testsupport"
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

// A hostname with no certificate yet will be issued, and one whose certificate is ordered but cannot be served yet waits
// with a reason; the states are read through the certificate store and nothing of a certificate reaches the answer.
func TestStackReviewIngressCertificatesThroughTheStore(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "review-certs")
	if _, _, err := f.ingressCertStore.CreatePending(testsupport.Context(t), ingresscert.PendingInput{
		Hostname: "b.apps.example.com", EnvID: e.ID, DomainID: seed.domainID, Issuer: string(ingress.IssuerStaging), CreatedBy: "admin", NotRequested: true,
	}, nil); err != nil {
		t.Fatalf("queue a certificate: %v", err)
	}
	saveCompose(t, admin, st.ID, ingressCompose("a.apps.example.com", "b.apps.example.com"), "[]")

	rec := admin.do(http.MethodPost, "stacks/"+st.ID+"/review", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %.300s", rec.Code, rec.Body)
	}
	var got ingressReviewAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if got.Ingress == nil || len(got.Ingress.Certificates) != 2 {
		t.Fatalf("ingress = %+v, want two certificate rows", got.Ingress)
	}
	a, b := got.Ingress.Certificates[0], got.Ingress.Certificates[1]
	if a.Hostname != "a.apps.example.com" || a.State != "will_issue" || a.Reason != "" {
		t.Errorf("first row = %+v, want a.apps.example.com will_issue with no reason", a)
	}
	if b.Hostname != "b.apps.example.com" || b.State != "waiting" || b.Reason == "" {
		t.Errorf("second row = %+v, want b.apps.example.com waiting with a reason", b)
	}
	for _, leak := range []string{"PRIVATE KEY", "BEGIN CERTIFICATE", "keySealed", "chainPEM"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the review answer holds %q, want no certificate material", leak)
		}
	}
}

// With ingress off, the review warns that the routes will not serve, and still lists the routes and the certificates.
func TestStackReviewIngressOffWarns(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "review-off")
	saveCompose(t, admin, st.ID, ingressCompose("a.apps.example.com"), "[]")

	rv, code, raw := admin.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %.300s", code, raw)
	}
	var found bool
	for _, p := range rv.Problems {
		if p.Code == "ingress_disabled" {
			found = true
			if p.Severity != "warning" || p.Message != "Ingress is off for this environment. The routes will serve once it is switched on and the stack is redeployed." {
				t.Errorf("problem = %+v, want the warning with the UI sentence", p)
			}
		}
	}
	if !found {
		t.Errorf("problems = %+v, want ingress_disabled while ingress is off", rv.Problems)
	}
}

// A standalone host whose ingress is off warns the same way as a swarm (D-17, WR-B1): the stack claims a hostname the
// host will not serve, and the review says so.
func TestStackReviewIngressOffWarnsOnAStandaloneHost(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "review-off-standalone")
	if st.Target != "standalone" {
		t.Fatalf("stack target = %q, want standalone", st.Target)
	}
	saveCompose(t, admin, st.ID, ingressStandaloneCompose("a.apps.example.com"), "[]")

	rv, code, raw := admin.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %.300s", code, raw)
	}
	var found bool
	for _, p := range rv.Problems {
		if p.Code == "ingress_disabled" {
			found = true
			if p.Severity != "warning" {
				t.Errorf("problem = %+v, want a warning", p)
			}
		}
	}
	if !found {
		t.Errorf("problems = %+v, want ingress_disabled while ingress is off on a standalone host", rv.Problems)
	}
}
