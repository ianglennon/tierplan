// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// fakeIngress is the ingress model a test hands the stack pipeline: it answers AttachFor with what it was given and
// counts the asks. The rest of the interface is not used by the paths these tests run.
type fakeIngress struct {
	attach *stacks.IngressAttach
	err    error
	asks   int
}

func (f *fakeIngress) AttachFor(context.Context, string, stacks.Target) (*stacks.IngressAttach, error) {
	f.asks++
	return f.attach, f.err
}

func (f *fakeIngress) World(context.Context, string, func(string) bool) (ingress.World, error) {
	return ingress.World{}, nil
}

func (f *fakeIngress) ReplaceStackRoutes(context.Context, ingressstore.ReplaceInput) (ingress.Plan, error) {
	return ingress.Plan{}, nil
}

func (f *fakeIngress) ReleaseStack(context.Context, string, string, string) error { return nil }
func (f *fakeIngress) Notify(string)                                              {}

// routedGlobal is a stack whose one service declares a route and runs globally, so a stop removes it and a start
// makes it again from the revision.
const routedGlobal = `services:
  web:
    image: nginx:1.27-alpine
    deploy:
      mode: global
      labels:
        tierplan.ingress.host: app.apps.example.com
        tierplan.ingress.port: "80"
`

// attachedBy is the attachment a fake ingress with ingress on answers.
func attachedBy() *stacks.IngressAttach { return &stacks.IngressAttach{Network: ingress.NetworkName} }

// networkNames are the networks of a service spec by the target a conversion gave them (a name, before the deploy
// resolves it to an ID).
func joins(networks []swarm.NetworkAttachmentConfig, name string) (joined, aliased bool) {
	for _, n := range networks {
		if n.Target == name {
			return true, len(n.Aliases) > 0
		}
	}
	return false, false
}

// TestStartRecreatesWithIngressAttachment is D-38 and Pitfall 3: a routed service that a stop removed is made again
// by a start with the shared ingress network among its networks, though the revision it came from was deployed
// without it.
func TestStartRecreatesWithIngressAttachment(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{
			ID: "net-tierplan-ingress", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm",
			Labels: map[string]string{ingress.LabelComponent: "network"},
		})
	})
	st, store := rig.deployedStack(t, "shop", routedGlobal, nil)
	if stopRes, _ := rig.stop(t, st, store); !stopRes.OK() || !slices.Equal(stopRes.Removed, []string{"shop_web"}) {
		t.Fatalf("stop = %+v, want the routed service removed", stopRes)
	}
	creates := len(rig.fake.ServiceCreates())

	rig.deps.Ingress = &fakeIngress{attach: attachedBy()}
	res, job := rig.start(t, st, store)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("start = %+v %v, want it done", res, itemStates(job))
	}
	made := rig.fake.ServiceCreates()[creates:]
	if len(made) != 1 {
		t.Fatalf("start created %d services, want 1", len(made))
	}
	var targets []string
	for _, n := range made[0].Spec.TaskTemplate.Networks {
		targets = append(targets, n.Target)
	}
	if !slices.ContainsFunc(made[0].Spec.TaskTemplate.Networks, func(n swarm.NetworkAttachmentConfig) bool {
		return n.Target == ingress.NetworkName || n.Target == "net-tierplan-ingress"
	}) {
		t.Errorf("recreated service networks = %v, want the ingress network among them", targets)
	}
}

// revisionOf is the revision of a stack the tests prepare a roll back to.
func revisionOf(t *testing.T, name, composeText string) (stackops.Stack, stackops.Snapshot) {
	t.Helper()
	st, in := checkedStackInput(t, name, composeText, nil)
	return st, stackops.Snapshot{ID: "rev-1", StackID: st.ID, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood, Copy: in.Copy}
}

// planNetworks are the networks the prepared plan gives the named service.
func planNetworks(t *testing.T, plan stackops.RollbackPlan, service string) []swarm.NetworkAttachmentConfig {
	t.Helper()
	for _, sp := range plan.Plan.Services {
		if sp.Name == service {
			return sp.Spec.TaskTemplate.Networks
		}
	}
	t.Fatalf("the plan has no service %q", service)
	return nil
}

// pinsFor is a stored image for each service, as a revision records them.
func pinsFor(services ...string) map[string]string {
	out := map[string]string{}
	for _, s := range services {
		out[s] = "nginx:1.27-alpine@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}
	return out
}

// TestPrepareRollbackAttaches is D-38: the plan converts the routed service with the attachment when the ingress
// model answers with one, and without it when no ingress is wired.
func TestPrepareRollbackAttaches(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, rev := revisionOf(t, "shop", routedGlobal)
	pins := pinsFor("shop_web")
	ctx := testsupport.Context(t)

	bare, err := rig.deps.PrepareRollbackWith(ctx, st, rev, pins, nil)
	if err != nil {
		t.Fatalf("PrepareRollbackWith without ingress: %v", err)
	}
	if joined, _ := joins(planNetworks(t, bare, "shop_web"), ingress.NetworkName); joined {
		t.Errorf("with no ingress wired the routed service joins %s: %+v", ingress.NetworkName, planNetworks(t, bare, "shop_web"))
	}

	fi := &fakeIngress{attach: attachedBy()}
	rig.deps.Ingress = fi
	got, err := rig.deps.PrepareRollbackWith(ctx, st, rev, pins, nil)
	if err != nil {
		t.Fatalf("PrepareRollbackWith with ingress: %v", err)
	}
	joined, aliased := joins(planNetworks(t, got, "shop_web"), ingress.NetworkName)
	if !joined || aliased {
		t.Errorf("networks = %+v, want %s joined with no alias", planNetworks(t, got, "shop_web"), ingress.NetworkName)
	}
	if fi.asks != 1 {
		t.Errorf("AttachFor asked %d times, want once", fi.asks)
	}

	// Ingress wired but off for the environment: AttachFor answers nil and nothing is attached.
	rig.deps.Ingress = &fakeIngress{}
	off, err := rig.deps.PrepareRollbackWith(ctx, st, rev, pins, nil)
	if err != nil {
		t.Fatalf("PrepareRollbackWith with ingress off: %v", err)
	}
	if joined, _ := joins(planNetworks(t, off, "shop_web"), ingress.NetworkName); joined {
		t.Errorf("with ingress off the routed service joins %s", ingress.NetworkName)
	}
}

// TestPrepareRollbackRefusesOnAttachReadError is T-24-18: a read of the ingress switch that fails refuses the prepare
// at its own step instead of converting without the attachment.
func TestPrepareRollbackRefusesOnAttachReadError(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, rev := revisionOf(t, "shop", routedGlobal)
	rig.deps.Ingress = &fakeIngress{err: errors.New("the ingress store is unreadable")}

	plan, err := rig.deps.PrepareRollbackWith(testsupport.Context(t), st, rev, pinsFor("shop_web"), nil)
	var pe *stackops.PrepareError
	if !errors.As(err, &pe) || pe.Stage != stackops.PrepareIngress {
		t.Fatalf("err = %v, want a *PrepareError at step %q", err, stackops.PrepareIngress)
	}
	if plan.Loaded != nil || len(plan.Plan.Services) != 0 {
		t.Errorf("plan = %+v, want none", plan)
	}
}
