// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"maps"
	"reflect"
	"slices"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// StripOwnership returns a copy of spec without any of TierPlan's own tierplan.* labels (IsOwnershipLabel; the user's
// tierplan.keep stays, so removing it shows as drift), on the service, on its containers and on each of
// its volume mounts. Drift is judged on both sides through it: an adopted stack's services carry none of TierPlan's
// labels until its first deploy, and the labels say who owns a service, not how it is set up (D-21, D-24). The ID the
// converter puts on a named volume's mount is one of them: a stale one is replaced by the first deploy, and drift
// never reports it (D-11). The spec given is not changed.
func StripOwnership(spec swarm.ServiceSpec) swarm.ServiceSpec {
	spec.Labels = withoutOwnership(spec.Labels)
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		stripped := *cs
		stripped.Labels = withoutOwnership(cs.Labels)
		if cs.Mounts != nil {
			stripped.Mounts = make([]mount.Mount, len(cs.Mounts))
			for i, m := range cs.Mounts {
				if m.VolumeOptions != nil {
					vo := *m.VolumeOptions
					vo.Labels = withoutOwnership(vo.Labels)
					m.VolumeOptions = &vo
				}
				stripped.Mounts[i] = m
			}
		}
		spec.TaskTemplate.ContainerSpec = &stripped
	}
	return spec
}

// StripIngressAttachment returns a copy of spec without any TaskTemplate.Networks entry whose Target is the ingress
// network, by name (ingress.NetworkName) or by any of ids (the network's ID, for a live spec whose targets were not
// turned into names). The attachment is TierPlan's, added at deploy time (D-19): comparisons that must not see it
// (drift, the drift rows of a review, the adopt review, the ownership-only comparison) call this on both sides; the
// deploy review's own service diff does not, so a join or a leave stays a listed change there (D-20, D-21). The spec
// given is not changed, and one without the attachment comes back as it was.
func StripIngressAttachment(spec swarm.ServiceSpec, ids ...string) swarm.ServiceSpec {
	isIngress := func(target string) bool {
		return target == ingress.NetworkName || (target != "" && slices.Contains(ids, target))
	}
	if !slices.ContainsFunc(spec.TaskTemplate.Networks, func(n swarm.NetworkAttachmentConfig) bool { return isIngress(n.Target) }) {
		return spec
	}
	kept := make([]swarm.NetworkAttachmentConfig, 0, len(spec.TaskTemplate.Networks))
	for _, n := range spec.TaskTemplate.Networks {
		if !isIngress(n.Target) {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 {
		kept = nil
	}
	spec.TaskTemplate.Networks = kept
	return spec
}

// StripIngressStandalone returns a copy of svc without TierPlan's attachment to the host's ingress bridge (D-16, D-20):
// the networks entry keyed ingress.NetworkName or by any of keys (the key the project gives the shared network when the
// file declares it under another one), and the default network when the injection wrote it for svc (inj.DefaultAdded).
// A service the file already listed on the network (inj.AliasAdded) keeps its entry with all of the user's settings
// and loses only the alias TierPlan put on it (ingress.Alias of project and the service). Every other network is left
// alone, so a real change of the same service still shows.
//
// Comparisons that must not see the attachment (drift, and the rows of a review that revert what drift found) call it
// on both sides, the desired service and the one rebuilt from a container; the deploy review's own diff does not, so a
// join or a leave stays a listed change there. inj is taken in place of a flag because it is what the injection
// recorded, and nil is allowed: with ingress off the desired side has nothing injected, and a container that was made
// attached still has to lose the entry. The service given is not changed, and one without the attachment comes back
// as it was.
func StripIngressStandalone(svc types.ServiceConfig, project string, inj *InjectedIngress, keys ...string) types.ServiceConfig {
	isShared := func(key string) bool { return key == ingress.NetworkName || slices.Contains(keys, key) }
	aliasOnly := inj != nil && slices.Contains(inj.AliasAdded, svc.Name)
	dropDefault := inj != nil && slices.Contains(inj.DefaultAdded, svc.Name)
	alias := ingress.Alias(project, svc.Name)
	touched := false
	for key, cfg := range svc.Networks {
		switch {
		case isShared(key) && !aliasOnly, key == defaultNetworkKey && dropDefault:
			touched = true
		case isShared(key) && cfg != nil && slices.Contains(cfg.Aliases, alias):
			touched = true
		}
	}
	if !touched {
		return svc
	}
	nets := make(map[string]*types.ServiceNetworkConfig, len(svc.Networks))
	for key, cfg := range svc.Networks {
		switch {
		case isShared(key) && !aliasOnly, key == defaultNetworkKey && dropDefault:
			continue
		case isShared(key) && cfg != nil:
			entry := *cfg
			entry.Aliases = slices.DeleteFunc(slices.Clone(entry.Aliases), func(a string) bool { return a == alias })
			if len(entry.Aliases) == 0 {
				entry.Aliases = nil
			}
			if reflect.DeepEqual(entry, types.ServiceNetworkConfig{}) {
				nets[key] = nil
			} else {
				nets[key] = &entry
			}
		default:
			nets[key] = cfg
		}
	}
	if len(nets) == 0 {
		nets = nil
	}
	svc.Networks = nets
	return svc
}

// StandaloneIngressStripper is StripIngressStandalone for each service of a loaded project, ready to call on both sides
// of a comparison. inj is what the load injected (Loaded.Ingress, nil with ingress off). A service of the project that
// lists the shared network itself, whether the file wrote it or the injection added only its alias, keeps that entry;
// on any other service the entry goes, so a container made attached reads as the file does with ingress off.
func StandaloneIngressStripper(p *types.Project, inj *InjectedIngress) func(types.ServiceConfig) types.ServiceConfig {
	keys := []string{ingress.NetworkName}
	if key, ok := sharedNetworkKey(p, ingress.NetworkName); ok {
		keys = append(keys, key)
	}
	view := &InjectedIngress{Network: ingress.NetworkName}
	if inj != nil {
		view.DefaultAdded = inj.DefaultAdded
	}
	for name, svc := range p.Services {
		declared := false
		for _, key := range keys {
			_, has := svc.Networks[key]
			declared = declared || has
		}
		if inj != nil && slices.Contains(inj.Services, name) {
			declared = false
		}
		if declared {
			view.AliasAdded = append(view.AliasAdded, name)
		}
	}
	return func(svc types.ServiceConfig) types.ServiceConfig {
		return StripIngressStandalone(svc, p.Name, view, keys...)
	}
}

// withoutOwnership is labels less TierPlan's own bookkeeping keys (IsOwnershipLabel), as a new map; nil stays nil.
func withoutOwnership(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := maps.Clone(labels)
	for k := range out {
		if IsOwnershipLabel(k) {
			delete(out, k)
		}
	}
	return out
}
