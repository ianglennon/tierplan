// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rebuild

import (
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// VolumeInfo is a volume of the host as the engine inspects it: what the file needs to write it.
type VolumeInfo struct {
	Name, Driver    string
	Labels, Options map[string]string
}

// networkKey and volumeKey are the keys of the compose file an object is declared under.
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

// NetworkCandidates are the renderings of an existing network that a file could declare, minimal first.
func NetworkCandidates(info NetworkInfo, project, key string) []types.NetworkConfig {
	cfg := types.NetworkConfig{
		Driver:     info.Driver,
		Internal:   info.Internal,
		Attachable: info.Attachable,
		Labels:     userObjectLabels(info.Labels),
	}
	if cfg.Driver == "bridge" {
		cfg.Driver = ""
	}
	if info.Name != engineName(project, key) {
		cfg.Name = info.Name
	}
	if info.IPv6 {
		on := true
		cfg.EnableIPv6 = &on
	}
	return []types.NetworkConfig{cfg}
}

// VolumeCandidates are the renderings of an existing volume that a file could declare, minimal first.
func VolumeCandidates(info VolumeInfo, project, key string) []types.VolumeConfig {
	cfg := types.VolumeConfig{Labels: userObjectLabels(info.Labels)}
	if info.Driver != "" && info.Driver != "local" {
		cfg.Driver = info.Driver
	}
	if info.Name != engineName(project, key) {
		cfg.Name = info.Name
	}
	return []types.VolumeConfig{cfg}
}
