// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package swarmspec is the D-13 host-escalation check over a decoded swarm
// service spec. Where internal/rbac/compose reads the text of a compose file,
// this package reads the swarm.ServiceSpec that is about to be forwarded to
// Docker, so the syntax of an editor (JSON, YAML, a form) cannot hide anything.
package swarmspec

import (
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/rbac/compose"
	"github.com/ianglennon/tierplan/proto/hostpath"
)

// The rule names of a finding.
const (
	RuleBindPath      = "bind_path"
	RuleVolumeDevice  = "volume_device"
	RuleNamedPipe     = "npipe"
	RuleCapAdd        = "cap_add"
	RuleSELinux       = "selinux"
	RuleSeccomp       = "seccomp"
	RuleAppArmor      = "apparmor"
	RuleHostNetwork   = "host_network"
	RulePluginRuntime = "plugin_runtime"
)

// hostNetwork is the name of the engine's host network.
const hostNetwork = "host"

// NetworkResolver answers the name and driver of the network a task attaches to
// by target (an ID or a name). ok is false when the network is not known; an
// unknown network is not a finding, because Docker refuses it itself.
type NetworkResolver func(target string) (name, driver string, ok bool)

// Check returns the host-escalation findings in spec, in the order of the spec.
// It is the same list of features the agent's own check refuses (06-03), the
// bind rule being hostpath.Bad and the local-volume rule hostpath.LocalVolumeHostAccess
// (D-38), which read a bind source as compose does and a local volume as the daemon
// mounts it, plus the plugin-runtime rule that only the server applies. The server
// refuses the same inline volumes as the agent, so an Operator is refused them on the
// server's own host too, which has no agent. Service is the spec's name, Key the spec path,
// Value the offending value, and Line is 0 because a decoded spec has no lines.
//
// A nil resolver, or one that does not know a network, finds nothing for a
// target that is not literally named host.
func Check(spec swarm.ServiceSpec, resolve NetworkResolver) []compose.Finding {
	var out []compose.Finding
	add := func(key, value, rule, why string) {
		out = append(out, compose.Finding{Service: spec.Name, Key: key, Value: value, Rule: rule, Why: why})
	}
	const task = "TaskTemplate."
	tt := spec.TaskTemplate
	if tt.Runtime == swarm.RuntimePlugin || tt.Runtime == swarm.RuntimeNetworkAttachment {
		add(task+"Runtime", string(tt.Runtime), RulePluginRuntime, "a plugin or network-attachment task runs code the Docker engine trusts")
	} else if tt.PluginSpec != nil {
		add(task+"PluginSpec", tt.PluginSpec.Name, RulePluginRuntime, "a plugin service installs privileged code on the node")
	} else if tt.NetworkAttachmentSpec != nil {
		add(task+"NetworkAttachmentSpec", "", RulePluginRuntime, "a network-attachment task runs code the Docker engine trusts")
	}
	if cs := tt.ContainerSpec; cs != nil {
		const base = task + "ContainerSpec."
		for i, m := range cs.Mounts {
			at := fmt.Sprintf("%sMounts[%d]", base, i)
			switch m.Type {
			case mount.TypeNamedPipe:
				add(at+".Source", m.Source, RuleNamedPipe, "a named pipe mount can reach the Docker engine on a Windows host")
			case mount.TypeBind, "":
				if hostpath.Bad(m.Source) {
					add(at+".Source", m.Source, RuleBindPath, "a bind of this host path gives the task access outside its own data")
				}
			}
			if vo := m.VolumeOptions; vo != nil && vo.DriverConfig != nil {
				if name := strings.ToLower(strings.TrimSpace(vo.DriverConfig.Name)); name == "" || name == "local" {
					switch key := hostpath.LocalVolumeHostAccess(vo.DriverConfig.Options); key {
					case "device":
						add(at+".VolumeOptions.DriverConfig.Options.device", vo.DriverConfig.Options["device"], RuleVolumeDevice,
							"a local volume that binds this host path is a bind in disguise; the engine reads a relative path from its own working directory")
					case "type":
						add(at+".VolumeOptions.DriverConfig.Options.type", vo.DriverConfig.Options["type"], RuleVolumeDevice,
							"a local volume of this filesystem type mounts a host device, a host filesystem or host paths given in its options")
					}
				}
			}
		}
		for i, c := range cs.CapabilityAdd {
			add(fmt.Sprintf("%sCapabilityAdd[%d]", base, i), c, RuleCapAdd, "an added Linux capability widens what the task may do on the host")
		}
		if p := cs.Privileges; p != nil {
			if se := p.SELinuxContext; se != nil {
				switch {
				case se.Disable:
					add(base+"Privileges.SELinuxContext.Disable", "true", RuleSELinux, "SELinux confinement is turned off for the task")
				case strings.EqualFold(se.Type, "spc_t"):
					add(base+"Privileges.SELinuxContext.Type", se.Type, RuleSELinux, "the spc_t SELinux type is a super privileged container")
				}
			}
			if sc := p.Seccomp; sc != nil && (sc.Mode == swarm.SeccompModeUnconfined || sc.Mode == swarm.SeccompModeCustom) {
				add(base+"Privileges.Seccomp.Mode", string(sc.Mode), RuleSeccomp, "the default seccomp profile is replaced")
			}
			if aa := p.AppArmor; aa != nil && aa.Mode == swarm.AppArmorModeDisabled {
				add(base+"Privileges.AppArmor.Mode", string(aa.Mode), RuleAppArmor, "AppArmor confinement is turned off for the task")
			}
		}
	}
	known := map[string]bool{}
	for i, n := range tt.Networks {
		host, seen := known[n.Target]
		if !seen {
			host = n.Target == hostNetwork
			if !host && resolve != nil {
				if name, driver, ok := resolve(n.Target); ok {
					host = name == hostNetwork || strings.EqualFold(driver, hostNetwork)
				}
			}
			known[n.Target] = host
		}
		if host {
			add(fmt.Sprintf("%sNetworks[%d].Target", task, i), n.Target, RuleHostNetwork, "the host network shares the node's network stack with the task")
		}
	}
	return out
}

// FormatSpec renders a finding as "{path}: {value}", the way a refusal names the
// offending setting of a spec.
func FormatSpec(f compose.Finding) string {
	return f.Key + ": " + f.Value
}
