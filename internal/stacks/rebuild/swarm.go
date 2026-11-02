// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rebuild

import (
	"cmp"
	"fmt"
	"maps"
	"math/big"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// SwarmService is one service of the stack as the engine holds it. Name is the swarm name (<namespace>_web) and
// Spec the service's spec with its network targets already turned from IDs into network names.
type SwarmService struct {
	ID, Name string
	Spec     swarm.ServiceSpec
}

// NetworkInfo is a network a service attaches, or any network of the swarm: what the file needs to write it.
type NetworkInfo struct {
	ID, Name, Driver string
	Attachable       bool
	Internal         bool
	IPv6             bool
	Labels, Options  map[string]string
	IPAMDriver       string
	Pools            []Pool
}

// Pool is one IPAM address pool of a network.
type Pool struct{ Subnet, Gateway, IPRange string }

// ObjectInfo is a secret or a config of the swarm: its ID and its name, never its content. Labels are its labels,
// which tell a secret or a config the stack made from one of its files (tierplan.stack.object) from one it only uses.
type ObjectInfo struct {
	ID, Name string
	Labels   map[string]string
}

// SwarmInput is what the swarm direction needs. Namespace is the stack's com.docker.stack.namespace; Networks the
// networks the services attach (a name no entry describes is written as an external network); Secrets and
// Configs the objects the services reference, by ID, for a reference that carries no name. Base, when set, is the
// definition the stack already has, which the rebuild is made on top of (see Base).
type SwarmInput struct {
	Namespace        string
	Services         []SwarmService
	Networks         []NetworkInfo
	Secrets, Configs []ObjectInfo
	Base             *Base
}

// Swarm rebuilds the compose project of a swarm stack from its services' specs, the reverse of convert.Convert:
// converting the result gives the specs back, apart from TierPlan's own labels, once the pins and the platforms are
// applied: the image of a service is its pin (Result.Pins) and its Placement.Platforms are Result.Platforms, both
// kept out of the file. Services come out in name order and so does everything else, so the same input always gives
// the same file.
func Swarm(in SwarmInput) (Result, error) {
	b := newBuilder(in)
	services := slices.Clone(in.Services)
	slices.SortFunc(services, func(x, y SwarmService) int { return cmp.Compare(x.Name, y.Name) })
	b.prepareFiles(services)

	project := &types.Project{Name: in.Namespace, Services: types.Services{}}
	for _, svc := range services {
		short, cfg, err := b.service(svc)
		if err != nil {
			return Result{}, err
		}
		project.Services[short] = cfg
	}
	if len(b.networks) > 0 {
		project.Networks = b.networks
	}
	if len(b.volumes) > 0 {
		project.Volumes = b.volumes
	}
	if len(b.secrets) > 0 {
		project.Secrets = b.secrets
	}
	if len(b.configs) > 0 {
		project.Configs = b.configs
	}
	yml, err := marshal(project, &b.subs)
	if err != nil {
		return Result{}, err
	}
	for name, svc := range project.Services {
		b.subs.unfold(&svc)
		project.Services[name] = svc
	}
	return Result{Project: project, YAML: yml, Pins: b.pins, Platforms: b.platforms, Notes: b.sortedNotes(), Objects: b.sortedObjects()}, nil
}

// keys hands out the keys of one kind of top-level entry: one for each engine name, no two the same.
type keys struct{ byName, byKey map[string]string }

func newKeys() *keys { return &keys{byName: map[string]string{}, byKey: map[string]string{}} }

// reserve claims key for name, whatever the name's own wishes are.
func (k *keys) reserve(key, name string) {
	k.byName[name], k.byKey[key] = key, name
}

// hold keeps key from being given to any name, until bind gives it to one.
func (k *keys) hold(key string) { k.byKey[key] = "" }

// try gives name the key if it is a valid one that nothing else has.
func (k *keys) try(name, key string) bool {
	if _, done := k.byName[name]; done {
		return true
	}
	key = safeKey(key)
	if key == "" {
		return false
	}
	if _, taken := k.byKey[key]; taken {
		return false
	}
	k.reserve(key, name)
	return true
}

// assign is the key of name: the first candidate nothing has, else the name made safe with a number.
func (k *keys) assign(name string, candidates ...string) string {
	for _, c := range candidates {
		if k.try(name, c) {
			return k.byName[name]
		}
	}
	base := safeKey(name)
	if base == "" {
		base = "item"
	}
	for i := 2; ; i++ {
		key := base + "-" + strconv.Itoa(i)
		if k.try(name, key) {
			return key
		}
	}
}

// safeKey makes a name a key of a top-level entry: letters, digits, dot, dash and underscore. A name with
// anything else keeps what it can.
func safeKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "._-")
}

// builder is one rebuild.
type builder struct {
	in  SwarmInput
	ns  string
	net map[string]NetworkInfo

	netKeys, volKeys, secretKeys, configKeys *keys
	networks                                 types.Networks
	volumes                                  types.Volumes
	secrets                                  types.Secrets
	configs                                  types.Configs

	pins      map[string]string
	platforms map[string][]swarm.Platform
	notes     []Note
	objects   map[string]Object
	subs      substitutions
}

func newBuilder(in SwarmInput) *builder {
	b := &builder{
		in: in, ns: in.Namespace, net: map[string]NetworkInfo{},
		netKeys: newKeys(), volKeys: newKeys(), secretKeys: newKeys(), configKeys: newKeys(),
		networks: types.Networks{}, volumes: types.Volumes{}, secrets: types.Secrets{}, configs: types.Configs{},
		pins: map[string]string{}, platforms: map[string][]swarm.Platform{}, objects: map[string]Object{},
	}
	for _, n := range in.Networks {
		b.net[n.Name] = n
	}
	// The key default is the stack's own default network, which a file uses without declaring: no other network
	// gets it.
	b.netKeys.hold(defaultKey)
	return b
}

// defaultKey is the key of the network a service joins when it names none.
const defaultKey = "default"

// strip is name without the namespace prefix, and whether it had one.
func (b *builder) strip(name string) (string, bool) {
	prefix := b.ns + "_"
	if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
		return name[len(prefix):], true
	}
	return name, false
}

func (b *builder) note(n Note) { b.notes = append(b.notes, n) }

func (b *builder) object(kind, name, treatment string) {
	b.objects[kind+"\x00"+name] = Object{Kind: kind, Name: name, Treatment: treatment}
}

func (b *builder) sortedNotes() []Note { return sortNotes(b.notes) }

func (b *builder) sortedObjects() []Object { return sortObjects(b.objects) }

// ownLabel is a label of the stack's own bookkeeping: the namespace, the image text and anything of TierPlan's (not the
// user's tierplan.keep, which a rebuild keeps, JAN-07).
func ownLabel(key string) bool {
	return key == stacks.LabelSwarmNamespace || key == stacks.LabelSwarmImage || stacks.IsOwnershipLabel(key)
}

// userLabels are the labels a person put on an object, without the stack's own.
func userLabels(in map[string]string) types.Labels {
	var out types.Labels
	for k, v := range in {
		if ownLabel(k) {
			continue
		}
		if out == nil {
			out = types.Labels{}
		}
		out[k] = v
	}
	return out
}

// prepareFiles gives every secret and config its key before any service is written, so the key a reference uses
// can be the name of the file it mounts as (db_password) when that is a plain name and nothing else has it. A
// target that is a path (/etc/ssl/key.pem) is not a key.
func (b *builder) prepareFiles(services []SwarmService) {
	for _, svc := range services {
		cs := svc.Spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		for _, r := range cs.Secrets {
			if name := b.secretName(r); name != "" && b.baseKey(b.in.Secrets, name) != "" {
				b.secretKeys.try(name, b.baseKey(b.in.Secrets, name))
			} else if name != "" && r.File != nil && safeKey(r.File.Name) == r.File.Name {
				b.secretKeys.try(name, r.File.Name)
			}
		}
		for _, r := range cs.Configs {
			if name := b.configName(r); name != "" && b.baseKey(b.in.Configs, name) != "" {
				b.configKeys.try(name, b.baseKey(b.in.Configs, name))
			} else if name != "" && r.File != nil && r.Runtime == nil && safeKey(r.File.Name) == r.File.Name {
				b.configKeys.try(name, r.File.Name)
			}
		}
	}
}

func (b *builder) secretName(r *swarm.SecretReference) string {
	if r == nil {
		return ""
	}
	if r.SecretName != "" {
		return r.SecretName
	}
	for _, o := range b.in.Secrets {
		if o.ID == r.SecretID {
			return o.Name
		}
	}
	return ""
}

func (b *builder) configName(r *swarm.ConfigReference) string {
	if r == nil {
		return ""
	}
	if r.ConfigName != "" {
		return r.ConfigName
	}
	for _, o := range b.in.Configs {
		if o.ID == r.ConfigID {
			return o.Name
		}
	}
	return ""
}

// withoutDigest is an image reference without the @sha256:... that pins it.
func withoutDigest(ref string) string {
	if i := strings.LastIndex(ref, "@"); i > 0 {
		return ref[:i]
	}
	return ref
}

// hasDigest says whether an image reference is pinned: it carries a digest, or it is an image ID.
func hasDigest(ref string) bool {
	return strings.Contains(ref, "@") || strings.HasPrefix(ref, "sha256:")
}

// service rebuilds one service. It returns the service's name in the file.
func (b *builder) service(svc SwarmService) (string, types.ServiceConfig, error) {
	spec := svc.Spec
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return "", types.ServiceConfig{}, fmt.Errorf("rebuild: the service %s has no container spec, so it is not a container service", svc.Name)
	}
	short, prefixed := b.strip(svc.Name)
	if !prefixed {
		b.note(notePrefix(b.ns, short, svc.Name))
	}

	out := types.ServiceConfig{Name: short}
	out.Image = cmp.Or(spec.Labels[stacks.LabelSwarmImage], withoutDigest(cs.Image))
	if cs.Image != "" {
		b.pins[svc.Name] = cs.Image
		if pl := spec.TaskTemplate.Placement; pl != nil && len(pl.Platforms) > 0 {
			b.platforms[svc.Name] = slices.Clone(pl.Platforms)
		}
		if !hasDigest(cs.Image) {
			b.note(noteUnpinned(short, cs.Image))
		}
	}
	out.Entrypoint = types.ShellCommand(slices.Clone(cs.Command))
	out.Command = types.ShellCommand(slices.Clone(cs.Args))
	out.Environment = environment(cs.Env)
	b.in.Base.keepImage(short, &out, &b.subs)
	b.in.Base.hideSecrets(&out, &b.subs)
	out.Labels = userLabels(cs.Labels)

	d, err := b.deploy(short, spec)
	if err != nil {
		return "", types.ServiceConfig{}, fmt.Errorf("rebuild: service %s: %w", svc.Name, err)
	}
	out.Deploy = d
	out.Ports = ports(spec.EndpointSpec)
	out.Networks = b.serviceNetworks(short, spec.TaskTemplate.Networks)
	out.Volumes = b.serviceMounts(cs.Mounts)
	out.HealthCheck = healthcheck(cs.Healthcheck)
	if ld := spec.TaskTemplate.LogDriver; ld != nil {
		out.Logging = &types.LoggingConfig{Driver: ld.Name, Options: types.Options(maps.Clone(ld.Options))}
	}
	out.Secrets = b.secretRefs(cs.Secrets)
	out.Configs = b.configRefs(cs.Configs)
	b.containerSettings(short, cs, &out)
	return short, out, nil
}

// containerSettings rebuilds the settings of the container spec that are a plain inverse of what convert reads:
// the names it gives the container, how it runs and the limits of the process.
func (b *builder) containerSettings(short string, cs *swarm.ContainerSpec, out *types.ServiceConfig) {
	out.Hostname = cs.Hostname
	out.ExtraHosts = extraHosts(cs.Hosts)
	if dns := cs.DNSConfig; dns != nil {
		for _, ns := range dns.Nameservers {
			out.DNS = append(out.DNS, ns.String())
		}
		out.DNSSearch = types.StringList(slices.Clone(dns.Search))
		out.DNSOpts = slices.Clone(dns.Options)
	}
	out.WorkingDir = cs.Dir
	out.User = cs.User
	out.GroupAdd = slices.Clone(cs.Groups)
	out.StopGracePeriod = durationPtr(cs.StopGracePeriod)
	out.StopSignal = cs.StopSignal
	out.Tty = cs.TTY
	out.StdinOpen = cs.OpenStdin
	out.ReadOnly = cs.ReadOnly
	if cs.Init != nil {
		v := *cs.Init
		out.Init = &v
	}
	if len(cs.Sysctls) > 0 {
		out.Sysctls = types.Mapping(maps.Clone(cs.Sysctls))
	}
	out.CapAdd, out.CapDrop = capabilities(cs.CapabilityAdd), capabilities(cs.CapabilityDrop)
	out.Ulimits = ulimits(cs.Ulimits)
	out.OomScoreAdj = cs.OomScoreAdj
	if !isolationIsDefault(cs.Isolation) {
		out.Isolation = string(cs.Isolation)
	}
	if p := cs.Privileges; p != nil && p.CredentialSpec != nil {
		out.CredentialSpec = b.credentialSpec(cs, p.CredentialSpec)
	}
}

// extraHosts turns swarm's "IP-address hostname" entries into extra_hosts, which is a map from a name to the
// addresses it has.
func extraHosts(hosts []string) types.HostsList {
	var out types.HostsList
	for _, h := range hosts {
		fields := strings.Fields(h)
		if len(fields) < 2 {
			continue
		}
		if out == nil {
			out = types.HostsList{}
		}
		for _, name := range fields[1:] {
			out[name] = append(out[name], fields[0])
		}
	}
	return out
}

// capabilities are capability names the way a person writes them: convert gives each the CAP_ prefix, so it is
// left off here.
func capabilities(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = strings.TrimPrefix(c, "CAP_")
	}
	return out
}

// ulimits turns the limits of a container spec into the map a file writes: one number when the soft and hard limits
// are the same.
func ulimits(in []*container.Ulimit) map[string]*types.UlimitsConfig {
	var out map[string]*types.UlimitsConfig
	for _, u := range in {
		if u == nil {
			continue
		}
		if out == nil {
			out = map[string]*types.UlimitsConfig{}
		}
		if u.Soft == u.Hard {
			out[u.Name] = &types.UlimitsConfig{Single: int(u.Soft)}
		} else {
			out[u.Name] = &types.UlimitsConfig{Soft: int(u.Soft), Hard: int(u.Hard)}
		}
	}
	return out
}

// credentialSpec rebuilds credential_spec. A spec read from a config names the config by ID; the file names it by
// the key of an external top-level config, like any other reference to a config.
func (b *builder) credentialSpec(cs *swarm.ContainerSpec, in *swarm.CredentialSpec) *types.CredentialSpecConfig {
	out := &types.CredentialSpecConfig{File: in.File, Registry: in.Registry}
	if in.Config != "" {
		name := ""
		for _, r := range cs.Configs {
			if r != nil && r.ConfigID == in.Config {
				name = b.configName(r)
			}
		}
		if name == "" {
			for _, o := range b.in.Configs {
				if o.ID == in.Config {
					name = o.Name
				}
			}
		}
		if name != "" {
			out.Config = b.declareConfig(name)
		}
	}
	return out
}

// durationPtr is a compose duration for a pointer to one, nil for nil.
func durationPtr(d *time.Duration) *types.Duration {
	if d == nil {
		return nil
	}
	v := types.Duration(*d)
	return &v
}

// environment turns KEY=value entries into the file's map. An entry with no = names a variable with no value, which
// sets nothing in a container and which convert leaves out of a service as well, so it is left out here: a file
// that named it would fail the check ("has no value and no default").
func environment(env []string) types.MappingWithEquals {
	var out types.MappingWithEquals
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if out == nil {
			out = types.MappingWithEquals{}
		}
		out[k] = &v
	}
	return out
}

// deploy rebuilds the deploy block: the mode and replicas, the labels, the resources, the restart, update and
// rollback policies, the placement and the endpoint mode.
func (b *builder) deploy(short string, spec swarm.ServiceSpec) (*types.DeployConfig, error) {
	d := &types.DeployConfig{Labels: userLabels(spec.Labels)}
	mode, replicas, err := serviceMode(spec.Mode)
	if err != nil {
		return nil, err
	}
	d.Mode, d.Replicas = mode, replicas
	if ep := spec.EndpointSpec; ep != nil && !endpointModeIsDefault(ep.Mode) {
		d.EndpointMode = string(ep.Mode)
	}
	d.UpdateConfig = updateConfig(spec.UpdateConfig)
	d.RollbackConfig = updateConfig(spec.RollbackConfig)
	var notes []Note
	d.Resources, notes = resources(spec.TaskTemplate.Resources)
	for _, n := range notes {
		n.Service = short
		b.note(n)
	}
	d.RestartPolicy = restartPolicy(spec.TaskTemplate.RestartPolicy)
	d.Placement = placement(spec.TaskTemplate.Placement)
	if isZero(*d) {
		return nil, nil
	}
	return d, nil
}

func isZero(d types.DeployConfig) bool {
	return d.Mode == "" && d.Replicas == nil && len(d.Labels) == 0 && d.UpdateConfig == nil && d.RollbackConfig == nil &&
		d.Resources.Limits == nil && d.Resources.Reservations == nil && d.RestartPolicy == nil &&
		len(d.Placement.Constraints) == 0 && len(d.Placement.Preferences) == 0 && d.Placement.MaxReplicas == 0 &&
		d.EndpointMode == ""
}

// serviceMode is deploy.mode and deploy.replicas. A replicated service of one task is the default and says
// nothing (it mirrors the replicas rule of stacks.NormalizeSpec).
func serviceMode(m swarm.ServiceMode) (string, *int, error) {
	switch {
	case m.Replicated != nil:
		if replicasAreDefault(m.Replicated.Replicas) {
			return "", nil, nil
		}
		n := int(*m.Replicated.Replicas) //nolint:gosec // a replica count that does not fit an int is not a count anyone set
		return "", &n, nil
	case m.Global != nil:
		return "global", nil, nil
	case m.ReplicatedJob != nil:
		count := cmp.Or(m.ReplicatedJob.MaxConcurrent, m.ReplicatedJob.TotalCompletions)
		var n *int
		if count != nil {
			v := int(*count) //nolint:gosec // as above
			n = &v
		}
		return "replicated-job", n, nil
	case m.GlobalJob != nil:
		return "global-job", nil, nil
	}
	return "", nil, nil
}

// ports rebuilds the published ports. The tcp protocol and the ingress mode are what the short form means, and
// Marshal writes it that way.
func ports(ep *swarm.EndpointSpec) []types.ServicePortConfig {
	if ep == nil || len(ep.Ports) == 0 {
		return nil
	}
	out := make([]types.ServicePortConfig, 0, len(ep.Ports))
	for _, p := range ep.Ports {
		pc := types.ServicePortConfig{Name: p.Name, Target: p.TargetPort}
		if !publishModeIsDefault(p.PublishMode) {
			pc.Mode = string(p.PublishMode)
		}
		if p.PublishedPort != 0 {
			pc.Published = strconv.FormatUint(uint64(p.PublishedPort), 10)
		}
		if !protocolIsDefault(string(p.Protocol)) {
			pc.Protocol = string(p.Protocol)
		}
		out = append(out, pc)
	}
	return out
}

// networkKeyOf is the key a network has in the file, declaring it on the way.
func (b *builder) networkKey(name string) string {
	if key, ok := b.netKeys.byName[name]; ok {
		return key
	}
	info, known := b.net[name]
	owned := known && info.Labels[stacks.LabelSwarmNamespace] == b.ns
	rest, prefixed := b.strip(name)
	if name == b.defaultNetworkName() {
		// The stack's default network is the key default. One the engine does not describe is the plain network
		// convert makes.
		b.netKeys.byName[name] = defaultKey
		if known && !owned {
			b.networks[defaultKey] = types.NetworkConfig{Name: name, External: true}
			b.object(KindNetwork, name, TreatmentExternal)
			return defaultKey
		}
		b.declareOwnedNetwork(defaultKey, name, info)
		return defaultKey
	}
	if owned {
		candidates := []string{name}
		if prefixed {
			candidates = []string{rest, name}
		}
		key := b.netKeys.assign(name, candidates...)
		b.declareOwnedNetwork(key, name, info)
		return key
	}
	candidates := []string{name}
	if prefixed {
		candidates = append(candidates, rest)
	}
	key := b.netKeys.assign(name, candidates...)
	b.networks[key] = types.NetworkConfig{Name: name, External: true}
	b.object(KindNetwork, name, TreatmentExternal)
	return key
}

// defaultNetworkName is the name of the stack's own default network.
func (b *builder) defaultNetworkName() string { return b.ns + "_" + defaultKey }

// declareOwnedNetwork writes a network the stack owns: under its existing name, with what the engine holds. A
// network that is not an overlay network cannot be created by a stack, so it is written external with a note.
func (b *builder) declareOwnedNetwork(key, name string, info NetworkInfo) {
	if info.Driver != "" && info.Driver != "overlay" {
		b.networks[key] = types.NetworkConfig{Name: name, External: true}
		b.object(KindNetwork, name, TreatmentExternal)
		b.note(noteNotOverlay(name, info.Driver))
		return
	}
	b.object(KindNetwork, name, TreatmentOwned)
	if declared, ok := b.in.Base.ownedNetwork(key); ok {
		b.networks[key] = declared
		return
	}
	if b.in.Base != nil && key == defaultKey && name == b.defaultNetworkName() {
		// The deployed revision's file declares no default network: it is the plain one every stack has.
		return
	}
	cfg := types.NetworkConfig{
		Attachable: info.Attachable, Internal: info.Internal,
		Labels: userLabels(info.Labels),
	}
	if name != b.ns+"_"+key {
		cfg.Name = name
	}
	if key == defaultKey && name == b.defaultNetworkName() && plainDefault(info) {
		return
	}
	cfg.DriverOpts = types.Options(networkOptions(info.Options))
	if info.IPAMDriver != "" && info.IPAMDriver != "default" {
		cfg.Ipam.Driver = info.IPAMDriver
	}
	for _, p := range info.Pools {
		cfg.Ipam.Config = append(cfg.Ipam.Config, &types.IPAMPool{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
	}
	b.networks[key] = cfg
}

// serviceNetworks rebuilds a service's attachments. A service that joins only the stack's default network says
// nothing, as a hand-written file does.
func (b *builder) serviceNetworks(short string, atts []swarm.NetworkAttachmentConfig) map[string]*types.ServiceNetworkConfig {
	if len(atts) == 0 {
		return nil
	}
	out := map[string]*types.ServiceNetworkConfig{}
	for _, att := range atts {
		key := b.networkKey(att.Target)
		var cfg *types.ServiceNetworkConfig
		var aliases []string
		for _, a := range att.Aliases {
			// convert adds the service's own name as an alias on every user-defined network.
			if a != short {
				aliases = append(aliases, a)
			}
		}
		if len(aliases) > 0 || len(att.DriverOpts) > 0 {
			cfg = &types.ServiceNetworkConfig{Aliases: aliases, DriverOpts: types.Options(maps.Clone(att.DriverOpts))}
		}
		out[key] = cfg
	}
	if cfg, only := out[defaultKey]; only && len(out) == 1 && cfg == nil && b.implicitDefault() {
		return nil
	}
	return out
}

// implicitDefault says whether the default network is written nowhere in the file.
func (b *builder) implicitDefault() bool {
	_, declared := b.networks[defaultKey]
	return !declared
}

// volumeKey is the key of the named volume a mount uses, declaring it on the way. A volume the stack's labels are
// on is the stack's own; any other is external (D-18).
func (b *builder) volumeKey(m mount.Mount) string {
	name := m.Source
	owned := m.VolumeOptions != nil && m.VolumeOptions.Labels[stacks.LabelSwarmNamespace] == b.ns
	if key, ok := b.volKeys.byName[name]; ok {
		if owned && bool(b.volumes[key].External) {
			b.volumes[key] = b.ownedVolume(key, name, m.VolumeOptions)
			b.object(KindVolume, name, TreatmentOwned)
		}
		return key
	}
	rest, prefixed := b.strip(name)
	if owned {
		candidates := []string{name}
		if prefixed {
			candidates = []string{rest, name}
		}
		key := b.volKeys.assign(name, candidates...)
		b.volumes[key] = b.ownedVolume(key, name, m.VolumeOptions)
		b.object(KindVolume, name, TreatmentOwned)
		return key
	}
	key := b.volKeys.assign(name, name, rest)
	b.volumes[key] = types.VolumeConfig{Name: name, External: true}
	b.object(KindVolume, name, TreatmentExternal)
	return key
}

func (b *builder) ownedVolume(key, name string, vo *mount.VolumeOptions) types.VolumeConfig {
	if declared, ok := b.in.Base.ownedVolume(key); ok {
		return declared
	}
	cfg := types.VolumeConfig{Labels: userLabels(vo.Labels)}
	if name != b.ns+"_"+key {
		cfg.Name = name
	}
	if dc := vo.DriverConfig; dc != nil {
		cfg.Driver = dc.Name
		cfg.DriverOpts = types.Options(maps.Clone(dc.Options))
	}
	return cfg
}

// serviceMounts rebuilds a service's mounts, in the order the service lists them.
func (b *builder) serviceMounts(mounts []mount.Mount) []types.ServiceVolumeConfig {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]types.ServiceVolumeConfig, 0, len(mounts))
	for _, m := range mounts {
		v := types.ServiceVolumeConfig{Type: string(m.Type), Target: m.Target, ReadOnly: m.ReadOnly, Consistency: string(m.Consistency)}
		switch m.Type {
		case mount.TypeVolume:
			if m.Source != "" {
				v.Source = b.volumeKey(m)
			}
			if vo := m.VolumeOptions; vo != nil && (vo.NoCopy || vo.Subpath != "") {
				v.Volume = &types.ServiceVolumeVolume{NoCopy: vo.NoCopy, Subpath: vo.Subpath}
			}
		case mount.TypeBind:
			v.Source = m.Source
			if bo := m.BindOptions; bo != nil && bo.Propagation != "" {
				// create_host_path is true unless the file says otherwise, and is then left out of the file.
				v.Bind = &types.ServiceVolumeBind{Propagation: string(bo.Propagation), CreateHostPath: true}
			}
		case mount.TypeTmpfs:
			if to := m.TmpfsOptions; to != nil && (to.SizeBytes != 0 || to.Mode != 0) {
				v.Tmpfs = &types.ServiceVolumeTmpfs{Size: types.UnitBytes(to.SizeBytes), Mode: uint32(to.Mode)}
			}
		case mount.TypeImage:
			v.Source = m.Source
			if io := m.ImageOptions; io != nil && io.Subpath != "" {
				v.Image = &types.ServiceVolumeImage{SubPath: io.Subpath}
			}
		default:
			v.Source = m.Source
		}
		out = append(out, v)
	}
	return out
}

// healthcheck rebuilds a health check. The test NONE is a disabled one.
func healthcheck(hc *container.HealthConfig) *types.HealthCheckConfig {
	if hc == nil {
		return nil
	}
	if len(hc.Test) == 1 && hc.Test[0] == "NONE" {
		return &types.HealthCheckConfig{Disable: true}
	}
	out := &types.HealthCheckConfig{Test: types.HealthCheckTest(slices.Clone(hc.Test))}
	out.Timeout = duration(hc.Timeout)
	out.Interval = duration(hc.Interval)
	out.StartPeriod = duration(hc.StartPeriod)
	out.StartInterval = duration(hc.StartInterval)
	if hc.Retries > 0 {
		n := uint64(hc.Retries)
		out.Retries = &n
	}
	return out
}

// duration is a compose duration for a non-zero one, nil for zero.
func duration(d time.Duration) *types.Duration {
	if d == 0 {
		return nil
	}
	v := types.Duration(d)
	return &v
}

// secretRefs rebuilds a service's secrets as references to external top-level secrets, named as the swarm names
// them (D-13). Only the name is ever read; the value is not TierPlan's to know.
func (b *builder) secretRefs(refs []*swarm.SecretReference) []types.ServiceSecretConfig {
	var out []types.ServiceSecretConfig
	for _, r := range refs {
		name := b.secretName(r)
		if name == "" {
			continue
		}
		key := b.secretKeys.assign(name, b.stripped(name), name)
		if declared, ok := b.in.Base.ownedSecret(key); ok && b.baseKey(b.in.Secrets, name) == key {
			// A secret the stack made from one of its files stays that file (D-54).
			b.secrets[key] = declared
			b.object(KindSecret, name, TreatmentOwned)
		} else {
			b.secrets[key] = types.SecretConfig{Name: name, External: true}
			b.object(KindSecret, name, TreatmentExternal)
		}
		ref := types.ServiceSecretConfig{Source: key}
		if f := r.File; f != nil {
			ref.Target, ref.UID, ref.GID, ref.Mode = fileReference(key, f.Name, f.UID, f.GID, f.Mode)
		}
		out = append(out, ref)
	}
	return out
}

// configRefs does the same for configs. A reference that only lets the credential spec read a config (a runtime
// reference) is not a file of the service; the credential spec names it.
func (b *builder) configRefs(refs []*swarm.ConfigReference) []types.ServiceConfigObjConfig {
	var out []types.ServiceConfigObjConfig
	for _, r := range refs {
		name := b.configName(r)
		if name == "" || r.Runtime != nil {
			continue
		}
		key := b.declareConfig(name)
		ref := types.ServiceConfigObjConfig{Source: key}
		if f := r.File; f != nil {
			ref.Target, ref.UID, ref.GID, ref.Mode = fileReference(key, f.Name, f.UID, f.GID, f.Mode)
		}
		out = append(out, ref)
	}
	return out
}

// declareConfig writes an external top-level config under the key it has, and returns the key.
func (b *builder) declareConfig(name string) string {
	key := b.configKeys.assign(name, b.stripped(name), name)
	if declared, ok := b.in.Base.ownedConfig(key); ok && b.baseKey(b.in.Configs, name) == key {
		b.configs[key] = declared
		b.object(KindConfig, name, TreatmentOwned)
		return key
	}
	b.configs[key] = types.ConfigObjConfig{Name: name, External: true}
	b.object(KindConfig, name, TreatmentExternal)
	return key
}

// stripped is the name without the namespace prefix, or the name when it has none.
func (b *builder) stripped(name string) string {
	rest, _ := b.strip(name)
	return rest
}

// fileReference is the target, ownership and mode of a secret or config reference, each left out when it is
// what convert gives a reference that says nothing: the target is the key, ownership root and the mode 0444.
func fileReference(key, name, uid, gid string, mode os.FileMode) (target, outUID, outGID string, outMode *types.FileMode) {
	if name != key {
		target = name
	}
	if uid != "" && uid != "0" {
		outUID = uid
	}
	if gid != "" && gid != "0" {
		outGID = gid
	}
	if mode != 0o444 {
		m := types.FileMode(mode)
		outMode = &m
	}
	return target, outUID, outGID, outMode
}

// updateConfig rebuilds update_config and rollback_config. Parallelism is written unless it is one, which is what
// convert gives a policy that names none (zero, all at once, is a value of its own); the failure action and the
// order are left out at their defaults.
func updateConfig(uc *swarm.UpdateConfig) *types.UpdateConfig {
	if uc == nil {
		return nil
	}
	out := &types.UpdateConfig{
		Delay: types.Duration(uc.Delay), Monitor: types.Duration(uc.Monitor), MaxFailureRatio: uc.MaxFailureRatio,
	}
	if uc.Parallelism != 1 {
		p := uc.Parallelism
		out.Parallelism = &p
	}
	if !failureActionIsDefault(uc.FailureAction) {
		out.FailureAction = string(uc.FailureAction)
	}
	if !updateOrderIsDefault(uc.Order) {
		out.Order = string(uc.Order)
	}
	return out
}

// restartPolicy rebuilds deploy.restart_policy. A limit of zero attempts is the same as none.
func restartPolicy(rp *swarm.RestartPolicy) *types.RestartPolicy {
	if rp == nil {
		return nil
	}
	out := &types.RestartPolicy{Condition: string(rp.Condition), Delay: durationPtr(rp.Delay), Window: durationPtr(rp.Window)}
	if !attemptsAreDefault(rp.MaxAttempts) {
		n := *rp.MaxAttempts
		out.MaxAttempts = &n
	}
	return out
}

// placement rebuilds deploy.placement: the constraints, the spread preferences and the limit of tasks per node.
func placement(p *swarm.Placement) types.Placement {
	var out types.Placement
	if p == nil {
		return out
	}
	out.Constraints = slices.Clone(p.Constraints)
	out.MaxReplicas = p.MaxReplicas
	for _, pref := range p.Preferences {
		if pref.Spread != nil {
			out.Preferences = append(out.Preferences, types.PlacementPreferences{Spread: pref.Spread.SpreadDescriptor})
		}
	}
	return out
}

// resources rebuilds deploy.resources. A processor count is exact to nine decimals in a swarm spec but compose-go
// holds it as a 32-bit float, so one that a float cannot carry is written as near as it can and noted.
func resources(rr *swarm.ResourceRequirements) (types.Resources, []Note) {
	var out types.Resources
	var notes []Note
	if rr == nil {
		return out, nil
	}
	cpus := func(nano int64, where string) types.NanoCPUs {
		v, exact := cpusOf(nano)
		if !exact {
			notes = append(notes, noteCPUs(where, nano))
		}
		return v
	}
	if l := rr.Limits; l != nil {
		out.Limits = &types.Resource{NanoCPUs: cpus(l.NanoCPUs, "limit"), MemoryBytes: types.UnitBytes(l.MemoryBytes), Pids: l.Pids}
	}
	if r := rr.Reservations; r != nil {
		res := &types.Resource{NanoCPUs: cpus(r.NanoCPUs, "reservation"), MemoryBytes: types.UnitBytes(r.MemoryBytes)}
		for _, g := range r.GenericResources {
			if g.DiscreteResourceSpec != nil {
				res.GenericResources = append(res.GenericResources, types.GenericResource{
					DiscreteResourceSpec: &types.DiscreteGenericResource{Kind: g.DiscreteResourceSpec.Kind, Value: g.DiscreteResourceSpec.Value},
				})
			}
		}
		out.Reservations = res
	}
	return out, notes
}

// cpusOf is a processor count in nano-CPUs as the float compose-go holds it, and whether converting it back gives
// the same number of nano-CPUs (convert reads a float32 through its shortest decimal, so 0.1 is exact).
func cpusOf(nano int64) (types.NanoCPUs, bool) {
	if nano == 0 {
		return 0, true
	}
	decimal := new(big.Rat).SetFrac64(nano, 1_000_000_000).FloatString(9)
	decimal = strings.TrimRight(strings.TrimRight(decimal, "0"), ".")
	f, err := strconv.ParseFloat(decimal, 32)
	if err != nil {
		return 0, false
	}
	back, ok := new(big.Rat).SetString(strconv.FormatFloat(float64(float32(f)), 'f', -1, 32))
	if !ok {
		return types.NanoCPUs(f), false
	}
	back.Mul(back, big.NewRat(1_000_000_000, 1))
	return types.NanoCPUs(f), back.IsInt() && back.Num().Int64() == nano
}

// baseKey is the key a secret or config that the stack made from one of its files has in the base's file, "" for
// any other object and when there is no base. The object says it in its tierplan.stack.object label.
func (b *builder) baseKey(rows []ObjectInfo, name string) string {
	if b.in.Base == nil {
		return ""
	}
	for _, o := range rows {
		if o.Name == name {
			return o.Labels[stacks.LabelObject]
		}
	}
	return ""
}
