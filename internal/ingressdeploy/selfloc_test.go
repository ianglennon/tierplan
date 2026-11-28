// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// namedEnvs is an Environments that also names the environment TierPlan runs in.
type namedEnvs struct {
	*fakeEnvs
	name string
}

func (n namedEnvs) SelfEnvironmentName(context.Context) (string, bool) { return n.name, n.name != "" }

// TestSelfLocationHereElsewhereUnknown answers where TierPlan runs for every kind of environment and every way the read can fail:
// anything that cannot be told is a refusal, never yes (D-01, D-03, D-04, D-05).
func TestSelfLocationHereElsewhereUnknown(t *testing.T) {
	swarmEnv := ingressdeploy.EnvInfo{
		Name: "prod", Swarm: true, ClusterID: "cluster-a",
		Nodes: []ingressdeploy.NodeInfo{{SwarmNodeID: "node-1", Role: "manager", Connected: true}, {SwarmNodeID: "node-2", Role: "worker", Connected: true}},
	}
	standaloneEnv := ingressdeploy.EnvInfo{Name: "box", EngineID: "engine-1", Nodes: []ingressdeploy.NodeInfo{{AgentID: "agent-1", Connected: true}}}
	service := ingressdeploy.SelfFacts{
		ContainerID: "c1", Swarm: true, ServiceName: "tierplan_tierplan", ServiceID: "svc1", StackNamespace: "tierplan",
		Networks: []string{"tierplan_default"},
	}
	onSelfNet := service
	onSelfNet.Networks = []string{"tierplan_default", "tierplan-self"}
	plain := ingressdeploy.SelfFacts{ContainerID: "c2", Networks: []string{"bridge"}}
	member := func(cluster, node string) envs.SwarmMembership {
		return envs.SwarmMembership{Known: true, Member: true, ClusterID: cluster, NodeID: node, EngineID: "engine-x"}
	}
	engine := envs.SwarmMembership{Known: true, EngineID: "engine-1"}
	cases := []struct {
		name     string
		info     ingressdeploy.EnvInfo
		local    envs.SwarmMembership
		localErr error
		facts    *ingressdeploy.SelfFacts
		selfErr  error
		running  string
		want     ingressdeploy.SelfPlace
		code     string
		contains string
	}{
		{name: "a manager of the environment's cluster", info: swarmEnv, local: member("cluster-a", "node-1"), facts: &service,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereYes, Kind: "swarm_service", EnvName: "prod", ServiceName: "tierplan_tierplan", ServiceID: "svc1", Namespace: "tierplan"}},
		{name: "on the self network", info: swarmEnv, local: member("cluster-a", "node-1"), facts: &onSelfNet,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereYes, Kind: "swarm_service", EnvName: "prod", ServiceName: "tierplan_tierplan", ServiceID: "svc1", Namespace: "tierplan", Attached: true}},
		{name: "a worker the environment lists", info: swarmEnv, local: member("", "node-2"), facts: &service,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereYes, Kind: "swarm_service", EnvName: "prod", ServiceName: "tierplan_tierplan", ServiceID: "svc1", Namespace: "tierplan"}},
		{name: "another cluster, the environment TierPlan runs in is named", info: swarmEnv, local: member("cluster-b", "node-9"), facts: &service, running: "lab",
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereNo, EnvName: "lab"}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "runs in: lab."},
		{name: "another cluster, no name to give", info: swarmEnv, local: member("cluster-b", "node-9"), facts: &service,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereNo}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "a standalone engine against a swarm", info: swarmEnv, local: engine, facts: &plain, running: "box",
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereNo, EnvName: "box"}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "runs in: box."},
		{name: "the probe failed", info: swarmEnv, localErr: errors.New("probe failed"), facts: &service,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereUnknown}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "no socket", info: swarmEnv, facts: &service,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereUnknown}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "a worker the environment does not list", info: swarmEnv, local: member("", "stranger"), facts: &service,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereUnknown}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "no self read", info: swarmEnv, local: member("cluster-a", "node-1"),
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereUnknown}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "the self read failed", info: swarmEnv, local: member("cluster-a", "node-1"), selfErr: errors.New("no container"),
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereUnknown}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "a plain container on the swarm", info: swarmEnv, local: member("cluster-a", "node-1"), facts: &plain,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereYes, Kind: "swarm_container", EnvName: "prod", ContainerID: "c2"},
			code: ingressdeploy.CodeSelfRouteNeedsService, contains: "install-in-swarm.md"},
		{name: "the local standalone host", info: func() ingressdeploy.EnvInfo { i := standaloneEnv; i.Local = true; return i }(), facts: &plain,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereYes, Kind: "standalone", EnvName: "box", ContainerID: "c2"}},
		{name: "an agent host with the local engine's ID", info: standaloneEnv, local: engine, facts: &plain,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereYes, Kind: "standalone", EnvName: "box", ContainerID: "c2"}},
		{name: "an agent host with another engine's ID", info: standaloneEnv, local: envs.SwarmMembership{Known: true, EngineID: "engine-2"}, facts: &plain, running: "lab",
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereNo, EnvName: "lab"}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "runs in: lab."},
		{name: "an agent host whose engine is not known", info: func() ingressdeploy.EnvInfo { i := standaloneEnv; i.EngineID = ""; return i }(), local: engine, facts: &plain,
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereUnknown}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "a standalone host with no container found", info: func() ingressdeploy.EnvInfo { i := standaloneEnv; i.Local = true; return i }(), selfErr: errors.New("not in a container"),
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereUnknown}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "a standalone host with an empty container ID", info: func() ingressdeploy.EnvInfo { i := standaloneEnv; i.Local = true; return i }(), facts: &ingressdeploy.SelfFacts{},
			want: ingressdeploy.SelfPlace{Here: ingressdeploy.SelfHereUnknown}, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := ingressdeploy.Deps{Envs: namedEnvs{fakeEnvs: &fakeEnvs{}, name: tc.running}}
			// No socket answers the zero membership, which is not Known.
			deps.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) { return tc.local, tc.localErr }
			if tc.facts != nil || tc.selfErr != nil {
				deps.Self = func(context.Context) (ingressdeploy.SelfFacts, error) {
					if tc.selfErr != nil {
						return ingressdeploy.SelfFacts{}, tc.selfErr
					}
					return *tc.facts, nil
				}
			}
			svc := ingressdeploy.New(deps)
			got, findings, err := svc.SelfLocation(testsupport.Context(t), tc.info)
			if err != nil {
				t.Fatalf("SelfLocation: %v", err)
			}
			if got.Here != tc.want.Here || got.Kind != tc.want.Kind || got.EnvName != tc.want.EnvName || got.ServiceName != tc.want.ServiceName ||
				got.ServiceID != tc.want.ServiceID || got.Namespace != tc.want.Namespace || got.ContainerID != tc.want.ContainerID || got.Attached != tc.want.Attached {
				t.Errorf("place = %+v, want %+v", got, tc.want)
			}
			if tc.code == "" {
				if len(findings) != 0 {
					t.Errorf("findings = %+v, want none", findings)
				}
				return
			}
			f, ok := findingOf(findings, tc.code)
			if !ok || len(findings) != 1 {
				t.Fatalf("findings = %+v, want exactly one %s", findings, tc.code)
			}
			if f.Severity != ingressdeploy.SeverityError {
				t.Errorf("severity = %v, want an error: the switch is refused, not warned", f.Severity)
			}
			if !strings.Contains(f.Message, tc.contains) {
				t.Errorf("message = %q, want it to contain %q", f.Message, tc.contains)
			}
		})
	}
}
