// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"errors"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// A stack Bayplan does not manage is never taken over (plan 07-14, D-12): a name that is in use
// on the target, under the swarm namespace or the Compose project, by something without this
// stack's ID is a clash.

var (
	swarmNS    = map[string]string{"com.docker.stack.namespace": "web"}
	swarmOwned = map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stack-web"}
	swarmOther = map[string]string{"com.docker.stack.namespace": "blog", "bayplan.stack.id": "stack-blog"}
	projNS     = map[string]string{"com.docker.compose.project": "web", "com.docker.compose.service": "app"}
	projOwned  = map[string]string{"com.docker.compose.project": "web", "com.docker.compose.service": "app", "bayplan.stack.id": "stack-web"}
	projOther  = map[string]string{"com.docker.compose.project": "blog"}
)

func standalone(c *dockertest.FakeConfig) { c.SwarmState, c.SwarmControl, c.NodeRows = "", false, nil }

func TestForeignStackOnASwarmFindsWhateverCarriesTheNamespaceWithoutThisStacksID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tune    func(*dockertest.FakeConfig)
		svcs    []dockertest.FakeService
		stackID string
		want    bool
	}{
		"an empty swarm": {nil, nil, "stack-web", false},
		"a foreign service": {nil, []dockertest.FakeService{
			{ID: "s1", Name: "web_app", Mode: "replicated", Image: "x", Labels: swarmNS}}, "stack-web", true},
		"the stack's own service": {nil, []dockertest.FakeService{
			{ID: "s1", Name: "web_app", Mode: "replicated", Image: "x", Labels: swarmOwned}}, "stack-web", false},
		"another stack's service": {nil, []dockertest.FakeService{
			{ID: "s1", Name: "blog_app", Mode: "replicated", Image: "x", Labels: swarmOther}}, "stack-web", false},
		"a foreign network": {func(c *dockertest.FakeConfig) {
			c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "web_default", Driver: "overlay", Scope: "swarm", Labels: swarmNS}}
		}, nil, "stack-web", true},
		"the stack's own network": {func(c *dockertest.FakeConfig) {
			c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "web_default", Driver: "overlay", Scope: "swarm", Labels: swarmOwned}}
		}, nil, "stack-web", false},
		"a foreign secret": {func(c *dockertest.FakeConfig) {
			c.Secrets = []dockertest.FakeSecret{{ID: "k1", Name: "web_tok", Labels: swarmNS}}
		}, nil, "stack-web", true},
		"a foreign config": {func(c *dockertest.FakeConfig) {
			c.Configs = []dockertest.FakeConfigObject{{ID: "g1", Name: "web_conf", Labels: swarmNS, Data: []byte("x")}}
		}, nil, "stack-web", true},
		"the stack's own secret": {func(c *dockertest.FakeConfig) {
			c.Secrets = []dockertest.FakeSecret{{ID: "k1", Name: "web_tok", Labels: swarmOwned}}
		}, nil, "stack-web", false},
		"a stack being created finds any object of the name": {nil, []dockertest.FakeService{
			{ID: "s1", Name: "web_app", Mode: "replicated", Image: "x", Labels: swarmOwned}}, "", true},
	} {
		rig := newSwarmRig(t, tc.tune, tc.svcs...)
		got, err := rig.deps.ForeignStack(testsupport.Context(t), rig.envID, stacks.TargetSwarm, "web", tc.stackID)
		if err != nil || got != tc.want {
			t.Errorf("%s: ForeignStack = %v, %v, want %v", name, got, err, tc.want)
		}
	}
}

func TestForeignStackOnAStandaloneHostFindsAComposeProjectOfTheNameWithoutThisStacksID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tune func(*dockertest.FakeConfig)
		want bool
	}{
		"an empty host": {standalone, false},
		"a foreign container": {func(c *dockertest.FakeConfig) {
			standalone(c)
			c.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/web-app-1"}, Image: "x", State: "exited", Labels: projNS}}
		}, true},
		"the stack's own container": {func(c *dockertest.FakeConfig) {
			standalone(c)
			c.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/web-app-1"}, Image: "x", State: "running", Labels: projOwned}}
		}, false},
		"another project": {func(c *dockertest.FakeConfig) {
			standalone(c)
			c.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/blog-app-1"}, Image: "x", State: "running", Labels: projOther}}
		}, false},
		"a foreign volume": {func(c *dockertest.FakeConfig) {
			standalone(c)
			c.Volumes = []dockertest.FakeVolume{{Name: "web_data", Driver: "local", Labels: projNS}}
		}, true},
		"a foreign network": {func(c *dockertest.FakeConfig) {
			standalone(c)
			c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "web_default", Driver: "bridge", Scope: "local", Labels: projNS}}
		}, true},
	} {
		rig := newSwarmRig(t, tc.tune)
		got, err := rig.deps.ForeignStack(testsupport.Context(t), rig.envID, stacks.TargetStandalone, "web", "stack-web")
		if err != nil || got != tc.want {
			t.Errorf("%s: ForeignStack = %v, %v, want %v", name, got, err, tc.want)
		}
	}
}

// TestForeignStackOfCountsOnlyWhatTheHostCLICouldNotHaveAdded: a container the host CLI adds to a managed standalone
// stack's Compose project (docker compose up --scale) carries the project and no bayplan.stack.id. Beside a container
// that carries the stack's ID and runs the same service it is no name clash; another stack's ID, a project the stack holds
// no container of, or a service no container of the stack runs, still is (G-08-6, WR-03, user decisions 2026-10-27;
// P7 D-12, T-08-47, T-08-107, T-08-113).
func TestForeignStackOfCountsOnlyWhatTheHostCLICouldNotHaveAdded(t *testing.T) {
	t.Parallel()
	c := func(id string, labels map[string]string) dockertest.FakeContainer {
		return dockertest.FakeContainer{ID: id, Names: []string{"/web-app-" + id}, Image: "x", State: "running", Labels: labels}
	}
	oneOff := map[string]string{"com.docker.compose.project": "web", "com.docker.compose.oneoff": "True"}
	anotherStack := map[string]string{"com.docker.compose.project": "web", "bayplan.stack.id": "stack-other"}
	otherService := map[string]string{"com.docker.compose.project": "web", "com.docker.compose.service": "postgres"}
	for name, tc := range map[string]struct {
		containers []dockertest.FakeContainer
		want       bool
	}{
		"the stack's own container and a CLI one":       {[]dockertest.FakeContainer{c("1", projOwned), c("2", projNS)}, false},
		"the stack's own container and a compose run":   {[]dockertest.FakeContainer{c("1", projOwned), c("2", oneOff)}, false},
		"the stack's own container and another stack's": {[]dockertest.FakeContainer{c("1", projOwned), c("2", anotherStack)}, true},
		"the stack's own container and another service": {[]dockertest.FakeContainer{c("1", projOwned), c("2", otherService)}, true},
		"a CLI container beside another stack's alone":  {[]dockertest.FakeContainer{c("1", anotherStack), c("2", projNS)}, true},
		"CLI containers and none of the stack's own":    {[]dockertest.FakeContainer{c("1", projNS), c("2", projNS)}, true},
	} {
		rig := newSwarmRig(t, func(cfg *dockertest.FakeConfig) {
			standalone(cfg)
			cfg.Containers = tc.containers
		})
		st := stackops.Stack{ID: "stack-web", Name: "web", Target: stacks.TargetStandalone}
		got, err := rig.deps.ForeignStackOf(testsupport.Context(t), rig.envID, st)
		if err != nil || got != tc.want {
			t.Errorf("%s: ForeignStackOf = %v, %v, want %v", name, got, err, tc.want)
		}
		// The create path asks for a stack that has no container yet, so it stays strict whatever sits in the project.
		if got, err := rig.deps.ForeignStack(testsupport.Context(t), rig.envID, stacks.TargetStandalone, "web", ""); err != nil || !got {
			t.Errorf("%s: ForeignStack of a stack being created = %v, %v, want a name clash", name, got, err)
		}
	}
}

// TestCreateClashStaysStrict: the create path's check asks the ownership of a stack that is not adopted, so an object
// that an adopted stack would own (no ID, or an ID it recorded, a network it recorded) is still foreign to a stack
// being created, and a stored stack that is not adopted still owns only what carries its own ID (plan 08-18).
func TestCreateClashStaysStrict(t *testing.T) {
	t.Parallel()
	stale := map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stale-id"}
	staleProj := map[string]string{"com.docker.compose.project": "web", "bayplan.stack.id": "stale-id"}
	ctx := testsupport.Context(t)

	for name, labels := range map[string]map[string]string{"no ID": swarmNS, "a stale ID": stale} {
		rig := newSwarmRig(t, nil, dockertest.FakeService{ID: "s1", Name: "web_app", Mode: "replicated", Image: "x", Labels: labels})
		for who, stackID := range map[string]string{"a stack being created": "", "a stack with an ID": "stack-web"} {
			if got, err := rig.deps.ForeignStack(ctx, rig.envID, stacks.TargetSwarm, "web", stackID); err != nil || !got {
				t.Errorf("%s, %s: ForeignStack = %v, %v, want the service to be foreign", name, who, got, err)
			}
		}
		managed := stackops.Stack{ID: "stack-web", Name: "web", Target: stacks.TargetSwarm}
		if got, err := rig.deps.ForeignStackOf(ctx, rig.envID, managed); err != nil || !got {
			t.Errorf("%s: ForeignStackOf(managed) = %v, %v, want the service to be foreign to a stack that was not adopted", name, got, err)
		}
	}
	for name, labels := range map[string]map[string]string{"no ID": projNS, "a stale ID": staleProj} {
		rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
			standalone(c)
			c.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/web-app-1"}, Image: "x", State: "running", Labels: labels}}
		})
		for who, stackID := range map[string]string{"a stack being created": "", "a stack with an ID": "stack-web"} {
			if got, err := rig.deps.ForeignStack(ctx, rig.envID, stacks.TargetStandalone, "web", stackID); err != nil || !got {
				t.Errorf("%s, %s: ForeignStack = %v, %v, want the container to be foreign", name, who, got, err)
			}
		}
	}
}

func TestCheckTargetRefusesAnEnvironmentThatIsNoLongerTheKindTheStackWasCreatedFor(t *testing.T) {
	t.Parallel()
	swarmStack := stackops.Stack{ID: "s", Name: "web", Target: stacks.TargetSwarm}
	soloStack := stackops.Stack{ID: "s", Name: "web", Target: stacks.TargetStandalone}

	onSwarm := newSwarmRig(t, nil)
	if err := onSwarm.deps.CheckTarget(testsupport.Context(t), onSwarm.envID, swarmStack); err != nil {
		t.Errorf("a swarm stack on a swarm: %v", err)
	}
	if err := onSwarm.deps.CheckTarget(testsupport.Context(t), onSwarm.envID, soloStack); !errors.Is(err, stackops.ErrTargetChanged) {
		t.Errorf("a standalone stack on a swarm: %v, want ErrTargetChanged", err)
	}
	onHost := newSwarmRig(t, standalone)
	if err := onHost.deps.CheckTarget(testsupport.Context(t), onHost.envID, swarmStack); !errors.Is(err, stackops.ErrTargetChanged) {
		t.Errorf("a swarm stack on a standalone host: %v, want ErrTargetChanged", err)
	}
	if err := onHost.deps.CheckTarget(testsupport.Context(t), onHost.envID, soloStack); err != nil {
		t.Errorf("a standalone stack on a standalone host: %v, want it accepted (the Compose engine exists since plan 07-20)", err)
	}
}
