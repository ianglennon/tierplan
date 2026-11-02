// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/rebuild"
)

// RebuildOptions say how a live rebuild is made.
type RebuildOptions struct {
	// Reveal is whether the caller may read the values the engine holds for the stack: an environment value, a
	// command, a health check command, a log option (secrets.reveal). Without it the specs are masked before the
	// file is made, so nothing the caller may not read is ever in it (D-47).
	Reveal bool
	// Base, when set, is the definition the stack already has, which the rebuild is made on top of (D-54): the
	// stack's own secrets and configs stay files, an image keeps the text it was written with, and a value that
	// is a secret variable's is written as the variable. Adoption has none and writes what runs as literals (D-15).
	Base *rebuild.Base
	// Beside is the rest of the stack's working copy: the files and variables the rebuilt compose.yaml is checked
	// beside, so a variable it refers to or a file it names is found as it will be at a deploy. Empty for none.
	Beside stacks.WorkingCopy
}

// composeBeside is a working copy of the rebuilt compose.yaml with the files and variables of beside.
func composeBeside(beside stacks.WorkingCopy, compose []byte) stacks.WorkingCopy {
	files := make([]stacks.File, 0, len(beside.Files)+1)
	files = append(files, stacks.File{Path: stacks.ComposeFile, Content: compose})
	for _, f := range beside.Files {
		if f.Path != stacks.ComposeFile {
			files = append(files, f)
		}
	}
	return stacks.WorkingCopy{Files: files, Vars: beside.Vars}
}

// RebuildError is a rebuild that came out as a file that cannot be used: the services could not be written as
// compose, or the file does not pass the check every stack passes. Reason is a sentence for the page and holds no
// value Reveal did not allow.
type RebuildError struct{ Reason string }

func (e *RebuildError) Error() string { return "stackops: rebuild: " + e.Reason }

// ErrRebuildTarget is a rebuild of a stack whose target has no rebuilder.
var ErrRebuildTarget = errors.New("stackops: rebuilding a stack for this target is not supported")

// RebuildLive rebuilds the compose file of a stack from what runs it (D-12). It reads the engine and writes
// nothing. A swarm stack is rebuilt from its services, read once through a manager (the services of the
// namespace, every network, and the names of the secrets and configs): the content of a secret or a config is
// never read (D-13). A standalone project is rebuilt from its containers and images, read from the host, and
// from its networks and volumes. Whatever the target, a caller without secrets.reveal gets <hidden> where the
// engine holds an environment value, a command, a health check command or a log option, because the specs and
// the inspect documents are masked before the file is made (D-47).
//
// The rebuilt file has to pass stacks.Check, the check every stack passes before it is deployed; one that does not
// is a *RebuildError naming the first problem.
func (d Deps) RebuildLive(ctx context.Context, envID string, st Stack, o RebuildOptions) (rebuild.Result, error) {
	switch st.Target {
	case stacks.TargetSwarm:
		return d.rebuildSwarm(ctx, envID, st, o)
	case stacks.TargetStandalone:
		return d.rebuildStandalone(ctx, envID, st, o)
	}
	return rebuild.Result{}, ErrRebuildTarget
}

// checkRebuilt is the check every stack passes, held against a rebuilt file: the first error is the rebuild's.
func checkRebuilt(ctx context.Context, st Stack, res rebuild.Result, beside stacks.WorkingCopy) error {
	check := stacks.Check(ctx, stacks.CheckInput{
		Name: st.Name, Target: st.Target, Copy: composeBeside(beside, res.YAML),
	})
	for _, p := range check.Problems {
		if p.Severity == stacks.SeverityError {
			return &RebuildError{Reason: p.Message}
		}
	}
	return nil
}

func (d Deps) rebuildSwarm(ctx context.Context, envID string, st Stack, o RebuildOptions) (rebuild.Result, error) {
	read, err := d.readSwarmForRebuild(ctx, envID, st)
	if err != nil {
		return rebuild.Result{}, err
	}
	own := OwnershipOf(st)
	idName := make(map[string]string, len(read.networks))
	for _, n := range read.networks {
		idName[n.ID] = n.Name
	}
	in := rebuild.SwarmInput{Namespace: st.Name, Base: o.Base}
	attached := map[string]bool{}
	var owned []dockerx.ServiceDetail
	for _, det := range read.services {
		if !own.owns(det.Stack, det.Labels) {
			continue
		}
		owned = append(owned, det)
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
		in.Secrets = append(in.Secrets, rebuild.ObjectInfo{ID: s.ID, Name: s.Name, Labels: s.Labels})
	}
	for _, c := range read.configs {
		in.Configs = append(in.Configs, rebuild.ObjectInfo{ID: c.ID, Name: c.Name, Labels: c.Labels})
	}

	res, err := rebuild.Swarm(in)
	if err != nil {
		return rebuild.Result{}, &RebuildError{Reason: err.Error()}
	}
	if err := checkRebuilt(ctx, st, res, o.Beside); err != nil {
		return rebuild.Result{}, err
	}
	res.Fingerprint = swarmFingerprint(owned)
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
		IPv6: row.IPv6, Labels: row.Labels,
	}
	if det.ID != "" {
		info.Options = det.Options
		for _, p := range det.IPAM {
			info.Pools = append(info.Pools, rebuild.Pool{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
		}
	}
	return info
}

// rebuildStandalone rebuilds the compose file of a docker compose up project from its containers (D-12).
func (d Deps) rebuildStandalone(ctx context.Context, envID string, st Stack, o RebuildOptions) (rebuild.Result, error) {
	node, err := d.StandaloneNode(ctx, envID)
	if err != nil {
		return rebuild.Result{}, err
	}
	in, fingerprint, err := d.readStandaloneForRebuild(ctx, envID, node, st.Name, o.Reveal)
	if err != nil {
		return rebuild.Result{}, err
	}
	if len(in.Containers) == 0 {
		return rebuild.Result{}, &RebuildError{Reason: "no container of " + st.Name + " is on the host"}
	}
	in.Base = o.Base
	res, err := d.rebuildMatchingHashes(ctx, st, in, o.Beside)
	if err != nil {
		return rebuild.Result{}, err
	}
	if err := checkRebuilt(ctx, st, res, o.Beside); err != nil {
		return rebuild.Result{}, err
	}
	res.Fingerprint = fingerprint
	return res, nil
}

// rebuildMatchingHashes rebuilds a standalone project so that its first deploy leaves every network and volume
// it owns as it is (D-46, D-18). The Compose SDK recreates a network, and asks to recreate a volume with its data,
// when the hash of the config the project declares differs from the config-hash label the object was made with.
// So for each owned network and volume that carries the label, each rendering of it (rebuild.NetworkCandidates
// and VolumeCandidates, fewest settings first) is written into the file, the file is loaded and prepared as a
// deploy loads and prepares it, and the SDK's own hash of the object is compared with the label; the first
// rendering that is equal is kept. When none is, the object is written external, with a note, and the SDK never
// touches it. An object with no label is left alone by the SDK whatever it is declared as, and keeps the first.
func (d Deps) rebuildMatchingHashes(ctx context.Context, st Stack, in rebuild.StandaloneInput, beside stacks.WorkingCopy) (rebuild.Result, error) {
	in.Choice = map[string]int{}
	res, err := rebuild.Standalone(in)
	if err != nil {
		return rebuild.Result{}, &RebuildError{Reason: err.Error()}
	}
	changed := false
	for _, o := range res.Objects {
		if o.Treatment != rebuild.TreatmentOwned || (o.Kind != rebuild.KindNetwork && o.Kind != rebuild.KindVolume) {
			continue
		}
		label := hashLabel(in, o)
		if label == "" {
			continue
		}
		pick := o.Kind + ":" + o.Key
		matched := -1
		for i := range res.Variants[pick] {
			in.Choice[pick] = i
			trial, err := rebuild.Standalone(in)
			if err != nil {
				return rebuild.Result{}, &RebuildError{Reason: err.Error()}
			}
			got, err := hashOfObject(ctx, st, trial, o, beside)
			if err != nil {
				return rebuild.Result{}, err
			}
			if got == label {
				matched = i
				break
			}
		}
		in.Choice[pick] = matched
		changed = changed || matched != 0
	}
	if !changed {
		return res, nil
	}
	res, err = rebuild.Standalone(in)
	if err != nil {
		return rebuild.Result{}, &RebuildError{Reason: err.Error()}
	}
	return res, nil
}

// hashLabel is the config hash the host holds for an owned network or volume: empty for an object made by a Compose
// that stamped none.
func hashLabel(in rebuild.StandaloneInput, o rebuild.Object) string {
	switch o.Kind {
	case rebuild.KindNetwork:
		for _, n := range in.Networks {
			if n.Name == o.Name {
				return n.Labels[stacks.LabelComposeConfigHash]
			}
		}
	case rebuild.KindVolume:
		for _, v := range in.Volumes {
			if v.Name == o.Name {
				return v.Labels[stacks.LabelComposeConfigHash]
			}
		}
	}
	return ""
}

// hashOfObject is the hash the SDK would compute for an object of a rebuilt file: the file is loaded the way
// every deploy loads it (stacks.Deployable) and prepared the way a standalone deploy prepares it
// (stacks.PrepareStandalone), so the name the SDK gives the object, and whatever else preparation sets, is what is
// hashed. TierPlan's own labels on the object are custom labels and are not in the hash. A loaded project that does
// not hold the object (the default network a file leaves implicit is always held) is a rebuild that cannot be used.
func hashOfObject(ctx context.Context, st Stack, res rebuild.Result, o rebuild.Object, beside stacks.WorkingCopy) (string, error) {
	copy := composeBeside(beside, res.YAML)
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: st.Name, Target: stacks.TargetStandalone, Copy: copy})
	if err != nil {
		var refused *stacks.NotDeployableError
		if errors.As(err, &refused) && len(refused.Problems) > 0 {
			return "", &RebuildError{Reason: refused.Problems[0].Message}
		}
		return "", &RebuildError{Reason: err.Error()}
	}
	project := loaded.Project
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, stacks.NewFileSet(copy.Files), nil)
	if err != nil {
		return "", &RebuildError{Reason: err.Error()}
	}
	if err := stacks.PrepareStandalone(project, st.ID, res.Pins, objs); err != nil {
		return "", &RebuildError{Reason: err.Error()}
	}
	switch o.Kind {
	case rebuild.KindNetwork:
		n, ok := project.Networks[o.Key]
		if !ok {
			return "", &RebuildError{Reason: "the network " + o.Name + " is not in the rebuilt file"}
		}
		return dockerx.ComposeNetworkHash(&n)
	default:
		v, ok := project.Volumes[o.Key]
		if !ok {
			return "", &RebuildError{Reason: "the volume " + o.Name + " is not in the rebuilt file"}
		}
		return dockerx.ComposeVolumeHash(v)
	}
}

// readStandaloneForRebuild reads, from the host, the containers of a Compose project with their images, and the
// networks and volumes that carry the project's label with the detail the file needs of them. Each container's
// inspect document is made safe before the rebuild sees it: what the image supplies is taken out, as the
// comparison of a running service takes it out, and a caller who may not reveal gets every value masked. The
// string is the fingerprint of the containers the project held when they were listed.
func (d Deps) readStandaloneForRebuild(ctx context.Context, envID, node, project string, reveal bool) (rebuild.StandaloneInput, string, error) {
	in := rebuild.StandaloneInput{Project: project}
	fingerprint := ""
	err := d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
		containers, e := c.ComposeContainers(ctx, project)
		if e != nil {
			return e
		}
		fingerprint = standaloneFingerprint(containers)
		reader := &hostReader{host: c, images: map[string]*image.InspectResponse{}}
		for _, cc := range containers {
			insp, img, ok, e := reader.read(ctx, cc)
			if e != nil {
				return e
			}
			if insp.ID == "" || insp.Config == nil {
				continue // the container is gone, or is not one a service made
			}
			if !ok {
				img = image.InspectResponse{}
			}
			insp = withoutImageDefaults(insp, img)
			if !reveal {
				if insp, e = maskedInspect(insp); e != nil {
					return e
				}
			}
			in.Containers = append(in.Containers, rebuild.StandaloneContainer{Inspect: insp, Image: img})
		}
		nets, e := c.NetworksWithLabel(ctx, stacks.LabelComposeProject, project)
		if e != nil {
			return e
		}
		for _, n := range nets {
			det, _, e := c.NetworkDetail(ctx, n.ID, false)
			if e != nil {
				return e
			}
			in.Networks = append(in.Networks, networkInfo(n, det))
		}
		vols, e := c.VolumesWithLabel(ctx, stacks.LabelComposeProject, project)
		if e != nil {
			return e
		}
		for _, v := range vols {
			if v.Anonymous {
				continue
			}
			det, _, e := c.VolumeDetail(ctx, v.Name)
			if e != nil {
				return e
			}
			in.Volumes = append(in.Volumes, rebuild.VolumeInfo{Name: v.Name, Driver: det.Driver, Labels: det.Labels, Options: det.Options})
		}
		return nil
	})
	return in, fingerprint, err
}

// withoutImageDefaults is a container's inspect document without the environment entries, the command and the
// entrypoint its image supplies. The comparison of a running service takes the same out, so the rebuild is the
// same, and a masked document does not show the image's own settings as hidden values.
func withoutImageDefaults(insp container.InspectResponse, img image.InspectResponse) container.InspectResponse {
	def := stacks.ImageDefaultsOf(img)
	cfg := *insp.Config
	cfg.Env = slices.DeleteFunc(slices.Clone(cfg.Env), func(e string) bool { return slices.Contains(def.Env, e) })
	if slices.Equal(cfg.Cmd, def.Cmd) {
		cfg.Cmd = nil
	}
	if slices.Equal(cfg.Entrypoint, def.Entrypoint) {
		cfg.Entrypoint = nil
	}
	insp.Config = &cfg
	return insp
}

// maskedInspect is a container's inspect document with every value a caller without secrets.reveal may not read
// replaced, by the same code that masks the Inspect tab.
func maskedInspect(insp container.InspectResponse) (container.InspectResponse, error) {
	raw, err := json.Marshal(insp)
	if err != nil {
		return insp, fmt.Errorf("stackops: encode the inspect document of container %s: %w", insp.Name, err)
	}
	masked, _, err := redact.MaskContainerInspect(raw, false)
	if err != nil {
		return insp, fmt.Errorf("stackops: mask the inspect document of container %s: %w", insp.Name, err)
	}
	var out container.InspectResponse
	if err := json.Unmarshal(masked, &out); err != nil {
		return insp, fmt.Errorf("stackops: read the masked inspect document of container %s: %w", insp.Name, err)
	}
	return out, nil
}
