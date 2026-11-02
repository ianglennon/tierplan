// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"slices"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// The engine's placeholders for an image that has no name. They are the same words the Images page
// reads as "no name", and a name that is one of them is not a name.
const (
	noneTag    = "<none>"
	noneTagTag = "<none>:<none>"
	noneDigest = "<none>@<none>"
)

// hasRealName reports whether any of names is a name and not an empty string or a placeholder.
func hasRealName(names []string, placeholders ...string) bool {
	return slices.ContainsFunc(names, func(n string) bool {
		return n != "" && !slices.Contains(placeholders, n)
	})
}

// shortImageID is how an image is named in the plan: its ID without sha256:, twelve characters.
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(len(id), 12)]
}

// ClassifyImage says what the janitor makes of one image on one node at now. It removes dangling
// images in Docker's strict sense and nothing more (D-07, JAN-05): an image with no tag and no
// digest reference, that no container on its node uses in any state, that no swarm service spec
// names, and that is old enough. The engine's own dangling filter is never the decision: an image
// pulled by digest carries a digest and no tag, and a service scaled to zero still needs it.
//
// listed is false when the image is simply not the janitor's business. The first rule that matches
// wins:
//
//  1. a real tag or any digest: not listed (a tagged image is somebody's; a digest-pulled one is
//     what a swarm service runs)
//  2. a container on this node, in any state, was made from it: not listed
//  3. a swarm whose service specs were not read (servicesRead false): cant_tell (services_unread),
//     because a service scaled to zero would still need it
//  4. a service spec names it: not listed
//  5. labelled tierplan.keep=true: protected (kept_label)
//  6. created less than the grace period ago: grace (younger_than_grace); at exactly the grace
//     period it is removable
//  7. otherwise removable (dangling_image), with its age and size
//
// usedOnNode and serviceRef are the answers to rules 2 and 4 for this image; servicesRead is true
// for an environment that is not a swarm.
func ClassifyImage(img Image, usedOnNode, serviceRef, servicesRead bool, p Policy, now time.Time) (Item, bool) {
	if hasRealName(img.RepoTags, noneTag, noneTagTag) || hasRealName(img.RepoDigests, noneDigest) {
		return Item{}, false
	}
	if usedOnNode {
		return Item{}, false
	}
	item := Item{
		Type: KindImage, NodeID: img.NodeID, Node: img.Node, Ref: img.ID, Name: shortImageID(img.ID), Size: img.Size,
	}
	if item.Size < 0 {
		item.Size = -1
	}
	if !img.Created.IsZero() {
		item.Age = max(now.Sub(img.Created), 0)
		item.Reason.Age = item.Age
	}
	if !servicesRead {
		item.Class, item.Reason.Code = ClassCantTell, ReasonServicesUnread
		return item, true
	}
	if serviceRef {
		return Item{}, false
	}
	if img.Labels[stacks.LabelKeep] == "true" {
		item.Class, item.Reason.Code = ClassProtected, ReasonKeptLabel
		return item, true
	}
	if remaining := img.Created.Add(p.grace()).Sub(now); remaining > 0 {
		item.Class, item.Reason.Code, item.Reason.Remaining = ClassGrace, ReasonYoungerThanGrace, remaining
		return item, true
	}
	item.Class, item.Reason.Code = ClassRemovable, ReasonDanglingImage
	return item, true
}

// ImageNamedBySpec reports whether a swarm service spec names the image: by its ID, or by a
// repository@digest whose digest is the image's ID (the containerd image store makes an image's ID
// the digest of its index). A spec that names a tag the image no longer has does not name it: a
// swarm would pull that tag again.
func ImageNamedBySpec(specs []string, id string) bool {
	return slices.ContainsFunc(specs, func(spec string) bool {
		return spec == id || strings.HasSuffix(spec, "@"+id) || spec == strings.TrimPrefix(id, "sha256:")
	})
}

// addImages adds the images of the snapshot to the plan. A container on a node counts against an
// image of that node only: the same image ID on two nodes is two items, judged per node.
func addImages(plan *Plan, s Snapshot, p Policy, now time.Time) {
	used := make(map[[2]string]bool, len(s.Containers))
	for _, c := range s.Containers {
		if c.ImageID != "" {
			used[[2]string{c.NodeID, c.ImageID}] = true
		}
	}
	servicesRead := !s.Swarm || s.ServicesRead
	for _, img := range s.Images {
		item, listed := ClassifyImage(img, used[[2]string{img.NodeID, img.ID}], ImageNamedBySpec(s.ServiceImages, img.ID), servicesRead, p, now)
		switch {
		case !listed:
		case item.Class == ClassRemovable:
			plan.Items = append(plan.Items, item)
		default:
			plan.NotRemoved = append(plan.NotRemoved, item)
		}
	}
}
