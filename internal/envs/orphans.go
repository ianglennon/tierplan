// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// seenWriteEvery is how stale a network's last-in-use time must be before it is written again.
// SQLite has one writer, so a busy network costs one write in five minutes and not one per pass
// (D-09, T-10-25).
const seenWriteEvery = 5 * time.Minute

// StackFacts is what TierPlan's own stacks know about the networks of an environment. The stack
// manager implements it (stackops.NetFacts); this package never imports the stacks.
type StackFacts interface {
	// Stacks are the names and the IDs of the TierPlan stacks of the environment.
	Stacks(ctx context.Context, envID string) (names, ids map[string]bool, err error)
	// ExternalNetworks are the names of the networks any TierPlan stack of the environment
	// declares external. unknown names the stacks that did not load, whose declarations are
	// therefore not known.
	ExternalNetworks(ctx context.Context, envID string) (names map[string]bool, unknown []string, err error)
	// Busy reports the stack that holds the environment's operation lock, if one does.
	Busy(envID string) (stackName string, ok bool)
}

// WithStackFacts sets where the orphan check learns about TierPlan's stacks. It returns s, and is
// called before the reader is used.
func (s *AddressSpaces) WithStackFacts(f StackFacts) *AddressSpaces {
	s.facts = f
	return s
}

// GraceSource says how long a network of an environment must have been idle before the orphan check
// judges it: the environment's grace setting (D-20). The server backs it with the janitor's policy
// store; this package never imports the janitor.
type GraceSource func(ctx context.Context, envID string) (time.Duration, error)

// WithGrace sets where the orphan check learns an environment's grace, the one the janitor reads for
// every resource it removes. It returns s, and is called before the reader is used. With none set, or
// with a source that answers zero, the check uses netspace.GracePeriod.
func (s *AddressSpaces) WithGrace(g GraceSource) *AddressSpaces {
	s.grace = g
	return s
}

// graceOf is the grace a check of the environment judges by, read once for the whole pass. A source
// that fails, or answers nothing usable, never fails the check: the check falls back to the default of
// 24 hours (T-11-64) and logs the failure. That can list a network earlier than the environment's own
// grace would; the janitor still holds each network to the policy's grace itself before it removes one,
// and a person's delete from the Address space page never waits for a grace period.
func (s *AddressSpaces) graceOf(ctx context.Context, envID string) time.Duration {
	if s.grace == nil {
		return netspace.GracePeriod
	}
	g, err := s.grace(ctx, envID)
	switch {
	case err != nil:
		s.log.Warn("could not read the grace period of an environment, using the default for the orphan check", "environment", envID, "default", netspace.GracePeriod, "error", err)
		return netspace.GracePeriod
	case g <= 0:
		return netspace.GracePeriod
	}
	return g
}

// OrphanRow is one network the orphan check lists: orphaned (its stack is gone), unused (no stack
// ever owned it) or cant_tell (TierPlan cannot be sure).
type OrphanRow struct {
	NetworkID, Name, Node, NodeID, Scope, Driver string
	// Builtin is true for the engine's or swarm's own network, a config-only network and the
	// target of a config-from: never removed.
	Builtin bool
	Subnets []string
	Class   netspace.Class
	// FormerOwner is the stack or compose project the network's labels name, and FormerOwnerKind
	// is stack or project; both are empty for a network with no owner label.
	FormerOwner, FormerOwnerKind string
	// IdleSince is when the network's idle time started: the later of its creation and the last
	// time TierPlan saw it in use. IdleFromSeen is true when that was the last time seen in use.
	IdleSince    time.Time
	IdleFromSeen bool
}

// OrphanReport is the answer of the orphan check for one environment.
type OrphanReport struct {
	// Rows are the networks listed, orphaned first, then unused, then cant_tell, each by name.
	Rows []OrphanRow
	// WithheldNodes names the nodes that did not report, because of which a network was not
	// judged. WithheldStacks names the stacks that did not load. ManagerMissing is true when a
	// swarm's managers did not answer. All are empty when nothing was withheld.
	WithheldNodes, WithheldStacks []string
	ManagerMissing                bool
	// Complete is true when every node reported (and on a swarm a manager answered): the check
	// saw everything it needed.
	Complete bool
	// Grace is the environment's grace the check judged by: a network idle for less is not listed.
	Grace time.Duration
}

func cloneReport(r OrphanReport) OrphanReport {
	r.Rows = slices.Clone(r.Rows)
	for i := range r.Rows {
		r.Rows[i].Subnets = slices.Clone(r.Rows[i].Subnets)
	}
	r.WithheldNodes = slices.Clone(r.WithheldNodes)
	r.WithheldStacks = slices.Clone(r.WithheldStacks)
	return r
}

// Orphans lists the networks of an environment that nothing uses any more, split into orphaned
// and unused, once they have been idle for the environment's grace (24 hours unless the grace
// source says otherwise, D-20), and the ones it cannot judge.
// It reads who uses every network fresh (ResolveUsage), so it is never judged on a stale view,
// stamps the networks it sees in use so the grace clock has something to run on, and keeps the
// answer for the reader's TTL. Listing is report-only: removing re-checks (D-12).
func (s *AddressSpaces) Orphans(ctx context.Context, envID string) (OrphanReport, error) {
	e := s.entry(envID)
	e.oread.Lock()
	defer e.oread.Unlock()

	now := s.now()
	e.omu.Lock()
	if e.haveOrphans && now.Sub(e.orphansAt) < s.ttl && now.Sub(e.orphansAt) >= 0 {
		v := cloneReport(e.orphans)
		e.omu.Unlock()
		return v, nil
	}
	e.omu.Unlock()

	rep, _, err := s.judge(ctx, envID, now, nil)
	if err != nil {
		return OrphanReport{}, err
	}
	e.omu.Lock()
	e.orphans, e.orphansAt, e.haveOrphans = rep, now, true
	e.omu.Unlock()
	orphaned := 0
	for _, r := range rep.Rows {
		if r.Class == netspace.ClassOrphaned {
			orphaned++
		}
	}
	s.setOrphaned(envID, orphaned)
	return cloneReport(rep), nil
}

// JudgedRow is one network the orphan check judged, whatever its class, with why a kept network
// is kept: by the label on it, by a keep row a person made, or both.
type JudgedRow struct {
	OrphanRow
	KeptByLabel, KeptByRow bool
}

// JudgeReport is every network of an environment judged afresh: all of them, whatever the class,
// where an OrphanReport lists only the three a person is shown.
type JudgeReport struct {
	// Rows are all the networks judged, in the order the engines listed them.
	Rows []JudgedRow
	// WithheldNodes names the nodes that did not report and WithheldStacks the stacks that did not
	// load. ManagerMissing is true when a swarm's managers did not answer, or its services could not
	// be read. All are empty when Complete is true.
	WithheldNodes, WithheldStacks []string
	ManagerMissing                bool
	// Complete is true when every node reported, on a swarm a manager answered and its services were
	// read: the check saw everything a verdict on a network needs.
	Complete bool
	// Grace is the environment's grace the check judged by.
	Grace time.Duration
}

// judgeMeta is what a check learned beyond the report it lists: the reasons a verdict could be
// withheld, whether or not a listed network was withheld by them.
type judgeMeta struct {
	missing        []string
	managerMissing bool
	workComplete   bool
}

// Judge judges every network of an environment from a fresh read and answers all of them. It
// never reads or fills the cache of Orphans, so it is what a caller that must not act on an old
// picture asks. It stamps the networks it sees in use, as every check does, so the grace clock
// keeps running.
func (s *AddressSpaces) Judge(ctx context.Context, envID string) (JudgeReport, error) {
	e := s.entry(envID)
	e.oread.Lock()
	defer e.oread.Unlock()

	var rows []JudgedRow
	rep, meta, err := s.judge(ctx, envID, s.now(), func(r JudgedRow) { rows = append(rows, r) })
	if err != nil {
		return JudgeReport{}, err
	}
	out := JudgeReport{
		Rows: rows, WithheldNodes: []string{}, WithheldStacks: slices.Clone(rep.WithheldStacks),
		Complete: rep.Complete && meta.workComplete, Grace: rep.Grace,
	}
	if out.Rows == nil {
		out.Rows = []JudgedRow{}
	}
	if !out.Complete {
		out.WithheldNodes = slices.Clone(meta.missing)
		out.ManagerMissing = meta.managerMissing
	}
	return out, nil
}

// judge makes one orphan check. observe, when set, is called for every network the check judged
// whatever its class, so a caller can ask about one network that Orphans would not list.
func (s *AddressSpaces) judge(ctx context.Context, envID string, now time.Time, observe func(JudgedRow)) (OrphanReport, judgeMeta, error) {
	usage, agg, err := ResolveUsage(ctx, s.agg, s.clusters, envID)
	if err != nil {
		return OrphanReport{}, judgeMeta{}, err
	}
	seenRows, err := s.svc.st.RQ.ListNetworkSeen(ctx, envID)
	if err != nil {
		return OrphanReport{}, judgeMeta{}, fmt.Errorf("read when networks were last in use: %w", err)
	}
	seen := make(map[NetKey]sqlc.NetworkSeen, len(seenRows))
	for _, r := range seenRows {
		seen[NetKey{NodeID: r.NodeID, NetworkID: r.NetworkID}] = r
	}

	keepRows, err := s.svc.st.RQ.ListNetworkKeeps(ctx, envID)
	if err != nil {
		return OrphanReport{}, judgeMeta{}, fmt.Errorf("read the networks kept from the orphan list: %w", err)
	}
	keeps := make(map[NetKey]bool, len(keepRows))
	for _, k := range keepRows {
		keeps[NetKey{NodeID: k.NodeID, NetworkID: k.Name}] = true
	}
	sf, err := s.stackFacts(ctx, envID)
	if err != nil {
		return OrphanReport{}, judgeMeta{}, err
	}
	work := s.workloads(ctx, envID, agg, usage)
	targets := map[string]bool{}
	for _, row := range agg.Networks {
		if row.ConfigFrom != "" {
			targets[row.ConfigFrom] = true
		}
	}

	grace := s.graceOf(ctx, envID)
	rep := OrphanReport{Rows: []OrphanRow{}, WithheldNodes: []string{}, WithheldStacks: []string{}, Grace: grace}
	complete := len(usage.Missing) == 0 && (!usage.Swarm || usage.ManagerRead)
	rep.Complete = complete
	var stamps []seenStamp
	done := map[NetKey]bool{}
	var byKnowledge, byStacks bool
	for _, row := range agg.Networks {
		key := usage.key(row)
		if done[key] {
			continue
		}
		done[key] = true
		inUse := usage.InUse(row)
		stamps = append(stamps, seenStamp{key: key, name: row.Name, inUse: inUse})

		old, have := seen[key]
		in := netspace.OrphanInput{
			Builtin:          dockerx.IsBuiltinNetwork(row.Name, row.Ingress) || row.ConfigOnly || targets[row.Name],
			InUse:            inUse,
			Known:            usage.Known(row),
			Created:          row.Created,
			KeepLabel:        row.Labels[LabelKeep] == "true",
			KeepRow:          keeps[NetKey{NodeID: key.NodeID, NetworkID: row.Name}],
			External:         sf.externals[row.Name],
			ExternalsUnknown: len(sf.unknown) > 0,
			Grace:            grace,
		}
		if have {
			if in.Created.IsZero() {
				in.Created = time.Unix(0, old.FirstSeenAt).UTC()
			}
			if old.LastInUseAt > 0 {
				in.LastInUse = time.Unix(0, old.LastInUseAt).UTC()
			}
		}
		if in.Created.IsZero() {
			// A network that reports no creation time and has no clock yet starts its idle time now.
			in.Created = now
		}
		if inUse {
			in.LastInUse = now
		}
		owner, kind := networkOwnerOf(row.Labels)
		in.OwnerLabel = owner
		if owner != "" {
			in.StackExists = sf.exists(owner, row.Labels[labelStackID]) || work.carry(kind, owner)
			// What the workloads say is only the whole story when they were all read.
			in.Known = in.Known && work.complete
		}
		in.Busy = sf.busy != "" && owner == sf.busy

		class := netspace.Classify(in, now)
		idle := netspace.IdleSince(in.Created, in.LastInUse)
		orow := OrphanRow{
			NetworkID: row.ID, Name: row.Name, Node: row.Node, NodeID: row.NodeID, Scope: row.Scope, Driver: row.Driver,
			Builtin: in.Builtin, Subnets: slices.Clone(row.Subnets), Class: class, FormerOwner: owner, FormerOwnerKind: kind,
			IdleSince: idle, IdleFromSeen: in.LastInUse.After(in.Created),
		}
		if observe != nil {
			observe(JudgedRow{OrphanRow: orow, KeptByLabel: in.KeepLabel, KeptByRow: in.KeepRow})
		}
		switch class {
		case netspace.ClassOrphaned, netspace.ClassUnused, netspace.ClassCantTell:
		default:
			continue
		}
		if class == netspace.ClassCantTell {
			if in.Known {
				byStacks = true
			} else {
				byKnowledge = true
			}
		}
		rep.Rows = append(rep.Rows, orow)
	}
	if byKnowledge {
		rep.WithheldNodes = append(rep.WithheldNodes, usage.Missing...)
		rep.ManagerMissing = usage.Swarm && (!usage.ManagerRead || !work.complete)
	}
	if byStacks {
		rep.WithheldStacks = append(rep.WithheldStacks, sf.unknown...)
	}
	sortOrphans(rep.Rows)
	s.noteSeen(ctx, envID, now, stamps, seen, complete)
	meta := judgeMeta{
		missing: slices.Clone(usage.Missing), workComplete: work.complete,
		managerMissing: usage.Swarm && (!usage.ManagerRead || !work.complete),
	}
	return rep, meta, nil
}

// ErrNetworkGone is Recheck's answer for a network no node that reported holds, when every node
// reported: it is gone. ErrNetworkAmbiguous is its answer when a local network's ID is held by
// more than one node and the caller named none.
var (
	ErrNetworkGone      = errors.New("envs: no such network")
	ErrNetworkAmbiguous = errors.New("envs: more than one node has a network with this ID")
)

// Recheck judges one network again from a fresh read, never from the cached report, and says
// what the orphan check makes of it now (D-12, Pitfall 7). It is the last thing a removal asks
// before it acts, so what it answers is the picture of this moment: who uses the network
// (ResolveUsage), the stacks, the keeps and the grace period are all read again.
//
// A swarm-scoped network is found by its ID; a local one by its ID and nodeID (an empty nodeID
// finds it when one node holds it). When the network is on no node that reported and some node
// did not report, the answer is netspace.ClassCantTell with a row that names only the ID it was
// asked for: the network may well be on the node that is out of reach. An error is the
// environment not being readable, ErrNetworkGone or ErrNetworkAmbiguous.
func (s *AddressSpaces) Recheck(ctx context.Context, envID, networkID, nodeID string) (OrphanRow, netspace.Class, error) {
	e := s.entry(envID)
	e.oread.Lock()
	defer e.oread.Unlock()

	var found []OrphanRow
	rep, _, err := s.judge(ctx, envID, s.now(), func(r JudgedRow) {
		if r.NetworkID != networkID {
			return
		}
		if r.Scope != dockerx.ScopeSwarm && nodeID != "" && r.NodeID != nodeID {
			return
		}
		found = append(found, r.OrphanRow)
	})
	if err != nil {
		return OrphanRow{}, "", err
	}
	switch len(found) {
	case 0:
		if !rep.Complete {
			return OrphanRow{NetworkID: networkID, NodeID: nodeID}, netspace.ClassCantTell, nil
		}
		return OrphanRow{}, "", ErrNetworkGone
	case 1:
		return found[0], found[0].Class, nil
	}
	return OrphanRow{}, "", ErrNetworkAmbiguous
}

// KeptRow is one network kept out of the orphan list (D-11): by a keep row a person made, by the
// tierplan.keep=true label on the network, or both.
type KeptRow struct {
	Name string
	// Node is the node's name, empty for a swarm-wide network and for a keep row whose node is no
	// longer known. NodeID is the node's ID as the keep is keyed, "" for a swarm-wide network.
	Node, NodeID string
	// Subnets are the network's, when it is present.
	Subnets []string
	// ByLabel is true when the network carries the keep label, which only the network's owner
	// can remove. KeptBy (a user ID) and KeptAt come from the keep row, empty when there is none.
	ByLabel bool
	KeptBy  string
	KeptAt  time.Time
	// Present is true when a network of that name (and node) exists now.
	Present bool
}

// KeepNodeKey is the node a keep for the network row is keyed by: "" for a swarm-wide network, the
// node's ID for a local one.
func KeepNodeKey(row NetworkRow) string {
	if row.Scope == dockerx.ScopeSwarm {
		return ""
	}
	return row.NodeID
}

// Kept lists what is kept out of the orphan list: every keep row, whether or not its network
// exists now, and every network that carries the keep label. A network that has both is one row.
// Rows are ordered by name, then node.
func (s *AddressSpaces) Kept(ctx context.Context, envID string) ([]KeptRow, error) {
	keepRows, err := s.svc.st.RQ.ListNetworkKeeps(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("read the networks kept from the orphan list: %w", err)
	}
	agg, _, err := s.agg.Snapshot(ctx, envID)
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]string, len(agg.Nodes))
	for _, n := range agg.Nodes {
		nodes[n.ID] = n.Name
	}

	out := make([]KeptRow, 0, len(keepRows))
	at := make(map[NetKey]int, len(keepRows))
	for _, k := range keepRows {
		key := NetKey{NodeID: k.NodeID, NetworkID: k.Name}
		at[key] = len(out)
		out = append(out, KeptRow{
			Name: k.Name, NodeID: k.NodeID, Node: nodes[k.NodeID], Subnets: []string{},
			KeptBy: k.KeptBy, KeptAt: time.Unix(0, k.KeptAt).UTC(),
		})
	}
	seen := map[NetKey]bool{}
	for _, n := range agg.Networks {
		key := NetKey{NodeID: KeepNodeKey(n), NetworkID: n.Name}
		if seen[key] {
			continue
		}
		seen[key] = true
		i, have := at[key]
		labelled := n.Labels[LabelKeep] == "true"
		if !have && !labelled {
			continue
		}
		if !have {
			i = len(out)
			out = append(out, KeptRow{Name: n.Name})
		}
		r := &out[i]
		r.NodeID, r.Node, r.Present, r.ByLabel = KeepNodeKey(n), n.Node, true, labelled
		r.Subnets = slices.Clone(n.Subnets)
		if r.Subnets == nil {
			r.Subnets = []string{}
		}
	}
	slices.SortFunc(out, func(a, b KeptRow) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Node, b.Node)
	})
	return out, nil
}

// ForgetOrphans drops the cached orphan report of an environment, so the next read judges again.
// A removal or a change of the keeps calls it, so the list never shows what was just changed.
func (s *AddressSpaces) ForgetOrphans(envID string) {
	e := s.entry(envID)
	// Waiting for a check in flight keeps it from storing, after this, a report it began before.
	e.oread.Lock()
	defer e.oread.Unlock()
	e.omu.Lock()
	e.haveOrphans = false
	e.omu.Unlock()
}

// stackKnowledge is what TierPlan's own stacks say about an environment, read once for a check.
type stackKnowledge struct {
	names, ids map[string]bool
	// externals are the names of the networks a TierPlan stack declares external; unknown names
	// the stacks that did not load.
	externals map[string]bool
	unknown   []string
	// busy is the stack holding the environment's operation lock, empty for none.
	busy string
}

// exists reports whether a TierPlan stack record has this name or this ID.
func (k stackKnowledge) exists(name, id string) bool {
	return k.names[name] || (id != "" && k.ids[id])
}

// stackFacts reads what TierPlan's stacks say. With no source of facts no stack record exists and
// nothing is declared external; the workload rule still applies.
func (s *AddressSpaces) stackFacts(ctx context.Context, envID string) (stackKnowledge, error) {
	k := stackKnowledge{names: map[string]bool{}, ids: map[string]bool{}, externals: map[string]bool{}, unknown: []string{}}
	if s.facts == nil {
		return k, nil
	}
	var err error
	if k.names, k.ids, err = s.facts.Stacks(ctx, envID); err != nil {
		return stackKnowledge{}, fmt.Errorf("read the stacks of the environment: %w", err)
	}
	if k.externals, k.unknown, err = s.facts.ExternalNetworks(ctx, envID); err != nil {
		return stackKnowledge{}, fmt.Errorf("read the external networks the stacks declare: %w", err)
	}
	k.busy, _ = s.facts.Busy(envID)
	return k, nil
}

// workloadLabels are the stack and compose-project names that containers and services of an
// environment still carry, whether or not a TierPlan stack of that name exists (user decision 4).
type workloadLabels struct {
	stacks, projects map[string]bool
	// complete is false when a swarm's services could not be read, so a label may be carried by
	// a service the check did not see.
	complete bool
}

// carry reports whether some workload still carries the label of this owner kind and name.
func (w workloadLabels) carry(kind, name string) bool {
	if kind == "stack" {
		return w.stacks[name]
	}
	return w.projects[name]
}

// workloads collects the owner labels of every container the aggregate holds and, on a swarm, of
// every service of the last cluster read.
func (s *AddressSpaces) workloads(ctx context.Context, envID string, agg Aggregate, usage Usage) workloadLabels {
	w := workloadLabels{stacks: map[string]bool{}, projects: map[string]bool{}, complete: true}
	for _, c := range agg.Containers {
		if v := c.Labels[labelStackNamespace]; v != "" {
			w.stacks[v] = true
		}
		if v := c.Labels[labelComposeProject]; v != "" {
			w.projects[v] = true
		}
	}
	if !usage.Swarm {
		return w
	}
	st, err := s.clusters.State(ctx, envID)
	if err != nil && st.FetchedAt.IsZero() {
		s.log.Debug("the services of a swarm could not be read to tell whether a stack is gone", "environment", envID, "error", err)
		w.complete = false
		return w
	}
	for _, sv := range st.Read.Services {
		if sv.Stack != "" {
			w.stacks[sv.Stack] = true
		}
	}
	return w
}

// the owner label keys of a network: a stack's namespace and a compose project.
const (
	labelStackNamespace = "com.docker.stack.namespace"
	labelComposeProject = "com.docker.compose.project"
	// labelStackID is the label TierPlan puts on what it deploys, the ID of the stack record.
	labelStackID = "tierplan.stack.id"
	// LabelKeep, set to "true" on a network, exempts it from the orphan check (D-11). It is the same label as
	// stacks.LabelKeep (JAN-07): envs never imports internal/stacks, so the value is written here once more and a
	// test in internal/api pins the two equal.
	LabelKeep = "tierplan.keep"
)

// networkOwnerOf is the stack or compose project a network's labels name; a stack wins when both
// are set. Both empty is no owner.
func networkOwnerOf(labels map[string]string) (owner, kind string) {
	if v := labels[labelStackNamespace]; v != "" {
		return v, "stack"
	}
	if v := labels[labelComposeProject]; v != "" {
		return v, "project"
	}
	return "", ""
}

// sortOrphans orders rows orphaned first, then unused, then cant_tell, each by name, node and ID.
func sortOrphans(rows []OrphanRow) {
	rank := func(c netspace.Class) int {
		switch c {
		case netspace.ClassOrphaned:
			return 0
		case netspace.ClassUnused:
			return 1
		default:
			return 2
		}
	}
	slices.SortFunc(rows, func(a, b OrphanRow) int {
		if c := rank(a.Class) - rank(b.Class); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := strings.Compare(a.Node, b.Node); c != 0 {
			return c
		}
		return strings.Compare(a.NetworkID, b.NetworkID)
	})
}

// seenStamp is one network of a pass and whether it was in use.
type seenStamp struct {
	key   NetKey
	name  string
	inUse bool
}

// noteSeen records the pass in network_seen, in one transaction: a network seen for the first
// time starts its clock, a network in use has its last-in-use time moved to now when the stored
// one is more than seenWriteEvery old (or new), and after a complete pass the rows of networks
// that no longer exist are removed. A failed write is logged and never fails the check: the
// check judged on what it read, and the next pass writes again.
func (s *AddressSpaces) noteSeen(ctx context.Context, envID string, now time.Time, stamps []seenStamp, seen map[NetKey]sqlc.NetworkSeen, complete bool) {
	var writes []seenStamp
	live := make(map[NetKey]bool, len(stamps))
	for _, st := range stamps {
		live[st.key] = true
		old, have := seen[st.key]
		if (st.inUse && (!have || now.Sub(time.Unix(0, old.LastInUseAt)) > seenWriteEvery)) || (!st.inUse && !have) {
			writes = append(writes, st)
		}
	}
	var stale []NetKey
	if complete {
		for k := range seen {
			if !live[k] {
				stale = append(stale, k)
			}
		}
	}
	if len(writes) == 0 && len(stale) == 0 {
		return
	}
	if err := s.writeSeen(ctx, envID, now, writes, stale); err != nil {
		s.log.Warn("could not record when networks were last in use", "environment", envID, "error", err)
	}
}

// writeSeen applies the writes of one pass in one transaction.
func (s *AddressSpaces) writeSeen(ctx context.Context, envID string, now time.Time, writes []seenStamp, stale []NetKey) error {
	tx, err := s.svc.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.svc.st.Q.WithTx(tx)
	at := now.UTC().UnixNano()
	for _, w := range writes {
		if w.inUse {
			err = q.UpsertNetworkSeen(ctx, sqlc.UpsertNetworkSeenParams{
				EnvID: envID, NodeID: w.key.NodeID, NetworkID: w.key.NetworkID, Name: w.name, FirstSeenAt: at, LastInUseAt: at,
			})
		} else {
			err = q.InsertNetworkSeenIfAbsent(ctx, sqlc.InsertNetworkSeenIfAbsentParams{
				EnvID: envID, NodeID: w.key.NodeID, NetworkID: w.key.NetworkID, Name: w.name, FirstSeenAt: at,
			})
		}
		if err != nil {
			return err
		}
	}
	for _, k := range stale {
		if _, err = q.DeleteNetworkSeen(ctx, sqlc.DeleteNetworkSeenParams{EnvID: envID, NodeID: k.NodeID, NetworkID: k.NetworkID}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
