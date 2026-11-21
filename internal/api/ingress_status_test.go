// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The ingress status, access-log and resume routes (Phase 24, D-10, D-30, D-42).

type statusResp struct {
	Enabled      bool `json:"enabled"`
	Paused       bool `json:"paused"`
	AccessLogs   bool `json:"accessLogs"`
	ObjectsKnown bool `json:"objectsKnown"`
	Objects      struct {
		Network, Service, Secret bool
	} `json:"objects"`
	Nodes struct {
		Expected, Enrolled, Ready int
	} `json:"nodes"`
	LastEnsure struct {
		At, Class string
	} `json:"lastEnsure"`
	PendingWipes        int  `json:"pendingWipes"`
	DisablingIncomplete bool `json:"disablingIncomplete"`
	TokenDisabled       bool `json:"tokenDisabled"`
	Redeploy            []struct {
		StackID, StackName string
		HoldsClaims        bool
	} `json:"redeploy"`
}

// enableViaRoute switches ingress on through the route and waits for the job.
func (f *deployFix) enableViaRoute(t *testing.T) {
	t.Helper()
	rec := f.credCall(t, f.adminA, http.MethodPost, "/api/env/"+f.envA+"/ingress/enable", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	start := decodeBody[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if _, err := f.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
		t.Fatalf("wait for the enable job: %v", err)
	}
}

// TestIngressStatusRoute: an Admin reads the status of a new environment and of an enabled one, the body carries no token
// or secret field, an Operator is refused and a standalone environment is a 409.
func TestIngressStatusRoute(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	path := "/api/env/" + f.envA + "/ingress/status"

	rec := f.credCall(t, f.adminA, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	st := decodeBody[statusResp](t, rec)
	if st.Enabled || st.Paused || st.AccessLogs || !st.ObjectsKnown || st.Objects.Network || st.Objects.Service || st.Objects.Secret || st.DisablingIncomplete {
		t.Errorf("a new environment's status = %+v, want everything off (accessLogs false by default)", st)
	}
	if st.Nodes.Expected != 1 || st.Redeploy == nil {
		t.Errorf("nodes = %+v, redeploy = %v; want one expected node and an empty list", st.Nodes, st.Redeploy)
	}

	f.enableViaRoute(t)
	rec = f.credCall(t, f.adminA, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status after enable: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	st = decodeBody[statusResp](t, rec)
	if !st.Enabled || !st.Objects.Network || !st.Objects.Service || !st.Objects.Secret || st.TokenDisabled {
		t.Errorf("enabled status = %+v, want on with all three objects", st)
	}
	for _, banned := range []string{"\"token\":", "\"secretName\"", "\"secretId\"", "\"password\"", "\"joinToken\""} {
		if strings.Contains(body, banned) {
			t.Errorf("the status body carries %s: %s", banned, body)
		}
	}

	if rec := f.credCall(t, f.operatorA, http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
		t.Errorf("an Operator's status read = %d, want 403", rec.Code)
	}
	if rec := f.credCall(t, f.adminB, http.MethodGet, path, ""); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("another environment's Admin's status read = %d, want 403 or 404", rec.Code)
	}

	sa := newDeployFix(t, true)
	rec = sa.credCall(t, sa.adminA, http.MethodGet, "/api/env/"+sa.envA+"/ingress/status", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("standalone status: %d %s, want 409", rec.Code, rec.Body)
	}
	if typ, _ := deployProblem(t, rec.Body.Bytes()); !strings.HasSuffix(typ, "ingress_standalone_unavailable") {
		t.Errorf("standalone problem type = %q, want ingress_standalone_unavailable", typ)
	}
}

const (
	stacksTestNet    = "net-ingress-1"
	stacksTestSuffix = "apps.example.com"
)

// routedStacksFix is a deploy fixture whose environment holds three swarm stacks: "claimed" holds a hostname claim,
// "legacy" holds none but its newest good revision declares an ingress label, and "plain" has neither. The fake swarm
// runs a routed service for each of the first two, and holds the ingress network only when withNetwork is set; then
// claimed's service lists it and legacy's does not.
func routedStacksFix(t *testing.T, withNetwork bool) *deployFix {
	t.Helper()
	f := newDeployFixDeps(t, false, func(c *dockertest.FakeConfig) {
		claimed := dockertest.FakeService{
			ID: "svc-claimed", Name: "claimed_web", Mode: "replicated", Running: 1, Desired: 1,
			Labels: map[string]string{"com.docker.stack.namespace": "claimed", "tierplan.ingress.host": "claimed." + stacksTestSuffix},
		}
		legacy := dockertest.FakeService{
			ID: "svc-legacy", Name: "legacy_web", Mode: "replicated", Running: 1, Desired: 1,
			Labels: map[string]string{"com.docker.stack.namespace": "legacy", "tierplan.ingress.host": "legacy." + stacksTestSuffix},
		}
		if withNetwork {
			c.Networks = append(c.Networks, dockertest.FakeNetwork{
				ID: stacksTestNet, Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.4.0/22"},
			})
			claimed.Networks = []string{stacksTestNet}
		}
		c.ServiceRows = append(c.ServiceRows, claimed, legacy)
	}, func(f *deployFix, d *ingressdeploy.Deps) {
		repo := stackops.Repo{Store: f.st, Vault: f.v}
		read := func(ctx context.Context, _ string, fn func(*dockerx.Client) error) error { return fn(f.cli) }
		d.Stacks = api.NewIngressStacks(repo, f.ingressStore, read)
	})
	f.register(t, f.adminA, f.envA, stacksTestSuffix)
	ctx := testsupport.Context(t)
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	good := func(name, compose string) stackops.Stack {
		t.Helper()
		st, _, err := repo.CreateStack(ctx, f.envA, name, stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}})
		if err != nil {
			t.Fatalf("CreateStack %s: %v", name, err)
		}
		rev, err := repo.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
		if err != nil {
			t.Fatalf("NewRevision %s: %v", name, err)
		}
		if err := repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
			t.Fatalf("SetOutcome %s: %v", name, err)
		}
		return st
	}
	labelled := func(host string) string {
		return "services:\n  web:\n    image: nginx\n    deploy:\n      labels:\n        - tierplan.ingress.host=" + host + "\n"
	}
	claimed := good("claimed", labelled("claimed."+stacksTestSuffix))
	good("legacy", labelled("legacy."+stacksTestSuffix))
	good("plain", "services:\n  web:\n    image: nginx\n")
	if _, err := f.ingressStore.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{
		EnvID: f.envA, StackID: claimed.ID, Actor: "ada",
		Decls: []ingress.Declaration{{Service: "web", Host: "claimed." + stacksTestSuffix, Label: "tierplan.ingress.host", Port: 80}},
	}); err != nil {
		t.Fatalf("claim the hostname: %v", err)
	}
	return f
}

// TestIngressStacksToRedeploy: the adapter lists a stack that holds claims and one whose newest good revision declares
// ingress labels but holds none, never one with neither, ordered by name; Attached is false while the network is absent
// and, once it is there, true only for the stack whose routed service lists it; the enable preview's warning and the
// status name the same stacks.
func TestIngressStacksToRedeploy(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)

	t.Run("network absent", func(t *testing.T) {
		t.Parallel()
		f := routedStacksFix(t, false)
		refs, err := api.NewIngressStacks(stackops.Repo{Store: f.st, Vault: f.v}, f.ingressStore,
			func(ctx context.Context, _ string, fn func(*dockerx.Client) error) error { return fn(f.cli) }).WithRoutes(ctx, f.envA)
		if err != nil {
			t.Fatalf("WithRoutes: %v", err)
		}
		want := []ingressdeploy.StackRef{{Name: "claimed", HoldsClaims: true}, {Name: "legacy"}}
		if len(refs) != 2 {
			t.Fatalf("refs = %+v, want claimed and legacy", refs)
		}
		for i, r := range refs {
			if r.Name != want[i].Name || r.HoldsClaims != want[i].HoldsClaims || r.Attached || r.ID == "" {
				t.Errorf("refs[%d] = %+v, want %s holds=%v unattached", i, r, want[i].Name, want[i].HoldsClaims)
			}
		}

		rec := f.credCall(t, f.adminA, http.MethodPost, "/api/env/"+f.envA+"/ingress/enable/preview", `{}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview: %d %s", rec.Code, rec.Body)
		}
		pv := decodeBody[previewResp](t, rec)
		if len(pv.Redeploy) != 2 || pv.Redeploy[0].StackName != "claimed" || pv.Redeploy[1].StackName != "legacy" {
			t.Errorf("preview redeploy = %+v, want claimed and legacy", pv.Redeploy)
		}
		warned := false
		for _, fd := range pv.Findings {
			warned = warned || fd.Code == ingressdeploy.CodeRedeployNeeded
		}
		if !warned {
			t.Errorf("the preview carries no %s warning: %+v", ingressdeploy.CodeRedeployNeeded, pv.Findings)
		}
		rec = f.credCall(t, f.adminA, http.MethodGet, "/api/env/"+f.envA+"/ingress/status", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body)
		}
		st := decodeBody[statusResp](t, rec)
		if len(st.Redeploy) != 2 || st.Redeploy[0].StackName != "claimed" || st.Redeploy[1].StackName != "legacy" || !st.Redeploy[0].HoldsClaims || st.Redeploy[1].HoldsClaims {
			t.Errorf("status redeploy = %+v, want claimed (claims) and legacy (none)", st.Redeploy)
		}
	})

	t.Run("network present", func(t *testing.T) {
		t.Parallel()
		f := routedStacksFix(t, true)
		refs, err := api.NewIngressStacks(stackops.Repo{Store: f.st, Vault: f.v}, f.ingressStore,
			func(ctx context.Context, _ string, fn func(*dockerx.Client) error) error { return fn(f.cli) }).WithRoutes(ctx, f.envA)
		if err != nil {
			t.Fatalf("WithRoutes: %v", err)
		}
		if len(refs) != 2 || refs[0].Name != "claimed" || !refs[0].Attached || refs[1].Name != "legacy" || refs[1].Attached {
			t.Fatalf("refs = %+v, want claimed attached and legacy not", refs)
		}
		rec := f.credCall(t, f.adminA, http.MethodGet, "/api/env/"+f.envA+"/ingress/status", "")
		st := decodeBody[statusResp](t, rec)
		if len(st.Redeploy) != 1 || st.Redeploy[0].StackName != "legacy" {
			t.Errorf("status redeploy = %+v, want only legacy", st.Redeploy)
		}
	})
}
