// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/rebuild"
)

// RebuildOptions say how a live rebuild is made.
type RebuildOptions struct {
	// Reveal is whether the caller may read the values the engine holds for the stack: an environment value, a
	// command, a health check command, a log option (secrets.reveal). Without it the specs are masked before the
	// file is made, so nothing the caller may not read is ever in it (D-47).
	Reveal bool
}

// RebuildError is a rebuild that came out as a file that cannot be used: the services could not be written as
// compose, or the file does not pass the check every stack passes. Reason is a sentence for the page and holds no
// value Reveal did not allow.
type RebuildError struct{ Reason string }

func (e *RebuildError) Error() string { return "stackops: rebuild: " + e.Reason }

// ErrRebuildTarget is a rebuild of a stack the engine direction does not support yet.
var ErrRebuildTarget = errors.New("stackops: rebuilding a stack that runs on a standalone host is not supported here")

// RebuildLive rebuilds the compose file of a stack from the services that run it (D-12). It reads the engine once,
// through a manager (the services of the namespace, every network, and the names of the secrets and configs), and
// writes nothing: the content of a secret or a config is never read (D-13). The services the stack's ownership
// owns are the ones rebuilt, and each spec passes through redact.MaskServiceSpec first, so a caller without
// secrets.reveal gets <hidden> where the engine holds an environment value, a command or a log option.
//
// The rebuilt file has to pass stacks.Check, the check every stack passes before it is deployed; one that does not
// is a *RebuildError naming the first problem.
func (d Deps) RebuildLive(ctx context.Context, envID string, st Stack, o RebuildOptions) (rebuild.Result, error) {
	if st.Target != stacks.TargetSwarm {
		return rebuild.Result{}, ErrRebuildTarget
	}
	read, err := d.readSwarmForRebuild(ctx, envID, st)
	if err != nil {
		return rebuild.Result{}, err
	}
	own := OwnershipOf(st)
	idName := make(map[string]string, len(read.networks))
	for _, n := range read.networks {
		idName[n.ID] = n.Name
	}
	in := rebuild.SwarmInput{Namespace: st.Name}
	attached := map[string]bool{}
	for _, det := range read.services {
		if !own.owns(det.Stack, det.Labels) {
			continue
		}
		var spec swarm.ServiceSpec
		if err := json.Unmarshal(det.Spec, &spec); err != nil {
			return rebuild.Result{}, fmt.Errorf("stackops: read the spec of service %s: %w", det.Name, err)
		}
		spec, _ = redact.MaskServiceSpec(spec, o.Reveal)
		// The masked spec shares nothing with the one the engine returned, but the attachments are written to,
		// so they are copied first.
		spec.TaskTemplate.Networks = slices.Clone(spec.TaskTemplate.Networks)
		for i, n := range spec.TaskTemplate.Networks {
			if name, ok := idName[n.Target]; ok {
				spec.TaskTemplate.Networks[i].Target = name
			}
			attached[spec.TaskTemplate.Networks[i].Target] = true
		}
		in.Services = append(in.Services, rebuild.SwarmService{ID: det.ID, Name: det.Name, Spec: spec})
	}
	if len(in.Services) == 0 {
		return rebuild.Result{}, &RebuildError{Reason: "no service of " + st.Name + " is running"}
	}
	for _, n := range read.networks {
		if attached[n.Name] {
			in.Networks = append(in.Networks, networkInfo(n, read.detail[n.ID]))
		}
	}
	for _, s := range read.secrets {
		in.Secrets = append(in.Secrets, rebuild.ObjectInfo{ID: s.ID, Name: s.Name})
	}
	for _, c := range read.configs {
		in.Configs = append(in.Configs, rebuild.ObjectInfo{ID: c.ID, Name: c.Name})
	}

	res, err := rebuild.Swarm(in)
	if err != nil {
		return rebuild.Result{}, &RebuildError{Reason: err.Error()}
	}
	check := stacks.Check(ctx, stacks.CheckInput{
		Name: st.Name, Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: res.YAML}}},
	})
	for _, p := range check.Problems {
		if p.Severity == stacks.SeverityError {
			return rebuild.Result{}, &RebuildError{Reason: p.Message}
		}
	}
	return res, nil
}

// rebuildRead is what one ClusterRead of a rebuild holds.
type rebuildRead struct {
	services []dockerx.ServiceDetail
	networks []dockerx.NetworkRow
	// detail holds the inspect of each network the stack's services attach and the stack's label is on, which
	// is what the file needs of an owned network (driver options and address pools).
	detail           map[string]dockerx.NetworkDetail
	secrets, configs []objectRow
}

// readSwarmForRebuild reads, through a manager, the services of the namespace, every network, the networks the
// stack owns in detail, and the secrets and configs by name (a list, never an inspect).
func (d Deps) readSwarmForRebuild(ctx context.Context, envID string, st Stack) (rebuildRead, error) {
	var out rebuildRead
	out.detail = map[string]dockerx.NetworkDetail{}
	err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		if out.services, e = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, st.Name); e != nil {
			return e
		}
		if out.networks, e = c.Networks(ctx); e != nil {
			return e
		}
		for _, n := range out.networks {
			if n.Labels[stacks.LabelSwarmNamespace] != st.Name {
				continue
			}
			det, _, e := c.NetworkDetail(ctx, n.ID, false)
			if e != nil {
				return e
			}
			out.detail[n.ID] = det
		}
		secrets, e := c.SecretList(ctx)
		if e != nil {
			return e
		}
		configs, e := c.ConfigList(ctx)
		if e != nil {
			return e
		}
		out.secrets, out.configs = secretRows(secrets), configRows(configs)
		return nil
	})
	return out, err
}

// networkInfo is what the rebuild needs of a network: its row from the list, and the inspect when it has one.
func networkInfo(row dockerx.NetworkRow, det dockerx.NetworkDetail) rebuild.NetworkInfo {
	info := rebuild.NetworkInfo{
		ID: row.ID, Name: row.Name, Driver: row.Driver, Attachable: row.Attachable, Internal: row.Internal,
		Labels: row.Labels,
	}
	if det.ID != "" {
		info.Options = det.Options
		for _, p := range det.IPAM {
			info.Pools = append(info.Pools, rebuild.Pool{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
		}
	}
	return info
}
