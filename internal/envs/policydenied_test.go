// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAnAgentRefusalInADockerErrorBecomesErrAgentPolicyDeniedNamingTheEndpoint(t *testing.T) {
	for name, tc := range map[string]struct{ err, endpoint string }{
		"the daemon error the client shows": {"Error response from daemon: agent_policy_denied: GET /containers/json", "GET /containers/json"},
		"the bare message":                  {"agent_policy_denied: POST /containers/create", "POST /containers/create"},
		"a versioned path with a prefix":    {"request failed: Error response from daemon: agent_policy_denied: GET /v1.47/swarm", "GET /v1.47/swarm"},
		"text after a newline is dropped":   {"agent_policy_denied: GET /swarm\nsomething else", "GET /swarm"},
	} {
		got := ClassifyDockerError(fmt.Errorf("list containers: %w", errors.New(tc.err)))
		var denied *AgentPolicyDeniedError
		if !errors.Is(got, ErrAgentPolicyDenied) || !errors.As(got, &denied) {
			t.Errorf("%s: %v is not an agent policy denial", name, got)
			continue
		}
		if denied.Endpoint != tc.endpoint {
			t.Errorf("%s: endpoint %q, want %q", name, denied.Endpoint, tc.endpoint)
		}
	}
}

func TestAnyOtherDockerErrorIsLeftAlone(t *testing.T) {
	for _, in := range []error{
		nil,
		errors.New("Error response from daemon: no such container"),
		errors.New("agent_policy_denied without a colon"),
		errors.New("the agent_engine_unreachable: the Docker engine did not answer"),
	} {
		if got := ClassifyDockerError(in); got != in { //nolint:errorlint // identity is the point: an error that is not a refusal is returned as it came
			t.Errorf("ClassifyDockerError(%v) = %v, want it unchanged", in, got)
		}
	}
}

func TestTheEndpointOfARefusalIsCapped(t *testing.T) {
	got := ClassifyDockerError(errors.New("agent_policy_denied: GET /" + strings.Repeat("a", 2000)))
	var denied *AgentPolicyDeniedError
	if !errors.As(got, &denied) || len(denied.Endpoint) > 256 {
		t.Fatalf("endpoint of %d bytes from %v, want at most 256", len(denied.Endpoint), got)
	}
}

func TestAHostPolicyRefusalIsMarkedAndMatchesErrAgentHostPolicy(t *testing.T) {
	for name, tc := range map[string]struct{ err, endpoint string }{
		"the daemon error the client shows": {"Error response from daemon: agent_policy_denied: host_policy: POST /services/x/update", "POST /services/x/update"},
		"the bare message":                  {"agent_policy_denied: host_policy: POST /services/create", "POST /services/create"},
		"a versioned path":                  {"request failed: agent_policy_denied: host_policy: POST /v1.52/services/web/update", "POST /v1.52/services/web/update"},
		"extra spaces after the marker":     {"agent_policy_denied:   host_policy:   POST /services/create", "POST /services/create"},
	} {
		got := ClassifyDockerError(fmt.Errorf("update service: %w", errors.New(tc.err)))
		var denied *AgentPolicyDeniedError
		if !errors.As(got, &denied) {
			t.Errorf("%s: %v is not an agent policy denial", name, got)
			continue
		}
		if !denied.HostPolicy || denied.Endpoint != tc.endpoint {
			t.Errorf("%s: HostPolicy %v endpoint %q, want true and %q", name, denied.HostPolicy, denied.Endpoint, tc.endpoint)
		}
		if !errors.Is(got, ErrAgentHostPolicy) || !errors.Is(got, ErrAgentPolicyDenied) {
			t.Errorf("%s: %v must match both ErrAgentHostPolicy and ErrAgentPolicyDenied", name, got)
		}
	}
}

func TestAPlainRefusalIsNotAHostPolicyRefusal(t *testing.T) {
	for _, in := range []string{
		"agent_policy_denied: POST /services/x/update",
		"agent_policy_denied: GET /host_policy: x",
		"agent_policy_denied: read_only_flag: POST /services/create",
	} {
		got := ClassifyDockerError(errors.New(in))
		var denied *AgentPolicyDeniedError
		if !errors.As(got, &denied) {
			t.Fatalf("%q is not an agent policy denial", in)
		}
		if denied.HostPolicy || errors.Is(got, ErrAgentHostPolicy) {
			t.Errorf("%q was read as a host-policy refusal", in)
		}
		if !errors.Is(got, ErrAgentPolicyDenied) {
			t.Errorf("%q no longer matches ErrAgentPolicyDenied", in)
		}
	}
}
