// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TierPlan's ingress attachment (plan 24-06, D-20): a routed swarm service carries the shared ingress network from
// the deploy on, and no comparison, adoption or rebuild may see it as the user's own.

const (
	ingressNetID = "net-tierplan-ingress"

	attachedCompose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.apps.example.com
        tierplan.ingress.port: "80"
  worker:
    image: nginx:1.28
`
)

// ingressNetwork is the shared network as the swarm holds it.
func ingressNetwork() dockertest.FakeNetwork {
	return dockertest.FakeNetwork{
		ID: ingressNetID, Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm",
		Labels: map[string]string{ingress.LabelComponent: "network"},
	}
}

// attachedRig stands the converted compose text on a fake swarm as a TierPlan deploy with ingress on leaves it: the
// routed services carry the attachment the check injects, with network IDs as targets, and the ingress network
// exists. TierPlan's own labels are left off, as for any stack that was not made by a TierPlan deploy.
func attachedRig(t *testing.T, content string) *swarmRig {
	t.Helper()
	res := stacks.Check(testsupport.Context(t), stacks.CheckInput{
		Name: "shop", Target: stacks.TargetSwarm,
		Copy:    stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(content)}}},
		Ingress: &stacks.IngressAttach{Network: ingress.NetworkName},
	})
	if res.Loaded == nil || res.Problems.Errors() != 0 || res.Loaded.Ingress == nil || len(res.Loaded.Ingress.Services) == 0 {
		t.Fatalf("check = %+v, want a loaded project with the ingress network injected", res.Problems)
	}
	plan, err := convert.Convert(res.Loaded.Project, convert.Options{StackName: "shop", StackID: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	ns := map[string]string{stacks.LabelSwarmNamespace: "shop"}
	var services []dockertest.FakeService
	attached := 0
	for _, sp := range plan.Services {
		spec := sp.Spec
		spec.Labels = withoutTierPlanLabels(spec.Labels)
		cs := *spec.TaskTemplate.ContainerSpec
		cs.Labels = withoutTierPlanLabels(cs.Labels)
		cs.Image += "@" + rebuildDigest
		spec.TaskTemplate.ContainerSpec = &cs
		spec.TaskTemplate.Networks = slices.Clone(spec.TaskTemplate.Networks)
		for i, n := range spec.TaskTemplate.Networks {
			if n.Target == ingress.NetworkName {
				attached++
				spec.TaskTemplate.Networks[i].Target = ingressNetID
				continue
			}
			spec.TaskTemplate.Networks[i].Target = "net-" + n.Target
		}
		services = append(services, dockertest.FakeService{
			ID: "svc-" + sp.Name, Name: sp.Name, Mode: "replicated", Running: 1, Desired: 1, Labels: spec.Labels, Spec: &spec,
		})
	}
	if attached == 0 {
		t.Fatal("no converted service carries the ingress attachment: the fixture proves nothing")
	}
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		for _, n := range plan.Networks {
			if n.Name == ingress.NetworkName {
				continue
			}
			c.Networks = append(c.Networks, dockertest.FakeNetwork{
				ID: "net-" + n.Name, Name: n.Name, Driver: n.Driver, Scope: "swarm", Labels: ns, Attachable: n.Attachable, Internal: n.Internal,
			})
		}
		c.Networks = append(c.Networks, ingressNetwork())
	}, services...)
}

// TestAdoptRebuildRoundtripHasNoIngress is D-20 and T-24-13: a stack whose routed service carries the ingress
// attachment is adopted and rebuilt, and neither the definition the adoption stores nor the rebuilt file names the
// network, as a top-level network or under a service.
func TestAdoptRebuildRoundtripHasNoIngress(t *testing.T) {
	t.Parallel()
	rig := attachedRig(t, attachedCompose)
	ctx := testsupport.Context(t)

	live := rig.ownedNow(t, stackops.DiscoveredStack(rig.envID, "shop", stacks.TargetSwarm))
	var spec swarm.ServiceSpec
	mustUnmarshal(t, live["shop_web"].Spec, &spec)
	if !slices.ContainsFunc(spec.TaskTemplate.Networks, func(n swarm.NetworkAttachmentConfig) bool { return n.Target == ingressNetID }) {
		t.Fatalf("the live web service has attachments %+v, want the ingress network among them", spec.TaskTemplate.Networks)
	}

	rebuilt, err := rig.deps.RebuildLive(ctx, rig.envID, stackops.DiscoveredStack(rig.envID, "shop", stacks.TargetSwarm), stackops.RebuildOptions{Reveal: true})
	if err != nil {
		t.Fatalf("RebuildLive: %v", err)
	}
	if text := string(rebuilt.YAML); strings.Contains(text, ingress.NetworkName) {
		t.Errorf("the rebuilt file names %s:\n%s", ingress.NetworkName, text)
	}
	for _, o := range rebuilt.Objects {
		if o.Name == ingress.NetworkName {
			t.Errorf("objects = %+v, want the ingress network not among them", rebuilt.Objects)
		}
	}

	repo := adoptRepo(t, rig)
	st, err := rig.adopt(t, repo, stacks.TargetSwarm, "shop", stackops.SelfMarks{})
	if err != nil {
		t.Fatalf("AdoptCommit: %v", err)
	}
	rev, err := repo.Revision(ctx, st.ID, 1)
	if err != nil {
		t.Fatalf("Revision 1: %v", err)
	}
	if text := string(rev.Copy.Compose()); strings.Contains(text, ingress.NetworkName) {
		t.Errorf("the stored definition names %s:\n%s", ingress.NetworkName, text)
	}
	if rendered, err := repo.RevisionRendering(ctx, rev.ID); err != nil || strings.Contains(string(rendered), ingress.NetworkName) {
		t.Errorf("the stored rendering = %q, %v, want it without %s", rendered, err, ingress.NetworkName)
	}
}
