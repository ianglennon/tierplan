// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TierPlan's own route over the typed API (Phase 28, SELF-01, D-17, D-11). The deployer is the real one over the fake swarm of
// the deploy fixture, with ingress on in the first environment, TierPlan's public URL set and its own certificate renewing over
// HTTP-01, so a preview of a switch-on carries all five items.

const (
	selfRouteDomain   = "apps.example.org"
	selfRouteHost     = "tp.apps.example.org"
	selfRoutePublic   = "https://tierplan.example.org"
	selfRouteHostBody = `{"hostname":"` + selfRouteHost + `"}`
)

// selfRouteFix is the deploy fixture with what TierPlan's own route reads.
type selfRouteFix struct {
	*deployFix
	domainID string
}

// newSelfRouteFix builds the fixture. deps adjusts the deployer's dependencies after the route's own are set.
func newSelfRouteFix(t *testing.T, deps func(*deployFix, *ingressdeploy.Deps)) *selfRouteFix {
	t.Helper()
	df := newDeployFixDeps(t, false, nil, func(f *deployFix, d *ingressdeploy.Deps) {
		d.SelfURLs = func() ingressdeploy.SelfURLs {
			return ingressdeploy.SelfURLs{PublicURL: selfRoutePublic, ListenPort: 7480}
		}
		d.OwnHTTP01InUse = func(context.Context) (bool, error) { return true, nil }
		if deps != nil {
			deps(f, d)
		}
	})
	ctx := testsupport.Context(t)
	if _, err := df.ingressStore.SetEnabled(ctx, df.envA, true, "u-admin", nil); err != nil {
		t.Fatalf("switch ingress on: %v", err)
	}
	dom, err := df.ingressStore.CreateDomain(ctx, ingressstore.CreateDomainInput{
		EnvID: df.envA, Suffix: selfRouteDomain, Issuer: ingress.IssuerStaging, Actor: "admin",
	}, nil)
	if err != nil {
		t.Fatalf("create the domain: %v", err)
	}
	return &selfRouteFix{deployFix: df, domainID: dom.ID}
}

// claim stores TierPlan's own route in the first environment directly, with no job.
func (f *selfRouteFix) claim(t *testing.T) {
	t.Helper()
	if err := f.ingressStore.SetSystemClaim(testsupport.Context(t), ingressstore.SystemClaimInput{
		EnvID: f.envA, Hostname: selfRouteHost, DomainID: f.domainID, Service: "tierplan", Port: 7480, Actor: "admin",
	}, nil); err != nil {
		t.Fatalf("SetSystemClaim: %v", err)
	}
}

// selfPath is a self route path of the first environment.
func (f *selfRouteFix) selfPath(rest string) string { return "/api/env/" + f.envA + "/ingress/self" + rest }

// selfPreviewResp is the shape of a self route preview.
type selfPreviewResp struct {
	Items []struct {
		Code    string `json:"code"`
		State   string `json:"state"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Snippet *struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"snippet"`
	} `json:"items"`
	Host          string `json:"host"`
	DirectAddress string `json:"directAddress"`
}

func (r selfPreviewResp) codes() []string {
	out := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, it.Code)
	}
	return out
}

// startSelfJob calls a route that answers 202 with a job as u, checks no-store, waits for the job and reads it back through
// the ingress job route.
func (f *selfRouteFix) startSelfJob(t *testing.T, u credentialUser, method, path, body string) jobResp {
	t.Helper()
	rec := f.credCall(t, u, method, path, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%s %s: %d %s, want 202", method, path, rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("%s %s Cache-Control = %q, want no-store", method, path, cc)
	}
	start := decodeBody[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if start.JobID == "" {
		t.Fatalf("%s %s answered no job: %s", method, path, rec.Body)
	}
	if _, err := f.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
		t.Fatalf("wait for the job: %v", err)
	}
	rec = f.credCall(t, u, http.MethodGet, "/api/env/"+f.envA+"/ingress/jobs/"+start.JobID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read the job: %d %s", rec.Code, rec.Body)
	}
	job := decodeBody[jobResp](t, rec)
	if job.ID != start.JobID {
		t.Errorf("job id = %q, want %q", job.ID, start.JobID)
	}
	return job
}

// TestSelfRoutePreviewAndEnable is the tracer (D-17): an instance Admin previews switching TierPlan's own route on and gets the
// five items in order with the direct address, then switches it on and gets a job the ingress job route reads. Both answers are
// no-store, and the preview writes no claim.
func TestSelfRoutePreviewAndEnable(t *testing.T) {
	t.Parallel()
	f := newSelfRouteFix(t, nil)

	rec := f.credCall(t, f.admin, http.MethodPost, f.selfPath("/preview"), selfRouteHostBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("preview Cache-Control = %q, want no-store", cc)
	}
	pv := decodeBody[selfPreviewResp](t, rec)
	want := []string{
		ingressdeploy.ItemSelfHostname, ingressdeploy.ItemSelfCertificate, ingressdeploy.ItemSelfNetwork,
		ingressdeploy.ItemSelfOrigin, ingressdeploy.ItemSelfCarveOut,
	}
	if got := pv.codes(); len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("items = %v, want %v", got, want)
			}
		}
	}
	for _, it := range pv.Items {
		switch it.State {
		case ingressdeploy.SelfItemOK, ingressdeploy.SelfItemWarning, ingressdeploy.SelfItemRefused:
		default:
			t.Errorf("item %s state = %q", it.Code, it.State)
		}
		if it.Message == "" {
			t.Errorf("item %s has no message", it.Code)
		}
		if (it.State == ingressdeploy.SelfItemOK) != (it.Reason == "") {
			t.Errorf("item %s state %s reason %q: a reason goes with a warning or a refusal only", it.Code, it.State, it.Reason)
		}
	}
	if pv.Items[0].State != ingressdeploy.SelfItemOK {
		t.Errorf("hostname item = %+v, want ok", pv.Items[0])
	}
	if pv.Host != selfRouteHost {
		t.Errorf("host = %q, want %q", pv.Host, selfRouteHost)
	}
	if pv.DirectAddress != selfRoutePublic {
		t.Errorf("directAddress = %q, want %q", pv.DirectAddress, selfRoutePublic)
	}
	if _, held, err := f.ingressStore.SystemClaim(testsupport.Context(t)); err != nil || held {
		t.Fatalf("after the preview: claim held %v (%v), want none", held, err)
	}

	job := f.startSelfJob(t, f.admin, http.MethodPost, f.selfPath(""), selfRouteHostBody)
	if job.Kind != ingressdeploy.JobKindSelfRouteEnable {
		t.Errorf("job kind = %q, want %q", job.Kind, ingressdeploy.JobKindSelfRouteEnable)
	}
	if len(job.Items) == 0 || job.Items[0].ID != ingressdeploy.ItemSelfCheck {
		t.Errorf("job items = %+v, want self_check first", job.Items)
	}
}
