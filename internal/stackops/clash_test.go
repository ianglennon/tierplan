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
	projNS     = map[string]string{"com.docker.compose.project": "web"}
	projOwned  = map[string]string{"com.docker.compose.project": "web", "bayplan.stack.id": "stack-web"}
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
	if err := onHost.deps.CheckTarget(testsupport.Context(t), onHost.envID, soloStack); !errors.Is(err, stackops.ErrTargetEngineMissing) {
		t.Errorf("a standalone stack on a standalone host: %v, want ErrTargetEngineMissing until the Compose engine exists", err)
	}
}
