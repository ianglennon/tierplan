// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rebuild

import (
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
)

// The engine fills in some values when it stores a service or a network, and a file that repeated them would not
// read like a hand-written one. Each function here is one such rule and says which rule of stacks.NormalizeSpec
// (or which convert default) it mirrors: a value is left out only when converting the result gives it back.

// vxlanOption is the driver option the engine adds to every overlay network it creates: it names the VXLAN IDs
// it chose. A file that repeated it would fix them, so it is never written.
const vxlanOption = "com.docker.network.driver.overlay.vxlanid_list"

// networkOptions are the driver options of a network without the ones the engine chose itself.
func networkOptions(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		if strings.EqualFold(k, vxlanOption) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// plainDefault says whether the stack's default network is the one convert makes when a file declares none: an
// overlay network that is neither attachable nor internal and carries no option or label of a person's. Its address
// pool is the engine's own pick, so it is not part of the test.
func plainDefault(info NetworkInfo) bool {
	return !info.Attachable && !info.Internal && len(networkOptions(info.Options)) == 0 && len(userLabels(info.Labels)) == 0 &&
		(info.IPAMDriver == "" || info.IPAMDriver == "default")
}

// replicasAreDefault mirrors the replicas rule of stacks.NormalizeSpec: a replicated service with no count has one
// replica, so a count of one says nothing.
func replicasAreDefault(replicas *uint64) bool { return replicas == nil || *replicas == 1 }

// endpointModeIsDefault mirrors the endpoint rule of stacks.NormalizeSpec: a spec with no mode resolves through a
// virtual IP, so vip says nothing (dnsrr is written as deploy.endpoint_mode).
func endpointModeIsDefault(m swarm.ResolutionMode) bool {
	return m == "" || m == swarm.ResolutionModeVIP
}

// protocolIsDefault mirrors the port rule of stacks.NormalizeSpec: a port with no protocol is tcp, which is also
// what convert reads for a port that names none.
func protocolIsDefault(p string) bool { return p == "" || p == "tcp" }

// publishModeIsDefault mirrors the port rule of stacks.NormalizeSpec: a port with no publish mode is ingress.
func publishModeIsDefault(m swarm.PortConfigPublishMode) bool {
	return m == "" || m == swarm.PortConfigPublishModeIngress
}

// failureActionIsDefault mirrors the update rule of stacks.NormalizeSpec: a policy with no failure action reads back
// as pause.
func failureActionIsDefault(a swarm.FailureAction) bool {
	return a == "" || a == swarm.UpdateFailureActionPause
}

// updateOrderIsDefault mirrors the update rule of stacks.NormalizeSpec: a policy with no order reads back as
// stop-first.
func updateOrderIsDefault(o swarm.UpdateOrder) bool {
	return o == "" || o == swarm.UpdateOrderStopFirst
}

// attemptsAreDefault mirrors the restart rule of stacks.NormalizeSpec: a restart policy with no attempt limit reads
// back with a limit of 0, which is unlimited.
func attemptsAreDefault(n *uint64) bool { return n == nil || *n == 0 }

// isolationIsDefault mirrors the isolation rule of stacks.NormalizeSpec: a container spec that names none reads
// back as "default".
func isolationIsDefault(i container.Isolation) bool { return i == "" || i == "default" }
