// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/registry"
)

// How a container or a service was matched to an image (D-28, P4 D-41).
const (
	matchedByImageID = "image_id"
	matchedByTag     = "tag"
	matchedByDigest  = "digest"
)

// The state a service's spec reads when no task runs it and none is wanted. The spec still
// names the image, so it still blocks removing it (D-28).
const serviceNoTasks = "no_tasks"

// ImageUsedByItem is one container or swarm service that uses an image.
type ImageUsedByItem struct {
	Kind      string `json:"kind" enum:"container,service" doc:"container for a container on a node, service for a swarm service"`
	ID        string `json:"id" doc:"The container's or the service's ID"`
	Name      string `json:"name" doc:"The container name or the service name"`
	NodeID    string `json:"nodeId" doc:"The ID of the node a container runs on; empty for a service, which can run on any node"`
	Node      string `json:"node" doc:"The display name of that node; empty for a service"`
	State     string `json:"state" doc:"A container's state; for a service running, starting or no_tasks (its spec names the image but no task runs it); empty when the cluster read did not have it"`
	MatchedBy string `json:"matchedBy" enum:"image_id,tag,digest" doc:"image_id for a container, which is matched by the image ID on its own node; digest for a service pinned to a digest the image has; tag for a service matched by its repository and tag"`
}

// ImageUsedByBody is everything that uses one image, containers then services, each by name.
type ImageUsedByBody struct {
	Items            []ImageUsedByItem `json:"items" doc:"Containers, running or stopped, on the nodes that hold the image, then swarm services whose spec names it, even with no task"`
	UsedByContainers int               `json:"usedByContainers" doc:"How many of the items are containers"`
	UsedByServices   int               `json:"usedByServices" doc:"How many of the items are services"`
	NotReporting     []string          `json:"notReporting" doc:"Nodes that should have answered and did not, so the containers may be incomplete"`
}

// mergeKey is what makes two node copies of an image one row of the merged list (D-16): the
// image ID, which on the containerd store is the index digest and so equal on every
// architecture, and on the classic store the platform's config digest, so mixed
// architectures stay apart. With no ID the first repo digest stands in, else the sorted tags.
func mergeKey(row dockerx.ImageRow) string {
	switch {
	case row.ID != "":
		return row.ID
	case len(row.RepoDigests) > 0:
		return row.RepoDigests[0]
	}
	tags := slices.Clone(row.RepoTags)
	slices.Sort(tags)
	return strings.Join(tags, ",")
}

// imageServiceUse is a swarm service whose spec names an image, and how it was matched.
type imageServiceUse struct {
	ref       dockerx.ServiceRef
	row       dockerx.ServiceRow
	known     bool
	matchedBy string
}

// imageUsage is what uses the images of an environment: containers per node and image ID,
// and the swarm services whose specs name an image, per merge key.
type imageUsage struct {
	containers map[[2]string][]envs.ContainerRow
	services   map[string][]imageServiceUse
}

// imageUsageOf works out the users of every image in an aggregate. A container counts for
// the image it was created from on its own node, whether it runs or not. A service counts
// from its spec, from the cluster read of a swarm whatever age it has, and even with no
// task: pinned to a digest it matches the image whose repo digests hold it, even after the
// tag moved; otherwise, or when no image holds the digest, it matches by normalised
// repository and tag (D-28, P4 D-41, D-46). The engine's own per-image container count is
// never used: a list does not compute it by default.
func imageUsageOf(ctx context.Context, clusters *envs.Clusters, envID string, agg envs.Aggregate) imageUsage {
	if clusters == nil || agg.Target.Probe.Type != dockerx.TypeSwarmManager {
		return imageUsageFrom(agg, nil, nil)
	}
	// A failed read leaves what the last good one held.
	st, _ := clusters.State(ctx, envID)
	return imageUsageFrom(agg, st.ServiceRefs, st.Read.Services)
}

// imageUsageFrom is imageUsageOf over the service specs and rows already read: refs are
// the specs, and services their task counts, which only word a state and may be empty. A
// caller that must not trust a cache reads the specs itself and passes them here.
func imageUsageFrom(agg envs.Aggregate, refs []dockerx.ServiceRef, services []dockerx.ServiceRow) imageUsage {
	u := imageUsage{containers: map[[2]string][]envs.ContainerRow{}, services: map[string][]imageServiceUse{}}
	for _, c := range agg.Containers {
		if c.ImageID != "" {
			k := [2]string{c.NodeID, c.ImageID}
			u.containers[k] = append(u.containers[k], c)
		}
	}
	if len(refs) == 0 {
		return u
	}
	byDigest, byTag := map[string][]string{}, map[string][]string{}
	add := func(index map[string][]string, name, key string) {
		if !slices.Contains(index[name], key) {
			index[name] = append(index[name], key)
		}
	}
	for _, im := range agg.Images {
		key := mergeKey(im.ImageRow)
		for _, d := range im.RepoDigests {
			if ref, err := registry.ParseRef(d); err == nil && ref.Digest != "" {
				add(byDigest, ref.Repo()+"@"+ref.Digest, key)
			}
		}
		for _, t := range im.RepoTags {
			if t == "<none>" || t == "<none>:<none>" {
				continue
			}
			if ref, err := registry.ParseRef(t); err == nil && ref.Tag != "" {
				add(byTag, ref.Repo()+":"+ref.Tag, key)
			}
		}
	}
	rows := map[string]dockerx.ServiceRow{}
	for _, row := range services {
		rows[row.ID] = row
	}
	for _, ref := range refs {
		spec, err := registry.ParseRef(ref.Image)
		if err != nil {
			continue
		}
		use := imageServiceUse{ref: ref}
		use.row, use.known = rows[ref.ID]
		var keys []string
		if spec.Digest != "" {
			keys = byDigest[spec.Repo()+"@"+spec.Digest]
			use.matchedBy = matchedByDigest
		}
		if len(keys) == 0 && spec.Tag != "" {
			keys = byTag[spec.Repo()+":"+spec.Tag]
			use.matchedBy = matchedByTag
		}
		for _, key := range keys {
			u.services[key] = append(u.services[key], use)
		}
	}
	return u
}

// containersOf are the containers of one node that were created from an image ID.
func (u imageUsage) containersOf(nodeID, imageID string) []envs.ContainerRow {
	return u.containers[[2]string{nodeID, imageID}]
}

// servicesOf are the services whose specs name the image of a row.
func (u imageUsage) servicesOf(row dockerx.ImageRow) []imageServiceUse {
	return u.services[mergeKey(row)]
}

// serviceUseState words a service's state from its task counts: running when any task
// runs, no_tasks when none runs and none is wanted, starting otherwise. Empty when the
// cluster read did not have the service.
func serviceUseState(use imageServiceUse) string {
	switch {
	case !use.known:
		return ""
	case use.row.Running > 0:
		return "running"
	case use.row.Desired == 0:
		return serviceNoTasks
	}
	return "starting"
}

// usedBy lists what uses one image on the given nodes, containers then services, each by
// name then ID.
func (u imageUsage) usedBy(rows []envs.ImageRow) ImageUsedByBody {
	body := ImageUsedByBody{Items: []ImageUsedByItem{}}
	if len(rows) == 0 {
		return body
	}
	for _, r := range rows {
		for _, c := range u.containersOf(r.NodeID, r.ID) {
			body.Items = append(body.Items, ImageUsedByItem{
				Kind: "container", ID: c.ID, Name: c.Name, NodeID: c.NodeID, Node: c.Node, State: c.State, MatchedBy: matchedByImageID,
			})
		}
	}
	body.UsedByContainers = len(body.Items)
	slices.SortFunc(body.Items, func(a, b ImageUsedByItem) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Node, b.Node), strings.Compare(a.ID, b.ID))
	})
	var services []ImageUsedByItem
	for _, use := range u.servicesOf(rows[0].ImageRow) {
		services = append(services, ImageUsedByItem{
			Kind: "service", ID: use.ref.ID, Name: use.ref.Name, State: serviceUseState(use), MatchedBy: use.matchedBy,
		})
	}
	slices.SortFunc(services, func(a, b ImageUsedByItem) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	body.UsedByServices = len(services)
	body.Items = append(body.Items, services...)
	return body
}

// RunningRef is a container that runs an image, for the Running in marker of a tag.
type RunningRef struct {
	Kind   string
	ID     string
	Name   string
	NodeID string
	Node   string
}

// digestKey names a digest of one repository, the way a reference to it is normalised:
// docker.io/library/nginx@sha256:...
func digestKey(repo, digest string) string { return repo + "@" + digest }

// runnable says whether a container's state is one where its image is in use as a running
// one. A created, exited, dead or removing container is not.
func runnable(state string) bool {
	return state == "running" || state == "restarting" || state == "paused"
}

// runningDigests lists, by repository and digest, the running containers of an aggregate
// (D-22, Pitfall 5). A container counts for every repository digest of the image it was
// created from on its own node, and for the digest its image name pins: swarm tasks run
// repo:tag@sha256:... Every reference is normalised with registry.ParseRef, so the short form
// docker@sha256:... and docker.io/library/docker@sha256:... are one key. A tag's text is never
// used, so a tag that has moved still shows where its old image runs.
func runningDigests(agg envs.Aggregate) map[string][]RunningRef {
	repoDigests := map[[2]string][]string{}
	for _, im := range agg.Images {
		repoDigests[[2]string{im.NodeID, im.ID}] = im.RepoDigests
	}
	out := map[string][]RunningRef{}
	for _, c := range agg.Containers {
		if !runnable(c.State) {
			continue
		}
		ref := RunningRef{Kind: "container", ID: c.ID, Name: c.Name, NodeID: c.NodeID, Node: c.Node}
		seen := map[string]bool{}
		add := func(s string) {
			r, err := registry.ParseRef(s)
			if err != nil || r.Digest == "" {
				return
			}
			key := digestKey(r.Repo(), r.Digest)
			if !seen[key] {
				seen[key] = true
				out[key] = append(out[key], ref)
			}
		}
		for _, d := range repoDigests[[2]string{c.NodeID, c.ImageID}] {
			add(d)
		}
		add(c.Image)
	}
	for _, refs := range out {
		slices.SortFunc(refs, func(a, b RunningRef) int {
			return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Node, b.Node), strings.Compare(a.ID, b.ID))
		})
	}
	return out
}

// isRunning reports whether a tag runs somewhere: its index digest, or the digest of any of
// its platforms, is one a running container holds, in the same repository.
func isRunning(running map[string][]RunningRef, repo, digest string, platformDigests []string) bool {
	if digest != "" && len(running[digestKey(repo, digest)]) > 0 {
		return true
	}
	return slices.ContainsFunc(platformDigests, func(d string) bool { return d != "" && len(running[digestKey(repo, d)]) > 0 })
}
