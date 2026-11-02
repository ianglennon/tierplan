// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// StandaloneNode is the ID of the one node of a standalone environment: the environment's own ID for the
// server's local socket, the agent's ID otherwise. Reads and writes of a standalone stack are routed to it
// by ID (envs.Aggregator.OnNode, WriteOnNode), so a host that shares a name with another is never mistaken
// for it.
func (d Deps) StandaloneNode(ctx context.Context, envID string) (string, error) {
	if d.Aggregator == nil {
		return "", errors.New("stackops: no aggregator to read the host with")
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return "", err
	}
	if len(agg.Nodes) == 0 {
		return "", envs.ErrNodeUnavailable
	}
	return agg.Nodes[0].ID, nil
}

// nodeReader reads through the engine of one node of the environment.
func (d Deps) nodeReader(envID, nodeID string) reader {
	return func(ctx context.Context, fn func(*dockerx.Client) error) error {
		return d.Aggregator.OnNode(ctx, envID, nodeID, fn)
	}
}

// standaloneLive is what a standalone host holds under a stack's Compose project, read once.
type standaloneLive struct {
	Containers []dockerx.ComposeContainer
	Networks   []dockerx.NetworkRow
	Volumes    []dockerx.VolumeRow
}

// readStandalone reads the containers (stopped ones too), networks and volumes that carry the Compose
// project label of the stack, from one node. It is a read.
func (d Deps) readStandalone(ctx context.Context, envID, nodeID, project string) (standaloneLive, error) {
	var out standaloneLive
	err := d.Aggregator.OnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		var e error
		if out.Containers, e = c.ComposeContainers(ctx, project); e != nil {
			return e
		}
		if out.Networks, e = c.NetworksWithLabel(ctx, stacks.LabelComposeProject, project); e != nil {
			return e
		}
		out.Volumes, e = c.VolumesWithLabel(ctx, stacks.LabelComposeProject, project)
		return e
	})
	return out, err
}

// standaloneItems are the pin items of a loaded project: its services that have an image, by the name in
// the compose file, in name order.
func standaloneItems(p *types.Project) []pinItem {
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]pinItem, 0, len(names))
	for _, n := range names {
		if img := p.Services[n].Image; img != "" {
			items = append(items, pinItem{Name: n, Short: n, AsWritten: img})
		}
	}
	return items
}

// imageIDRe matches an image ID, as the engine shows one where a reference once was.
var imageIDRe = regexp.MustCompile(`^(sha256:)?[a-f0-9]{12,64}$`)

// standaloneLivePins are the images the host's containers run and the text each was deployed from, by
// service: the tierplan.stack.image label and the reference the container was created with. A container that
// records no text, or whose reference the engine has replaced with an image ID, says nothing about what was
// deployed, and the stack's last good revision answers for the service instead.
func standaloneLivePins(cs []dockerx.ComposeContainer) map[string]stacks.LivePin {
	sorted := slices.Clone(cs)
	slices.SortFunc(sorted, func(a, b dockerx.ComposeContainer) int { return strings.Compare(a.Name, b.Name) })
	out := map[string]stacks.LivePin{}
	for _, c := range sorted {
		written := c.Labels[stacks.LabelStackImage]
		if c.Service == "" || written == "" || c.Image == "" || imageIDRe.MatchString(c.Image) {
			continue
		}
		if _, have := out[c.Service]; !have {
			out[c.Service] = stacks.LivePin{AsWritten: written, Image: c.Image}
		}
	}
	return out
}

// standaloneCurrent is the image each service's container runs now, for the review's table.
func standaloneCurrent(cs []dockerx.ComposeContainer) map[string]string {
	out := map[string]string{}
	for _, c := range cs {
		if c.Service != "" && c.Image != "" {
			if _, have := out[c.Service]; !have {
				out[c.Service] = c.Image
			}
		}
	}
	return out
}

// standalonePins are the references a project is prepared with: the pin of each service that has one, and
// for a service without one (its image could not be looked up, which the review reports as a problem) the
// image as written, so a hash can still be made of it.
func standalonePins(items []pinItem, pinned PinOutcome) map[string]string {
	out := make(map[string]string, len(items))
	for _, it := range items {
		if p, ok := pinned.Pins[it.Name]; ok {
			out[it.Name] = p.Ref
			continue
		}
		out[it.Name] = it.AsWritten
	}
	return out
}

// standaloneDiff is what a deploy of a prepared project would do to the containers that exist, by comparing
// each service's config hash with the label its containers carry (STK-06).
type standaloneDiff struct {
	// Added are the services with no container, Changed the ones with a container whose hash differs
	// from the service's now, Unchanged the ones whose containers all match, and Removed the services
	// the containers belong to that the project no longer declares (the deploy removes them).
	Added, Changed, Unchanged, Removed []string
	// Forced are the services whose containers carry the hash the service has now but whose running
	// configuration differs from it (a change made by hand on the host): the SDK compares labels only and
	// would leave them as they are, so the deploy has them recreated (compareStandalone).
	Forced []string
	// Carried are the services the SDK recreates because a service they share a namespace or volumes with
	// is recreated (stacks.RecreateCascade).
	Carried []string
	// Hashes are the desired hash of each service.
	Hashes map[string]string
	// Counts is how many containers each removed service has.
	Orphans map[string]int
}

// containersByService groups the containers of a project by service, each group in the order of the
// containers' names, which is the order the SDK takes the first container of a service in.
func containersByService(containers []dockerx.ComposeContainer) map[string][]dockerx.ComposeContainer {
	byService := map[string][]dockerx.ComposeContainer{}
	for _, c := range containers {
		byService[c.Service] = append(byService[c.Service], c)
	}
	for _, list := range byService {
		slices.SortFunc(list, func(a, b dockerx.ComposeContainer) int { return strings.Compare(a.Name, b.Name) })
	}
	return byService
}

// containerIDs are the IDs of each service's containers in the order of their names.
func containerIDs(byService map[string][]dockerx.ComposeContainer) map[string][]string {
	out := make(map[string][]string, len(byService))
	for svc, list := range byService {
		for _, c := range list {
			out[svc] = append(out[svc], c.ID)
		}
	}
	return out
}

// recreated are the services whose containers the SDK recreates for their own sake, by their hash or by force
// (not the ones it recreates because another service is), each once, in name order.
func (d standaloneDiff) recreated() []string {
	out := slices.Concat(d.Changed, d.Forced)
	slices.Sort(out)
	return slices.Compact(out)
}

// diffStandalone classifies the services of a prepared project against the containers of its Compose
// project. A service that shares another's namespace or volumes is hashed with the reference resolved to
// the other's container, as the SDK hashes it when it creates the container (stacks.ResolveServiceRefs).
func diffStandalone(p *types.Project, containers []dockerx.ComposeContainer) (standaloneDiff, error) {
	diff := standaloneDiff{Hashes: map[string]string{}, Orphans: map[string]int{}}
	byService := containersByService(containers)
	ids := containerIDs(byService)
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h, err := dockerx.ServiceHash(stacks.ResolveServiceRefs(p.Services[n], ids))
		if err != nil {
			return diff, err
		}
		diff.Hashes[n] = h
		have := byService[n]
		switch {
		case len(have) == 0:
			diff.Added = append(diff.Added, n)
		case slices.ContainsFunc(have, func(c dockerx.ComposeContainer) bool { return c.ConfigHash != h }):
			diff.Changed = append(diff.Changed, n)
		default:
			diff.Unchanged = append(diff.Unchanged, n)
		}
	}
	orphans := make([]string, 0)
	for svc, have := range byService {
		if _, declared := p.Services[svc]; !declared && svc != "" {
			orphans = append(orphans, svc)
			diff.Orphans[svc] = len(have)
		}
	}
	sort.Strings(orphans)
	diff.Removed = orphans
	return diff, nil
}

// missingNetworks and missingVolumes are the project's own networks and volumes (not external) that the
// host does not hold yet, by name, in name order: what the deploy creates.
func missingNetworks(p *types.Project, live []dockerx.NetworkRow) []string {
	have := map[string]bool{}
	for _, n := range live {
		have[n.Name] = true
	}
	var out []string
	for _, n := range p.Networks {
		if !bool(n.External) && n.Name != "" && !have[n.Name] {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

func missingVolumes(p *types.Project, live []dockerx.VolumeRow) []string {
	have := map[string]bool{}
	for _, v := range live {
		have[v.Name] = true
	}
	var out []string
	for _, v := range p.Volumes {
		if !bool(v.External) && v.Name != "" && !have[v.Name] {
			out = append(out, v.Name)
		}
	}
	sort.Strings(out)
	return out
}
