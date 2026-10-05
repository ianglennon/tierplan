// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ianglennon/bayplan/internal/tunnel"
)

// The mapping from an observation to its D-40 code (plan 02-11, ENV-08). The
// daemon bodies are the ones RESEARCH Pattern 5 recorded from a real swarm.

const (
	workerBody = `Error response from daemon: This node is not a swarm manager. Worker nodes can't be used to view or modify cluster state. Please run this command on a manager node or promote the current node to a manager.`
	quorumBody = `Error response from daemon: rpc error: code = Unknown desc = The swarm does not have a leader. It's possible that too few managers are online. Make sure more than half of the managers are online.`
)

func TestReasonForMapsEachObservationToItsCode(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"the worker 503 body":                        {errors.New(workerBody), ReasonNotAManager},
		"the worker body wrapped by the caller":      {fmt.Errorf("list services: %w", errors.New(workerBody)), ReasonNotAManager},
		"the quorum-loss 500 body":                   {errors.New(quorumBody), ReasonNoQuorum},
		"the quorum-loss body wrapped by the caller": {fmt.Errorf("list nodes: %w", errors.New(quorumBody)), ReasonNoQuorum},
		"the agent is gone":                          {tunnel.ErrAgentGone, ReasonAgentOffline},
		"the agent is gone, wrapped":                 {fmt.Errorf("dial: %w", tunnel.ErrAgentGone), ReasonAgentOffline},
		"the agent refused by policy":                {errors.New("Error response from daemon: agent_policy_denied: GET /swarm"), ReasonAgentPolicyDenied},
		"a typed policy denial":                      {ClassifyDockerError(errors.New("agent_policy_denied: POST /x")), ReasonAgentPolicyDenied},
		"a deadline":                                 {context.DeadlineExceeded, ReasonTimeout},
		"a deadline, wrapped":                        {fmt.Errorf("probe: %w", context.DeadlineExceeded), ReasonTimeout},
		"a handshake refusal carries its own code":   {&tunnel.RefuseError{Code: ReasonClockSkew, Detail: "text from the agent side"}, ReasonClockSkew},
		"a refusal code outside the fixed set":       {&tunnel.RefuseError{Code: "made_up_by_a_peer"}, ""},
		"an error that is none of them":              {errors.New("connection reset"), ""},
		"no error":                                   {nil, ""},
	} {
		if got := ReasonFor(tc.err); got != tc.want {
			t.Errorf("%s: ReasonFor = %q, want %q", name, got, tc.want)
		}
	}
}

func TestReasonForNeverReturnsTextAnAgentWrote(t *testing.T) {
	// The message is arbitrary, the answer is always from the fixed set or empty.
	for _, msg := range []string{"agent_policy_denied: GET /\x1b[31mred", "<script>alert(1)</script>", "This node is not a swarm manager" + "\n" + "x"} {
		if got := ReasonFor(errors.New(msg)); got != "" && !IsReason(got) {
			t.Errorf("ReasonFor(%q) = %q, which is not a fixed code", msg, got)
		}
	}
}

func TestIsReasonKnowsExactlyTheFifteenD40CodesAndTheCertificateIssue(t *testing.T) {
	codes := []string{
		ReasonAgentOffline, ReasonTLSUntrusted, ReasonAuthRejected, ReasonCertExpired, ReasonRevoked, ReasonBlocked,
		ReasonVersionIncompatible, ReasonClockSkew, ReasonTimeout, ReasonNotAManager, ReasonNoQuorum, ReasonClusterChanged,
		ReasonEngineUnreachable, ReasonUnsupportedEngine, ReasonAgentPolicyDenied,
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if c == "" || seen[c] {
			t.Errorf("code %q is empty or repeated", c)
		}
		seen[c] = true
		if !IsReason(c) {
			t.Errorf("IsReason(%q) = false", c)
		}
	}
	if len(codes) != 15 {
		t.Errorf("%d codes, D-40 has 15", len(codes))
	}
	if !IsReason(IssueCertExpiring) {
		t.Errorf("IsReason(%q) = false", IssueCertExpiring)
	}
	for _, c := range []string{"", "AGENT_OFFLINE", "unreachable", "podman", "agent_offline ", "duplicate_environment"} {
		if IsReason(c) {
			t.Errorf("IsReason(%q) = true", c)
		}
	}
}

func TestReasonConstantsAreTheWireCodes(t *testing.T) {
	// The UI and the OpenAPI contract show the wire spelling.
	for got, want := range map[string]string{
		ReasonAgentOffline: "agent_offline", ReasonNotAManager: "not_a_manager", ReasonNoQuorum: "no_quorum",
		ReasonClusterChanged: "cluster_changed", ReasonEngineUnreachable: "engine_unreachable",
		ReasonUnsupportedEngine: "unsupported_engine", ReasonAgentPolicyDenied: "agent_policy_denied", ReasonTimeout: "timeout",
	} {
		if got != want {
			t.Errorf("constant %q, want %q", got, want)
		}
	}
}
