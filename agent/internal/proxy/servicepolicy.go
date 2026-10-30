// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"

	"github.com/ianglennon/coaming/proto/hostpath"
)

// errHostPolicy is the refusal of a service spec that gives tasks host access, of a
// container create that gives the container host access, or of a volume create that would
// mount the host or name a volume plugin, on a host whose owner did not set
// COAMING_AGENT_ALLOW_PRIVILEGED_SERVICES (D-36, and D-36/D-38 as amended). The handler
// answers it 403 with the host_policy message, not as a plain allowlist refusal, so the
// server can tell the user which setting lifts it.
var errHostPolicy = errors.New("the request gives host access")

// errVolumeLookup is the engine not answering the lookup of a named volume. It is an
// engine fault and not host access: the handler answers it 502 agent_engine_unreachable,
// not 403 host_policy, so a transient fault does not tell the person to lower the host's
// security setting.
var errVolumeLookup = errors.New("the engine did not answer a volume lookup")

// networkLookup answers the name and driver of a network on the agent's own
// engine. A target may be an ID, and an ID cannot be told from its text to be the
// host network, so the agent asks the engine that holds it.
type networkLookup func(ctx context.Context, target string) (name, driver string, err error)

// volumeLookup answers the driver and options of a named volume on the agent's own
// engine. found false with a nil error means the engine answered that it has no such
// volume.
type volumeLookup func(ctx context.Context, name string) (driver string, opts map[string]string, found bool, err error)

// hostNetwork is the name of the engine's host network, which Docker creates on
// every node and which a service names to share the node's network stack.
const hostNetwork = "host"

// escalations returns the spec path of each feature in spec that gives tasks
// access to the host, in the order of the spec. It is the whole definition of "host
// access" for a service, and the embedded vectors of proto/hostpath pin it:
//
//   - a bind mount whose source fails hostpath.Bad (the same rule the compose
//     policy applies, D-38) and any named-pipe mount;
//   - a volume with the local driver (named or not) whose options give host access as
//     the daemon mounts them (hostpath.LocalVolumeHostAccess): a bind of a device that
//     is relative or fails hostpath.Bad, or a filesystem type other than tmpfs or a
//     network share;
//   - any added capability;
//   - SELinux disabled or the spc_t type, seccomp unconfined or custom, AppArmor
//     disabled;
//   - a network attachment to the host network, named host or found by lookup to
//     be a network of the host driver or named host.
//
// A lookup that is nil or fails finds nothing: the engine refuses an unknown
// network itself when it is handed the spec.
//
// The agent adds two rules of its own on top (D-36/D-38 as amended), which the server
// and the compose policies do not apply and which SpecVector.AgentRefuses marks:
// pluginVolumes, because a plugin's options cannot be judged, and namedVolumes,
// because the server does not inspect volumes.
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
					if key := hostpath.LocalVolumeHostAccess(vo.DriverConfig.Options); key != "" {
						out = append(out, at+".VolumeOptions.DriverConfig.Options."+key)
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

// volumeEscalation names what gives tasks host access in a volume create, or returns
// "" when nothing does. It is the volume-create half of the D-36 host-access
// definition (D-36/D-38 as amended): a volume created here can be mounted by name in
// a service whose spec carries no host path for escalations to judge. A local volume
// (named or not) is judged by hostpath.LocalVolumeHostAccess, the way the daemon mounts
// it: "DriverOpts.device" for a bind of a relative or disallowed path, "DriverOpts.type"
// for a filesystem type other than tmpfs or a network share. Any other driver is
// "Driver", because a plugin's options cannot be judged and so are host access until the
// owner opts in.
func volumeEscalation(req volume.CreateRequest) string {
	if name := strings.ToLower(strings.TrimSpace(req.Driver)); name != "" && name != "local" {
		return "Driver"
	}
	if key := hostpath.LocalVolumeHostAccess(req.DriverOpts); key != "" {
		return "DriverOpts." + key
	}
	return ""
}

// pluginVolumes returns the spec path of each mount that names a volume plugin, in the
// order of the spec: the DriverConfig name of a mount whose driver is neither empty nor
// local (compared lower-cased and trimmed), and the Type of a cluster mount, which is
// a CSI volume and so always a plugin. A plugin's options cannot be judged, so they are
// host access until the owner opts in; the server and the compose policies pass them
// (D-38), the agent is stricter on purpose.
func pluginVolumes(spec swarm.ServiceSpec) []string {
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return nil
	}
	var out []string
	for i, m := range cs.Mounts {
		at := fmt.Sprintf("TaskTemplate.ContainerSpec.Mounts[%d]", i)
		if vo := m.VolumeOptions; vo != nil && vo.DriverConfig != nil {
			if name := strings.ToLower(strings.TrimSpace(vo.DriverConfig.Name)); name != "" && name != "local" {
				out = append(out, at+".VolumeOptions.DriverConfig.Name")
			}
		}
		if m.Type == mount.TypeCluster {
			out = append(out, at+".Type")
		}
	}
	return out
}

// namedVolumes returns the spec path of the Source of each volume mount that cannot be
// shown to be free of host access, in the order of the spec. Docker mounts a volume
// that exists as it is, and a service spec says nothing of what backs its name, so the
// agent asks its own engine and judges the answer as a volume create (volumeEscalation).
// A mount is listed when:
//
//   - its Source is not shaped like a volume name (no lookup is made for it);
//   - lookup is nil, because the zero env cannot see what backs a name (a finding, not
//     an error);
//   - the volume exists and volumeEscalation finds host access in its driver or options.
//
// A volume the engine does not have adds nothing: Docker creates it from the mount's
// own VolumeOptions, which escalations and pluginVolumes judge, so it is judged like an
// anonymous volume (no Source), which is never looked up.
//
// A lookup the engine does not answer (an error, a timeout or an unexpected status) adds
// no finding and the first such error is returned, wrapped with errVolumeLookup. Unlike
// the network lookup this still stops the write, because the engine refuses an unknown
// network itself but mounts an existing volume without asking; it is reported as an engine
// fault and not as host access. Each distinct Source is looked up once. The agent sees only its own engine, so a volume of the same name on another
// node is not judged (T-06-123).
func namedVolumes(ctx context.Context, spec swarm.ServiceSpec, lookup volumeLookup) ([]string, error) {
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return nil, nil
	}
	var out []string
	var lookupErr error
	bad := map[string]bool{}
	for i, m := range cs.Mounts {
		if m.Type != mount.TypeVolume || m.Source == "" {
			continue
		}
		refused, seen := bad[m.Source]
		if !seen {
			refused = !volumeNameRe.MatchString(m.Source) || lookup == nil
			if !refused {
				driver, opts, found, err := lookup(ctx, m.Source)
				if err != nil {
					if lookupErr == nil {
						lookupErr = fmt.Errorf("%w: %w", errVolumeLookup, err)
					}
				} else {
					refused = found && volumeEscalation(volume.CreateRequest{Driver: driver, DriverOpts: opts}) != ""
				}
			}
			bad[m.Source] = refused
		}
		if refused {
			out = append(out, fmt.Sprintf("TaskTemplate.ContainerSpec.Mounts[%d].Source", i))
		}
	}
	return out, lookupErr
}

// checkHostPolicy refuses a spec that gives tasks host access unless the host
// allows it. A named-volume lookup the engine did not answer also stops the write, but
// as errVolumeLookup (an engine fault) and not as host access, unless a real finding
// is present. The paths of the findings are in the error for the log; they are
// never sent back to the server, whose answer is the typed refusal.
func checkHostPolicy(spec swarm.ServiceSpec, env bodyEnv) error {
	if env.policy.AllowPrivilegedServices {
		return nil
	}
	ctx := env.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	found := escalations(ctx, spec, env.lookup)
	found = append(found, pluginVolumes(spec)...)
	named, lookupErr := namedVolumes(ctx, spec, env.volumes)
	found = append(found, named...)
	if len(found) > 0 {
		// A real finding is named as host access even when a lookup also failed.
		return fmt.Errorf("%w: %w: %s", errBodyRefused, errHostPolicy, strings.Join(found, ", "))
	}
	return lookupErr
}
