// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
)

// ImageDefaults are the values an image supplies to a container that the compose file did not set: the
// engine merges them into the container's configuration at create time, so they appear in its inspect
// document as though the file had written them. NormalizeComposeService removes them from both sides of a
// comparison.
type ImageDefaults struct {
	Env, Cmd, Entrypoint     []string
	User, WorkingDir, Signal string
	Labels                   map[string]string
	ExposedPorts             []string
	Healthcheck              *types.HealthCheckConfig
}

// ImageDefaultsOf reads the defaults from an image inspect document (the Config of the image).
func ImageDefaultsOf(in image.InspectResponse) ImageDefaults {
	cfg := in.Config
	if cfg == nil {
		return ImageDefaults{}
	}
	d := ImageDefaults{
		Env: slices.Clone(cfg.Env), Cmd: slices.Clone(cfg.Cmd), Entrypoint: slices.Clone(cfg.Entrypoint),
		User: cfg.User, WorkingDir: cfg.WorkingDir, Signal: cfg.StopSignal, Labels: maps.Clone(cfg.Labels),
	}
	for p := range cfg.ExposedPorts {
		d.ExposedPorts = append(d.ExposedPorts, p)
	}
	sort.Strings(d.ExposedPorts)
	if h := cfg.Healthcheck; h != nil {
		d.Healthcheck = healthOf(h.Test, h.Interval, h.Timeout, h.StartPeriod, h.StartInterval, h.Retries)
	}
	return d
}

// RunningContext is what a comparison needs to know about the project the containers belong to: its
// name, the containers of its services by ID, and the compose key of each network and volume by the name
// the engine holds it under.
type RunningContext struct {
	Project string
	// Peers is the service name of each container of the project, by container ID.
	Peers map[string]string
	// Networks and Volumes give the compose key of an object by the name the engine holds it under.
	Networks, Volumes map[string]string
}

// RunningContextOf builds the context of a project: the engine name of each network and volume it declares
// (prepared, so the names the SDK used) and the containers of its services.
func RunningContextOf(p *types.Project, peers map[string]string) RunningContext {
	rc := RunningContext{Project: p.Name, Peers: peers, Networks: map[string]string{}, Volumes: map[string]string{}}
	for key, n := range p.Networks {
		name := n.Name
		if name == "" {
			name = p.Name + "_" + key
		}
		rc.Networks[name] = key
	}
	for key, v := range p.Volumes {
		name := v.Name
		if name == "" {
			name = p.Name + "_" + key
		}
		rc.Volumes[name] = key
	}
	return rc
}

// serviceOfPeer is the service a container ID belongs to, accepting an ID prefix of at least 12
// characters.
func (rc RunningContext) serviceOfPeer(id string) (string, bool) {
	if s, ok := rc.Peers[id]; ok {
		return s, true
	}
	if len(id) < 12 {
		return "", false
	}
	for full, s := range rc.Peers {
		if strings.HasPrefix(full, id) {
			return s, true
		}
	}
	return "", false
}

// The names of the lists the engine does not record and the hash does not cover, which a stored revision
// supplies (RunningService's recorded argument).
func copyRecorded(dst *types.ServiceConfig, from types.ServiceConfig) {
	dst.DependsOn = from.DependsOn
	dst.Secrets = from.Secrets
	dst.Configs = from.Configs
	dst.Profiles = from.Profiles
	dst.PullPolicy = from.PullPolicy
	dst.Scale = from.Scale
	if from.Deploy != nil && from.Deploy.Replicas != nil {
		dst.Deploy = &types.DeployConfig{Replicas: from.Deploy.Replicas}
	}
}

// RunningService rebuilds a compose service from the inspect document of one of its containers: what the
// engine records of the service's settings, as the SDK wrote them at create time. A setting the engine does
// not record (depends_on, secrets and configs, profiles, pull_policy, scale) is copied from recorded when it
// is not nil, and left empty otherwise. Image-supplied values and the SDK's own labels are still in the
// result: NormalizeComposeService removes them, from this side and from the desired one. img is not used
// here; it is the image the container runs, for the caller to pass on to NormalizeComposeService.
func RunningService(c container.InspectResponse, _ ImageDefaults, recorded *types.ServiceConfig, rc RunningContext) types.ServiceConfig {
	cfg, hc := c.Config, c.HostConfig
	if cfg == nil {
		cfg = &container.Config{}
	}
	if hc == nil {
		hc = &container.HostConfig{}
	}
	svc := types.ServiceConfig{Name: cfg.Labels[LabelComposeService]}
	svc.Image = cfg.Image
	svc.Command = slices.Clone(cfg.Cmd)
	svc.Entrypoint = slices.Clone(cfg.Entrypoint)
	svc.Environment = envOf(cfg.Env)
	if len(cfg.Labels) > 0 {
		svc.Labels = maps.Clone(cfg.Labels)
	}
	svc.User, svc.WorkingDir, svc.StopSignal = cfg.User, cfg.WorkingDir, cfg.StopSignal
	svc.Hostname, svc.DomainName = cfg.Hostname, cfg.Domainname
	svc.Tty, svc.StdinOpen = cfg.Tty, cfg.OpenStdin
	if cfg.StopTimeout != nil {
		d := types.Duration(time.Duration(*cfg.StopTimeout) * time.Second)
		svc.StopGracePeriod = &d
	}
	if h := cfg.Healthcheck; h != nil {
		svc.HealthCheck = healthOf(h.Test, h.Interval, h.Timeout, h.StartPeriod, h.StartInterval, h.Retries)
	}

	// Ports: the bindings that were asked for; a port that is only exposed is in Expose.
	published := map[string]bool{}
	for port, bindings := range hc.PortBindings {
		for _, b := range bindings {
			p := types.ServicePortConfig{Target: uint32(port.Num()), Protocol: string(port.Proto()), Published: b.HostPort, Mode: "ingress"}
			if b.HostIP.IsValid() {
				p.HostIP = b.HostIP.String()
			}
			svc.Ports = append(svc.Ports, p)
		}
		published[fmt.Sprintf("%d/%s", port.Num(), port.Proto())] = true
	}
	for port := range cfg.ExposedPorts {
		key := fmt.Sprintf("%d/%s", port.Num(), port.Proto())
		if !published[key] {
			svc.Expose = append(svc.Expose, key)
		}
	}

	// Volumes: what was asked for (binds and mounts), not the anonymous volumes an image declares.
	for _, b := range hc.Binds {
		svc.Volumes = append(svc.Volumes, volumeOfBind(b, rc))
	}
	for _, m := range hc.Mounts {
		src := m.Source
		if string(m.Type) == types.VolumeTypeVolume {
			if key, ok := rc.Volumes[src]; ok {
				src = key
			}
		}
		svc.Volumes = append(svc.Volumes, types.ServiceVolumeConfig{Type: string(m.Type), Source: src, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	for path, opts := range hc.Tmpfs {
		if opts != "" {
			path += ":" + opts
		}
		svc.Tmpfs = append(svc.Tmpfs, path)
	}
	svc.VolumesFrom = slices.Clone(hc.VolumesFrom)

	// Networks: the SDK names the primary network as the network mode; an explicit mode (host, none,
	// bridge, container:...) is not one of the project's.
	mode := string(hc.NetworkMode)
	if _, ok := rc.Networks[mode]; ok || mode == "" {
		if ns := c.NetworkSettings; ns != nil {
			svc.Networks = map[string]*types.ServiceNetworkConfig{}
			for name, ep := range ns.Networks {
				key := name
				if k, ok := rc.Networks[name]; ok {
					key = k
				}
				svc.Networks[key] = networkOf(ep, c.ID, strings.TrimPrefix(c.Name, "/"))
			}
		}
	} else {
		svc.NetworkMode = mode
	}
	if name := strings.TrimPrefix(c.Name, "/"); name != "" && !defaultContainerName(name, rc.Project, svc.Name) {
		svc.ContainerName = name
	}

	svc.Restart = restartOf(hc.RestartPolicy)
	svc.Ipc, svc.Pid, svc.Uts = string(hc.IpcMode), string(hc.PidMode), string(hc.UTSMode)
	svc.UserNSMode = string(hc.UsernsMode)
	svc.Privileged, svc.ReadOnly = hc.Privileged, hc.ReadonlyRootfs
	svc.CapAdd, svc.CapDrop = slices.Clone(hc.CapAdd), slices.Clone(hc.CapDrop)
	svc.GroupAdd = slices.Clone(hc.GroupAdd)
	svc.SecurityOpt = slices.Clone(hc.SecurityOpt)
	svc.DNSSearch, svc.DNSOpts = slices.Clone(hc.DNSSearch), slices.Clone(hc.DNSOptions)
	for _, a := range hc.DNS {
		svc.DNS = append(svc.DNS, a.String())
	}
	if len(hc.ExtraHosts) > 0 {
		svc.ExtraHosts = types.HostsList{}
		for _, h := range hc.ExtraHosts {
			host, ip, _ := strings.Cut(h, ":")
			svc.ExtraHosts[host] = append(svc.ExtraHosts[host], ip)
		}
	}
	if len(hc.Sysctls) > 0 {
		svc.Sysctls = types.Mapping(maps.Clone(hc.Sysctls))
	}
	svc.Init = hc.Init
	svc.ShmSize = types.UnitBytes(hc.ShmSize)
	svc.MemLimit, svc.MemReservation, svc.MemSwapLimit = types.UnitBytes(hc.Memory), types.UnitBytes(hc.MemoryReservation), types.UnitBytes(hc.MemorySwap)
	svc.CPUS = float32(hc.NanoCPUs) / 1e9
	svc.CPUShares, svc.CPUSet = hc.CPUShares, hc.CpusetCpus
	if hc.PidsLimit != nil {
		svc.PidsLimit = *hc.PidsLimit
	}
	for _, d := range hc.Devices {
		svc.Devices = append(svc.Devices, types.DeviceMapping{Source: d.PathOnHost, Target: d.PathInContainer, Permissions: d.CgroupPermissions})
	}
	if hc.LogConfig.Type != "" || len(hc.LogConfig.Config) > 0 {
		svc.Logging = &types.LoggingConfig{Driver: hc.LogConfig.Type, Options: types.Options(maps.Clone(hc.LogConfig.Config))}
	}
	if len(hc.Ulimits) > 0 {
		svc.Ulimits = map[string]*types.UlimitsConfig{}
		for _, u := range hc.Ulimits {
			svc.Ulimits[u.Name] = &types.UlimitsConfig{Soft: int(u.Soft), Hard: int(u.Hard)}
		}
	}
	if recorded != nil {
		copyRecorded(&svc, *recorded)
	}
	return svc
}

// volumeOfBind reads a legacy bind string, "source:target[:options]".
func volumeOfBind(b string, rc RunningContext) types.ServiceVolumeConfig {
	parts := strings.Split(b, ":")
	v := types.ServiceVolumeConfig{}
	switch len(parts) {
	case 1:
		v.Target = parts[0]
	default:
		v.Source, v.Target = parts[0], parts[1]
	}
	if len(parts) > 2 {
		for _, o := range strings.Split(parts[2], ",") {
			if o == "ro" {
				v.ReadOnly = true
			}
		}
	}
	switch {
	case v.Source == "":
		v.Type = types.VolumeTypeVolume
	case strings.HasPrefix(v.Source, "/"):
		v.Type = types.VolumeTypeBind
	default:
		v.Type = types.VolumeTypeVolume
		if key, ok := rc.Volumes[v.Source]; ok {
			v.Source = key
		}
	}
	return v
}

// networkOf reads one endpoint of a container's network settings.
func networkOf(ep *network.EndpointSettings, id, name string) *types.ServiceNetworkConfig {
	if ep == nil {
		return nil
	}
	cfg := &types.ServiceNetworkConfig{GatewayPriority: ep.GwPriority, DriverOpts: types.Options(maps.Clone(ep.DriverOpts))}
	short := id
	if len(short) > 12 {
		short = short[:12]
	}
	for _, a := range ep.Aliases {
		if a == short || a == name {
			continue
		}
		cfg.Aliases = append(cfg.Aliases, a)
	}
	if ip := ep.IPAMConfig; ip != nil {
		if ip.IPv4Address.IsValid() {
			cfg.Ipv4Address = ip.IPv4Address.String()
		}
		if ip.IPv6Address.IsValid() {
			cfg.Ipv6Address = ip.IPv6Address.String()
		}
		for _, l := range ip.LinkLocalIPs {
			cfg.LinkLocalIPs = append(cfg.LinkLocalIPs, l.String())
		}
	}
	const ifname = "com.docker.network.endpoint.ifname"
	if v, ok := cfg.DriverOpts[ifname]; ok {
		cfg.InterfaceName = v
		delete(cfg.DriverOpts, ifname)
	}
	return cfg
}

// defaultContainerName reports whether name is the name the SDK gives a replica of a service.
func defaultContainerName(name, project, service string) bool {
	for _, sep := range []string{"-", "_"} {
		rest, ok := strings.CutPrefix(name, project+sep+service+sep)
		if ok && rest != "" && allDigits(rest) {
			return true
		}
	}
	return false
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func envOf(env []string) types.MappingWithEquals {
	if len(env) == 0 {
		return nil
	}
	out := types.MappingWithEquals{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			out[k] = &v
		} else {
			out[k] = nil
		}
	}
	return out
}

func healthOf(test []string, interval, timeout, start, startInterval time.Duration, retries int) *types.HealthCheckConfig {
	h := &types.HealthCheckConfig{Test: slices.Clone(test)}
	if len(test) == 1 && test[0] == "NONE" {
		h.Test, h.Disable = nil, true
	}
	dur := func(d time.Duration) *types.Duration {
		if d == 0 {
			return nil
		}
		v := types.Duration(d)
		return &v
	}
	h.Interval, h.Timeout, h.StartPeriod, h.StartInterval = dur(interval), dur(timeout), dur(start), dur(startInterval)
	if retries > 0 {
		r := uint64(retries)
		h.Retries = &r
	}
	return h
}

// restartOf writes a restart policy as compose does: "" for none, always, unless-stopped, on-failure or
// on-failure:N.
func restartOf(p container.RestartPolicy) string {
	switch p.Name {
	case "", container.RestartPolicyDisabled:
		return ""
	case container.RestartPolicyOnFailure:
		if p.MaximumRetryCount > 0 {
			return "on-failure:" + strconv.Itoa(p.MaximumRetryCount)
		}
		return "on-failure"
	}
	return string(p.Name)
}

// desiredRestart is the restart policy the SDK sends for a service, in the same words (getRestartPolicy).
func desiredRestart(svc types.ServiceConfig) string {
	pol := container.RestartPolicy{}
	if svc.Restart != "" {
		name, num, ok := strings.Cut(svc.Restart, ":")
		n := 0
		if ok {
			n, _ = strconv.Atoi(num)
		}
		pol = container.RestartPolicy{Name: restartMode(name), MaximumRetryCount: n}
	}
	if svc.Deploy != nil && svc.Deploy.RestartPolicy != nil {
		rp := *svc.Deploy.RestartPolicy
		n := 0
		if rp.MaxAttempts != nil {
			n = int(min(*rp.MaxAttempts, math.MaxInt32))
		}
		pol = container.RestartPolicy{Name: restartMode(rp.Condition), MaximumRetryCount: n}
	}
	return restartOf(pol)
}

func restartMode(c string) container.RestartPolicyMode {
	switch c {
	case "none", "no":
		return container.RestartPolicyDisabled
	case "on-failure":
		return container.RestartPolicyOnFailure
	case "unless-stopped":
		return container.RestartPolicyUnlessStopped
	case "any", "always":
		return container.RestartPolicyAlways
	}
	return container.RestartPolicyMode(c)
}

var shortContainerName = regexp.MustCompile(`^[0-9a-f]{12}$`)

// NormalizeComposeService returns the settings of a service that a deploy sets and the engine keeps, in one
// stable form, so that the desired service and the one rebuilt from a running container compare equal when
// they mean the same thing. It is applied to both sides, as NormalizeSpec is for a swarm service
// (Pitfall 5): it removes what the image or the engine fills in rather than the compose file (the image's
// own environment, command, entrypoint, user, working directory, labels, exposed ports and healthcheck, the
// SDK's com.docker.compose.* labels, Bayplan's own bayplan.* labels, the aliases the SDK adds, the engine's
// default restart policy and hostname), orders every list stably, writes a deploy: resources limit and
// restart policy as the SDK sends them, and reads a container:<ID> of a container of the project back as
// the service it is. Anything it does not name is left out of the result, so a setting the engine cannot be
// asked about never makes a service look changed. The image is not compared: the review's image table shows it.
func NormalizeComposeService(svc types.ServiceConfig, img ImageDefaults, rc RunningContext) types.ServiceConfig {
	out := types.ServiceConfig{Name: svc.Name}

	// What the engine does not record is passed through for a stored revision to supply.
	out.DependsOn = svc.DependsOn
	out.Secrets = slices.Clone(svc.Secrets)
	slices.SortStableFunc(out.Secrets, func(a, b types.ServiceSecretConfig) int { return cmp.Compare(a.Source, b.Source) })
	out.Configs = slices.Clone(svc.Configs)
	slices.SortStableFunc(out.Configs, func(a, b types.ServiceConfigObjConfig) int { return cmp.Compare(a.Source, b.Source) })
	out.Profiles = slices.Clone(svc.Profiles)
	slices.Sort(out.Profiles)
	out.PullPolicy = svc.PullPolicy
	if n := svc.GetScale(); n != 1 {
		out.Scale = &n
	}

	// Command and entrypoint: the image's are what a container gets when the file sets none.
	out.Entrypoint = nilIfSame(svc.Entrypoint, img.Entrypoint)
	out.Command = nilIfSame(svc.Command, img.Cmd)

	// Environment: entries the image sets to the same value are not the file's; an entry with no value
	// cannot be compared (the engine resolves it at run time).
	imgEnv := map[string]bool{}
	for _, e := range img.Env {
		imgEnv[e] = true
	}
	for k, v := range svc.Environment {
		if v == nil || imgEnv[k+"="+*v] {
			continue
		}
		if out.Environment == nil {
			out.Environment = types.MappingWithEquals{}
		}
		out.Environment[k] = v
	}

	// Labels: the SDK's and Bayplan's own labels, and the image's, are not the file's.
	for k, v := range svc.Labels {
		if strings.HasPrefix(k, "com.docker.compose.") || strings.HasPrefix(k, "bayplan.") {
			continue
		}
		if iv, ok := img.Labels[k]; ok && iv == v {
			continue
		}
		if out.Labels == nil {
			out.Labels = types.Labels{}
		}
		out.Labels[k] = v
	}

	out.User = sameOrEmpty(svc.User, img.User)
	out.WorkingDir = sameOrEmpty(svc.WorkingDir, img.WorkingDir)
	out.StopSignal = sameOrEmpty(svc.StopSignal, img.Signal)
	if !shortContainerName.MatchString(svc.Hostname) {
		out.Hostname = svc.Hostname
	}
	out.DomainName = svc.DomainName
	out.Tty, out.StdinOpen = svc.Tty, svc.StdinOpen
	out.ContainerName = svc.ContainerName
	if svc.StopGracePeriod != nil {
		d := types.Duration(time.Duration(*svc.StopGracePeriod).Truncate(time.Second))
		out.StopGracePeriod = &d
	}
	out.HealthCheck = normHealth(svc.HealthCheck, img.Healthcheck)

	out.Ports, out.Expose = normPorts(svc.Ports, svc.Expose, img.ExposedPorts)
	out.Volumes = normVolumes(svc.Volumes)
	out.Tmpfs = sortedStrings(svc.Tmpfs)
	out.VolumesFrom = normVolumesFrom(svc.VolumesFrom, rc)

	out.NetworkMode = normMode(svc.NetworkMode, rc)
	out.Ipc, out.Pid = normMode(svc.Ipc, rc), normMode(svc.Pid, rc)
	out.Uts, out.UserNSMode = svc.Uts, svc.UserNSMode
	if out.NetworkMode == "" {
		out.Networks = normNetworks(svc.Networks, rc.Project, svc.Name)
	}

	out.Restart = desiredRestart(svc)
	out.Privileged, out.ReadOnly = svc.Privileged, svc.ReadOnly
	out.CapAdd, out.CapDrop = normCaps(svc.CapAdd), normCaps(svc.CapDrop)
	out.GroupAdd = sortedStrings(svc.GroupAdd)
	out.SecurityOpt = normSecurityOpt(svc.SecurityOpt)
	out.DNS = sortedStrings(svc.DNS)
	out.DNSSearch, out.DNSOpts = sortedStrings(svc.DNSSearch), sortedStrings(svc.DNSOpts)
	if len(svc.ExtraHosts) > 0 {
		out.ExtraHosts = types.HostsList{}
		for h, ips := range svc.ExtraHosts {
			out.ExtraHosts[h] = sortedStrings(ips)
		}
	}
	if len(svc.Sysctls) > 0 {
		out.Sysctls = types.Mapping(maps.Clone(svc.Sysctls))
	}
	out.Init = svc.Init
	out.ShmSize = svc.ShmSize
	out.Devices = normDevices(svc.Devices)
	out.Logging = normLogging(svc)
	out.Ulimits = normUlimits(svc.Ulimits)

	// Resources: deploy.resources.limits overrides the top-level setting, as the SDK applies it.
	out.MemLimit, out.MemReservation, out.MemSwapLimit = svc.MemLimit, svc.MemReservation, svc.MemSwapLimit
	nano := float64(svc.CPUS) * 1e9
	out.PidsLimit = svc.PidsLimit
	if d := svc.Deploy; d != nil {
		if l := d.Resources.Limits; l != nil {
			if l.MemoryBytes != 0 {
				out.MemLimit = l.MemoryBytes
			}
			if l.NanoCPUs != 0 {
				nano = float64(l.NanoCPUs) * 1e9
			}
			if l.Pids > 0 {
				out.PidsLimit = l.Pids
			}
		}
		if r := d.Resources.Reservations; r != nil && r.MemoryBytes != 0 {
			out.MemReservation = r.MemoryBytes
		}
	}
	if nano != 0 {
		// To the microsecond of CPU: the SDK sends whole nanoseconds, computed in single precision.
		out.CPUS = float32(math.Round(nano/1e3) / 1e6)
	}
	if out.PidsLimit < 0 {
		out.PidsLimit = 0
	}
	out.CPUShares, out.CPUSet = svc.CPUShares, svc.CPUSet
	return out
}

// AlignDaemonDefaults makes a running service agree with the desired one on the settings a daemon fills in
// from its own configuration when the container sets none (the default log driver, the default ulimits, the
// shared memory size, the IPC mode): where the file sets nothing, the running side is taken to set nothing
// either, so a daemon's configuration never makes a service look changed.
func AlignDaemonDefaults(desired, running types.ServiceConfig) types.ServiceConfig {
	if desired.Logging == nil {
		running.Logging = nil
	} else if running.Logging != nil && desired.Logging.Driver == "" {
		running.Logging = &types.LoggingConfig{Options: running.Logging.Options}
	}
	if len(desired.Ulimits) == 0 {
		running.Ulimits = nil
	} else {
		keep := map[string]*types.UlimitsConfig{}
		for n, u := range running.Ulimits {
			if _, ok := desired.Ulimits[n]; ok {
				keep[n] = u
			}
		}
		running.Ulimits = keep
	}
	if desired.ShmSize == 0 {
		running.ShmSize = 0
	}
	if desired.Ipc == "" {
		running.Ipc = ""
	}
	if desired.Init == nil {
		running.Init = nil
	}
	if desired.PidsLimit == 0 {
		running.PidsLimit = 0
	}
	return running
}

func nilIfSame(have, def []string) types.ShellCommand {
	if len(have) == 0 || slices.Equal(have, def) {
		return nil
	}
	return slices.Clone(have)
}

func sameOrEmpty(have, def string) string {
	if have == def {
		return ""
	}
	return have
}

func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// normHealth writes a healthcheck in one form: a disabled one as Disable, zero durations and zero retries
// as unset; the image's own check is no check of the file's.
func normHealth(h, def *types.HealthCheckConfig) *types.HealthCheckConfig {
	if h == nil {
		return nil
	}
	n := healthOf(h.Test, durOf(h.Interval), durOf(h.Timeout), durOf(h.StartPeriod), durOf(h.StartInterval), retriesOf(h.Retries))
	if h.Disable {
		n.Test, n.Disable = nil, true
	}
	if len(n.Test) == 0 && !n.Disable && n.Interval == nil && n.Timeout == nil && n.StartPeriod == nil && n.StartInterval == nil && n.Retries == nil {
		return nil
	}
	if def != nil {
		d := healthOf(def.Test, durOf(def.Interval), durOf(def.Timeout), durOf(def.StartPeriod), durOf(def.StartInterval), retriesOf(def.Retries))
		if def.Disable {
			d.Test, d.Disable = nil, true
		}
		if string(mustJSON(d)) == string(mustJSON(n)) {
			return nil
		}
	}
	return n
}

func durOf(d *types.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return time.Duration(*d)
}

func retriesOf(r *uint64) int {
	if r == nil {
		return 0
	}
	return int(min(*r, math.MaxInt32))
}

// normPorts writes the published ports in one order without the swarm-only settings, and the exposed ports
// as "N/proto" without the ones that are published or that the image exposes already.
func normPorts(ports []types.ServicePortConfig, expose types.StringOrNumberList, imgExposed []string) ([]types.ServicePortConfig, types.StringOrNumberList) {
	var outPorts []types.ServicePortConfig
	pub := map[string]bool{}
	for _, p := range ports {
		proto := cmp.Or(p.Protocol, "tcp")
		outPorts = append(outPorts, types.ServicePortConfig{Target: p.Target, Published: p.Published, Protocol: proto, HostIP: p.HostIP})
		pub[fmt.Sprintf("%d/%s", p.Target, proto)] = true
	}
	slices.SortStableFunc(outPorts, func(a, b types.ServicePortConfig) int {
		return cmp.Or(cmp.Compare(a.Target, b.Target), strings.Compare(a.Protocol, b.Protocol),
			strings.Compare(a.Published, b.Published), strings.Compare(a.HostIP, b.HostIP))
	})
	skip := map[string]bool{}
	for _, e := range imgExposed {
		skip[e] = true
	}
	var outExpose types.StringOrNumberList
	seen := map[string]bool{}
	for _, e := range expose {
		for _, key := range expandPorts(e) {
			if pub[key] || skip[key] || seen[key] {
				continue
			}
			seen[key] = true
			outExpose = append(outExpose, key)
		}
	}
	slices.Sort(outExpose)
	return outPorts, outExpose
}

// expandPorts expands "80", "80/udp", "8000-8002" and "8000-8002/tcp" to "N/proto" each.
func expandPorts(e string) []string {
	spec, proto, _ := strings.Cut(e, "/")
	if proto == "" {
		proto = "tcp"
	}
	lo, hi, ok := strings.Cut(spec, "-")
	a, err1 := strconv.Atoi(lo)
	if err1 != nil {
		return []string{e}
	}
	b := a
	if ok {
		var err2 error
		if b, err2 = strconv.Atoi(hi); err2 != nil || b < a {
			return []string{e}
		}
	}
	var out []string
	for p := a; p <= b; p++ {
		out = append(out, fmt.Sprintf("%d/%s", p, proto))
	}
	return out
}

// normVolumes keeps what a mount is made of (its type, source, target and access) in the order of the
// targets: the options the engine does not give back are not compared.
func normVolumes(in []types.ServiceVolumeConfig) []types.ServiceVolumeConfig {
	var out []types.ServiceVolumeConfig
	for _, v := range in {
		out = append(out, types.ServiceVolumeConfig{Type: v.Type, Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly})
	}
	slices.SortStableFunc(out, func(a, b types.ServiceVolumeConfig) int { return cmp.Compare(a.Target, b.Target) })
	return out
}

// normMode reads container:<ID> of a container of the project as service:<name>.
func normMode(mode string, rc RunningContext) string {
	if id, ok := strings.CutPrefix(mode, types.ContainerPrefix); ok {
		if s, ok := rc.serviceOfPeer(id); ok {
			return types.NetworkModeServicePrefix + s
		}
	}
	return mode
}

// normVolumesFrom reads the ID of a container of the project as the name of its service, and drops an
// access mode, which the SDK drops too.
func normVolumesFrom(in []string, rc RunningContext) []string {
	var out []string
	for _, v := range in {
		name := strings.TrimPrefix(v, types.ContainerPrefix)
		name, _, _ = strings.Cut(name, ":")
		if s, ok := rc.serviceOfPeer(name); ok {
			name = s
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// normNetworks writes the networks of a service without the aliases the SDK adds and without the settings
// the engine does not keep (the order of connection, the MAC address the engine generated).
func normNetworks(in map[string]*types.ServiceNetworkConfig, project, service string) map[string]*types.ServiceNetworkConfig {
	if len(in) == 0 {
		return nil
	}
	out := map[string]*types.ServiceNetworkConfig{}
	for key, cfg := range in {
		if cfg == nil {
			out[key] = nil
			continue
		}
		n := &types.ServiceNetworkConfig{
			Ipv4Address: cfg.Ipv4Address, Ipv6Address: cfg.Ipv6Address, GatewayPriority: cfg.GatewayPriority,
			InterfaceName: cfg.InterfaceName, LinkLocalIPs: sortedStrings(cfg.LinkLocalIPs),
		}
		if len(cfg.DriverOpts) > 0 {
			n.DriverOpts = types.Options(maps.Clone(cfg.DriverOpts))
		}
		for _, a := range cfg.Aliases {
			if a == service || defaultContainerName(a, project, service) {
				continue
			}
			n.Aliases = append(n.Aliases, a)
		}
		slices.Sort(n.Aliases)
		if string(mustJSON(n)) == "{}" {
			n = nil
		}
		out[key] = n
	}
	return out
}

// normCaps writes capability names in one form: upper case without the CAP_ prefix.
func normCaps(in []string) []string {
	var out []string
	for _, c := range in {
		out = append(out, strings.TrimPrefix(strings.ToUpper(c), "CAP_"))
	}
	slices.Sort(out)
	return out
}

// normSecurityOpt writes "key:value" as "key=value" and leaves out a seccomp profile (the SDK sends the
// profile's content, the file holds a path).
func normSecurityOpt(in []string) []string {
	var out []string
	for _, o := range in {
		if k, v, ok := strings.Cut(o, ":"); ok && !strings.Contains(k, "=") {
			o = k + "=" + v
		}
		if strings.HasPrefix(o, "seccomp=") && o != "seccomp=unconfined" {
			continue
		}
		out = append(out, o)
	}
	slices.Sort(out)
	return out
}

func normDevices(in []types.DeviceMapping) []types.DeviceMapping {
	var out []types.DeviceMapping
	for _, d := range in {
		if d.Target == "" {
			d.Target = d.Source
		}
		if d.Permissions == "" {
			d.Permissions = "rwm"
		}
		out = append(out, types.DeviceMapping{Source: d.Source, Target: d.Target, Permissions: d.Permissions})
	}
	slices.SortStableFunc(out, func(a, b types.DeviceMapping) int { return cmp.Compare(a.Source, b.Source) })
	return out
}

// normLogging writes the logging driver and options of a service (the older log_driver and log_opt are
// folded into it).
func normLogging(svc types.ServiceConfig) *types.LoggingConfig {
	l := svc.Logging
	if l == nil && svc.LogDriver == "" && len(svc.LogOpt) == 0 {
		return nil
	}
	n := &types.LoggingConfig{}
	if l != nil {
		n.Driver, n.Options = l.Driver, l.Options
	}
	if len(n.Options) == 0 {
		n.Options = nil
	}
	return n
}

// normUlimits writes every limit as a soft and a hard value, as the SDK sends them.
func normUlimits(in map[string]*types.UlimitsConfig) map[string]*types.UlimitsConfig {
	if len(in) == 0 {
		return nil
	}
	out := map[string]*types.UlimitsConfig{}
	for name, u := range in {
		if u == nil {
			continue
		}
		soft, hard := u.Single, u.Single
		if u.Soft != 0 {
			soft = u.Soft
		}
		if u.Hard != 0 {
			hard = u.Hard
		}
		out[name] = &types.UlimitsConfig{Soft: soft, Hard: hard}
	}
	return out
}
