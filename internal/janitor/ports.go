// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// The ports of the janitor. Nothing here is a Docker type: the server fills these from the
// engines and the planner judges them, so the rules are a table test and the package imports
// neither the engine client nor the HTTP layer.

// Node is one node the snapshot was read from.
type Node struct {
	ID, Name string
}

// Container is one container as a list reports it, and the node it is on.
type Container struct {
	ID, Name string
	NodeID   string
	Node     string
	// State is the engine's state: created, running, paused, restarting, removing, exited or dead.
	State   string
	ImageID string
	Created time.Time
	Labels  map[string]string
}

// Service is one swarm service the manager listed.
type Service struct {
	Name string
	// Global is true for a service with a task on every node, whose containers are grouped per
	// node and not per slot.
	Global bool
}

// Task is one swarm task the manager listed as wanted running.
type Task struct {
	// ID is the task's ID, ContainerID the container it runs, empty until it has one.
	ID, ContainerID string
	ServiceID       string
	NodeID          string
	DesiredState    string
	Slot            int
}

// Image is one image on one node as a list reports it.
type Image struct {
	ID, NodeID, Node string
	// RepoTags and RepoDigests are the image's names on this node. The engine's none placeholders
	// are ignored by the planner, so an untagged image may carry either them or nothing.
	RepoTags, RepoDigests []string
	Size                  int64
	Created               time.Time
	Labels                map[string]string
}

// StackFact is one TierPlan stack of the environment and how it holds the janitor back (D-10, D-19). A person can
// exclude it (Excluded), its own compose file can say x-tierplan: {keep: true} (Declared), and a stack whose
// definition could not be read (Unknown) is left alone because what it owns cannot be judged. Everything a stack
// owns is found by its ID (tierplan.stack.id), or by its name as the swarm namespace or the compose project of
// the object.
type StackFact struct {
	ID, Name string
	Excluded bool
	// Declared is true when the stack's compose file keeps the whole stack out of cleanup.
	Declared bool
	// Unknown is true when the stack's definition did not load.
	Unknown bool
}

// Snapshot is what the janitor reads of an environment at one moment.
type Snapshot struct {
	// Swarm is true for a swarm environment; ManagerRead is true when a manager answered the
	// questions about services and tasks.
	Swarm, ManagerRead bool
	// Nodes are the nodes that reported.
	Nodes []Node
	// NotReporting names the nodes that were expected and did not report. None of their objects
	// is in the snapshot.
	NotReporting []string
	// Unreachable are the same nodes with their IDs, which is how a ticked item names its node: a
	// run skips the items it finds on one of them without sending anything.
	Unreachable []Node
	Containers  []Container
	// Images are the images of every node that reported.
	Images []Image
	// ServiceImages are the image references and digests the swarm's service specs name, whatever
	// the services' tasks are doing: a service scaled to zero still names its image. ServicesRead
	// is true when they were read (always, for an environment that is not a swarm); on a swarm where
	// it is false no image is judged.
	ServiceImages []string
	ServicesRead  bool
	// Services are the swarm's services by ID, nil when no manager answered.
	Services map[string]Service
	// Tasks are the tasks the swarm wants running. Without a manager there are none.
	Tasks []Task
	// StackNames and StackIDs are the names and IDs of the TierPlan stacks of the environment, and
	// StacksRead is true when they were read. A compose-labelled container is judged by whether its
	// stack or project still exists, so while StacksRead is false none of them is removable.
	StackNames, StackIDs map[string]bool
	StacksRead           bool
	// SwarmNodeOf maps the ID of a node of this snapshot (the one Container.NodeID and Image.NodeID carry)
	// to its swarm node ID, which is what a node's own settings are stored under: an agent that is
	// enrolled again is a new node ID with the same swarm node ID. A node with no entry is looked up by
	// its own ID. Nil for an environment that is not a swarm.
	SwarmNodeOf map[string]string
	// NodeTimes is the time each node's engine reported when it was read, by node ID. A node whose
	// engine did not say what time it is has no entry (or the zero time).
	NodeTimes map[string]time.Time
	// Keeps are the objects a person kept out of cleanup from the Janitor page (D-12), read with the
	// snapshot so a preview and every run judge by the same ones. A container or image with a keep is
	// never removed. Nil means none.
	Keeps map[KeepKey]bool
	// StackFacts are the TierPlan stacks of the environment with their janitor exclusion, read with the
	// snapshot so a preview and every run judge by the same ones. A container or image a stack with
	// Excluded owns is never removed. Nil means none.
	StackFacts []StackFact
	ReadAt     time.Time
}

// HeldByStack says whether an object with these labels is held back by a stack in facts, and why: the reason code
// is stack_excluded when a person excluded the stack, stack_declared_keep when its compose file keeps it out of
// cleanup, and stack_unknown when its definition could not be read. An object belongs to a stack when it carries
// the stack's ID, or the stack's name as its swarm namespace or compose project. When several stacks match, the
// switch beats the compose file, which beats an unreadable definition. Held is false when no stack holds it.
func HeldByStack(facts []StackFact, labels map[string]string) (code ReasonCode, held bool) {
	if len(labels) == 0 {
		return "", false
	}
	rank := func(c ReasonCode) int {
		switch c {
		case ReasonStackExcluded:
			return 3
		case ReasonStackDeclaredKeep:
			return 2
		}
		return 1
	}
	for _, f := range facts {
		var c ReasonCode
		switch {
		case f.Excluded:
			c = ReasonStackExcluded
		case f.Declared:
			c = ReasonStackDeclaredKeep
		case f.Unknown:
			c = ReasonStackUnknown
		default:
			continue
		}
		owned := (f.ID != "" && labels[stacks.LabelStackID] == f.ID) ||
			(f.Name != "" && (labels[stacks.LabelSwarmNamespace] == f.Name || labels[stacks.LabelComposeProject] == f.Name))
		if owned && (!held || rank(c) > rank(code)) {
			code, held = c, true
		}
	}
	return code, held
}

// heldByStackLabels reports whether any stack holds an object with these labels back.
func (s Snapshot) heldByStackLabels(labels map[string]string) bool {
	_, held := s.heldByStack(labels)
	return held
}

// heldByStack is HeldByStack over the snapshot's stacks.
func (s Snapshot) heldByStack(labels map[string]string) (ReasonCode, bool) {
	return HeldByStack(s.StackFacts, labels)
}

// kept reports whether a person kept the container or image of kind k, on node nodeID, with ID ref.
func (s Snapshot) kept(k Kind, nodeID, ref string) bool {
	return s.Keeps[KeepKey{Kind: k, NodeID: nodeID, Ref: ref}]
}

// ContainerEnd is how a container ended: what a list does not carry and an inspect does.
type ContainerEnd struct {
	StartedAt, FinishedAt time.Time
	ExitCode              int
	Error                 string
	OOMKilled             bool
	Image, ImageID        string
	// SizeRw is the size in bytes of the writable layer, -1 when it is not known.
	SizeRw int64
}

// ContainerRef names a container to inspect: its ID and the node it is on.
type ContainerRef struct {
	ID, NodeID string
}

// EvidenceSource is what the engine of a node says of a crashed container, before it is built into
// the evidence a run stores: the container's end, its labels, the last lines of its log as the
// engine wrote them and the secret values to hide in them. Nothing in it has been masked.
type EvidenceSource struct {
	// End is how the container ended, read fresh from its node.
	End ContainerEnd
	// Labels are the container's labels.
	Labels map[string]string
	// Lines are the last lines of its log, oldest first, with the engine's timestamp in front.
	Lines []string
	// LogsUnavailable is true when the container's log driver cannot be read back, so Lines is empty
	// for that reason and not because the container wrote nothing.
	LogsUnavailable bool
	// Secrets are values to hide wherever they appear in a line: the container's own secret-looking
	// environment values and, for a container of a TierPlan stack, that stack's secret values.
	Secrets []string
	// Mask, when set, hides the secret values of the container's TierPlan stack in a line; it runs
	// before the pattern masks. Nil means the container is not a stack's.
	Mask func(string) string
	// ImageDigest is the repo@sha256 name of the image the container ran on its node, empty when the
	// node has none. Node is the node's name; the runner fills it in when the engine does not.
	ImageDigest, Node string
}

// Engine is how the planner's input is read. The server implements it over its aggregator, so a
// snapshot is read fresh from the engines and never from a cache.
type Engine interface {
	// Snapshot reads the environment now.
	Snapshot(ctx context.Context, envID string) (Snapshot, error)
	// Inspect reads the end of the containers ids on one node. A container that could not be
	// inspected is left out of the answer.
	Inspect(ctx context.Context, envID, nodeID string, ids []string) (map[string]ContainerEnd, error)
	// RemoveContainer removes one container through the agent of its node, once and never retried,
	// without force and without its volumes. It inspects the container on that node first and
	// refuses one that is no longer exited or is labelled tierplan.keep, so what the planner
	// judged a moment ago is judged again at the engine that will do the removing.
	RemoveContainer(ctx context.Context, envID, nodeID, id string) error
	// RemoveImage removes one image through the agent of its node, once and never retried, by the
	// Images page's own removal: the node's containers and the swarm's service specs are read again
	// first, the image's names are read again and an image that has gained a name is refused, and
	// force is never sent. It answers the same errors as RemoveContainer.
	RemoveImage(ctx context.Context, envID, nodeID, imageID string) error
	// Evidence reads what a crashed container leaves behind, on its node, before it is removed: its
	// end, labels and the last EvidenceLines lines of its log. A log the engine cannot read back is
	// not an error: it is LogsUnavailable. An error means nothing could be read at all.
	Evidence(ctx context.Context, envID, nodeID, id string) (EvidenceSource, error)
}

// What RemoveContainer answers when it does not remove the container. The runner reads them with
// errors.Is; any other error is a failure of the removal itself.
var (
	// ErrNoLongerStale: the container is not exited any more, or is labelled tierplan.keep.
	ErrNoLongerStale = errors.New("janitor: the container is no longer stale")
	// ErrInUse: the engine refused the removal because the container is in use.
	ErrInUse = errors.New("janitor: the container is in use")
	// ErrGone: the container no longer exists on its node.
	ErrGone = errors.New("janitor: the container is already gone")
	// ErrNodeOffline: the node is not connected, so nothing was sent.
	ErrNodeOffline = errors.New("janitor: the node is not connected")
)

// The codes a failed removal is given when the engine's error is not one of the Err values above.
// They are the words the containers API gives for the same failures, so the SPA reads one set.
const (
	// CodeAgentPolicyDenied: the node's agent refused the request by its own policy.
	CodeAgentPolicyDenied = "agent_policy_denied"
	// CodeHostReadOnly: the host's owner made its agent read-only.
	CodeHostReadOnly = "host_read_only"
	// CodeNodeUnreachable: the node did not answer.
	CodeNodeUnreachable = "node_unreachable"
	// CodeActionFailed: any other failure of the removal itself.
	CodeActionFailed = "action_failed"
)

// CodedError is a failed removal with the stable word the run records for it. RemoveContainer
// answers one for every failure it can name, and the runner records the word as the item's code.
type CodedError struct {
	Code string
	Err  error
}

// Error implements error.
func (e *CodedError) Error() string {
	return "janitor: the removal failed (" + e.Code + "): " + e.Err.Error()
}

// Unwrap returns the engine's error.
func (e *CodedError) Unwrap() error { return e.Err }

// inspectNodes is how many nodes are inspected at once. A node is asked one container at a time.
const inspectNodes = 4

// InspectCandidates reads the end of every container in refs, node by node, at most inspectNodes
// nodes at once. The answer holds a container only when its inspect succeeded: one that failed
// (it was removed meanwhile, or its node stopped answering) is absent, and the planner calls it
// cant_tell and never removes it. It never returns an error, because a failure is exactly that
// answer.
func InspectCandidates(ctx context.Context, eng Engine, envID string, refs []ContainerRef) map[string]ContainerEnd {
	byNode := map[string][]string{}
	var order []string
	for _, r := range refs {
		if _, seen := byNode[r.NodeID]; !seen {
			order = append(order, r.NodeID)
		}
		byNode[r.NodeID] = append(byNode[r.NodeID], r.ID)
	}
	var (
		mu  sync.Mutex
		out = map[string]ContainerEnd{}
	)
	var g errgroup.Group
	g.SetLimit(inspectNodes)
	for _, node := range order {
		g.Go(func() error {
			ends, err := eng.Inspect(ctx, envID, node, byNode[node])
			if err != nil {
				return nil //nolint:nilerr // a node that cannot be inspected leaves its containers un-inspected, which the planner reads as cant_tell
			}
			mu.Lock()
			defer mu.Unlock()
			maps.Copy(out, ends)
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// NetworkRow is one network the orphan check judged, as the janitor reads it.
type NetworkRow struct {
	// ID is the engine's ID of the network and Name its name. NodeID and Node are the node a local
	// network is on; a swarm-wide network has no node, so NodeID is empty and Node says Cluster.
	ID, Name, NodeID, Node string
	// Scope is the engine's scope: local or swarm.
	Scope string
	// Class is what the orphan check made of the network.
	Class netspace.Class
	// FormerOwner is the stack or compose project the network's labels name, FormerOwnerKind
	// "stack" or "project"; both are empty for a network with no owner label.
	FormerOwner, FormerOwnerKind string
	// KeptByLabel and KeptByRow say why a network of class kept is kept.
	KeptByLabel, KeptByRow bool
	// IdleSince is when the network's idle time started.
	IdleSince time.Time
}

// NetworkReport is every network of an environment, judged afresh.
type NetworkReport struct {
	// Rows are all the networks judged, whatever the class.
	Rows []NetworkRow
	// Complete is true when every node reported and, on a swarm, a manager answered: the picture a
	// verdict on a network needs. While it is false no network is removable.
	Complete bool
	// WithheldNodes names the nodes that did not report, WithheldStacks the stacks that did not load.
	WithheldNodes, WithheldStacks []string
}

// Networks is how the janitor judges and removes networks. The server implements it with the
// Address space page's own code, so a network is judged by one check and removed by one path.
type Networks interface {
	// Report judges every network of the environment from a fresh read, never from a cache.
	Report(ctx context.Context, envID string) (NetworkReport, error)
	// Remove removes one network once, as by: it judges the network again, saves its whole masked
	// inspect to the audit log, sends one remove and audits it. It answers ErrInUse when the engine
	// or the fresh judgement finds the network in use, ErrNoLongerStale when it is no longer
	// orphaned or unused, ErrNodeOffline when a node it needs does not answer, ErrGone when it is not
	// there, and a *CodedError for any other failure.
	Remove(ctx context.Context, envID string, by *rbac.Principal, row NetworkRow) error
}

// CodeInspectTooLarge is the code of a network whose inspect document is too large to save in the
// audit log first, so it is not removed.
const CodeInspectTooLarge = "inspect_too_large"
