// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/rebuild"
)

// RebuildOptions say how a live rebuild is made.
type RebuildOptions struct {
	// Reveal is whether the caller may read the values the engine holds for the stack: an environment value, a
	// command, a health check command, a log option (secrets.reveal). Without it the specs are masked before the
	// file is made, so nothing the caller may not read is ever in it (D-47).
	Reveal bool
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
func checkRebuilt(ctx context.Context, st Stack, res rebuild.Result) error {
	check := stacks.Check(ctx, stacks.CheckInput{
		Name: st.Name, Target: st.Target,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: res.YAML}}},
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
	in := rebuild.SwarmInput{Namespace: st.Name}
	attached := map[string]bool{}
	for _, det := range read.services {
		if !own.owns(det.Stack, det.Labels) {
			continue
		}
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
		in.Secrets = append(in.Secrets, rebuild.ObjectInfo{ID: s.ID, Name: s.Name})
	}
	for _, c := range read.configs {
		in.Configs = append(in.Configs, rebuild.ObjectInfo{ID: c.ID, Name: c.Name})
	}

	res, err := rebuild.Swarm(in)
	if err != nil {
		return rebuild.Result{}, &RebuildError{Reason: err.Error()}
	}
	if err := checkRebuilt(ctx, st, res); err != nil {
		return rebuild.Result{}, err
	}
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
	in, err := d.readStandaloneForRebuild(ctx, envID, node, st.Name, o.Reveal)
	if err != nil {
		return rebuild.Result{}, err
	}
	if len(in.Containers) == 0 {
		return rebuild.Result{}, &RebuildError{Reason: "no container of " + st.Name + " is on the host"}
	}
	res, err := rebuild.Standalone(in)
	if err != nil {
		return rebuild.Result{}, &RebuildError{Reason: err.Error()}
	}
	if err := checkRebuilt(ctx, st, res); err != nil {
		return rebuild.Result{}, err
	}
	return res, nil
}

// readStandaloneForRebuild reads, from the host, the containers of a Compose project with their images, and the
// networks and volumes that carry the project's label with the detail the file needs of them. Each container's
// inspect document is made safe before the rebuild sees it: what the image supplies is taken out, as the
// comparison of a running service takes it out, and a caller who may not reveal gets every value masked.
func (d Deps) readStandaloneForRebuild(ctx context.Context, envID, node, project string, reveal bool) (rebuild.StandaloneInput, error) {
	in := rebuild.StandaloneInput{Project: project}
	err := d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
		containers, e := c.ComposeContainers(ctx, project)
		if e != nil {
			return e
		}
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
	return in, err
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
