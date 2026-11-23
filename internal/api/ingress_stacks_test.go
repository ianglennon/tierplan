// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The standalone side of the ingress stacks adapter (Phase 25, D-16, D-18): the stacks of a standalone host to redeploy,
// with Attached read from the host's containers.

const standaloneStacksSuffix = "apps.example.com"

// standaloneContainer is a running container of a Compose project; on lists the networks it is attached to.
func standaloneContainer(id, project, service string, routed bool, on ...string) dockertest.FakeContainer {
	labels := map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": service}
	if routed {
		labels["tierplan.ingress.host"] = service + "." + project + "." + standaloneStacksSuffix
	}
	nets := map[string]dockertest.FakeEndpoint{}
	for _, n := range on {
		nets[n] = dockertest.FakeEndpoint{NetworkID: "net-" + n}
	}
	return dockertest.FakeContainer{
		ID: id, Names: []string{"/" + project + "-" + service + "-1"}, Image: "nginx", State: "running", Status: "Up",
		Labels: labels, Networks: nets,
	}
}

// standaloneStacksFix seeds four standalone stacks over a fake host: "attached" and "detached" hold a claim, "partial"
// holds none but declares labels on two services, and "plain" has neither. attached's one routed service is on the ingress
// network, detached's is not, and of partial's two only one is.
func standaloneStacksFix(t *testing.T) *deployFix {
	t.Helper()
	f := newDeployFixDeps(t, true, func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			standaloneContainer("c-att-web", "attached", "web", true, ingress.NetworkName, "attached_default"),
			standaloneContainer("c-att-db", "attached", "db", false, "attached_default"),
			standaloneContainer("c-det-web", "detached", "web", true, "detached_default"),
			standaloneContainer("c-par-a", "partial", "a", true, ingress.NetworkName),
			standaloneContainer("c-par-b", "partial", "b", true, "partial_default"),
			standaloneContainer("c-plain-web", "plain", "web", false, "plain_default"),
		)
	}, nil)
	f.register(t, f.adminA, f.envA, standaloneStacksSuffix)
	ctx := testsupport.Context(t)
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	good := func(name, compose string) stackops.Stack {
		t.Helper()
		st, _, err := repo.CreateStack(ctx, f.envA, name, stacks.TargetStandalone, "ada", stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}})
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
		return "services:\n  web:\n    image: nginx\n    labels:\n      - tierplan.ingress.host=" + host + "\n"
	}
	claim := func(st stackops.Stack, host string) {
		t.Helper()
		if _, err := f.ingressStore.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{
			EnvID: f.envA, StackID: st.ID, Actor: "ada",
			Decls: []ingress.Declaration{{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: 80}},
		}); err != nil {
			t.Fatalf("claim %s: %v", host, err)
		}
	}
	claim(good("attached", labelled("web.attached."+standaloneStacksSuffix)), "web.attached."+standaloneStacksSuffix)
	claim(good("detached", labelled("web.detached."+standaloneStacksSuffix)), "web.detached."+standaloneStacksSuffix)
	good("partial", "services:\n  a:\n    image: nginx\n    labels:\n      - tierplan.ingress.host=a.partial."+standaloneStacksSuffix+"\n  b:\n    image: nginx\n    labels:\n      - tierplan.ingress.host=b.partial."+standaloneStacksSuffix+"\n")
	good("plain", "services:\n  web:\n    image: nginx\n")
	return f
}

// TestIngressStacksStandaloneAttached: a standalone host's stacks that hold claims, or whose newest good revision declares
// labels, are listed by name; Attached is true only when every routed service has a container on the ingress network, and
// false when the host cannot be read or no host read is given.
func TestIngressStacksStandaloneAttached(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	f := standaloneStacksFix(t)
	repo := stackops.Repo{Store: f.st, Vault: f.v}

	// The host read is asked for once for the lot, on the environment under test.
	reads := 0
	host := func(_ context.Context, envID string, fn func(*dockerx.Client) error) error {
		reads++
		if envID != f.envA {
			t.Errorf("host read for environment %q, want %q", envID, f.envA)
		}
		return fn(f.cli)
	}
	swarmRead := func(context.Context, string, func(*dockerx.Client) error) error {
		t.Error("a standalone environment's stacks were read through the swarm")
		return nil
	}

	type want struct {
		name     string
		claims   bool
		attached bool
	}
	check := func(t *testing.T, refs []ingressdeploy.StackRef, wants []want) {
		t.Helper()
		if len(refs) != len(wants) {
			t.Fatalf("refs = %+v, want %d stacks", refs, len(wants))
		}
		for i, w := range wants {
			r := refs[i]
			if r.Name != w.name || r.HoldsClaims != w.claims || r.Attached != w.attached || r.ID == "" {
				t.Errorf("refs[%d] = %+v, want %s holds=%v attached=%v", i, r, w.name, w.claims, w.attached)
			}
		}
	}

	t.Run("read from the host", func(t *testing.T) {
		refs, err := api.NewIngressStacks(repo, f.ingressStore, swarmRead, host).WithRoutes(ctx, f.envA)
		if err != nil {
			t.Fatalf("WithRoutes: %v", err)
		}
		check(t, refs, []want{{"attached", true, true}, {"detached", true, false}, {"partial", false, false}})
		if reads != 1 {
			t.Errorf("the host was read %d times, want once for the lot", reads)
		}
	})

	t.Run("host unreadable", func(t *testing.T) {
		gone := func(context.Context, string, func(*dockerx.Client) error) error { return envs.ErrNodeUnavailable }
		refs, err := api.NewIngressStacks(repo, f.ingressStore, swarmRead, gone).WithRoutes(ctx, f.envA)
		if err != nil {
			t.Fatalf("WithRoutes: %v", err)
		}
		check(t, refs, []want{{"attached", true, false}, {"detached", true, false}, {"partial", false, false}})
	})

	t.Run("no host read", func(t *testing.T) {
		refs, err := api.NewIngressStacks(repo, f.ingressStore, swarmRead, nil).WithRoutes(ctx, f.envA)
		if err != nil {
			t.Fatalf("WithRoutes: %v", err)
		}
		check(t, refs, []want{{"attached", true, false}, {"detached", true, false}, {"partial", false, false}})
	})
}

// TestIngressStacksSwarmUnchanged: a swarm environment's stacks are listed and attached from the swarm as before, and the
// host read is never made for them.
func TestIngressStacksSwarmUnchanged(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	f := routedStacksFix(t, true)
	host := func(context.Context, string, func(*dockerx.Client) error) error {
		t.Error("a swarm environment's stacks were read through the host")
		return nil
	}
	refs, err := api.NewIngressStacks(stackops.Repo{Store: f.st, Vault: f.v}, f.ingressStore,
		func(ctx context.Context, _ string, fn func(*dockerx.Client) error) error { return fn(f.cli) }, host).WithRoutes(ctx, f.envA)
	if err != nil {
		t.Fatalf("WithRoutes: %v", err)
	}
	if len(refs) != 2 || refs[0].Name != "claimed" || !refs[0].HoldsClaims || !refs[0].Attached || refs[1].Name != "legacy" || refs[1].HoldsClaims || refs[1].Attached {
		t.Fatalf("refs = %+v, want claimed (claims, attached) and legacy (no claims, not attached)", refs)
	}
}
