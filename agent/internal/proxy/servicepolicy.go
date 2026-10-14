// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/proto/hostpath"
)

// errHostPolicy is the refusal of a service spec that gives tasks host access on a
// host whose owner did not set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES (D-36). The
// handler answers it 403 with the host_policy message, not as a plain allowlist
// refusal, so the server can tell the user which setting lifts it.
var errHostPolicy = errors.New("the service spec gives tasks host access")

// networkLookup answers the name and driver of a network on the agent's own
// engine. A target may be an ID, and an ID cannot be told from its text to be the
// host network, so the agent asks the engine that holds it.
type networkLookup func(ctx context.Context, target string) (name, driver string, err error)

// hostNetwork is the name of the engine's host network, which Docker creates on
// every node and which a service names to share the node's network stack.
const hostNetwork = "host"

// escalations returns the spec path of each feature in spec that gives tasks
// access to the host, in the order of the spec. It is the whole definition of "host
// access" for a service, and the embedded vectors of proto/hostpath pin it:
//
//   - a bind mount whose source fails hostpath.Bad (the same rule the compose
//     policy applies, D-38) and any named-pipe mount;
//   - a volume with the local driver (named or not) whose options mount a device
//     that fails hostpath.Bad;
//   - any added capability;
//   - SELinux disabled or the spc_t type, seccomp unconfined or custom, AppArmor
//     disabled;
//   - a network attachment to the host network, named host or found by lookup to
//     be a network of the host driver or named host.
//
// A lookup that is nil or fails finds nothing: the engine refuses an unknown
// network itself when it is handed the spec.
func escalations(ctx context.Context, spec swarm.ServiceSpec, lookup networkLookup) []string {
	var out []string
	const task = "TaskTemplate."
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		const base = task + "ContainerSpec."
		for i, m := range cs.Mounts {
			at := fmt.Sprintf("%sMounts[%d]", base, i)
			switch m.Type {
			case mount.TypeNamedPipe:
				out = append(out, at+".Source")
			case mount.TypeBind, "":
				if hostpath.Bad(m.Source) {
					out = append(out, at+".Source")
				}
			}
			if vo := m.VolumeOptions; vo != nil && vo.DriverConfig != nil {
				if name := strings.ToLower(strings.TrimSpace(vo.DriverConfig.Name)); name == "" || name == "local" {
					if dev, mounted := hostpath.LocalDeviceMounted(vo.DriverConfig.Options); mounted && hostpath.Bad(dev) {
						out = append(out, at+".VolumeOptions.DriverConfig.Options.device")
					}
				}
			}
		}
		for i := range cs.CapabilityAdd {
			out = append(out, fmt.Sprintf("%sCapabilityAdd[%d]", base, i))
		}
		if p := cs.Privileges; p != nil {
			if se := p.SELinuxContext; se != nil && (se.Disable || strings.EqualFold(se.Type, "spc_t")) {
				out = append(out, base+"Privileges.SELinuxContext")
			}
			if sc := p.Seccomp; sc != nil && (sc.Mode == swarm.SeccompModeUnconfined || sc.Mode == swarm.SeccompModeCustom) {
				out = append(out, base+"Privileges.Seccomp.Mode")
			}
			if aa := p.AppArmor; aa != nil && aa.Mode == swarm.AppArmorModeDisabled {
				out = append(out, base+"Privileges.AppArmor.Mode")
			}
		}
	}
	isHost := map[string]bool{}
	for i, n := range spec.TaskTemplate.Networks {
		host, seen := isHost[n.Target]
		if !seen {
			host = n.Target == hostNetwork
			if !host && lookup != nil {
				if name, driver, err := lookup(ctx, n.Target); err == nil {
					host = name == hostNetwork || strings.EqualFold(driver, hostNetwork)
				}
			}
			isHost[n.Target] = host
		}
		if host {
			out = append(out, fmt.Sprintf("%sNetworks[%d].Target", task, i))
		}
	}
	return out
}

// checkHostPolicy refuses a spec that gives tasks host access unless the host
// allows it. The paths of the findings are in the error for the log; they are
// never sent back to the server, whose answer is the typed refusal.
func checkHostPolicy(spec swarm.ServiceSpec, env bodyEnv) error {
	if env.policy.AllowPrivilegedServices {
		return nil
	}
	ctx := env.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if found := escalations(ctx, spec, env.lookup); len(found) > 0 {
		return fmt.Errorf("%w: %w: %s", errBodyRefused, errHostPolicy, strings.Join(found, ", "))
	}
	return nil
}
