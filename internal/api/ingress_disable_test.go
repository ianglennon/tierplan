// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The ingress switch-off routes (Phase 24, DEP-07, D-25, D-26). The swarm is a fake daemon; the deployer is the real one.

type disablePreviewResp struct {
	Findings []struct {
		Code, Severity, Message string
		Items                   []string
	} `json:"findings"`
	Removes struct {
		Service, Network string
		Secrets          []string
		Volumes          []struct{ NodeID, Name string }
	} `json:"removes"`
	Unreachable []string `json:"unreachable"`
}

// enableThroughTheRoute switches ingress on for environment A and waits for the job.
func (f *deployFix) enableThroughTheRoute(t *testing.T) {
	t.Helper()
	rec := f.credCall(t, f.adminA, http.MethodPost, "/api/env/"+f.envA+"/ingress/enable", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	start := decodeBody[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if _, err := f.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
		t.Fatalf("wait for the enable: %v", err)
	}
}

// TestDisableRoute is the tracer: an Admin previews what a switch-off removes, switches ingress off and follows the job to
// done with every object gone; an Operator is refused both routes (T-24-51); a standalone environment answers 409.
func TestDisableRoute(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	base := "/api/env/" + f.envA + "/ingress"
	f.enableThroughTheRoute(t)

	for _, path := range []string{base + "/disable/preview", base + "/disable"} {
		if rec := f.credCall(t, f.operatorA, http.MethodPost, path, ``); rec.Code != http.StatusForbidden {
			t.Errorf("Operator POST %s: %d %s, want 403", path, rec.Code, rec.Body)
		}
	}
	if list := f.hub.List(func(j jobs.Job) bool { return j.Kind == ingressdeploy.JobKindDisable }); len(list) != 0 {
		t.Fatalf("a refused caller started %d disable jobs", len(list))
	}

	rec := f.credCall(t, f.adminA, http.MethodPost, base+"/disable/preview", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("preview Cache-Control = %q, want no-store", cc)
	}
	pv := decodeBody[disablePreviewResp](t, rec)
	if len(pv.Findings) != 0 || pv.Removes.Service != ingress.ServiceName || pv.Removes.Network != ingress.NetworkName ||
		len(pv.Removes.Secrets) != 1 || len(pv.Removes.Volumes) != 1 || len(pv.Unreachable) != 0 {
		t.Errorf("preview = %+v", pv)
	}
	if _, _, err := f.cli.ServiceInspect(testsupport.Context(t), ingress.ServiceName); err != nil {
		t.Fatalf("the preview removed the proxy service: %v", err)
	}

	rec = f.credCall(t, f.adminA, http.MethodPost, base+"/disable", ``)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	start := decodeBody[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if start.JobID == "" {
		t.Fatalf("disable answered no job: %s", rec.Body)
	}
	if _, err := f.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
		t.Fatalf("wait for the job: %v", err)
	}
	rec = f.credCall(t, f.adminA, http.MethodGet, base+"/jobs/"+start.JobID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read the job: %d %s", rec.Code, rec.Body)
	}
	job := decodeBody[jobResp](t, rec)
	if job.Kind != "ingress_disable" || job.State != jobs.JobDone || len(job.Items) != 7 {
		t.Fatalf("job = %+v, want a done ingress_disable with seven items", job)
	}
	var ids []string
	for _, it := range job.Items {
		ids = append(ids, it.ID)
		if it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", it.ID, it.State, it.Code)
		}
	}
	if want := []string{"switch_off", "service", "volumes", "network", "secret", "token", "nodes"}; !slices.Equal(ids, want) {
		t.Errorf("items = %v, want %v", ids, want)
	}
	if _, _, err := f.cli.ServiceInspect(testsupport.Context(t), ingress.ServiceName); err == nil {
		t.Error("the proxy service is still on the swarm")
	}
	if secrets, err := f.cli.SecretList(testsupport.Context(t)); err != nil || len(secrets) != 0 {
		t.Errorf("secrets = %v, %v; want none", secrets, err)
	}
	if st, err := f.ingressStore.DeployState(testsupport.Context(t), f.envA); err != nil || st.Enabled {
		t.Errorf("DeployState = %+v, %v; want ingress off", st, err)
	}
	// A job of this kind is read through its stream like an enable job.
	if rec := f.credCall(t, f.adminA, http.MethodGet, base+"/jobs/"+start.JobID+"/stream", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: done") {
		t.Errorf("stream: %d %s", rec.Code, rec.Body)
	}

	// A standalone environment has no ingress to switch off.
	sf := newDeployFix(t, true)
	for _, path := range []string{"/api/env/" + sf.envA + "/ingress/disable/preview", "/api/env/" + sf.envA + "/ingress/disable"} {
		rec := sf.credCall(t, sf.adminA, http.MethodPost, path, ``)
		typ, _ := deployProblem(t, rec.Body.Bytes())
		if rec.Code != http.StatusConflict || typ != "ingress_standalone_unavailable" {
			t.Errorf("standalone POST %s: %d type %q", path, rec.Code, typ)
		}
	}
}

// TestDisableRouteRefusedWhileRoutesLive: the switch-off answers 409 ingress_routes_live listing each hostname and path with
// its stack in errors, the preview answers 200 with the same finding, and neither removes anything or starts a job.
func TestDisableRouteRefusedWhileRoutesLive(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	base := "/api/env/" + f.envA + "/ingress"
	f.enableThroughTheRoute(t)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('stack-1', ?, 'web', 'swarm', 'tester', 1)`, []any{f.envA}},
		{`INSERT INTO ingress_domains (id, env_id, suffix, issuer, created_by, created_at, updated_at) VALUES ('dom-1', ?, 'apps.example.com', 'staging', 'tester', 1, 1)`, []any{f.envA}},
		{`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at) VALUES ('shop.apps.example.com', ?, 'dom-1', 'stack', 'stack-1', 'alice', 1)`, []any{f.envA}},
		{`INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, created_at, updated_at) VALUES ('r1', ?, 'shop.apps.example.com', '', '/', 'web', 8080, 1, 1)`, []any{f.envA}},
	} {
		if _, err := f.st.Write.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rec := f.credCall(t, f.adminA, http.MethodPost, base+"/disable/preview", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	pv := decodeBody[disablePreviewResp](t, rec)
	if len(pv.Findings) != 1 || pv.Findings[0].Code != "ingress_routes_live" || pv.Findings[0].Severity != "error" ||
		!slices.Equal(pv.Findings[0].Items, []string{"shop.apps.example.com/ (stack web)"}) {
		t.Fatalf("preview findings = %+v, want ingress_routes_live naming the route and its stack", pv.Findings)
	}

	rec = f.credCall(t, f.adminA, http.MethodPost, base+"/disable", ``)
	p := decodeBody[struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
		Errors []struct {
			Location string
			Value    []string
		} `json:"errors"`
	}](t, rec)
	// The detail says once that ingress cannot be switched off: the finding's sentence already says so.
	if rec.Code != http.StatusConflict || p.Type != "ingress_routes_live" || !strings.Contains(p.Detail, "routes are live") ||
		strings.Count(p.Detail, "cannot be switched off") != 1 ||
		len(p.Errors) != 1 || p.Errors[0].Location != "ingress_routes_live" || !slices.Equal(p.Errors[0].Value, []string{"shop.apps.example.com/ (stack web)"}) {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if _, _, err := f.cli.ServiceInspect(testsupport.Context(t), ingress.ServiceName); err != nil {
		t.Errorf("a refused switch-off removed the proxy service: %v", err)
	}
	if list := f.hub.List(func(j jobs.Job) bool { return j.Kind == ingressdeploy.JobKindDisable }); len(list) != 0 {
		t.Errorf("a refusal started %d jobs", len(list))
	}
}
