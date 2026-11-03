// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"slices"

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
}

// PinnedInFile reports whether the workload's reference is a digest with no tag, which can never
// update because there is nothing to follow.
func (w Workload) PinnedInFile() bool { return w.Ref.Tag == "" && w.Ref.Digest != "" }

// Checkable reports whether the registry can be asked about the workload: it has a tag to follow.
func (w Workload) Checkable() bool { return w.Ref.Tag != "" }

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
	for _, c := range agg.Containers {
		id := c.Labels[labelSwarmServiceID]
		if id == "" || !taskIsLive(c.State) {
			continue
		}
		nodes[id] = append(nodes[id], c.NodeID)
	}
	complete := len(agg.Unreachable) == 0 && len(agg.NotReporting) == 0
	out := make([]Workload, len(ws))
	copy(out, ws)
	for i, w := range out {
		if w.Kind != "service" {
			continue
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
