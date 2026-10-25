// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Ownership (plan 08-09, STK-12, D-09): one rule says what is a stack's, for a stack Bayplan holds and for
// one it found by its labels.

func TestOwnershipManagedIsExactID(t *testing.T) {
	t.Parallel()
	own := stackops.OwnershipOf(stackops.Stack{ID: "stk-1", Name: "web", Target: stacks.TargetSwarm})

	mine := map[string]string{stacks.LabelSwarmNamespace: "web", stacks.LabelStackID: "stk-1"}
	if !own.OwnsWorkload(mine) || !own.OwnsNetwork("web_default", mine) || !own.OwnsVolume("web_data", mine) {
		t.Errorf("a managed stack must own what carries its namespace and its ID")
	}
	noID := map[string]string{stacks.LabelSwarmNamespace: "web"}
	otherID := map[string]string{stacks.LabelSwarmNamespace: "web", stacks.LabelStackID: "stk-2"}
	for name, labels := range map[string]map[string]string{"no ID": noID, "another ID": otherID} {
		if own.OwnsWorkload(labels) || own.OwnsNetwork("web_default", labels) || own.OwnsVolume("web_data", labels) {
			t.Errorf("a managed stack must not own an object with %s", name)
		}
	}
	row := dockerx.ServiceRow{Stack: "web", Labels: mine}
	if !own.OwnsService(row) {
		t.Errorf("a managed stack must own a service of its namespace with its ID")
	}
	row.Labels = noID
	if own.OwnsService(row) {
		t.Errorf("a managed stack must not own a service of its namespace without its ID")
	}
	if stackops.OwnershipOf(stackops.Stack{Name: "web", Target: stacks.TargetSwarm}).OwnsWorkload(noID) {
		t.Errorf("a stored stack with no ID must own nothing, or an object with no ID would match it")
	}
}

func TestOwnershipDiscoveredOwnsTheLabel(t *testing.T) {
	t.Parallel()
	swarm := stackops.OwnershipOf(stackops.DiscoveredStack("env-1", "legacy", stacks.TargetSwarm))
	labelled := map[string]string{stacks.LabelSwarmNamespace: "legacy"}
	if !swarm.OwnsWorkload(labelled) || !swarm.OwnsNetwork("legacy_default", labelled) || !swarm.OwnsVolume("legacy_data", labelled) {
		t.Errorf("a discovered swarm stack must own everything under its namespace label")
	}
	if !swarm.OwnsService(dockerx.ServiceRow{Stack: "legacy"}) {
		t.Errorf("a discovered swarm stack must own a service of its namespace")
	}
	// An ID some object of it carries makes it no managed stack's: the ID is only ever a flag.
	withID := map[string]string{stacks.LabelSwarmNamespace: "legacy", stacks.LabelStackID: "gone"}
	if !swarm.OwnsWorkload(withID) {
		t.Errorf("a discovered stack must own an object whatever ID it carries")
	}

	host := stackops.OwnershipOf(stackops.DiscoveredStack("env-1", "shop", stacks.TargetStandalone))
	if !host.OwnsWorkload(map[string]string{stacks.LabelComposeProject: "shop"}) {
		t.Errorf("a discovered host stack must own the containers of its project")
	}
	if host.OwnsWorkload(map[string]string{stacks.LabelSwarmNamespace: "shop"}) {
		t.Errorf("a discovered host stack is named by the project label, not the namespace")
	}
}

func TestOwnershipRefusesAnotherNamespace(t *testing.T) {
	t.Parallel()
	for name, st := range map[string]stackops.Stack{
		"discovered swarm":   stackops.DiscoveredStack("env-1", "legacy", stacks.TargetSwarm),
		"managed swarm":      {ID: "stk-1", Name: "legacy", Target: stacks.TargetSwarm},
		"discovered host":    stackops.DiscoveredStack("env-1", "legacy", stacks.TargetStandalone),
		"discovered no name": stackops.DiscoveredStack("env-1", "", stacks.TargetSwarm),
		"managed host":       {ID: "stk-1", Name: "legacy", Target: stacks.TargetStandalone},
	} {
		own := stackops.OwnershipOf(st)
		other := map[string]string{
			stacks.LabelSwarmNamespace: "legacy2", stacks.LabelComposeProject: "legacy2", stacks.LabelStackID: "stk-1",
		}
		if own.OwnsWorkload(other) || own.OwnsNetwork("n", other) || own.OwnsVolume("v", other) || own.OwnsWorkload(nil) {
			t.Errorf("%s: must not own an object of another namespace or project, or one with no labels", name)
		}
		if own.OwnsService(dockerx.ServiceRow{Stack: "legacy2", Labels: other}) {
			t.Errorf("%s: must not own a service of another namespace", name)
		}
	}
}

// adoptedStack is a stack Bayplan took over: it owns the workloads of its namespace until its first deploy has
// labelled them, and the networks and volumes recorded at adoption for good (plan 08-13, D-11, D-18).
func adoptedStack(pending bool) stackops.Stack {
	return stackops.Stack{
		ID: "stk-1", Name: "legacy", Target: stacks.TargetSwarm,
		Source: stacks.Source{Kind: stacks.SourceAdopted, Ref: "legacy"},
		Adoption: stacks.Adoption{
			PriorID: "stk-old", Networks: []string{"legacy_back"}, Volumes: []string{"legacy_data"},
			Secrets: []string{"legacy_pw"}, Configs: []string{"legacy_conf"}, Pending: pending,
		},
	}
}

func TestOwnershipAdoptedPending(t *testing.T) {
	t.Parallel()
	own := stackops.OwnershipOf(adoptedStack(true))

	plain := map[string]string{stacks.LabelSwarmNamespace: "legacy"}
	prior := map[string]string{stacks.LabelSwarmNamespace: "legacy", stacks.LabelStackID: "stk-old"}
	mine := map[string]string{stacks.LabelSwarmNamespace: "legacy", stacks.LabelStackID: "stk-1"}
	for name, labels := range map[string]map[string]string{"no ID": plain, "the ID recorded at adoption": prior, "its own ID": mine} {
		if !own.OwnsWorkload(labels) || !own.OwnsService(dockerx.ServiceRow{Stack: "legacy", Labels: labels}) {
			t.Errorf("an adopted stack that was not deployed yet must own a workload with %s", name)
		}
	}

	// Networks and volumes: the names recorded at adoption, whatever their labels say; others only by the exact rule.
	if !own.OwnsNetwork("legacy_back", plain) || !own.OwnsVolume("legacy_data", nil) {
		t.Errorf("an adopted stack must own the network and the volume it recorded")
	}
	if own.OwnsNetwork("legacy_other", plain) || own.OwnsVolume("legacy_other", prior) {
		t.Errorf("an adopted stack must not own a network or a volume it did not record, even under its namespace or with the prior ID")
	}
	if !own.OwnsNetwork("legacy_new", mine) || !own.OwnsVolume("legacy_new", mine) {
		t.Errorf("an adopted stack must own an object that carries its namespace and its own ID")
	}
	for _, kind := range []string{"network", "volume", "secret", "config"} {
		name := map[string]string{"network": "legacy_back", "volume": "legacy_data", "secret": "legacy_pw", "config": "legacy_conf"}[kind]
		if !own.Knows(kind, name) || own.Knows(kind, "other") {
			t.Errorf("Knows(%s, %s) must be true and Knows(%s, other) false", kind, name, kind)
		}
	}
	if stackops.OwnershipOf(stackops.Stack{ID: "stk-2", Name: "web", Target: stacks.TargetSwarm}).Knows("network", "web_default") {
		t.Errorf("a stack that was not adopted must know no object")
	}
}

func TestOwnershipAdoptedAfterFirstDeploy(t *testing.T) {
	t.Parallel()
	own := stackops.OwnershipOf(adoptedStack(false))

	plain := map[string]string{stacks.LabelSwarmNamespace: "legacy"}
	prior := map[string]string{stacks.LabelSwarmNamespace: "legacy", stacks.LabelStackID: "stk-old"}
	mine := map[string]string{stacks.LabelSwarmNamespace: "legacy", stacks.LabelStackID: "stk-1"}
	if !own.OwnsWorkload(mine) {
		t.Errorf("a deployed adopted stack must own a workload that carries its own ID")
	}
	for name, labels := range map[string]map[string]string{"no ID": plain, "the prior ID": prior} {
		if own.OwnsWorkload(labels) || own.OwnsService(dockerx.ServiceRow{Stack: "legacy", Labels: labels}) {
			t.Errorf("a deployed adopted stack must not own a workload with %s: the first deploy labelled everything it owns", name)
		}
	}
	// The recorded objects stay the stack's for as long as it lives.
	if !own.OwnsNetwork("legacy_back", plain) || !own.OwnsVolume("legacy_data", plain) {
		t.Errorf("a deployed adopted stack must still own the network and the volume it recorded")
	}
}

// TestOwnershipNeverTrustsAnotherID: a bayplan.stack.id on an object never links it to a stack by itself (T-08-47).
func TestOwnershipNeverTrustsAnotherID(t *testing.T) {
	t.Parallel()
	for name, pending := range map[string]bool{"pending": true, "deployed": false} {
		own := stackops.OwnershipOf(adoptedStack(pending))
		stranger := map[string]string{stacks.LabelSwarmNamespace: "legacy", stacks.LabelStackID: "stk-9"}
		if own.OwnsWorkload(stranger) || own.OwnsService(dockerx.ServiceRow{Stack: "legacy", Labels: stranger}) ||
			own.OwnsNetwork("legacy_x", stranger) || own.OwnsVolume("legacy_x", stranger) {
			t.Errorf("%s: an adopted stack must not own an object that carries another stack's ID", name)
		}
		// The ID it carried before is no key to another namespace either.
		elsewhere := map[string]string{stacks.LabelSwarmNamespace: "other", stacks.LabelStackID: "stk-old"}
		if own.OwnsWorkload(elsewhere) || own.OwnsService(dockerx.ServiceRow{Stack: "other", Labels: elsewhere}) {
			t.Errorf("%s: the ID recorded at adoption must not link an object of another namespace", name)
		}
	}
	// An adopted stack with no prior ID and no stored ID of its own owns nothing it merely names.
	noPrior := adoptedStack(true)
	noPrior.Adoption.PriorID = ""
	own := stackops.OwnershipOf(noPrior)
	if own.OwnsWorkload(map[string]string{stacks.LabelSwarmNamespace: "legacy", stacks.LabelStackID: "stk-old"}) {
		t.Errorf("with no prior ID recorded, an object carrying an ID that is not the stack's must not be owned")
	}
	noID := adoptedStack(true)
	noID.ID = ""
	if stackops.OwnershipOf(noID).OwnsWorkload(map[string]string{stacks.LabelSwarmNamespace: "legacy"}) {
		t.Errorf("a stack with no ID must own nothing")
	}
}

// TestAfterTheFirstDeployANewUnlabelledServiceIsForeign: a workload with no bayplan.stack.id is the adopted stack's
// only while the adoption is pending. Once the first good deploy has cleared it, a service somebody adds to the
// namespace without the stack's ID is a name clash, not something to take over (T-08-60). The services here are the
// ones the CLI left, unlabelled, which is what such a service looks like to the stack.
func TestAfterTheFirstDeployANewUnlabelledServiceIsForeign(t *testing.T) {
	t.Parallel()
	a := adoptCorpus(t, nil)
	clash := func(rv stackops.Review) bool {
		return slices.ContainsFunc(rv.Problems, func(p stacks.Problem) bool { return p.Code == stackops.CodeNameClash })
	}

	if rv := a.review(t); clash(rv) {
		t.Fatalf("problems = %+v, want the services of a pending adoption to be the stack's", rv.Problems)
	}
	if err := a.repo.ClearAdoptionPending(testsupport.Context(t), a.stack.ID); err != nil {
		t.Fatal(err)
	}
	if rv := a.review(t); !clash(rv) {
		t.Fatalf("problems = %+v, want a name clash for services with no ID once the adoption is not pending", rv.Problems)
	}
}

// TestStandaloneOwnershipByProject: on a standalone host the name is the Compose project, and the same rule
// applies as on a swarm (plan 08-17, D-16, D-18). A managed stack owns what carries its project and its ID; an
// adopted one, until its first deploy, also the containers of its project with no ID or the prior one, and always
// the networks and volumes recorded at adoption. foreignLive, and the name-clash gate that reads the host, ask it.
func TestStandaloneOwnershipByProject(t *testing.T) {
	t.Parallel()
	project := func(id string) map[string]string {
		l := map[string]string{stacks.LabelComposeProject: "shop"}
		if id != "" {
			l[stacks.LabelStackID] = id
		}
		return l
	}
	managed := stackops.OwnershipOf(stackops.Stack{ID: "stk-1", Name: "shop", Target: stacks.TargetStandalone})
	if !managed.OwnsWorkload(project("stk-1")) || managed.OwnsWorkload(project("")) || managed.OwnsWorkload(project("stk-2")) {
		t.Errorf("a managed standalone stack must own the containers of its project that carry its ID, and no other")
	}
	if managed.OwnsWorkload(map[string]string{stacks.LabelSwarmNamespace: "shop", stacks.LabelStackID: "stk-1"}) {
		t.Errorf("a standalone stack is named by the Compose project: a swarm namespace label is not its name")
	}

	adopted := func(pending bool) stackops.Stack {
		return stackops.Stack{
			ID: "stk-1", Name: "shop", Target: stacks.TargetStandalone,
			Source:   stacks.Source{Kind: stacks.SourceAdopted, Ref: "shop"},
			Adoption: stacks.Adoption{PriorID: "stk-old", Networks: []string{"shop_back"}, Volumes: []string{"shop_data"}, Pending: pending},
		}
	}
	pending := stackops.OwnershipOf(adopted(true))
	for name, labels := range map[string]map[string]string{"no ID": project(""), "the prior ID": project("stk-old"), "its own ID": project("stk-1")} {
		if !pending.OwnsWorkload(labels) {
			t.Errorf("an adopted standalone stack that was not deployed yet must own a container with %s", name)
		}
	}
	if pending.OwnsWorkload(project("stk-2")) {
		t.Errorf("an ID of another stack never links a container to this one")
	}
	if !pending.OwnsNetwork("shop_back", project("")) || !pending.OwnsVolume("shop_data", nil) || pending.OwnsNetwork("shop_other", project("")) {
		t.Errorf("an adopted standalone stack must own the network and the volume it recorded, and no other without its ID")
	}
	deployed := stackops.OwnershipOf(adopted(false))
	if deployed.OwnsWorkload(project("")) || !deployed.OwnsWorkload(project("stk-1")) || !deployed.OwnsNetwork("shop_back", project("")) {
		t.Errorf("a deployed adopted standalone stack must own its labelled containers, not an unlabelled one, and still its recorded network")
	}
}

// TestStandaloneNameClashGateUsesTheOwnershipOfTheStack: the check that reads the host (ForeignStackOf) answers
// from the stack's ownership. A project docker compose made is no clash for the stack that adopted it until the
// first deploy has labelled it; a volume nobody recorded still is one (T-08-66).
func TestStandaloneNameClashGateUsesTheOwnershipOfTheStack(t *testing.T) {
	t.Parallel()
	rig := hostRig(t, stackopstest.Foreign(t, "shop", foreignAdoptable))
	repo := adoptRepo(t, rig)
	st, err := rig.adopt(t, repo, stacks.TargetStandalone, "shop", stackops.SelfMarks{})
	if err != nil {
		t.Fatalf("AdoptCommit: %v", err)
	}
	ctx := testsupport.Context(t)
	st, err = repo.Stack(ctx, rig.envID, st.ID)
	if err != nil {
		t.Fatal(err)
	}

	if foreign, err := rig.deps.ForeignStackOf(ctx, rig.envID, st); err != nil || foreign {
		t.Fatalf("ForeignStackOf(adopted, pending) = %v, %v, want the project it took over to be its own", foreign, err)
	}
	if foreign, err := rig.deps.ForeignStack(ctx, rig.envID, stacks.TargetStandalone, "shop", st.ID); err != nil || !foreign {
		t.Fatalf("ForeignStack(by ID) = %v, %v, want the unlabelled containers to count when only the ID is compared", foreign, err)
	}
	if err := repo.ClearAdoptionPending(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	st, err = repo.Stack(ctx, rig.envID, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if foreign, err := rig.deps.ForeignStackOf(ctx, rig.envID, st); err != nil || !foreign {
		t.Fatalf("ForeignStackOf(adopted, deployed) = %v, %v, want an unlabelled container to be a clash once the first deploy is done", foreign, err)
	}
}
