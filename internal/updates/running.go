// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"maps"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// Workload is one thing an image runs in: a swarm service now, a standalone container later. It is
// built from a live read of Docker on every call and never stored (D-04).
type Workload struct {
	EnvID string
	// Kind is "service" or "container".
	Kind string
	// ID and Name are the service's or container's own.
	ID, Name string
	// Stack is the stack it belongs to, empty for a workload made on its own.
	Stack string
	// Written is the reference as the user wrote it: the tierplan.stack.image label when present, else
	// the com.docker.stack.image label, else the spec image.
	Written string
	// Ref is the reference to check: the repository and tag of Written, the digest dropped. A
	// reference with a digest and no tag keeps its digest and has no tag, and is never checked.
	Ref registry.Ref
	// RunningDigest is the digest the workload runs, from its spec and never from an image ID; empty
	// when the spec names none.
	RunningDigest string
	// Platforms are the platforms the workload runs on and NodeIDs the nodes it runs on; the per-platform
	// judgement fills them in.
	Platforms, NodeIDs []string
	Labels             map[string]string
	// Task is true for a task container of a swarm service: its update is the service's.
	Task bool
	// ImageID is the ID of the image a container runs and RepoDigests the repo@digest names that image has
	// on the node, empty for a service.
	ImageID     string
	RepoDigests []string
	// LocalStore says, for a container, what its image tells about where it came from: LocalClassic when
	// the image has no repo digest at all (a local build on the classic image store), LocalContainerd when
	// its only digest is its own ID (a pulled image, or a local build, on the containerd store, which the
	// image alone cannot tell apart), empty otherwise.
	LocalStore string
}

// The LocalStore values.
const (
	LocalClassic    = "classic"
	LocalContainerd = "containerd"
)

// PinnedInFile reports whether the workload's reference is a digest with no tag, which can never
// update because there is nothing to follow.
func (w Workload) PinnedInFile() bool { return w.Ref.Tag == "" && w.Ref.Digest != "" }

// Checkable reports whether the registry can be asked about the workload: it has a tag to follow, and
// it is not an image the classic image store holds with no repo digest, which was built here and never
// pushed or pulled (D-03).
func (w Workload) Checkable() bool { return w.Ref.Tag != "" && w.LocalStore != LocalClassic }

// ServiceWorkloads turns the services of a swarm read into workloads. The TierPlan agent service and
// the services of the job modes are not workloads: the agent updates through its own mechanism, and a
// job runs once and is gone (RESEARCH Pitfall 14). A service at scale 0 is still one. A service whose
// image cannot be read as a reference is left out. EnvID is the caller's to set.
func ServiceWorkloads(rows []dockerx.ServiceRow) []Workload {
	var out []Workload
	for _, r := range rows {
		if r.Agent || r.Mode == "replicated-job" || r.Mode == "global-job" {
			continue
		}
		spec, err := registry.ParseRef(r.Image)
		if err != nil {
			continue
		}
		written := r.Image
		if v := r.Labels[stacks.LabelStackImage]; v != "" {
			written = v
		} else if v := r.Labels[stacks.LabelSwarmImage]; v != "" {
			written = v
		}
		ref, err := registry.ParseRef(written)
		if err != nil {
			written, ref = r.Image, spec
		}
		if ref.Tag != "" && ref.Digest != "" {
			// The digest is what the workload runs, not what to ask about: ask for the tag.
			if bare, err := registry.ParseRef(ref.Repo() + ":" + ref.Tag); err == nil {
				ref = bare
			}
		}
		out = append(out, Workload{
			Kind: "service", ID: r.ID, Name: r.Name, Stack: r.Stack, Written: written, Ref: ref,
			RunningDigest: spec.Digest, Labels: r.Labels,
		})
	}
	return out
}

// labelSwarmServiceID is the label Docker puts on every task container with the ID of its service.
const labelSwarmServiceID = "com.docker.swarm.service.id"

// WithPlatforms sets the NodeIDs and Platforms of each service of ws from the aggregate of its
// environment (D-02): the nodes its task containers run on, and "linux/" plus the architecture of each
// of those nodes, sorted and distinct. A task container counts while it runs or is about to; the
// exited ones a node keeps for a while are history.
//
// A service whose live task containers carry tierplan.updates=ignore (a compose file's `labels:` end up on the
// containers, not on the service) also carries that label in its own Labels, a copy of the map so the row the
// swarm cache holds is never written to, so Ignores.Match reads the label from one place (D-16).
//
// Platforms are left empty, and the verdict compares index digests, whenever the platforms the service
// runs on cannot be told for certain: no task container is seen yet, a node it runs on has not
// reported its architecture, or a node did not report at all and may be running tasks that are not
// seen. A service is never judged up to date on part of its platforms.
func WithPlatforms(ws []Workload, agg envs.Aggregate) []Workload {
	arch := make(map[string]string, len(agg.Nodes))
	for _, n := range agg.Nodes {
		arch[n.ID] = n.Arch
	}
	nodes := map[string][]string{} // service ID -> node IDs
	ignoring := map[string]bool{}  // service ID -> a live task container asks for its updates to be ignored
	for _, c := range agg.Containers {
		id := c.Labels[labelSwarmServiceID]
		if id == "" || !taskIsLive(c.State) {
			continue
		}
		nodes[id] = append(nodes[id], c.NodeID)
		if c.Labels[stacks.LabelUpdates] == labelIgnoreValue {
			ignoring[id] = true
		}
	}
	complete := len(agg.Unreachable) == 0 && len(agg.NotReporting) == 0
	out := make([]Workload, len(ws))
	copy(out, ws)
	for i, w := range out {
		if w.Kind != "service" {
			continue
		}
		if ignoring[w.ID] && w.Labels[stacks.LabelUpdates] != labelIgnoreValue {
			labels := make(map[string]string, len(w.Labels)+1)
			maps.Copy(labels, w.Labels)
			labels[stacks.LabelUpdates] = labelIgnoreValue
			out[i].Labels = labels
		}
		ids := nodes[w.ID]
		slices.Sort(ids)
		out[i].NodeIDs = slices.Compact(ids)
		out[i].Platforms = nil
		if len(ids) == 0 || !complete {
			continue
		}
		var ps []string
		known := true
		for _, id := range out[i].NodeIDs {
			a := arch[id]
			if a == "" {
				known = false
				break
			}
			ps = append(ps, "linux/"+a)
		}
		if known {
			slices.Sort(ps)
			out[i].Platforms = slices.Compact(ps)
		}
	}
	return out
}

// taskIsLive reports whether a container in this state is a task that runs or is starting.
func taskIsLive(state string) bool {
	return state == "running" || state == "restarting" || state == "created"
}

// labelAgent marks a TierPlan agent's container, which updates through its own mechanism (RESEARCH
// Pitfall 14) and is not a workload.
const labelAgent = "tierplan.agent"

// isImageID reports whether s is an image ID, which Docker shows in a container's list row, instead of
// the name the container was created with, once the tag has moved to another image (RESEARCH Pitfall 7).
// The name is then only in an inspect.
func isImageID(s string) bool {
	if rest, ok := strings.CutPrefix(s, "sha256:"); ok {
		return rest != ""
	}
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// needsImageText reports whether what c was created from is only to be had from an inspect: the list shows
// an image ID, so the tag moved. It gives the reference when no label does, and the digest when the store
// dropped the image.
func needsImageText(c envs.ContainerRow) bool {
	return isImageID(c.Image) && !isAgent(c)
}

func isAgent(c envs.ContainerRow) bool { return c.Labels[labelAgent] == "true" }

// ImageTextWanted lists the IDs of the containers of agg whose reference is only to be had from an
// inspect, in the order of the aggregate.
func ImageTextWanted(agg envs.Aggregate) []string {
	var ids []string
	for _, c := range agg.Containers {
		if needsImageText(c) {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// ContainerWorkloads turns the containers of an aggregate into workloads, each with its own node's view
// of its image (D-02, RESEARCH Pitfall 1). The TierPlan agent's containers are not workloads.
//
// The reference of a container is the label tierplan.stack.image when a stack wrote one, else the image
// text of the list row, else, when that is an image ID, text(container ID) from an inspect. A container
// whose reference is not known is still a workload, with no reference, and reads not_checked.
//
// The digest a container runs is the digest of the repo digest its image has, on its own node, for the
// repository the reference names. It is never the image ID: on the classic store that is a config digest
// no registry knows. A reference written with a digest and a tag keeps the digest it names when the image
// has no repo digest for it (a swarm task container is created that way).
func ContainerWorkloads(agg envs.Aggregate, text func(containerID string) (string, bool)) []Workload {
	arch := make(map[string]string, len(agg.Nodes))
	for _, n := range agg.Nodes {
		arch[n.ID] = n.Arch
	}
	type nodeImage struct{ node, id string }
	images := make(map[nodeImage]envs.ImageRow, len(agg.Images))
	for _, im := range agg.Images {
		images[nodeImage{im.NodeID, im.ID}] = im
	}
	var out []Workload
	for _, c := range agg.Containers {
		if isAgent(c) {
			continue
		}
		w := Workload{
			Kind: "container", ID: c.ID, Name: c.Name, Labels: c.Labels, ImageID: c.ImageID,
			NodeIDs: []string{c.NodeID}, Task: c.Labels[labelSwarmServiceID] != "",
			Stack: cmpOr(c.Labels[stacks.LabelSwarmNamespace], c.Labels[stacks.LabelComposeProject]),
		}
		if a := arch[c.NodeID]; a != "" {
			w.Platforms = []string{"linux/" + a}
		}
		im, haveImage := images[nodeImage{c.NodeID, c.ImageID}]
		if haveImage {
			w.RepoDigests = im.RepoDigests
			w.LocalStore = localStoreOf(im.ImageRow)
		}
		written := c.Labels[stacks.LabelStackImage]
		if written == "" {
			written = c.Image
			if isImageID(written) {
				written = ""
				if text != nil {
					if t, ok := text(c.ID); ok {
						written = t
					}
				}
			}
		}
		spec, err := registry.ParseRef(written)
		if written == "" || err != nil {
			out = append(out, w)
			continue
		}
		ref := spec
		if ref.Tag != "" && ref.Digest != "" {
			// The digest is what the container runs, not what to ask about: ask for the tag.
			if bare, err := registry.ParseRef(ref.Repo() + ":" + ref.Tag); err == nil {
				ref = bare
			}
		}
		w.Written, w.Ref = written, ref
		w.RunningDigest = runningDigest(im.RepoDigests, spec)
		if w.RunningDigest == "" && !haveImage && isImageID(c.Image) && text != nil {
			// The tag moved on this host and the store dropped the image the container still runs: the digest
			// it was started from is in the text it was created with.
			if t, ok := text(c.ID); ok {
				if r, err := registry.ParseRef(t); err == nil && r.Repo() == spec.Repo() {
					w.RunningDigest = r.Digest
				}
			}
		}
		out = append(out, w)
	}
	return out
}

// cmpOr is the first of a and b that is not empty.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// localStoreOf says whether an image is one that was never pulled: the classic store lists no repo digest
// for it, the containerd store lists its own ID as its only digest. The second is also what a pulled
// multi-platform image looks like, so it is only a hint, which a failed anonymous check turns into an
// answer (D-20).
func localStoreOf(im dockerx.ImageRow) string {
	if len(im.RepoDigests) == 0 {
		return LocalClassic
	}
	for _, d := range im.RepoDigests {
		if !strings.HasSuffix(d, "@"+im.ID) {
			return ""
		}
	}
	return LocalContainerd
}

// runningDigest is the digest of the repo digest, among those an image has, that is for the repository
// spec names; failing that, the digest spec itself names; failing that, none.
func runningDigest(repoDigests []string, spec registry.Ref) string {
	for _, d := range repoDigests {
		r, err := registry.ParseRef(d)
		if err == nil && r.Digest != "" && r.Repo() == spec.Repo() {
			return r.Digest
		}
	}
	return spec.Digest
}
