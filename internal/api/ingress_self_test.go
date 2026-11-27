// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
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
		// A job's wait for the ensure loop's report runs out at once: no ensure loop runs here.
		d.Clock = selfRouteClock{fixtureClock{f.clk}}
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

// selfRouteClock is the fixture's clock whose waits run out at once.
type selfRouteClock struct{ fixtureClock }

func (selfRouteClock) After(time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
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
func (f *selfRouteFix) selfPath(rest string) string {
	return "/api/env/" + f.envA + "/ingress/self" + rest
}

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

// selfRouteOp is one of the four self route operations.
type selfRouteOp struct {
	op, method, rest, body string
}

var selfRouteOps = []selfRouteOp{
	{op: "previewIngressSelfRoute", method: http.MethodPost, rest: "/preview", body: selfRouteHostBody},
	{op: "enableIngressSelfRoute", method: http.MethodPost, rest: "", body: selfRouteHostBody},
	{op: "previewIngressSelfRouteOff", method: http.MethodPost, rest: "/off/preview", body: `{}`},
	{op: "disableIngressSelfRoute", method: http.MethodDelete, rest: ""},
}

// instanceAdminDenials are the permission.denied rows of the instance-admin gate.
func (f *selfRouteFix) instanceAdminDenials(t *testing.T) []auditRowFull {
	t.Helper()
	var out []auditRowFull
	for _, r := range f.auditRows(t, audit.ActionPermissionDenied) {
		if r.Reason == "instance_admin_required" {
			out = append(out, r)
		}
	}
	return out
}

// TestSelfRouteNeedsAnInstanceAdmin (D-17, T-28-61, T-28-63): every self route operation is behind ingress.manage on the
// environment and then an instance Admin. A Viewer and an Operator are refused by the RBAC gate; the environment's Admin passes
// it and is refused 403 instance_admin_required with one permission.denied row; an anonymous caller gets 401; the instance Admin
// passes both gates.
func TestSelfRouteNeedsAnInstanceAdmin(t *testing.T) {
	t.Parallel()
	f := newSelfRouteFix(t, nil)
	viewer := f.asRoleOn(t, "self-viewer-a", f.envA, "viewer")

	for _, o := range selfRouteOps {
		path := f.selfPath(o.rest)
		if rec := f.call(t, authReq{method: o.method, path: path, body: o.body}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: anonymous got %d %s, want 401", o.op, rec.Code, rec.Body)
		}
		for name, u := range map[string]credentialUser{"Viewer": viewer, "Operator": f.operatorA} {
			rec := f.credCall(t, u, o.method, path, o.body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: a %s got %d %s, want 403", o.op, name, rec.Code, rec.Body)
				continue
			}
			if typ, _ := deployProblem(t, rec.Body.Bytes()); typ == "instance_admin_required" {
				t.Errorf("%s: a %s was refused by the instance-admin gate, want the ingress.manage gate first", o.op, name)
			}
		}

		before := len(f.instanceAdminDenials(t))
		rec := f.credCall(t, f.adminA, o.method, path, o.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: the environment's Admin got %d %s, want 403", o.op, rec.Code, rec.Body)
		}
		typ, detail := deployProblem(t, rec.Body.Bytes())
		if typ != "instance_admin_required" {
			t.Errorf("%s: problem type = %q, want instance_admin_required", o.op, typ)
		}
		if !strings.Contains(detail, "Only an instance admin can switch TierPlan's own route") {
			t.Errorf("%s: detail = %q", o.op, detail)
		}
		rows := f.instanceAdminDenials(t)
		if len(rows) != before+1 {
			t.Fatalf("%s: instance-admin denial rows = %d, want %d", o.op, len(rows), before+1)
		}
		last := rows[len(rows)-1]
		if last.Result != string(audit.ResultDenied) || last.TargetType != "route" || last.EnvID != f.envA ||
			last.TargetID != o.method+" /api/env/{envId}/ingress/self"+o.rest {
			t.Errorf("%s: denial row = %+v", o.op, last)
		}
	}
	// The gate runs before the body is read: a body that would not validate is still the gate's 403, never a 422.
	if rec := f.credCall(t, f.adminA, http.MethodPost, f.selfPath("/preview"), `{"hostname":7}`); rec.Code != http.StatusForbidden {
		t.Errorf("the environment's Admin with a malformed body got %d %s, want 403", rec.Code, rec.Body)
	} else if typ, _ := deployProblem(t, rec.Body.Bytes()); typ != "instance_admin_required" {
		t.Errorf("the environment's Admin with a malformed body: problem type %q, want instance_admin_required", typ)
	}
	if _, held, err := f.ingressStore.SystemClaim(testsupport.Context(t)); err != nil || held {
		t.Fatalf("after the refusals: claim held %v (%v), want none", held, err)
	}

	// The instance Admin passes both gates: what answers is the operation itself.
	for _, o := range selfRouteOps {
		rec := f.credCall(t, f.admin, o.method, f.selfPath(o.rest), o.body)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("%s: the instance admin got %d %s, want the gates passed", o.op, rec.Code, rec.Body)
		}
		if rec.Code == http.StatusAccepted {
			start := decodeBody[struct {
				JobID string `json:"jobId"`
			}](t, rec)
			if _, err := f.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
				t.Fatalf("wait for the job: %v", err)
			}
		}
	}
}

// unreachableEnvs is an Envs whose every read fails, as for an environment whose engine cannot be reached.
type unreachableEnvs struct{}

func (unreachableEnvs) Info(context.Context, string) (ingressdeploy.EnvInfo, error) {
	return ingressdeploy.EnvInfo{}, errors.New("the environment's engine cannot be reached")
}

// TestSelfRouteDisableIsNeverRefusedByAState (D-11): with the route on and the environment's engine unreachable, the switch-off
// preview answers its two items and the switch-off answers 202 and releases the route; with no route on, both answer 409
// self_route_not_on.
func TestSelfRouteDisableIsNeverRefusedByAState(t *testing.T) {
	t.Parallel()
	f := newSelfRouteFix(t, func(_ *deployFix, d *ingressdeploy.Deps) { d.Envs = unreachableEnvs{} })
	f.claim(t)

	rec := f.credCall(t, f.admin, http.MethodPost, f.selfPath("/off/preview"), `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("off preview: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("off preview Cache-Control = %q, want no-store", cc)
	}
	pv := decodeBody[selfPreviewResp](t, rec)
	if got := pv.codes(); len(got) != 2 || got[0] != ingressdeploy.ItemSelfOriginRemoved || got[1] != ingressdeploy.ItemSelfNetworkRemoved {
		t.Fatalf("off preview items = %v, want [self_origin_removed self_network_removed]", got)
	}
	for _, it := range pv.Items {
		if it.State != ingressdeploy.SelfItemOK || it.Message == "" {
			t.Errorf("off preview item = %+v, want ok with a sentence", it)
		}
	}
	if pv.Host != selfRouteHost {
		t.Errorf("off preview host = %q, want %q", pv.Host, selfRouteHost)
	}

	job := f.startSelfJob(t, f.admin, http.MethodDelete, f.selfPath(""), "")
	if job.Kind != ingressdeploy.JobKindSelfRouteDisable {
		t.Errorf("job kind = %q, want %q", job.Kind, ingressdeploy.JobKindSelfRouteDisable)
	}
	if len(job.Items) == 0 || job.Items[0].ID != ingressdeploy.ItemSelfRelease || job.Items[0].State != "done" {
		t.Errorf("job items = %+v, want self_release done first", job.Items)
	}
	if _, held, err := f.ingressStore.SystemClaim(testsupport.Context(t)); err != nil || held {
		t.Fatalf("after the switch-off: claim held %v (%v), want none", held, err)
	}

	for _, o := range []selfRouteOp{selfRouteOps[2], selfRouteOps[3]} {
		rec := f.credCall(t, f.admin, o.method, f.selfPath(o.rest), o.body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s with no route on: %d %s, want 409", o.op, rec.Code, rec.Body)
		}
		if typ, detail := deployProblem(t, rec.Body.Bytes()); typ != "self_route_not_on" || detail != "TierPlan's own route is not on." {
			t.Errorf("%s with no route on: problem %q %q, want self_route_not_on", o.op, typ, detail)
		}
	}
}
