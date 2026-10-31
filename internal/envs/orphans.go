// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/netspace"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// seenWriteEvery is how stale a network's last-in-use time must be before it is written again.
// SQLite has one writer, so a busy network costs one write in five minutes and not one per pass
// (D-09, T-10-25).
const seenWriteEvery = 5 * time.Minute

// StackFacts is what Coaming's own stacks know about the networks of an environment. The stack
// manager implements it (stackops.NetFacts); this package never imports the stacks.
type StackFacts interface {
	// Stacks are the names and the IDs of the Coaming stacks of the environment.
	Stacks(ctx context.Context, envID string) (names, ids map[string]bool, err error)
	// ExternalNetworks are the names of the networks any Coaming stack of the environment
	// declares external. unknown names the stacks that did not load, whose declarations are
	// therefore not known.
	ExternalNetworks(ctx context.Context, envID string) (names map[string]bool, unknown []string, err error)
	// Busy reports the stack that holds the environment's operation lock, if one does.
	Busy(envID string) (stackName string, ok bool)
}

// WithStackFacts sets where the orphan check learns about Coaming's stacks. It returns s, and is
// called before the reader is used.
func (s *AddressSpaces) WithStackFacts(f StackFacts) *AddressSpaces {
	s.facts = f
	return s
}

// OrphanRow is one network the orphan check lists: orphaned (its stack is gone), unused (no stack
// ever owned it) or cant_tell (Coaming cannot be sure).
type OrphanRow struct {
	NetworkID, Name, Node, NodeID, Scope string
	Subnets                              []string
	Class                                netspace.Class
	// FormerOwner is the stack or compose project the network's labels name, and FormerOwnerKind
	// is stack or project; both are empty for a network with no owner label.
	FormerOwner, FormerOwnerKind string
	// IdleSince is when the network's idle time started: the later of its creation and the last
	// time Coaming saw it in use. IdleFromSeen is true when that was the last time seen in use.
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
// and unused, once they have been idle for netspace.GracePeriod, and the ones it cannot judge.
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

	rep, err := s.judge(ctx, envID, now)
	if err != nil {
		return OrphanReport{}, err
	}
	e.omu.Lock()
	e.orphans, e.orphansAt, e.haveOrphans = rep, now, true
	e.omu.Unlock()
	return cloneReport(rep), nil
}

// judge makes one orphan check.
func (s *AddressSpaces) judge(ctx context.Context, envID string, now time.Time) (OrphanReport, error) {
	usage, agg, err := ResolveUsage(ctx, s.agg, s.clusters, envID)
	if err != nil {
		return OrphanReport{}, err
	}
	seenRows, err := s.svc.st.RQ.ListNetworkSeen(ctx, envID)
	if err != nil {
		return OrphanReport{}, fmt.Errorf("read when networks were last in use: %w", err)
	}
	seen := make(map[NetKey]sqlc.NetworkSeen, len(seenRows))
	for _, r := range seenRows {
		seen[NetKey{NodeID: r.NodeID, NetworkID: r.NetworkID}] = r
	}

	rep := OrphanReport{Rows: []OrphanRow{}, WithheldNodes: []string{}, WithheldStacks: []string{}}
	complete := len(usage.Missing) == 0 && (!usage.Swarm || usage.ManagerRead)
	rep.Complete = complete
	var stamps []seenStamp
	done := map[NetKey]bool{}
	var byKnowledge bool
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
			Builtin: dockerx.IsBuiltinNetwork(row.Name, row.Ingress),
			InUse:   inUse,
			Known:   usage.Known(row),
			Created: row.Created,
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
		// Until a network's owner is checked against the stacks and the workloads that still carry
		// its label, an owned network is taken to have a live owner.
		in.StackExists = owner != ""

		class := netspace.Classify(in, now)
		switch class {
		case netspace.ClassOrphaned, netspace.ClassUnused, netspace.ClassCantTell:
		default:
			continue
		}
		if class == netspace.ClassCantTell && !in.Known {
			byKnowledge = true
		}
		idle := netspace.IdleSince(in.Created, in.LastInUse)
		rep.Rows = append(rep.Rows, OrphanRow{
			NetworkID: row.ID, Name: row.Name, Node: row.Node, NodeID: row.NodeID, Scope: row.Scope,
			Subnets: slices.Clone(row.Subnets), Class: class, FormerOwner: owner, FormerOwnerKind: kind,
			IdleSince: idle, IdleFromSeen: in.LastInUse.After(in.Created),
		})
	}
	if byKnowledge {
		rep.WithheldNodes = append(rep.WithheldNodes, usage.Missing...)
		rep.ManagerMissing = usage.Swarm && !usage.ManagerRead
	}
	sortOrphans(rep.Rows)
	s.noteSeen(ctx, envID, now, stamps, seen, complete)
	return rep, nil
}

// the owner label keys of a network: a stack's namespace and a compose project.
const (
	labelStackNamespace = "com.docker.stack.namespace"
	labelComposeProject = "com.docker.compose.project"
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
