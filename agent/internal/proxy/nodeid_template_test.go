// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package proxy

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// TestServiceSpecEnvTemplatePassesUnchanged is the agent leg of spike S6 (D-01): the validator forwards a global-mode
// spec whose env holds the swarm node templates byte for byte, so the spec Phase 24's deployer sends reaches the
// engine node-independent and swarmkit expands the placeholders per task. The validator has no environment rule.
func TestServiceSpecEnvTemplatePassesUnchanged(t *testing.T) {
	env := []string{"TIERPLAN_INGRESS_NODE_ID={{.Node.ID}}", "TIERPLAN_INGRESS_NODE_NAME={{.Node.Hostname}}"}
	var spec swarm.ServiceSpec
	spec.Name = "tierplan-ingress-proxy"
	spec.Mode.Global = &swarm.GlobalService{}
	spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		Image:   "ghcr.io/acme/proxy:1",
		Command: []string{"sleep", "3600"},
		Env:     slices.Clone(env),
	}

	out, err := validateBody(BodyServiceSpec, bytes.NewReader([]byte(enc(t, spec))), bodyEnv{})
	if err != nil {
		t.Fatalf("a global spec with node templates in its env was refused: %v", err)
	}
	var back swarm.ServiceSpec
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("the forwarded bytes %s do not decode: %v", out, err)
	}
	if back.Mode.Global == nil {
		t.Fatalf("the forwarded spec lost its global mode: %s", out)
	}
	if back.TaskTemplate.ContainerSpec == nil || !slices.Equal(back.TaskTemplate.ContainerSpec.Env, env) {
		t.Fatalf("forwarded env = %q, want %q byte for byte", back.TaskTemplate.ContainerSpec.Env, env)
	}
}
