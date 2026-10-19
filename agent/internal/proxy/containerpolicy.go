// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"

	"github.com/ianglennon/bayplan/proto/hostpath"
)

// containerEscalations returns the path of each feature in a container create that gives the
// container access to the host, in a fixed order: Privileged, CapAdd, Devices, DeviceCgroupRules,
// DeviceRequests, NetworkMode, the pid, ipc, uts, user and cgroup namespace modes, CgroupParent,
// VolumesFrom, MaskedPaths, ReadonlyPaths, Runtime, SecurityOpt, Binds, Mounts and then the
// endpoints of NetworkingConfig. It is the whole definition of "host access" for a container, the
// container half of escalations, pluginVolumes and namedVolumes (D-36 and D-38 as amended), and
// the embedded vectors of proto/hostpath pin it (STK-02):
//
//   - Privileged, any added capability, a device, a device cgroup rule or a device request;
//   - a network mode of host or container:<id>, or a user-defined network found by lookup to be
//     the host network (the mode, and each key of the endpoints);
//   - the pid, ipc, uts, user and cgroup namespace shared with the host (host) or, for the first
//     two, with another container (container:);
//   - a cgroup parent, volumes taken from another container, an emptied or listed set of masked or
//     read-only paths, and a runtime other than runc;
//   - a security option that lowers the sandbox: seccomp unconfined or a profile other than the
//     built-in one, AppArmor unconfined, SELinux disabled or the spc_t type, system paths
//     unconfined, or any option the agent does not know;
//   - a bind (a Binds entry or a bind mount) whose source is not an absolute path or fails
//     hostpath.Bad (the rule the compose policy and the service check apply), and a named-pipe
//     mount;
//   - a mount of a type the agent does not forward, and a cluster mount, which is a volume plugin;
//   - a volume with the local driver (named by a mount's options or found by lookup) whose options
//     give host access as the daemon mounts them (hostpath.LocalVolumeHostAccess), and a volume
//     with any other driver, because a plugin's options cannot be judged.
//
// A named volume is looked up once on the agent's own engine. One that does not exist adds nothing:
// the engine creates it as a plain local volume (a mount's own options are judged inline). With no
// volumes lookup (the zero environment) no named volume can be shown to be free of host access and
// each one is a finding. A lookup that is nil for networks or fails finds nothing: the engine
// refuses an unknown network itself. A volume lookup the engine does not answer adds no finding
// and the first such error is returned, wrapped with errVolumeLookup.
func containerEscalations(ctx context.Context, req container.CreateRequest, networks networkLookup, volumes volumeLookup) ([]string, error) {
	var out []string
	const host = "HostConfig."
	vols := volumeJudge{ctx: ctx, lookup: volumes, bad: map[string]bool{}}
	nets := hostNetworkJudge{ctx: ctx, lookup: networks, seen: map[string]bool{}}
	if hc := req.HostConfig; hc != nil {
		if hc.Privileged {
			out = append(out, host+"Privileged")
		}
		for i := range hc.CapAdd {
			out = append(out, fmt.Sprintf("%sCapAdd[%d]", host, i))
		}
		if len(hc.Devices) > 0 {
			out = append(out, host+"Devices")
		}
		if len(hc.DeviceCgroupRules) > 0 {
			out = append(out, host+"DeviceCgroupRules")
		}
		if len(hc.DeviceRequests) > 0 {
			out = append(out, host+"DeviceRequests")
		}
		if mode := string(hc.NetworkMode); sharesWithHostOrContainer(mode) || nets.userDefinedIsHost(mode) {
			out = append(out, host+"NetworkMode")
		}
		for _, f := range []struct{ name, mode string }{
			{"PidMode", string(hc.PidMode)}, {"IpcMode", string(hc.IpcMode)}, {"UTSMode", string(hc.UTSMode)},
			{"UsernsMode", string(hc.UsernsMode)}, {"CgroupnsMode", string(hc.CgroupnsMode)},
		} {
			if sharesWithHostOrContainer(f.mode) {
				out = append(out, host+f.name)
			}
		}
		if hc.CgroupParent != "" {
			out = append(out, host+"CgroupParent")
		}
		if len(hc.VolumesFrom) > 0 {
			out = append(out, host+"VolumesFrom")
		}
		// A nil list is the engine's default set of masked and read-only paths; any other list,
		// the empty one the Compose SDK sends for systempaths=unconfined included, replaces it.
		if hc.MaskedPaths != nil {
			out = append(out, host+"MaskedPaths")
		}
		if hc.ReadonlyPaths != nil {
			out = append(out, host+"ReadonlyPaths")
		}
		if hc.Runtime != "" && hc.Runtime != "runc" {
			out = append(out, host+"Runtime")
		}
		for i, opt := range hc.SecurityOpt {
			if securityOptLowersTheSandbox(opt) {
				out = append(out, fmt.Sprintf("%sSecurityOpt[%d]", host, i))
			}
		}
		for i, b := range hc.Binds {
			// An entry with no colon is an anonymous volume at that path inside the container.
			src, _, hasTarget := strings.Cut(b, ":")
			if !hasTarget {
				continue
			}
			if strings.HasPrefix(src, "/") {
				if !hostPathAllowed(src) {
					out = append(out, fmt.Sprintf("%sBinds[%d]", host, i))
				}
			} else if vols.refused(src) {
				out = append(out, fmt.Sprintf("%sBinds[%d]", host, i))
			}
		}
		for i, m := range hc.Mounts {
			at := fmt.Sprintf("%sMounts[%d]", host, i)
			switch m.Type {
			case mount.TypeBind, "":
				if !hostPathAllowed(m.Source) {
					out = append(out, at+".Source")
				}
			case mount.TypeNamedPipe:
				out = append(out, at+".Source")
			case mount.TypeVolume:
				if vo := m.VolumeOptions; vo != nil && vo.DriverConfig != nil {
					if name := strings.ToLower(strings.TrimSpace(vo.DriverConfig.Name)); name != "" && name != "local" {
						out = append(out, at+".VolumeOptions.DriverConfig.Name")
					} else if key := hostpath.LocalVolumeHostAccess(vo.DriverConfig.Options); key != "" {
						out = append(out, at+".VolumeOptions.DriverConfig.Options."+key)
					}
				}
				if m.Source != "" && vols.refused(m.Source) {
					out = append(out, at+".Source")
				}
			case mount.TypeTmpfs, mount.TypeImage:
			default:
				// A cluster mount is a volume plugin and any other type is one the agent does not
				// forward.
				out = append(out, at+".Type")
			}
		}
	}
	if nc := req.NetworkingConfig; nc != nil {
		for _, name := range slices.Sorted(maps.Keys(nc.EndpointsConfig)) {
			if strings.EqualFold(strings.TrimSpace(name), hostNetwork) || nets.isHost(name) {
				out = append(out, fmt.Sprintf("NetworkingConfig.EndpointsConfig[%q]", name))
			}
		}
	}
	return out, vols.err
}

// sharesWithHostOrContainer reports whether a network or namespace mode shares the host's or another
// container's namespace. Case and white space are ignored, which refuses more than the engine
// reads, never less.
func sharesWithHostOrContainer(mode string) bool {
	m := strings.ToLower(strings.TrimSpace(mode))
	return m == hostNetwork || strings.HasPrefix(m, "container:")
}

// securityOptLowersTheSandbox reports whether a SecurityOpt entry turns off a protection or is one
// the agent does not know. The engine reads key=value and, for the older spelling, key:value; the
// key and the value are compared without regard to case or white space. Only no-new-privileges,
// seccomp, apparmor and label are forwarded: seccomp must be the built-in profile (anything else is
// unconfined or a custom profile), apparmor must not be unconfined and label must neither disable
// SELinux nor name the spc_t type. A key the agent does not know, systempaths=unconfined included,
// is refused.
func securityOptLowersTheSandbox(opt string) bool {
	key, val, ok := strings.Cut(opt, "=")
	if !ok {
		key, val, _ = strings.Cut(opt, ":")
	}
	key, val = strings.ToLower(strings.TrimSpace(key)), strings.ToLower(strings.TrimSpace(val))
	switch key {
	case "no-new-privileges":
		return false
	case "seccomp":
		return val != "builtin" && val != "default"
	case "apparmor":
		return val == "unconfined"
	case "label":
		return val == "disable" || val == "type:spc_t"
	}
	return true
}

// hostPathAllowed reports whether a bind source may be given to a container: an absolute path with
// no white space around it that hostpath.Bad does not refuse. The engine reads a relative bind
// source against its own working directory and not against a project directory, so a relative one
// is never allowed here.
func hostPathAllowed(src string) bool {
	return strings.HasPrefix(src, "/") && src == strings.TrimSpace(src) && !hostpath.Bad(src)
}

// volumeJudge decides, once for each name, whether a named volume cannot be shown to be free of host
// access. err is the first lookup the engine did not answer.
type volumeJudge struct {
	ctx    context.Context
	lookup volumeLookup
	bad    map[string]bool
	err    error
}

// refused reports whether the named volume is not a valid volume name, cannot be looked up or exists
// with a driver or options that give host access (volumeEscalation, as for a volume create).
func (j *volumeJudge) refused(name string) bool {
	if bad, seen := j.bad[name]; seen {
		return bad
	}
	bad := !volumeNameRe.MatchString(name) || j.lookup == nil
	if !bad {
		driver, opts, found, err := j.lookup(j.ctx, name)
		switch {
		case err != nil:
			if j.err == nil {
				j.err = fmt.Errorf("%w: %w", errVolumeLookup, err)
			}
		default:
			bad = found && volumeEscalation(volume.CreateRequest{Driver: driver, DriverOpts: opts}) != ""
		}
	}
	j.bad[name] = bad
	return bad
}

// hostNetworkJudge decides, once for each target, whether a network is the host network, by name or
// by asking the agent's own engine.
type hostNetworkJudge struct {
	ctx    context.Context
	lookup networkLookup
	seen   map[string]bool
}

// isHost reports whether the engine says target is the host network. A lookup that is nil or fails
// finds nothing.
func (j *hostNetworkJudge) isHost(target string) bool {
	if host, seen := j.seen[target]; seen {
		return host
	}
	host := target == hostNetwork
	if !host && j.lookup != nil {
		if name, driver, err := j.lookup(j.ctx, target); err == nil {
			host = name == hostNetwork || strings.EqualFold(driver, hostNetwork)
		}
	}
	j.seen[target] = host
	return host
}

// userDefinedIsHost reports whether a network mode that names a user-defined network is the host
// network by lookup. The standard modes (the default, bridge and none) are never looked up.
func (j *hostNetworkJudge) userDefinedIsHost(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "default", "bridge", "none":
		return false
	}
	return j.isHost(mode)
}

// checkContainerHostPolicy refuses a container create that gives the container host access unless
// the host allows it. A named-volume lookup the engine did not answer also stops the write, but as
// errVolumeLookup (an engine fault) and not as host access, unless a real finding is present. The
// paths of the findings are in the error for the log; they are never sent back to the server, whose
// answer is the typed refusal.
func checkContainerHostPolicy(req container.CreateRequest, env bodyEnv) error {
	if env.policy.AllowPrivilegedServices {
		return nil
	}
	ctx := env.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	found, lookupErr := containerEscalations(ctx, req, env.lookup, env.volumes)
	if len(found) > 0 {
		// A real finding is named as host access even when a lookup also failed.
		return fmt.Errorf("%w: %w: gives the container host access: %s", errBodyRefused, errHostPolicy, strings.Join(found, ", "))
	}
	return lookupErr
}
