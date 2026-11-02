// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package convert

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// errNoSuchNetwork is wrapped with the service and the network key.
var errNoSuchNetwork = errors.New("convert: the service uses a network the file does not declare")

// defaultNetwork is the key of the network a service joins when it names none.
const defaultNetwork = "default"

// networkPlans plans the networks the services use, in key order: the ones the file declares and
// the default network when a service joins it. A stack makes overlay networks only; another
// driver (macvlan, ipvlan, a plugin) reaches the host's network interfaces, which creating a
// network by hand needs Admin for, so it is refused until the review policy (plan 07-14).
func networkPlans(p *composetypes.Project, o Options, used map[string]bool) ([]NetworkPlan, error) {
	keys := make([]string, 0, len(used))
	for key := range used {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]NetworkPlan, 0, len(keys))
	for _, key := range keys {
		np := convertNetwork(key, p, o)
		if !np.External && np.Driver != "overlay" {
			return nil, &FieldError{Path: "networks." + key + ".driver", Err: &NotConvertedError{Service: "network " + key, Fields: []string{"driver: " + np.Driver}}}
		}
		out = append(out, np)
	}
	return out, nil
}

// serviceNetworks builds a service's network attachments, sorted by target, and returns the keys
// of the networks it joins. The attachment is the conversion of convertServiceNetworks in
// docker/cli's cli/compose/convert/service.go (Apache-2.0): the aliases the file gives come
// first, then the service's own name, and the name is added only on a user-defined network,
// because the built-in ones do not take aliases.
func serviceNetworks(svc composetypes.ServiceConfig, p *composetypes.Project, o Options) ([]swarm.NetworkAttachmentConfig, []string, error) {
	keys := networkKeys(svc)
	out := make([]swarm.NetworkAttachmentConfig, 0, len(keys))
	for _, key := range keys {
		if _, declared := p.Networks[key]; !declared && key != defaultNetwork {
			return nil, nil, fmt.Errorf("%w: service %s uses %q", errNoSuchNetwork, svc.Name, key)
		}
		target := convertNetwork(key, p, o).Name
		att := swarm.NetworkAttachmentConfig{Target: target}
		if cfg := svc.Networks[key]; cfg != nil {
			att.Aliases = slices.Clone(cfg.Aliases)
			if len(cfg.DriverOpts) > 0 {
				att.DriverOpts = maps.Clone(cfg.DriverOpts)
			}
		}
		if isUserDefined(target) && !slices.Contains(att.Aliases, svc.Name) {
			att.Aliases = append(att.Aliases, svc.Name)
		}
		out = append(out, att)
	}
	slices.SortFunc(out, func(a, b swarm.NetworkAttachmentConfig) int { return cmp.Compare(a.Target, b.Target) })
	return out, keys, nil
}

// isUserDefined reports whether target names a user-defined network: not the built-in default,
// bridge, host or none networks and not another container's namespace. It is the rule of
// IsUserDefined on docker's NetworkMode type.
func isUserDefined(target string) bool {
	switch target {
	case "default", "bridge", "host", "none":
		return false
	}
	return !strings.HasPrefix(target, "container:")
}

// networkKeys returns the keys of the networks a service joins, sorted: its own list, or the
// default network when it names none.
func networkKeys(svc composetypes.ServiceConfig) []string {
	if len(svc.Networks) == 0 {
		return []string{defaultNetwork}
	}
	keys := make([]string, 0, len(svc.Networks))
	for key := range svc.Networks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// convertNetwork converts the network the compose file calls key. The default network is a plain
// overlay network, not attachable, as docker stack deploy makes it (the parity oracle on a real
// swarm showed the engine holding attachable=false there); one the file declares keeps its own
// settings. An external network is referenced by the name the file gave it and is never created.
func convertNetwork(key string, p *composetypes.Project, o Options) NetworkPlan {
	decl, declared := p.Networks[key]
	np := NetworkPlan{Key: key, Name: o.StackName + "_" + key, Driver: "overlay"}
	if declared {
		if decl.Name != "" {
			np.Name = decl.Name
		}
		np.External = bool(decl.External)
		np.Driver = cmp.Or(decl.Driver, np.Driver)
		np.Internal = decl.Internal
		np.Attachable = decl.Attachable
		np.DriverOpts = maps.Clone(decl.DriverOpts)
		np.IPAMDriver = decl.Ipam.Driver
		for _, pool := range decl.Ipam.Config {
			if pool != nil {
				np.Pools = append(np.Pools, Pool{Subnet: pool.Subnet, Gateway: pool.Gateway, IPRange: pool.IPRange})
			}
		}
	}
	if np.External {
		return np
	}
	if subnet := o.Subnets[key]; subnet != "" && len(np.Pools) == 0 {
		np.Pools = []Pool{{Subnet: subnet}}
	}
	np.Labels = map[string]string{}
	if declared {
		maps.Copy(np.Labels, decl.Labels)
	}
	np.Labels[stacks.LabelSwarmNamespace] = o.StackName
	np.Labels[stacks.LabelStackID] = o.StackID
	return np
}
