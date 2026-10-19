// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/rbac/compose"
	"github.com/ianglennon/bayplan/internal/rbac/swarmspec"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// CodeNeedsAdmin is the code of the blocking problem for a host-escalation setting the caller may
// not deploy; CodePolicyUnreadable the code of the one for a compose file the policy cannot read.
const (
	CodeNeedsAdmin       = "needs_admin"
	CodePolicyUnreadable = "policy_unreadable"
)

// The kinds of host access a refusal names, in the order it lists them. The first four are the
// words of the Deploy button's tooltip (UI-SPEC, "Needs Admin to use ...").
const (
	KindBindMounts      = "bind mounts"
	KindPrivileges      = "extra privileges"
	KindHostNetworking  = "host networking"
	KindDeviceMounts    = "device mounts"
	KindHostNamespaces  = "host namespaces"
	KindUnresolvedValue = "unresolved values"
)

var kindOrder = []string{KindBindMounts, KindPrivileges, KindHostNetworking, KindDeviceMounts, KindHostNamespaces, KindUnresolvedValue}

// PolicyEntry is one host-escalation setting the caller may not deploy: what it is, and where.
type PolicyEntry struct {
	Text, File string
	Line       int
}

// PolicyResult is the host-escalation judgement of a stack for one caller. Refused is true when
// the caller may not deploy the stack; Problems then holds a blocking problem for each setting
// (and for a compose file the policy cannot read), Entries the same settings for the refusal of a
// deploy, and Kinds the kinds of host access they are. All three are empty when the caller may
// deploy it, an env Admin included: the settings are only listed for someone who may not.
type PolicyResult struct {
	Refused  bool
	Problems stacks.Problems
	Entries  []PolicyEntry
	Kinds    []string
}

// Policy judges the host access a stack asks for, on the values a deploy would use (AUTH-10, D-13).
// It runs the compose rules over compose.yaml with the stack's variables filled in, each finding at
// the line the value was written, and then the service rules over each converted service, which
// catch what the compose file only implies (the swarm's host network attached by name). A setting
// of the second kind is listed at its service when the compose rules did not already name that
// service. Below env Admin each setting is refused; an env Admin or an instance admin may deploy
// them all, and a system principal acting for an owner is judged as the owner.
//
// A missing checker or caller is ErrNoPolicy, never "allowed". Every text that could hold a secret
// value goes through the stack's masker.
func (d Deps) Policy(ctx context.Context, caller Caller, envID string, copy stacks.WorkingCopy, plan convert.Plan) (PolicyResult, error) {
	if d.Checker == nil || caller.Principal == nil {
		return PolicyResult{}, ErrNoPolicy
	}
	src := copy.Compose()
	m := stacks.NewMasker(copy.Vars, copy.Files)
	dec, err := d.Checker.CheckComposeDeployResolved(ctx, caller.Principal, envID, src, tableLookup(copy.Vars))
	if err != nil {
		return PolicyResult{}, err
	}
	var res PolicyResult
	if dec.Reason == rbac.ReasonComposeUnreadable {
		res.Refused = true
		res.Problems = append(res.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: CodePolicyUnreadable, Message: dec.Message, File: stacks.ComposeFile,
			Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
		return res, nil
	}
	kinds := map[string]bool{}
	flagged := map[string]bool{}
	if !dec.Allowed {
		for _, f := range dec.Findings {
			res.add(m, describeFinding(f), f.Key, f.Line, f.Column)
			kinds[kindOf(f.Rule)] = true
			flagged[f.Service] = true
		}
	}

	ix, _ := stacks.NewPositionIndex(src)
	networks := make(map[string]string, len(plan.Networks))
	for _, n := range plan.Networks {
		networks[n.Name] = n.Driver
	}
	resolve := func(target string) (string, string, bool) {
		driver, ok := networks[target]
		return target, driver, ok
	}
	for _, sp := range plan.Services {
		raw, err := json.Marshal(sp.Spec)
		if err != nil {
			return PolicyResult{}, err
		}
		sd, err := d.Checker.CheckServiceSpec(ctx, caller.Principal, envID, raw, resolve)
		if err != nil {
			return PolicyResult{}, err
		}
		if sd.Allowed {
			continue
		}
		if sd.Reason == rbac.ReasonServiceSpecUnreadable {
			res.Refused = true
			res.Problems = append(res.Problems, stacks.Problem{
				Severity: stacks.SeverityError, Code: CodePolicyUnreadable, Message: "The service " + sp.Short + " can't be read for the policy check.",
				File: stacks.ComposeFile, Key: "services." + sp.Short, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			})
			continue
		}
		for _, f := range sd.Findings {
			kinds[kindOf(f.Rule)] = true
			if flagged[sp.Short] {
				continue
			}
			line, col := 1, 1
			if pos, ok := ix.Lookup("services." + sp.Short); ok {
				line, col = pos.Line, pos.Col
			}
			res.add(m, describeSpecFinding(f), "services."+sp.Short, line, col)
		}
	}

	res.Refused = res.Refused || len(res.Entries) > 0 || len(kinds) > 0
	for _, k := range kindOrder {
		if kinds[k] {
			res.Kinds = append(res.Kinds, k)
		}
	}
	return res, nil
}

// add records one refused setting as an entry and a blocking problem, masked.
func (r *PolicyResult) add(m stacks.Masker, text, key string, line, col int) {
	text = m.Mask(text)
	r.Refused = true
	r.Entries = append(r.Entries, PolicyEntry{Text: text, File: stacks.ComposeFile, Line: line})
	r.Problems = append(r.Problems, stacks.Problem{
		Severity: stacks.SeverityError, Code: CodeNeedsAdmin, Message: "This needs Admin: " + text + ".",
		File: stacks.ComposeFile, Key: m.Mask(key), Line: line, Col: col, EndLine: line, EndCol: col + 1,
	})
}

// tableLookup is the lookup of the stack's variables a deploy interpolates with: a variable with a
// blank value has no value, as in stacks.Load.
func tableLookup(vars []stacks.Var) func(string) (string, bool) {
	table := make(map[string]string, len(vars))
	for _, v := range vars {
		if v.Value != "" {
			table[v.Name] = v.Value
		}
	}
	return func(name string) (string, bool) {
		v, ok := table[name]
		return v, ok
	}
}

// kindOf is the kind of host access a rule of the compose or the service checks stands for.
func kindOf(rule string) string {
	switch rule {
	case "volumes", swarmspec.RuleBindPath, swarmspec.RuleNamedPipe, "secret_file", "config_file":
		return KindBindMounts
	case "privileged", "security_opt", "userns_mode",
		swarmspec.RuleCapAdd, swarmspec.RuleSELinux, swarmspec.RuleSeccomp, swarmspec.RuleAppArmor, swarmspec.RulePluginRuntime:
		return KindPrivileges
	case "network_mode", swarmspec.RuleHostNetwork:
		return KindHostNetworking
	case "devices", "device_cgroup_rules", "volume_driver_opts", swarmspec.RuleVolumeDevice:
		return KindDeviceMounts
	case "pid", "ipc", "uts", "cgroup", "cgroup_parent":
		return KindHostNamespaces
	}
	return KindUnresolvedValue
}

// describeFinding says what a compose finding is, the way a refusal names it.
func describeFinding(f compose.Finding) string {
	switch f.Rule {
	case "volumes":
		return "a bind mount of " + f.Value
	case "volume_driver_opts":
		if strings.HasSuffix(f.Key, ".type") {
			return "a volume of type " + f.Value + ", which mounts a host device"
		}
		return "a volume that binds " + f.Value
	case "secret_file":
		return "a secret read from the host file " + f.Value
	case "config_file":
		return "a config read from the host file " + f.Value
	case "devices":
		return "a device mount of " + f.Value
	case "unresolvable":
		return "a value that can't be checked (" + f.Value + ")"
	}
	return f.Key + ": " + f.Value
}

// describeSpecFinding says what a finding in a converted spec is.
func describeSpecFinding(f compose.Finding) string {
	switch f.Rule {
	case swarmspec.RuleBindPath:
		return "a bind mount of " + f.Value
	case swarmspec.RuleVolumeDevice:
		if strings.HasSuffix(f.Key, ".type") {
			return "a volume of type " + f.Value + ", which mounts a host device"
		}
		return "a volume that binds " + f.Value
	case swarmspec.RuleHostNetwork:
		return "the " + f.Value + " network, which shares the node's network"
	case swarmspec.RuleCapAdd:
		return "cap_add: " + f.Value
	}
	return f.Key + ": " + f.Value
}
