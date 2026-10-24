// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
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
