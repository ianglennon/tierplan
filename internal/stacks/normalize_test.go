// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"encoding/json"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// The engine fills in defaults when it stores a service, so a review is only empty right after a
// deploy when both sides have been through the same ones. Each row is one default a swarm applies
// (the rule's comment in normalize.go names it).

func specOf(t *testing.T, raw string) swarm.ServiceSpec {
	t.Helper()
	var s swarm.ServiceSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("spec %s: %v", raw, err)
	}
	return s
}

func marshalSpec(t *testing.T, s swarm.ServiceSpec) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNormalizeSpecAppliesTheDefaultsAnEngineStoresSoBothSidesCompareEqual(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ written, stored string }{
		"the container runtime":   {`{"Name":"a","TaskTemplate":{"ContainerSpec":{"Image":"x"}}}`, `{"Name":"a","TaskTemplate":{"ContainerSpec":{"Image":"x","Isolation":"default"},"Runtime":"container"}}`},
		"an empty placement":      {`{"Name":"a","TaskTemplate":{"ContainerSpec":{"Image":"x"}}}`, `{"Name":"a","TaskTemplate":{"ContainerSpec":{"Image":"x"},"Placement":{}}}`},
		"a restart condition":     {`{"Name":"a","TaskTemplate":{"RestartPolicy":{"MaxAttempts":3}}}`, `{"Name":"a","TaskTemplate":{"RestartPolicy":{"Condition":"any","MaxAttempts":3}}}`},
		"an attempt limit":        {`{"Name":"a","TaskTemplate":{"RestartPolicy":{"Condition":"on-failure"}}}`, `{"Name":"a","TaskTemplate":{"RestartPolicy":{"Condition":"on-failure","MaxAttempts":0}}}`},
		"a replica count":         {`{"Name":"a","Mode":{"Replicated":{}}}`, `{"Name":"a","Mode":{"Replicated":{"Replicas":1}}}`},
		"no mode":                 {`{"Name":"a"}`, `{"Name":"a","Mode":{"Replicated":{"Replicas":1}}}`},
		"update failure action":   {`{"Name":"a","UpdateConfig":{"Parallelism":2}}`, `{"Name":"a","UpdateConfig":{"Parallelism":2,"FailureAction":"pause","Order":"stop-first"}}`},
		"rollback failure action": {`{"Name":"a","RollbackConfig":{"Parallelism":1}}`, `{"Name":"a","RollbackConfig":{"Parallelism":1,"FailureAction":"pause","Order":"stop-first"}}`},
		"a virtual IP":            {`{"Name":"a"}`, `{"Name":"a","EndpointSpec":{"Mode":"vip"}}`},
		"port defaults":           {`{"Name":"a","EndpointSpec":{"Ports":[{"TargetPort":80,"PublishedPort":8080}]}}`, `{"Name":"a","EndpointSpec":{"Mode":"vip","Ports":[{"Protocol":"tcp","TargetPort":80,"PublishedPort":8080,"PublishMode":"ingress"}]}}`},
		"the order of ports":      {`{"Name":"a","EndpointSpec":{"Ports":[{"TargetPort":81},{"TargetPort":80}]}}`, `{"Name":"a","EndpointSpec":{"Ports":[{"TargetPort":80},{"TargetPort":81}]}}`},
		"the order of env":        {`{"Name":"a","TaskTemplate":{"ContainerSpec":{"Env":["B=2","A=1"]}}}`, `{"Name":"a","TaskTemplate":{"ContainerSpec":{"Env":["A=1","B=2"]}}}`},
		"the order of networks":   {`{"Name":"a","TaskTemplate":{"Networks":[{"Target":"b"},{"Target":"a"}]}}`, `{"Name":"a","TaskTemplate":{"Networks":[{"Target":"a"},{"Target":"b"}]}}`},
		"the stop bookkeeping":    {`{"Name":"a","Labels":{"bayplan.stack.stopped-replicas":"3","x":"1"}}`, `{"Name":"a","Labels":{"x":"1"}}`},
		"empty versus absent":     {`{"Name":"a","TaskTemplate":{"ContainerSpec":{"Env":[],"Labels":{}}}}`, `{"Name":"a","TaskTemplate":{"ContainerSpec":{}}}`},
	} {
		w, s := stacks.NormalizeSpec(specOf(t, tc.written)), stacks.NormalizeSpec(specOf(t, tc.stored))
		if got, want := marshalSpec(t, w), marshalSpec(t, s); got != want {
			t.Errorf("%s: written %s, stored %s", name, got, want)
		}
	}
}

func TestNormalizeSpecKeepsARealDifference(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ a, b string }{
		"a replica count":      {`{"Name":"a","Mode":{"Replicated":{"Replicas":1}}}`, `{"Name":"a","Mode":{"Replicated":{"Replicas":2}}}`},
		"an image":             {`{"Name":"a","TaskTemplate":{"ContainerSpec":{"Image":"x:1"}}}`, `{"Name":"a","TaskTemplate":{"ContainerSpec":{"Image":"x:2"}}}`},
		"an env value":         {`{"Name":"a","TaskTemplate":{"ContainerSpec":{"Env":["A=1"]}}}`, `{"Name":"a","TaskTemplate":{"ContainerSpec":{"Env":["A=2"]}}}`},
		"a restart condition":  {`{"Name":"a","TaskTemplate":{"RestartPolicy":{"Condition":"none"}}}`, `{"Name":"a","TaskTemplate":{"RestartPolicy":{}}}`},
		"an attempt limit":     {`{"Name":"a","TaskTemplate":{"RestartPolicy":{"MaxAttempts":3}}}`, `{"Name":"a","TaskTemplate":{"RestartPolicy":{}}}`},
		"a published port":     {`{"Name":"a","EndpointSpec":{"Ports":[{"TargetPort":80,"PublishedPort":8080}]}}`, `{"Name":"a","EndpointSpec":{"Ports":[{"TargetPort":80,"PublishedPort":8081}]}}`},
		"the dnsrr endpoint":   {`{"Name":"a","EndpointSpec":{"Mode":"dnsrr"}}`, `{"Name":"a"}`},
		"a start-first update": {`{"Name":"a","UpdateConfig":{"Order":"start-first"}}`, `{"Name":"a","UpdateConfig":{}}`},
	} {
		a, b := stacks.NormalizeSpec(specOf(t, tc.a)), stacks.NormalizeSpec(specOf(t, tc.b))
		if marshalSpec(t, a) == marshalSpec(t, b) {
			t.Errorf("%s: normalising made %s equal to %s", name, tc.a, tc.b)
		}
	}
}

func TestNormalizeSpecChangesNothingOfTheSpecItIsGiven(t *testing.T) {
	t.Parallel()
	in := specOf(t, `{"Name":"a","TaskTemplate":{"ContainerSpec":{"Env":["B=2","A=1"]},"Networks":[{"Target":"b"},{"Target":"a"}]},"EndpointSpec":{"Ports":[{"TargetPort":81},{"TargetPort":80}]}}`)
	before := marshalSpec(t, in)
	_ = stacks.NormalizeSpec(in)
	if after := marshalSpec(t, in); after != before {
		t.Errorf("the spec changed:\n before %s\n after  %s", before, after)
	}
}
