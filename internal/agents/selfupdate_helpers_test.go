// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration || swarm

package agents_test

import (
	"encoding/json"
	"testing"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/server/servertest"
)

// Versions of the two agent images the self-update tests build with
// `make agent-image AGENT_VERSION=<v> AGENT_TAG=bayplan-agent:<v>`.
const (
	oldRelease = "v0.2.0"
	newRelease = "v0.2.1"
)

// loadReleases puts both release images on every node, under the names an install
// command and an update of each version use, so no registry is contacted.
func loadReleases(t *testing.T, nodes ...*dockertest.Node) {
	t.Helper()
	for _, v := range []string{oldRelease, newRelease} {
		dockertest.LoadAgentImageAs(t, "bayplan-agent:"+v, agents.AgentImage(v), nodes...)
	}
}

func summariseAgents(rows []servertest.Agent) string {
	b, _ := json.Marshal(rows)
	return string(b)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonEqual(a, b any) bool { return mustJSON(a) == mustJSON(b) }
