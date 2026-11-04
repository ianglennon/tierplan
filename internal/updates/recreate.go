// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"bytes"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

// shortIDLen is how many characters of its ID the engine uses as a container's default
// hostname and as its automatic network alias.
const shortIDLen = 12

// RecreateSpec derives the create document of a plain container's replacement from the inspect
// of the container it replaces, with only Config.Image changed (D-07). It is pure: it reads
// its arguments and nothing else, and it never modifies old.
//
// Everything the inspect carries is kept as it is: labels, environment, command, entrypoint,
// user, working directory, restart policy, healthcheck, published ports, named volumes, bind
// mounts, limits and the rest of the host configuration. What the inspect reports as runtime
// state is not: the addresses, gateways and endpoint IDs the engine handed out are left for it
// to hand out again, while the settings somebody chose (aliases, static addresses, MAC address,
// links, driver options) are kept per network.
//
// The old container's anonymous volumes are attached again by their names and dropped from
// Config.Volumes, because Config.Volumes would make the engine create new, empty ones and the
// data would stay behind in the old container's volumes (RESEARCH Pitfall 11). A hostname equal
// to the old container's short ID is the engine's default, so it is reset and the replacement
// gets its own; a hostname somebody set is kept. The old short ID is also taken off every
// network alias, for the same reason.
//
// first is the endpoint of the one network the create may carry (Engine 24 takes only one):
// the network the container's network mode names, or the first by name. more holds every other
// network, by network name, for the caller to connect before it starts the replacement. The
// caller treats the returned values as read-only apart from Config.Image: they share their
// maps and slices with old wherever RecreateSpec did not have to change them.
func RecreateSpec(old container.InspectResponse, image string) (cfg *container.Config, host *container.HostConfig, first *network.NetworkingConfig, more map[string]*network.EndpointSettings) {
	oldCfg := container.Config{}
	if old.Config != nil {
		oldCfg = *old.Config
	}
	oldHost := container.HostConfig{}
	if old.HostConfig != nil {
		oldHost = *old.HostConfig
	}
	cfg, host = &oldCfg, &oldHost
	cfg.Image = image

	shortID := old.ID
	if len(shortID) > shortIDLen {
		shortID = shortID[:shortIDLen]
	}
	if shortID != "" && cfg.Hostname == shortID {
		cfg.Hostname = ""
	}

	reattachAnonymousVolumes(old, cfg, host)

	more = map[string]*network.EndpointSettings{}
	names := []string{}
	if old.NetworkSettings != nil {
		for name := range old.NetworkSettings.Networks {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return cfg, host, nil, more
	}
	primary := primaryNetwork(string(host.NetworkMode), names)
	for _, name := range names {
		es := keptEndpoint(old.NetworkSettings.Networks[name], old.ID, shortID)
		if name == primary {
			first = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{name: es}}
			continue
		}
		more[name] = es
	}
	return cfg, host, first, more
}

// reattachAnonymousVolumes turns each volume the old container holds without having been asked
// for it by a bind or a mount into an explicit mount of that same volume, and takes its target
// off cfg.Volumes. A volume the container reaches through VolumesFrom is not in Config.Volumes
// and is left to VolumesFrom. cfg and host are copies of the old container's: only the map and
// the slice that change are cloned.
func reattachAnonymousVolumes(old container.InspectResponse, cfg *container.Config, host *container.HostConfig) {
	asked := map[string]bool{}
	for _, b := range host.Binds {
		if t, ok := bindTarget(b); ok {
			asked[t] = true
		}
	}
	for _, m := range host.Mounts {
		asked[m.Target] = true
	}
	cfg.Volumes = maps.Clone(cfg.Volumes)
	for _, mp := range old.Mounts {
		if mp.Type != mount.TypeVolume || mp.Name == "" || asked[mp.Destination] {
			continue
		}
		if _, declared := cfg.Volumes[mp.Destination]; !declared {
			continue
		}
		host.Mounts = append(slices.Clone(host.Mounts), mount.Mount{
			Type: mount.TypeVolume, Source: mp.Name, Target: mp.Destination, ReadOnly: !mp.RW,
		})
		delete(cfg.Volumes, mp.Destination)
		asked[mp.Destination] = true
	}
}

// bindTarget is the container path of a "source:target[:options]" bind, and false for a bind
// that does not have one.
func bindTarget(b string) (string, bool) {
	parts := strings.Split(b, ":")
	if len(parts) < 2 {
		return "", false
	}
	return parts[1], true
}

// primaryNetwork picks the network the create carries: the one the network mode names ("default"
// is the bridge), else the first of names, which is sorted.
func primaryNetwork(mode string, names []string) string {
	if mode == "default" {
		mode = "bridge"
	}
	if slices.Contains(names, mode) {
		return mode
	}
	return names[0]
}

// keptEndpoint is the part of an attachment somebody chose, as a fresh value: the aliases but
// the old container's ID, the static addresses, the MAC address somebody set, the links, the
// driver options and the gateway priority. The addresses and IDs the engine assigned are left
// out, and so is a MAC address the engine derived from the old address (engineMAC): kept, it
// would no longer match the replacement's own address and could be handed out a second time to
// the next container that takes the old one's.
func keptEndpoint(in *network.EndpointSettings, id, shortID string) *network.EndpointSettings {
	out := &network.EndpointSettings{}
	if in == nil {
		return out
	}
	out.IPAMConfig = in.IPAMConfig.Copy()
	out.Links = slices.Clone(in.Links)
	out.DriverOpts = maps.Clone(in.DriverOpts)
	out.GwPriority = in.GwPriority
	if !engineMAC(in) {
		out.MacAddress = slices.Clone(in.MacAddress)
	}
	for _, a := range in.Aliases {
		if a == id || (shortID != "" && a == shortID) {
			continue
		}
		out.Aliases = append(out.Aliases, a)
	}
	return out
}

// engineMAC reports whether the endpoint's MAC address is one the engine derived from an IPv4
// address: 02:42 followed by the four bytes of the address. The address is the one the endpoint
// has now, or the static one it asks for when the container is stopped. A MAC address somebody
// set is never read as derived unless it is exactly that, and one whose address is not known is
// kept.
func engineMAC(es *network.EndpointSettings) bool {
	mac := []byte(es.MacAddress)
	if len(mac) != 6 || mac[0] != 0x02 || mac[1] != 0x42 {
		return false
	}
	ip := es.IPAddress
	if !ip.IsValid() && es.IPAMConfig != nil {
		ip = es.IPAMConfig.IPv4Address
	}
	if !ip.Is4() {
		return false
	}
	b := ip.As4()
	return bytes.Equal(mac[2:], b[:])
}

// ForgetImageDefaults takes out of cfg what the old image contributed and the person did not
// choose, so the replacement takes it from the new image instead of keeping the old image's
// value. image is the config of the image the old container ran, which the inspect of that image
// gives. A value counts as the image's when it is exactly what the image has: the same
// environment entries, the same labels, the same entrypoint and command, health check, working
// directory, user and stop signal. cfg is a copy of the old container's config (RecreateSpec);
// only fields are replaced here and the old container's own maps and slices are never written.
//
// What the inspect cannot tell is a value the person set to the very value the image had: it is
// read as the image's and follows the new image. A command the person gave stays, and so does an
// entrypoint that is not the image's. The image's exposed ports are not taken out, since a
// published port is also exposed and the two cannot be told apart from the config alone.
func ForgetImageDefaults(cfg, image *container.Config) {
	if cfg == nil || image == nil {
		return
	}
	// The engine takes the image's command only when the container sets no entrypoint, so a
	// command is the image's only while the entrypoint is.
	if slices.Equal(cfg.Entrypoint, image.Entrypoint) {
		cfg.Entrypoint = nil
		if slices.Equal(cfg.Cmd, image.Cmd) {
			cfg.Cmd = nil
		}
	}
	if len(cfg.Env) > 0 && len(image.Env) > 0 {
		baked := make(map[string]bool, len(image.Env))
		for _, e := range image.Env {
			baked[e] = true
		}
		kept := make([]string, 0, len(cfg.Env))
		for _, e := range cfg.Env {
			if !baked[e] {
				kept = append(kept, e)
			}
		}
		cfg.Env = kept
	}
	if len(cfg.Labels) > 0 && len(image.Labels) > 0 {
		kept := make(map[string]string, len(cfg.Labels))
		for k, v := range cfg.Labels {
			if iv, ok := image.Labels[k]; ok && iv == v {
				continue
			}
			kept[k] = v
		}
		cfg.Labels = kept
	}
	if cfg.Healthcheck != nil && reflect.DeepEqual(cfg.Healthcheck, image.Healthcheck) {
		cfg.Healthcheck = nil
	}
	if cfg.WorkingDir == image.WorkingDir {
		cfg.WorkingDir = ""
	}
	if cfg.User == image.User {
		cfg.User = ""
	}
	if cfg.StopSignal == image.StopSignal {
		cfg.StopSignal = ""
	}
}
