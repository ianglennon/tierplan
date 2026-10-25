// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rebuild

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// VolumeInfo is a volume of the host as the engine inspects it: what the file needs to write it.
type VolumeInfo struct {
	Name, Driver    string
	Labels, Options map[string]string
}

// engineName is the name the SDK gives the object a project declares under key, when the file names none.
func engineName(project, key string) string { return project + "_" + key }

// userObjectLabels are the labels a person put on a network or a volume: not the SDK's own
// (com.docker.compose.*), Bayplan's, or the engine's mark of an anonymous volume.
func userObjectLabels(in map[string]string) types.Labels {
	var out types.Labels
	for k, v := range in {
		if strings.HasPrefix(k, "com.docker.compose.") || strings.HasPrefix(k, "bayplan.") || k == "com.docker.volume.anonymous" {
			continue
		}
		if out == nil {
			out = types.Labels{}
		}
		out[k] = v
	}
	return out
}

// NetworkCandidates are the renderings of an existing network that a file could declare, fewest settings first
// and the most faithful to the object last (D-46). The Compose SDK stamps a hash of the config the project
// declared on each network it makes, and recreates one whose hash differs from the config the project declares
// next; which rendering the original file was cannot be read from the object, because the engine fills in what a
// file leaves out (the bridge driver, an address pool). So there is one candidate for each way a file could have
// said less than the object holds: an explicit driver bridge or none, the driver options or none, and no address
// pool, the subnets, the subnets and gateways, the subnets and ranges, or all three. What the object cannot have
// without (a driver other than bridge, internal, attachable, IPv6, the labels a person set, a name that is not
// the project's) is in every one. The caller tries them against the hash the host holds.
func NetworkCandidates(info NetworkInfo, project, key string) []types.NetworkConfig {
	base := types.NetworkConfig{
		Internal:   info.Internal,
		Attachable: info.Attachable,
		Labels:     userObjectLabels(info.Labels),
	}
	if info.Name != engineName(project, key) {
		base.Name = info.Name
	}
	if info.IPv6 {
		on := true
		base.EnableIPv6 = &on
	}
	drivers := []string{""}
	switch info.Driver {
	case "bridge":
		drivers = []string{"", "bridge"}
	case "":
	default:
		drivers = []string{info.Driver}
	}
	options := []types.Options{nil}
	if len(info.Options) > 0 {
		options = append(options, types.Options(maps.Clone(info.Options)))
	}
	pools := ipamLevels(info)

	type pick struct{ d, o, p int }
	var picks []pick
	for d := range drivers {
		for o := range options {
			for p := range pools {
				picks = append(picks, pick{d, o, p})
			}
		}
	}
	slices.SortStableFunc(picks, func(x, y pick) int { return cmp.Compare(x.d+x.o+x.p, y.d+y.o+y.p) })
	out := make([]types.NetworkConfig, 0, len(picks))
	for _, pk := range picks {
		cfg := base
		cfg.Driver, cfg.DriverOpts, cfg.Ipam = drivers[pk.d], options[pk.o], pools[pk.p]
		out = append(out, cfg)
	}
	return out
}

// ipamLevels are the ways a file could have declared the address pools of a network, fewest settings first: none,
// then each pool with its subnet only, with its gateway too, with its range, and with both.
func ipamLevels(info NetworkInfo) []types.IPAMConfig {
	driver := ""
	if info.IPAMDriver != "" && info.IPAMDriver != "default" {
		driver = info.IPAMDriver
	}
	levels := []types.IPAMConfig{{Driver: driver}}
	var pools []Pool
	for _, p := range info.Pools {
		if p.Subnet != "" {
			pools = append(pools, p)
		}
	}
	if len(pools) == 0 {
		return levels
	}
	var gateways, ranges bool
	for _, p := range pools {
		gateways = gateways || p.Gateway != ""
		ranges = ranges || p.IPRange != ""
	}
	level := func(gateway, iprange bool) types.IPAMConfig {
		cfg := types.IPAMConfig{Driver: driver}
		for _, p := range pools {
			pool := &types.IPAMPool{Subnet: p.Subnet}
			if gateway {
				pool.Gateway = p.Gateway
			}
			if iprange {
				pool.IPRange = p.IPRange
			}
			cfg.Config = append(cfg.Config, pool)
		}
		return cfg
	}
	levels = append(levels, level(false, false))
	if gateways {
		levels = append(levels, level(true, false))
	}
	if ranges {
		levels = append(levels, level(false, true))
	}
	if gateways && ranges {
		levels = append(levels, level(true, true))
	}
	return levels
}

// VolumeCandidates are the renderings of an existing volume that a file could declare, fewest settings first: the
// SDK hashes the config the project declared (D-46), and the driver options may or may not have been in it. The
// driver local is the default and is not written (the SDK hashes an empty driver as local). What the volume
// cannot have without (a driver other than local, the labels a person set, a name that is not the project's) is in
// both.
func VolumeCandidates(info VolumeInfo, project, key string) []types.VolumeConfig {
	cfg := types.VolumeConfig{Labels: userObjectLabels(info.Labels)}
	if info.Driver != "" && info.Driver != "local" {
		cfg.Driver = info.Driver
	}
	if info.Name != engineName(project, key) {
		cfg.Name = info.Name
	}
	out := []types.VolumeConfig{cfg}
	if len(info.Options) > 0 {
		with := cfg
		with.DriverOpts = types.Options(maps.Clone(info.Options))
		out = append(out, with)
	}
	return out
}
