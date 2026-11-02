// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rebuild

import (
	"cmp"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// The labels the Compose SDK puts on what it creates, which the rebuild reads back.
const (
	labelNetwork   = "com.docker.compose.network"
	labelVolume    = "com.docker.compose.volume"
	labelDependsOn = "com.docker.compose.depends_on"
	labelNumber    = "com.docker.compose.container-number"
	labelRelay     = "com.docker.compose.relay-network"
)

// StandaloneContainer is a container of the project as the host inspects it, with the image it runs. Image is
// the zero value when the host no longer has the image.
type StandaloneContainer struct {
	Inspect container.InspectResponse
	Image   image.InspectResponse
}

// StandaloneInput is what the standalone direction needs. Project is the compose project's name (the
// com.docker.compose.project label of its containers). Containers are all the project's containers. Networks
// and Volumes describe the objects the host holds, those labelled with the project and any others the caller
// read: a network or volume that carries the project's label is the project's and is declared under its
// existing name, every other one a container uses is external (D-18). Choice says which rendering of an owned
// object to write, by "network:{key}" or "volume:{key}": the index into NetworkCandidates or VolumeCandidates,
// -1 for the object as an external one, absent for the first. Base, when set, is the definition the project already
// has, which the rebuild is made on top of (see Base): the declaration it holds of an owned network or volume is
// the first rendering of that object.
type StandaloneInput struct {
	Project    string
	Containers []StandaloneContainer
	Networks   []NetworkInfo
	Volumes    []VolumeInfo
	Choice     map[string]int
	Base       *Base
}

// Standalone rebuilds the compose project of a docker compose up project from its containers (D-12). Each
// service is made from its lowest-numbered container by the same code that compares a running service with a
// file (stacks.RunningService and stacks.NormalizeComposeService), then the three things the engine does not
// record are added: the image as the container was created from it and the pin that holds it (D-14), the
// dependencies from the com.docker.compose.depends_on label, and the scale from the number of containers.
// What the daemon supplies when a container sets nothing (the default log driver, a private IPC namespace,
// the default shared memory, a swap limit that follows the memory limit, no limit on processes, the default
// ulimits) is left out of the file, so it reads like one a person wrote. Services, objects and notes come out
// in name order, so the same project always gives the same file.
func Standalone(in StandaloneInput) (Result, error) {
	b := &standaloneBuild{
		in: in, netKeys: newKeys(), volKeys: newKeys(), netKey: map[string]string{}, volKey: map[string]string{},
		netOwned: map[string]NetworkInfo{}, volOwned: map[string]VolumeInfo{},
		objects: map[string]Object{}, variants: map[string]int{}, pins: map[string]string{},
	}
	return b.build()
}

type standaloneGroup struct {
	name       string
	containers []StandaloneContainer
}

type standaloneBuild struct {
	in StandaloneInput

	netKeys, volKeys *keys
	// netKey and volKey give the key of an object by the name the engine holds it under.
	netKey, volKey   map[string]string
	netOwned         map[string]NetworkInfo
	volOwned         map[string]VolumeInfo
	extNets, extVols []string

	networks types.Networks
	volumes  types.Volumes

	pins     map[string]string
	notes    []Note
	objects  map[string]Object
	variants map[string]int
	subs     substitutions
}

func (b *standaloneBuild) object(kind, name, treatment, key string) {
	b.objects[kind+"\x00"+name] = Object{Kind: kind, Name: name, Treatment: treatment, Key: key}
}

func (b *standaloneBuild) build() (Result, error) {
	groups := b.groups()
	if len(groups) == 0 {
		return Result{}, fmt.Errorf("rebuild: the project %s has no service container", b.in.Project)
	}
	b.discover(groups)

	known := map[string]bool{}
	peers := map[string]string{}
	for _, g := range groups {
		known[g.name] = true
		for _, c := range g.containers {
			peers[c.Inspect.ID] = g.name
		}
	}
	rc := stacks.RunningContext{Project: b.in.Project, Peers: peers, Networks: b.netKey, Volumes: b.volKey}

	project := &types.Project{Name: b.in.Project, Services: types.Services{}}
	for _, g := range groups {
		svc := b.service(g, known, rc)
		project.Services[g.name] = svc
	}
	if err := b.declare(project); err != nil {
		return Result{}, err
	}
	yml, err := marshal(project, &b.subs)
	if err != nil {
		return Result{}, err
	}
	for name, svc := range project.Services {
		b.subs.unfold(&svc)
		project.Services[name] = svc
	}
	return Result{
		Project: project, YAML: yml, Pins: b.pins, Notes: sortNotes(b.notes), Objects: sortObjects(b.objects),
		Variants: b.variants,
	}, nil
}

// groups are the project's services with their containers, one-off containers (docker compose run) left out,
// the lowest-numbered container first.
func (b *standaloneBuild) groups() []standaloneGroup {
	by := map[string][]StandaloneContainer{}
	for _, c := range b.in.Containers {
		cfg := c.Inspect.Config
		if cfg == nil {
			continue
		}
		name := cfg.Labels[stacks.LabelComposeService]
		if name == "" || strings.EqualFold(cfg.Labels[stacks.LabelComposeOneoff], "true") {
			continue
		}
		by[name] = append(by[name], c)
	}
	number := func(c StandaloneContainer) int {
		n, _ := strconv.Atoi(c.Inspect.Config.Labels[labelNumber])
		return n
	}
	out := make([]standaloneGroup, 0, len(by))
	for _, name := range slices.Sorted(maps.Keys(by)) {
		cs := by[name]
		slices.SortStableFunc(cs, func(x, y StandaloneContainer) int {
			return cmp.Or(cmp.Compare(number(x), number(y)), cmp.Compare(x.Inspect.Name, y.Inspect.Name))
		})
		out = append(out, standaloneGroup{name: name, containers: cs})
	}
	return out
}

// stripProject is name without the project prefix (project_), and whether it had one.
func stripProject(project, name string) string {
	return strings.TrimPrefix(name, project+"_")
}

// discover finds the project's networks and volumes, gives each a key, and finds the ones its containers use
// that are not the project's (external).
func (b *standaloneBuild) discover(groups []standaloneGroup) {
	nets := slices.Clone(b.in.Networks)
	slices.SortFunc(nets, func(x, y NetworkInfo) int { return cmp.Compare(x.Name, y.Name) })
	for _, n := range nets {
		if n.Labels[stacks.LabelComposeProject] != b.in.Project || n.Labels[labelRelay] != "" {
			continue
		}
		b.netOwned[n.Name] = n
		b.netKey[n.Name] = b.netKeys.assign(n.Name, n.Labels[labelNetwork], stripProject(b.in.Project, n.Name))
	}
	vols := slices.Clone(b.in.Volumes)
	slices.SortFunc(vols, func(x, y VolumeInfo) int { return cmp.Compare(x.Name, y.Name) })
	for _, v := range vols {
		if v.Labels[stacks.LabelComposeProject] != b.in.Project {
			continue
		}
		b.volOwned[v.Name] = v
		b.volKey[v.Name] = b.volKeys.assign(v.Name, v.Labels[labelVolume], stripProject(b.in.Project, v.Name))
	}

	usedNets, usedVols := map[string]bool{}, map[string]bool{}
	for _, g := range groups {
		for _, c := range g.containers {
			for _, name := range networksOf(c.Inspect) {
				usedNets[name] = true
			}
			for _, name := range namedVolumesOf(c.Inspect) {
				usedVols[name] = true
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(usedNets)) {
		if _, owned := b.netOwned[name]; !owned {
			b.extNets = append(b.extNets, name)
			b.netKey[name] = b.netKeys.assign(name, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(usedVols)) {
		if _, owned := b.volOwned[name]; !owned {
			b.extVols = append(b.extVols, name)
			b.volKey[name] = b.volKeys.assign(name, name)
		}
	}
}

// networksOf are the networks a container is attached to that a compose file names: none for a network mode
// of the engine's own (host, none, bridge, another container's), else every network of its settings and the
// network its mode names.
func networksOf(c container.InspectResponse) []string {
	if c.HostConfig == nil {
		return nil
	}
	mode := string(c.HostConfig.NetworkMode)
	switch {
	case mode == "bridge", mode == "host", mode == "none", mode == "default",
		strings.HasPrefix(mode, types.ContainerPrefix), strings.HasPrefix(mode, types.NetworkModeServicePrefix), strings.HasPrefix(mode, "ns:"):
		return nil
	}
	var out []string
	if mode != "" {
		out = append(out, mode)
	}
	if c.NetworkSettings != nil {
		for name := range c.NetworkSettings.Networks {
			if name != "bridge" && name != "host" && name != "none" && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// namedVolumesOf are the named volumes a container mounts, from the legacy binds and the mounts of its host
// configuration: a source that is not a path.
func namedVolumesOf(c container.InspectResponse) []string {
	if c.HostConfig == nil {
		return nil
	}
	var out []string
	for _, bind := range c.HostConfig.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) >= 2 && parts[0] != "" && !strings.HasPrefix(parts[0], "/") {
			out = append(out, parts[0])
		}
	}
	for _, m := range c.HostConfig.Mounts {
		if string(m.Type) == types.VolumeTypeVolume && m.Source != "" {
			out = append(out, m.Source)
		}
	}
	return out
}

// service rebuilds one service.
func (b *standaloneBuild) service(g standaloneGroup, known map[string]bool, rc stacks.RunningContext) types.ServiceConfig {
	first := g.containers[0]
	run := stacks.RunningService(first.Inspect, stacks.ImageDefaults{}, nil, rc)
	scale := len(g.containers)
	run.Scale = &scale
	svc := stacks.NormalizeComposeService(run, stacks.ImageDefaultsOf(first.Image), rc)
	svc.Name = g.name
	// The container holds its mounts in the order the file gave them, which the normal form sorts away.
	order := map[string]int{}
	for i, v := range run.Volumes {
		if _, seen := order[v.Target]; !seen {
			order[v.Target] = i
		}
	}
	slices.SortStableFunc(svc.Volumes, func(x, y types.ServiceVolumeConfig) int { return cmp.Compare(order[x.Target], order[y.Target]) })

	text, pin, local := standaloneImage(first.Inspect.Config.Image, first.Image)
	svc.Image = text
	b.in.Base.keepImage(g.name, &svc, &b.subs)
	b.in.Base.hideSecrets(&svc, &b.subs)
	if text != "" {
		b.pins[g.name] = pin
		switch {
		case local:
			b.notes = append(b.notes, noteImageLocal(g.name))
		case !hasDigest(pin):
			b.notes = append(b.notes, noteUnpinned(g.name, pin))
		}
	}
	svc.DependsOn = dependsOn(first.Inspect.Config.Labels[labelDependsOn], g.name, known)
	omitDaemonDefaults(&svc)
	return svc
}

var imageIDRe = regexp.MustCompile(`^(sha256:)?[a-f0-9]{12,64}$`)

// standaloneImage is the image text a service's file says, the pin that holds it (D-14), and whether the image
// is one no registry has. A container made from a reference keeps the text it was created with; its pin is that
// text with the digest the host's image has for the repository the text names. An image with no such digest was
// built on the host, and is pinned to its image ID. An image the host no longer has is pinned to the text, which
// is all there is.
func standaloneImage(text string, img image.InspectResponse) (file, pin string, local bool) {
	switch {
	case img.ID == "":
		return text, text, false
	case imageIDRe.MatchString(text):
		file = text
		if len(img.RepoTags) > 0 {
			file = img.RepoTags[0]
		}
		return file, img.ID, false
	case strings.Contains(text, "@"):
		return withoutDigest(text), text, false
	}
	named, err := reference.ParseNormalizedNamed(text)
	if err != nil {
		return text, img.ID, true
	}
	for _, rd := range img.RepoDigests {
		other, err := reference.ParseNormalizedNamed(rd)
		if err != nil || other.Name() != named.Name() {
			continue
		}
		if _, digest, ok := strings.Cut(rd, "@"); ok {
			return text, text + "@" + digest, false
		}
	}
	return text, img.ID, true
}

// dependsOn reads the label the SDK puts on a container: name:condition:restart for each dependency, joined by
// commas. A dependency on a service the project no longer has is left out, and the condition the label lacks is
// the default.
func dependsOn(label, self string, known map[string]bool) types.DependsOnConfig {
	var out types.DependsOnConfig
	for _, part := range strings.Split(label, ",") {
		fields := strings.Split(part, ":")
		name := fields[0]
		if name == "" || name == self || !known[name] {
			continue
		}
		dep := types.ServiceDependency{Condition: types.ServiceConditionStarted, Required: true}
		if len(fields) > 1 && fields[1] != "" {
			dep.Condition = fields[1]
		}
		dep.Restart = len(fields) > 2 && fields[2] == "true"
		if out == nil {
			out = types.DependsOnConfig{}
		}
		out[name] = dep
	}
	return out
}

// The settings the daemon fills in for a container that sets none, at the values a daemon with no configuration
// of its own uses. A container that has them cannot be told from one that was given them, so they are left
// out: a deploy of the file gives the same container.
const defaultShmSize = 64 << 20

func omitDaemonDefaults(svc *types.ServiceConfig) {
	if l := svc.Logging; l != nil && (l.Driver == "" || l.Driver == "json-file") && len(l.Options) == 0 {
		svc.Logging = nil
	}
	if svc.Ipc == "private" {
		svc.Ipc = ""
	}
	if svc.ShmSize == defaultShmSize {
		svc.ShmSize = 0
	}
	if svc.MemLimit > 0 && svc.MemSwapLimit == 2*svc.MemLimit {
		svc.MemSwapLimit = 0
	}
	if svc.PidsLimit < 0 {
		svc.PidsLimit = 0
	}
	if u := svc.Ulimits["nofile"]; u != nil && defaultNofile(u) {
		delete(svc.Ulimits, "nofile")
		if len(svc.Ulimits) == 0 {
			svc.Ulimits = nil
		}
	}
}

// defaultNofile reports whether a limit on open files is one dockerd gives a container with no setting of its
// own: its classic soft and hard values, or the limit systemd's infinity becomes.
func defaultNofile(u *types.UlimitsConfig) bool {
	return (u.Soft == 1024 && u.Hard == 524288) || (u.Soft == 1048576 && u.Hard == 1048576)
}

// declare writes the top-level networks and volumes of the project and the objects of the result.
func (b *standaloneBuild) declare(project *types.Project) error {
	b.networks, b.volumes = types.Networks{}, types.Volumes{}

	owned := slices.Sorted(maps.Keys(b.netOwned))
	for _, name := range owned {
		info, key := b.netOwned[name], b.netKey[name]
		cands := NetworkCandidates(info, b.in.Project, key)
		if declared, ok := b.in.Base.ownedNetwork(key); ok {
			cands = withFirst(cands, declared)
		}
		choice, err := b.choose("network:"+key, len(cands))
		if err != nil {
			return err
		}
		b.variants["network:"+key] = len(cands)
		if choice < 0 {
			b.networks[key] = types.NetworkConfig{Name: info.Name, External: true}
			b.object(KindNetwork, info.Name, TreatmentExternal, key)
			b.notes = append(b.notes, noteObjectExternal(info.Name))
			continue
		}
		b.networks[key] = cands[choice]
		b.object(KindNetwork, info.Name, TreatmentOwned, key)
	}
	for _, name := range b.extNets {
		key := b.netKey[name]
		b.networks[key] = types.NetworkConfig{Name: name, External: true}
		b.object(KindNetwork, name, TreatmentExternal, key)
	}
	for _, name := range slices.Sorted(maps.Keys(b.volOwned)) {
		info, key := b.volOwned[name], b.volKey[name]
		cands := VolumeCandidates(info, b.in.Project, key)
		if declared, ok := b.in.Base.ownedVolume(key); ok {
			cands = withFirst(cands, declared)
		}
		choice, err := b.choose("volume:"+key, len(cands))
		if err != nil {
			return err
		}
		b.variants["volume:"+key] = len(cands)
		if choice < 0 {
			b.volumes[key] = types.VolumeConfig{Name: info.Name, External: true}
			b.object(KindVolume, info.Name, TreatmentExternal, key)
			b.notes = append(b.notes, noteObjectExternal(info.Name))
			continue
		}
		b.volumes[key] = cands[choice]
		b.object(KindVolume, info.Name, TreatmentOwned, key)
	}
	for _, name := range b.extVols {
		key := b.volKey[name]
		b.volumes[key] = types.VolumeConfig{Name: name, External: true}
		b.object(KindVolume, name, TreatmentExternal, key)
	}

	b.dropImplicitDefault(project)
	if len(b.networks) > 0 {
		project.Networks = b.networks
	}
	if len(b.volumes) > 0 {
		project.Volumes = b.volumes
	}
	return nil
}

// choose is the rendering of the object a Choice picks: the first when it says nothing.
func (b *standaloneBuild) choose(key string, n int) (int, error) {
	i, ok := b.in.Choice[key]
	switch {
	case !ok:
		return 0, nil
	case i < 0:
		return -1, nil
	case i >= n:
		return 0, fmt.Errorf("rebuild: %s has %d renderings, not %d", key, n, i+1)
	}
	return i, nil
}

// dropImplicitDefault leaves the project's default network out of the file when it is the plain one that
// compose makes for a project whose services name no network: no service lists it beside another network, no
// service gives it settings, and at least one uses it. The services that use it alone then say nothing.
func (b *standaloneBuild) dropImplicitDefault(project *types.Project) {
	key, ok := b.netKey[engineName(b.in.Project, "default")]
	if !ok || key != "default" {
		return
	}
	if _, owned := b.netOwned[engineName(b.in.Project, "default")]; !owned {
		return
	}
	if cfg, declared := b.networks[key]; !declared || !isPlainNetwork(cfg) {
		return
	}
	using := 0
	for _, svc := range project.Services {
		nets, has := svc.Networks[key]
		if !has {
			continue
		}
		if len(svc.Networks) > 1 || nets != nil {
			return
		}
		using++
	}
	if using == 0 {
		return
	}
	for name, svc := range project.Services {
		if _, has := svc.Networks[key]; has {
			svc.Networks = nil
			project.Services[name] = svc
		}
	}
	delete(b.networks, key)
}

// isPlainNetwork reports whether a declared network is the one compose makes when nothing is declared.
func isPlainNetwork(cfg types.NetworkConfig) bool {
	return reflect.DeepEqual(cfg, types.NetworkConfig{})
}

// withFirst puts the declaration the base holds ahead of the renderings of an object. The caller tries them in
// order against the hash the host holds, so the base's own text is kept whenever it is the one the object was made
// from; a rendering equal to it is not listed twice.
func withFirst[T any](cands []T, declared T) []T {
	out := make([]T, 0, len(cands)+1)
	out = append(out, declared)
	for _, c := range cands {
		if !reflect.DeepEqual(c, declared) {
			out = append(out, c)
		}
	}
	return out
}
