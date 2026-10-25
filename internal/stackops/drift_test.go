// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Drift (plan 08-19, STK-14, D-21): the deployed revision, converted with its pins, against what the fake swarm runs.

const driftCompose = `services:
  web:
    image: nginx:1.27-alpine
    ports:
      - "8080:80"
    environment:
      MODE: prod
    deploy:
      replicas: 2
      labels:
        tier: front
      placement:
        constraints: ["node.role==manager"]
  api:
    image: nginx:1.28-alpine
  gone:
    image: nginx:1.29-alpine
`

// driftRig is a stack deployed from driftCompose, its store, and the input of a drift check of it.
type driftRig struct {
	*swarmRig
	st    stackops.Stack
	store *lifecycleStore
}

func newDriftRig(t *testing.T) *driftRig {
	t.Helper()
	rig := newSwarmRig(t, nil)
	st, store := rig.deployedStack(t, "drift", driftCompose, nil)
	return &driftRig{swarmRig: rig, st: st, store: store}
}

func (r *driftRig) input() stackops.DriftInput {
	rev := r.store.rev
	return stackops.DriftInput{EnvID: r.envID, Stack: r.st, Deployed: &rev, Pins: r.store.pins}
}

func (r *driftRig) drift(t *testing.T) stackops.DriftResult {
	t.Helper()
	res, err := r.deps.Drift(testsupport.Context(t), r.input())
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	return res
}

// editLive changes a deployed service as somebody outside Bayplan would, with an update of its spec.
func (r *driftRig) editLive(t *testing.T, service string, edit func(*swarm.ServiceSpec)) {
	t.Helper()
	svc, ok := r.ownedNow(t, r.st)[r.st.Name+"_"+service]
	if !ok {
		t.Fatalf("the swarm holds no service %s", service)
	}
	var spec swarm.ServiceSpec
	mustUnmarshal(t, svc.Spec, &spec)
	edit(&spec)
	err := r.deps.Aggregator.WriteOnManager(testsupport.Context(t), r.envID, func(c *dockerx.Client) error {
		_, err := c.ServiceUpdate(context.Background(), svc.ID, svc.Version, mustJSON(t, spec), "")
		return err
	})
	if err != nil {
		t.Fatalf("update %s: %v", service, err)
	}
}

func driftNames(res stackops.DriftResult) []string {
	var out []string
	for _, s := range res.Services {
		out = append(out, s.Change+" "+s.Short)
	}
	return out
}

func TestDriftOfWhatWasJustDeployedIsNone(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)

	res := rig.drift(t)

	if res.State != stackops.DriftNone || len(res.Services) != 0 || res.Count != 0 || res.Revision != 1 || res.CheckedAt.IsZero() {
		t.Fatalf("drift = %+v, want none at revision 1", res)
	}
	if !strings.HasPrefix(res.Fingerprint, "rev1,") {
		t.Errorf("fingerprint = %q, want it to name the revision and the services", res.Fingerprint)
	}
}

func TestDriftDetectsEachKind(t *testing.T) {
	t.Parallel()
	edits := []struct {
		name string
		edit func(*swarm.ServiceSpec)
		// shown is a text the running side of the row holds.
		shown string
	}{
		{"replicas", func(s *swarm.ServiceSpec) { n := uint64(3); s.Mode.Replicated.Replicas = &n }, `"Replicas":3`},
		{"image digest", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.ContainerSpec.Image = "nginx:1.27-alpine@sha256:" + strings.Repeat("e", 64)
		}, strings.Repeat("e", 64)},
		{"an environment value", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.ContainerSpec.Env = append(s.TaskTemplate.ContainerSpec.Env, "EXTRA=1")
		}, "EXTRA=1"},
		{"a port", func(s *swarm.ServiceSpec) { s.EndpointSpec.Ports[0].PublishedPort = 9090 }, `"PublishedPort":9090`},
		{"a label", func(s *swarm.ServiceSpec) { s.Labels["tier"] = "back" }, `"tier":"back"`},
		{"a mount", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.ContainerSpec.Mounts = append(s.TaskTemplate.ContainerSpec.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/data", Target: "/data"})
		}, "/srv/data"},
		{"a placement constraint", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.Placement.Constraints = append(s.TaskTemplate.Placement.Constraints, "node.labels.zone==a")
		}, "node.labels.zone==a"},
	}
	// Each kind is its own stack on its own fake swarm, judged in turn in this one test: a failure names the kind and
	// the others still report.
	judge := func(label string, check func(*driftRig) []string, prep func(*driftRig)) {
		rig := newDriftRig(t)
		prep(rig)
		for _, p := range check(rig) {
			t.Error(label + ": " + p)
		}
	}
	for _, tc := range edits {
		judge(tc.name, func(rig *driftRig) []string {
			res := rig.drift(t)
			if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"changed web"}) {
				return []string{fmt.Sprintf("drift = %s %v, want drifted with web changed", res.State, driftNames(res))}
			}
			var out []string
			svc := res.Services[0]
			if svc.Name != "drift_web" || svc.Diff.Settings < 1 || res.Count != svc.Diff.Settings {
				out = append(out, fmt.Sprintf("service = %+v count %d, want drift_web with its settings counted", svc, res.Count))
			}
			if !strings.Contains(string(svc.Diff.Current), tc.shown) {
				out = append(out, fmt.Sprintf("what runs now = %s, want it to hold %q", svc.Diff.Current, tc.shown))
			}
			if strings.Contains(string(svc.Diff.Next), tc.shown) {
				out = append(out, fmt.Sprintf("what was deployed = %s, want it without %q", svc.Diff.Next, tc.shown))
			}
			return out
		}, func(rig *driftRig) { rig.editLive(t, "web", tc.edit) })
	}

	judge("a missing service", func(rig *driftRig) []string {
		res := rig.drift(t)
		if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"missing gone"}) || res.Count != 1 {
			return []string{fmt.Sprintf("drift = %s %v count %d, want drifted with gone missing, counted once", res.State, driftNames(res), res.Count)}
		}
		if len(res.Services[0].Diff.Next) == 0 {
			return []string{"a missing service holds the spec the revision deployed"}
		}
		return nil
	}, func(rig *driftRig) {
		gone := rig.ownedNow(t, rig.st)["drift_gone"]
		err := rig.deps.Aggregator.WriteOnManager(testsupport.Context(t), rig.envID, func(c *dockerx.Client) error {
			return c.ServiceRemove(context.Background(), gone.ID)
		})
		if err != nil {
			t.Error(err)
		}
	})

	createService := func(owner string) func(*driftRig) {
		return func(rig *driftRig) {
			id := rig.st.ID
			if owner != "" {
				id = owner
			}
			spec := swarm.ServiceSpec{
				Annotations: swarm.Annotations{Name: "drift_extra", Labels: map[string]string{
					stacks.LabelSwarmNamespace: "drift", stacks.LabelStackID: id,
				}},
				TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox:1"}},
			}
			err := rig.deps.Aggregator.WriteOnManager(testsupport.Context(t), rig.envID, func(c *dockerx.Client) error {
				_, _, err := c.ServiceCreate(context.Background(), mustJSON(t, spec), "")
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}
	}
	judge("an extra owned service", func(rig *driftRig) []string {
		res := rig.drift(t)
		if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"extra extra"}) || res.Count != 1 {
			return []string{fmt.Sprintf("drift = %s %v count %d, want drifted with extra extra, counted once", res.State, driftNames(res), res.Count)}
		}
		return nil
	}, createService(""))
	judge("a service of the namespace that another owner made", func(rig *driftRig) []string {
		if res := rig.drift(t); res.State != stackops.DriftNone {
			return []string{fmt.Sprintf("drift = %s %v, want none: ownership decides what is the stack's", res.State, driftNames(res))}
		}
		return nil
	}, createService("someone-else"))
}

func TestDriftIgnoresNoise(t *testing.T) {
	t.Parallel()
	strip := func(labels map[string]string) {
		for k := range labels {
			if strings.HasPrefix(k, "bayplan.") {
				delete(labels, k)
			}
		}
	}

	forceUpdate := newDriftRig(t)
	forceUpdate.editLive(t, "web", func(s *swarm.ServiceSpec) { s.TaskTemplate.ForceUpdate += 3 })
	if res := forceUpdate.drift(t); res.State != stackops.DriftNone {
		t.Errorf("the force-update counter: drift = %s %v, want none: the counter is carried over as a deploy does", res.State, driftNames(res))
	}

	versions := newDriftRig(t)
	before := versions.drift(t)
	if !versions.fake.BumpService("drift_web") {
		t.Fatal("the fake has no service drift_web")
	}
	versions.fake.SetUpdateState("drift_web", "completed", "update completed")
	versions.fake.SetTaskState("drift_api", "running", "")
	res := versions.drift(t)
	if res.State != stackops.DriftNone {
		t.Errorf("the version index and the update status: drift = %s %v, want none", res.State, driftNames(res))
	}
	if res.Fingerprint == before.Fingerprint {
		t.Errorf("fingerprint = %q both times, want a service whose version moved to show", res.Fingerprint)
	}

	adopted := newDriftRig(t)
	for _, name := range []string{"web", "api", "gone"} {
		adopted.editLive(t, name, func(s *swarm.ServiceSpec) {
			strip(s.Labels)
			strip(s.TaskTemplate.ContainerSpec.Labels)
		})
	}
	// The labels are the owner's mark: with them gone only an adopted stack that is pending still owns the services.
	adopted.st.Source = stacks.Source{Kind: stacks.SourceAdopted, Ref: "drift"}
	adopted.st.Adoption = stacks.Adoption{Pending: true}
	if res := adopted.drift(t); res.State != stackops.DriftNone {
		t.Errorf("Bayplan's labels absent on an adopted stack that has not been deployed yet: drift = %s %v, want none: the labels say who owns a service, not how it is set up", res.State, driftNames(res))
	}
}

func TestDriftStoppedAndNotDeployed(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", func(s *swarm.ServiceSpec) { n := uint64(0); s.Mode.Replicated.Replicas = &n })
	before := len(rig.fake.Requests())

	in := rig.input()
	in.Stack.Stopped = true
	stopped, err := rig.deps.Drift(testsupport.Context(t), in)
	if err != nil {
		t.Fatal(err)
	}
	in = rig.input()
	in.Deployed = nil
	notDeployed, err := rig.deps.Drift(testsupport.Context(t), in)
	if err != nil {
		t.Fatal(err)
	}

	if stopped.State != stackops.DriftStopped || stopped.Revision != 1 || len(stopped.Services) != 0 {
		t.Errorf("a stopped stack = %+v, want stopped with nothing listed", stopped)
	}
	if notDeployed.State != stackops.DriftNotDeployed || len(notDeployed.Services) != 0 {
		t.Errorf("a stack with no good revision = %+v, want not_deployed with nothing listed", notDeployed)
	}
	if n := len(rig.fake.Requests()); n != before {
		t.Errorf("the engine was asked %d more times, want neither state to read it", n-before)
	}
	if res := rig.drift(t); res.State != stackops.DriftDrifted {
		t.Errorf("the same stack running = %s, want drifted: the scale-down is only excused by a stop", res.State)
	}
}

func TestDriftOfARevisionThatCannotBeRedeployedIsUnknown(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	in := rig.input()
	in.Pins = map[string]string{} // the revision stored no image for any service

	res, err := rig.deps.Drift(testsupport.Context(t), in)

	if err != nil || res.State != stackops.DriftUnknown {
		t.Fatalf("drift = %+v, %v, want unknown and no error", res, err)
	}
}

func TestDriftChangesNothingOnTheEngine(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", func(s *swarm.ServiceSpec) { n := uint64(5); s.Mode.Replicated.Replicas = &n })
	writes, updates, creates := len(rig.fake.Writes()), len(rig.fake.ServiceUpdates()), len(rig.fake.ServiceCreates())
	lookups := len(rig.fake.DistributionRefs())

	if res := rig.drift(t); res.State != stackops.DriftDrifted {
		t.Fatalf("drift = %s, want drifted", res.State)
	}

	if got := rig.fake.Writes(); len(got) != writes {
		t.Errorf("a drift check wrote %v", got[writes:])
	}
	if len(rig.fake.ServiceUpdates()) != updates || len(rig.fake.ServiceCreates()) != creates {
		t.Error("a drift check updated or created a service")
	}
	if n := len(rig.fake.DistributionRefs()); n != lookups {
		t.Errorf("a drift check asked a registry %d times", n-lookups)
	}
}
