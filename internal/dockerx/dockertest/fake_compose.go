// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

// ComposeOptions say how a replica of a compose service is made on the engine: the project, the number of
// the replica, the container's ID, the hash the SDK stamps on it, the names the engine holds the project's
// networks and volumes under (by compose key) and the image's own configuration, which the engine merges
// into the container's.
type ComposeOptions struct {
	Project string
	// Number is the replica's number, 1 when zero.
	Number int
	ID     string
	// ConfigHash is the com.docker.compose.config-hash label.
	ConfigHash string
	// Networks and Volumes are the engine names of the project's networks and volumes by compose key.
	Networks, Volumes map[string]string
	Image             FakeImage
}

// ComposeInspect is the inspect document the engine gives a container the Compose SDK created from a
// service: what getCreateConfigs sends, with what the engine adds (the image's environment, command, labels
// and healthcheck merged in, the container's short ID as its hostname and as a network alias, the default
// restart policy and log driver). It stands for a deployed container in tests that need a review right
// after a deploy to be empty, without a real engine. The service must be prepared the way a deploy prepares
// it, and a reference to another service already resolved to a container ID (stacks.ResolveServiceRefs).
// It covers the settings the tests use; the real-engine tests are the proof of the rest.
func ComposeInspect(svc types.ServiceConfig, o ComposeOptions) container.InspectResponse {
	number := max(o.Number, 1)
	name := o.Project + "-" + svc.Name + "-" + strconv.Itoa(number)
	if svc.ContainerName != "" {
		name = svc.ContainerName
	}
	short := o.ID
	if len(short) > 12 {
		short = short[:12]
	}

	labels := map[string]string{}
	maps.Copy(labels, o.Image.Labels)
	maps.Copy(labels, svc.Labels)
	maps.Copy(labels, svc.CustomLabels)
	labels["com.docker.compose.project"] = o.Project
	labels["com.docker.compose.service"] = svc.Name
	labels["com.docker.compose.container-number"] = strconv.Itoa(number)
	labels["com.docker.compose.oneoff"] = "False"
	if o.ConfigHash != "" {
		labels["com.docker.compose.config-hash"] = o.ConfigHash
	}

	env := slices.Clone(o.Image.Env)
	for k, v := range svc.Environment {
		entry := k
		if v != nil {
			entry = k + "=" + *v
		}
		replaced := false
		for i, e := range env {
			if n, _, _ := strings.Cut(e, "="); n == k {
				env[i], replaced = entry, true
			}
		}
		if !replaced {
			env = append(env, entry)
		}
	}

	cfg := container.Config{
		Hostname: short, Domainname: svc.DomainName, User: svc.User, WorkingDir: svc.WorkingDir, Tty: svc.Tty,
		OpenStdin: svc.StdinOpen, Image: svc.Image, Env: env, Labels: labels, StopSignal: svc.StopSignal,
		Cmd: slices.Clone(svc.Command), Entrypoint: slices.Clone(svc.Entrypoint),
	}
	if svc.Hostname != "" {
		cfg.Hostname = svc.Hostname
	}
	if len(cfg.Entrypoint) == 0 {
		if len(cfg.Cmd) == 0 {
			cfg.Cmd = slices.Clone(o.Image.Cmd)
		}
		if cfg.Entrypoint == nil {
			cfg.Entrypoint = slices.Clone(o.Image.Entrypoint)
		}
	}
	if svc.StopGracePeriod != nil {
		secs := int(time.Duration(*svc.StopGracePeriod).Seconds())
		cfg.StopTimeout = &secs
	}
	if h := svc.HealthCheck; h != nil {
		test := slices.Clone(h.Test)
		if h.Disable {
			test = []string{"NONE"}
		}
		hc := &container.HealthConfig{Test: test}
		if h.Interval != nil {
			hc.Interval = time.Duration(*h.Interval)
		}
		if h.Timeout != nil {
			hc.Timeout = time.Duration(*h.Timeout)
		}
		if h.StartPeriod != nil {
			hc.StartPeriod = time.Duration(*h.StartPeriod)
		}
		if h.StartInterval != nil {
			hc.StartInterval = time.Duration(*h.StartInterval)
		}
		if h.Retries != nil {
			hc.Retries = int(min(*h.Retries, math.MaxInt32))
		}
		cfg.Healthcheck = hc
	}

	host := container.HostConfig{
		RestartPolicy: composeRestart(svc),
		Privileged:    svc.Privileged, ReadonlyRootfs: svc.ReadOnly,
		CapAdd: slices.Clone(svc.CapAdd), CapDrop: slices.Clone(svc.CapDrop),
		VolumesFrom: slices.Clone(svc.VolumesFrom), IpcMode: container.IpcMode(svc.Ipc), PidMode: container.PidMode(svc.Pid),
		LogConfig: container.LogConfig{Type: "json-file"}, ShmSize: 64 << 20,
		GroupAdd: slices.Clone(svc.GroupAdd), DNSSearch: slices.Clone(svc.DNSSearch), DNSOptions: slices.Clone(svc.DNSOpts),
		SecurityOpt: slices.Clone(svc.SecurityOpt), Sysctls: maps.Clone(svc.Sysctls),
	}
	if host.IpcMode == "" {
		host.IpcMode = container.IPCModePrivate
	}
	if svc.Logging != nil {
		host.LogConfig = container.LogConfig{Type: svc.Logging.Driver, Config: maps.Clone(svc.Logging.Options)}
	}
	for _, a := range svc.DNS {
		if ip, err := netip.ParseAddr(a); err == nil {
			host.DNS = append(host.DNS, ip)
		}
	}
	host.ExtraHosts = svc.ExtraHosts.AsList(":")
	if len(svc.Tmpfs) > 0 {
		host.Tmpfs = map[string]string{}
		for _, t := range svc.Tmpfs {
			k, v, _ := strings.Cut(t, ":")
			host.Tmpfs[k] = v
		}
	}
	host.Memory, host.MemoryReservation = int64(svc.MemLimit), int64(svc.MemReservation)
	host.NanoCPUs = int64(svc.CPUS * 1e9)
	if d := svc.Deploy; d != nil && d.Resources.Limits != nil {
		if d.Resources.Limits.MemoryBytes != 0 {
			host.Memory = int64(d.Resources.Limits.MemoryBytes)
		}
		if d.Resources.Limits.NanoCPUs != 0 {
			host.NanoCPUs = int64(d.Resources.Limits.NanoCPUs * 1e9)
		}
	}
	for _, d := range svc.Devices {
		host.Devices = append(host.Devices, container.DeviceMapping{PathOnHost: d.Source, PathInContainer: d.Target, CgroupPermissions: d.Permissions})
	}
	host.Init = svc.Init

	// Ports and exposed ports.
	host.PortBindings = network.PortMap{}
	cfg.ExposedPorts = network.PortSet{}
	for _, p := range svc.Ports {
		port, err := network.ParsePort(fmt.Sprintf("%d/%s", p.Target, cmp.Or(p.Protocol, "tcp")))
		if err != nil {
			continue
		}
		b := network.PortBinding{HostPort: p.Published}
		if p.HostIP != "" {
			b.HostIP = netip.MustParseAddr(p.HostIP)
		}
		host.PortBindings[port] = append(host.PortBindings[port], b)
		cfg.ExposedPorts[port] = struct{}{}
	}
	for _, e := range svc.Expose {
		spec, proto, _ := strings.Cut(e, "/")
		lo, hi, ranged := strings.Cut(spec, "-")
		first, _ := strconv.Atoi(lo)
		last := first
		if ranged {
			last, _ = strconv.Atoi(hi)
		}
		for n := first; n <= last; n++ {
			if port, err := network.ParsePort(fmt.Sprintf("%d/%s", n, cmp.Or(proto, "tcp"))); err == nil {
				cfg.ExposedPorts[port] = struct{}{}
			}
		}
	}
	for _, e := range o.Image.ExposedPorts {
		if port, err := network.ParsePort(e); err == nil {
			cfg.ExposedPorts[port] = struct{}{}
		}
	}
	if len(host.PortBindings) == 0 {
		host.PortBindings = nil
	}

	// Volumes: simple ones are binds, in the legacy string.
	for _, v := range svc.Volumes {
		src := v.Source
		if v.Type == types.VolumeTypeVolume && src != "" {
			if n, ok := o.Volumes[src]; ok {
				src = n
			}
		}
		if src == "" || v.Type == types.VolumeTypeTmpfs {
			host.Mounts = append(host.Mounts, mountOf(v))
			continue
		}
		access := "rw"
		if v.ReadOnly {
			access = "ro"
		}
		host.Binds = append(host.Binds, src+":"+v.Target+":"+access)
	}

	// Networks: the primary one is the network mode, the container's name and the service's name and the
	// short ID are aliases on every user-defined network.
	settings := &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{}}
	if svc.NetworkMode != "" {
		host.NetworkMode = container.NetworkMode(svc.NetworkMode)
	} else {
		keys := svc.NetworksByPriority()
		for i, key := range keys {
			engine := o.Networks[key]
			if engine == "" {
				engine = o.Project + "_" + key
			}
			if i == 0 {
				host.NetworkMode = container.NetworkMode(engine)
			}
			ep := &network.EndpointSettings{Aliases: []string{name, svc.Name, short}}
			if nc := svc.Networks[key]; nc != nil {
				ep.Aliases = append(ep.Aliases, nc.Aliases...)
				ep.GwPriority = nc.GatewayPriority
				if nc.Ipv4Address != "" {
					ep.IPAMConfig = &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr(nc.Ipv4Address)}
				}
			}
			settings.Networks[engine] = ep
		}
	}

	return container.InspectResponse{
		ID: o.ID, Name: "/" + name, Config: &cfg, HostConfig: &host, NetworkSettings: settings,
		State: &container.State{Status: "running", Running: true},
	}
}

func mountOf(v types.ServiceVolumeConfig) mount.Mount {
	return mount.Mount{Type: mount.Type(v.Type), Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly}
}

// composeRestart is the restart policy the SDK sends (getRestartPolicy).
func composeRestart(svc types.ServiceConfig) container.RestartPolicy {
	cond := svc.Restart
	attempts := 0
	if name, num, ok := strings.Cut(cond, ":"); ok {
		cond = name
		attempts, _ = strconv.Atoi(num)
	}
	if d := svc.Deploy; d != nil && d.RestartPolicy != nil {
		cond = d.RestartPolicy.Condition
		if d.RestartPolicy.MaxAttempts != nil {
			attempts = int(min(*d.RestartPolicy.MaxAttempts, math.MaxInt32))
		}
	}
	switch cond {
	case "":
		return container.RestartPolicy{}
	case "none", "no":
		return container.RestartPolicy{Name: container.RestartPolicyDisabled}
	case "any", "always":
		return container.RestartPolicy{Name: container.RestartPolicyAlways}
	}
	return container.RestartPolicy{Name: container.RestartPolicyMode(cond), MaximumRetryCount: attempts}
}
