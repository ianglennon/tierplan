// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/engine"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ErrFoldNotOffered means the local host cannot be folded into that environment now:
// there is no local environment, the host is not a node of a swarm that environment is
// bound to, or the fold has been done already (D-48).
var ErrFoldNotOffered = errors.New("envs: folding the local host into that environment is not offered")

// ErrFoldNodeNotListed means the host is a swarm node by its own account but that
// environment's managers do not list it as one of their nodes, or none of the managers it
// reports is among theirs (D-06). Nothing was changed.
var ErrFoldNodeNotListed = errors.New("envs: that environment's managers do not list the local host as one of its nodes")

// ErrFoldIngressActive means the Local environment holds ingress state that folding would
// delete through the cascade, so the fold is refused and nothing is changed (D-12, D-17).
var ErrFoldIngressActive = errors.New("envs: the local environment holds ingress state a fold would delete")

// IngressActiveError says what ingress state the Local environment holds. Each field is how
// many of that kind exist; Enabled is whether ingress is turned on. A bare version counter
// (an ingress_env row with ingress off) is not state (D-17) and is not counted.
type IngressActiveError struct {
	Domains, Reserved, Hostnames, Routes int64
	Enabled                              bool
}

// Any is true when something blocks the fold.
func (e *IngressActiveError) Any() bool {
	return e.Domains > 0 || e.Reserved > 0 || e.Hostnames > 0 || e.Routes > 0 || e.Enabled
}

// Error names the kinds that exist and nothing else.
func (e *IngressActiveError) Error() string {
	var parts []string
	add := func(n int64, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, strconv.FormatInt(n, 10)+" "+many)
		}
	}
	add(e.Domains, "domain", "domains")
	add(e.Reserved, "reserved name", "reserved names")
	add(e.Hostnames, "hostname", "hostnames")
	add(e.Routes, "route", "routes")
	if e.Enabled {
		parts = append(parts, "ingress turned on")
	}
	return "envs: the local environment holds ingress state a fold would delete (" + strings.Join(parts, ", ") + ")"
}

// Is makes errors.Is(err, ErrFoldIngressActive) true for this error.
func (e *IngressActiveError) Is(target error) bool { return target == ErrFoldIngressActive }

// ingressActive is the refusal for a count, or nil when nothing blocks the fold.
func ingressActive(row sqlc.CountIngressStateByEnvRow) *IngressActiveError {
	e := &IngressActiveError{
		Domains: row.Domains, Reserved: row.Reserved, Hostnames: row.Hostnames, Routes: row.Routes,
		Enabled: row.Enabled > 0,
	}
	if !e.Any() {
		return nil
	}
	return e
}

const (
	// foldSourceLocal and foldSourceSocket say where a fold's host came from, in the audit
	// entry: an existing Local environment, or the socket of a TierPlan that has none.
	foldSourceLocal  = "local_environment"
	foldSourceSocket = "socket"

	// foldListMinAge is how old a node list may be before a fold request reads it again.
	foldListMinAge = 2 * time.Second
)

// Two more verdicts than the proof itself gives (fold_proof.go), decided by the environment
// row rather than the node list.
const (
	// verdictOtherCluster means a manager of another swarm than the one the environment is
	// bound to (D-06): the cluster ID a manager reports must be the environment's own.
	verdictOtherCluster = verdictNodeNotListed + 1
	// verdictNotATarget means the environment is not one a fold can go into at all: not an
	// enabled, unfolded, bound swarm agent environment, or its cluster was changed.
	verdictNotATarget = verdictNodeNotListed + 2
)

// foldSrc is the host a fold would bring in: where its socket is and what the socket said.
type foldSrc struct {
	localID, socket string
	probe           dockerx.ProbeResult
	kind            string
}

// foldPair is the host and the swarm environment it can be folded into.
type foldPair struct {
	// localID is the Local environment to retire; it is empty for a fold from a bare socket.
	localID, swarmID string
	// nodeID is what the socket said, clusterID what the swarm environment is bound to
	// (read from its row, never from the socket) and socket where the host is.
	nodeID, clusterID, socket string
	// source is foldSourceLocal or foldSourceSocket; role is manager or worker.
	source, role string
}

// localFoldSource is the host of the first enabled Local environment whose monitor says it
// is online. Only what the monitor holds is read; nothing is asked of Docker.
func (s *Service) localFoldSource(rows []sqlc.Environment) (foldSrc, bool) {
	for _, r := range rows {
		if r.Kind != KindLocal || r.Enabled == 0 {
			continue
		}
		st := s.mon.Status(r.ID)
		if st.State != StateOnline {
			return foldSrc{}, false
		}
		return foldSrc{localID: r.ID, socket: r.SocketPath, probe: st.Probe, kind: foldSourceLocal}, true
	}
	return foldSrc{}, false
}

// isFoldTarget is true for an enabled, unfolded, bound swarm agent environment whose cluster was
// not changed: the only kind of environment a fold can go into.
func isFoldTarget(r sqlc.Environment) bool {
	return r.Kind == KindAgent && r.Enabled != 0 && !r.LocalNodeID.Valid && !r.ClusterChangedAt.Valid &&
		r.EnvType.String == envTypeSwarm && r.SwarmClusterID.String != ""
}

// proveFold decides whether the host behind src can be folded into swarm environment row r,
// by identity and nothing else (D-06). The node ID must be listed by the environment's
// managers (a list read through one of its own agents, shared with a manager the node
// reports), and a manager must also report the very cluster ID the environment is bound to.
// The binding was made by manager agents under the join token (D-12), so a remote swarm
// cannot claim the host without a node of the host's swarm. No Docker call is made.
func (s *Service) proveFold(src foldSrc, r sqlc.Environment) (foldPair, foldVerdict) {
	if !isFoldTarget(r) {
		return foldPair{}, verdictNotATarget
	}
	p := src.probe
	role := roleWorker
	if p.Type == dockerx.TypeSwarmManager {
		role = roleManager
		if p.SwarmClusterID != r.SwarmClusterID.String {
			return foldPair{}, verdictOtherCluster
		}
	}
	if v := proveNode(p, s.foldNodeList(r.ID)); v != verdictOffered {
		return foldPair{}, v
	}
	return foldPair{
		localID: src.localID, swarmID: r.ID, nodeID: p.SwarmNodeID, clusterID: r.SwarmClusterID.String,
		socket: src.socket, source: src.kind, role: role,
	}, verdictOffered
}

// foldRank orders refusals by how much they tell: a listed-elsewhere host is the most telling.
func foldRank(v foldVerdict) int {
	switch v {
	case verdictNodeNotListed:
		return 4
	case verdictNoNodeList:
		return 3
	case verdictManagersMismatch:
		return 2
	case verdictOtherCluster:
		return 1
	}
	return 0
}

// findFold is the first environment among rows the host can be folded into. With none, the
// verdict is the most telling refusal of any row that could have taken it.
func (s *Service) findFold(rows []sqlc.Environment, src foldSrc) (foldPair, foldVerdict) {
	best := verdictNotInSwarm
	for _, r := range rows {
		p, v := s.proveFold(src, r)
		if v == verdictOffered {
			return p, v
		}
		if v != verdictNotATarget && foldRank(v) > foldRank(best) {
			best = v
		}
	}
	return foldPair{}, best
}

// verdictErr is the error a refused fold answers with; nil for an offer.
func verdictErr(v foldVerdict) error {
	switch v {
	case verdictOffered:
		return nil
	case verdictNoNodeList, verdictManagersMismatch, verdictNodeNotListed:
		return ErrFoldNodeNotListed
	}
	return ErrFoldNotOffered
}

// FoldOffer says whether the local host can be folded into a swarm environment, and
// which two environments the offer is between. envID may be either of them; any other
// environment, and a lookup that fails, is not offered (D-48).
func (s *Service) FoldOffer(ctx context.Context, envID string) (offered bool, localEnvID, swarmEnvID string) {
	rows, err := s.st.RQ.ListEnvironments(ctx)
	if err != nil {
		s.log.Warn("look for a local host to fold", "error", err)
		return false, "", ""
	}
	src, ok := s.localFoldSource(rows)
	if !ok {
		return false, "", ""
	}
	p, v := s.findFold(rows, src)
	if v != verdictOffered || (envID != p.localID && envID != p.swarmID) {
		return false, "", ""
	}
	return true, p.localID, p.swarmID
}

// foldOfferedFor is FoldOffer for a row already read, answering only for the two kinds
// of environment the offer can concern so that no other pays for the lookup.
func (s *Service) foldOfferedFor(ctx context.Context, row sqlc.Environment) bool {
	switch {
	case row.Kind == KindLocal:
	case row.Kind == KindAgent && row.EnvType.String == envTypeSwarm && !row.LocalNodeID.Valid:
	default:
		return false
	}
	offered, _, _ := s.FoldOffer(ctx, row.ID)
	return offered
}

// FoldLocal folds the local host into the swarm environment swarmEnvID (D-48). In one
// transaction the swarm environment records the host's swarm node ID and socket, the
// local environment is deleted and environment.local_folded is audited. Afterwards the
// local monitor is stopped and the swarm environment starts one of its own on the same
// socket, keyed by SocketTarget, so the node is reached through the socket from then
// on and no agent is expected on it. It changes nothing in Docker or on the host.
//
// ErrNotFound means there is no such environment; ErrFoldNotOffered that the local host is
// not a node of that environment's swarm, or that it was folded already; ErrFoldNodeNotListed
// that the host calls itself a node of it but its managers do not list it (D-06);
// *IngressActiveError (errors.Is ErrFoldIngressActive) that the local environment holds ingress
// state the fold would delete (D-12).
func (s *Service) FoldLocal(ctx context.Context, actor audit.Actor, swarmEnvID, ip, ua string) error {
	rows, err := s.st.RQ.ListEnvironments(ctx)
	if err != nil {
		return fmt.Errorf("list environments: %w", err)
	}
	var target sqlc.Environment
	found := false
	for _, r := range rows {
		if r.ID == swarmEnvID {
			target, found = r, true
			break
		}
	}
	if !found {
		return ErrNotFound
	}
	// Any ID a fold cannot go into (the Local environment's own, a standalone host, a swarm that
	// is folded already or disabled) is refused here, before the server reads a cluster or
	// probes its Docker socket on behalf of a request that cannot end in a fold.
	if !isFoldTarget(target) {
		return ErrFoldNotOffered
	}
	// The one read of the swarm's nodes a fold may need, made before anything is held: a cold
	// or older list is read through a manager agent first, so that a fold is refused only on
	// a list that is current. Nothing after this point calls Docker or a tunnel.
	if s.clusters != nil {
		s.clusters.RefreshNodeList(ctx, swarmEnvID, foldListMinAge)
		// The read took time: take the rows again, so the proof below sees what it left.
		if rows, err = s.st.RQ.ListEnvironments(ctx); err != nil {
			return fmt.Errorf("list environments: %w", err)
		}
	}
	src, ok := s.localFoldSource(rows)
	if !ok {
		// No Local environment to retire: the detected socket is the host (D-04). This is the
		// one place besides FoldOfferDetail where it is probed, and only on this request (D-15).
		src, ok = s.detectSocket(ctx, rows)
	}
	if !ok {
		return ErrFoldNotOffered
	}
	var p foldPair
	v := verdictNotATarget
	for _, r := range rows {
		if r.ID == swarmEnvID {
			p, v = s.proveFold(src, r)
			break
		}
	}
	if err := verdictErr(v); err != nil {
		return err
	}

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	// D-12, D-17: the Local environment's row is deleted below and every ingress table cascades
	// from it, so ingress state refuses the fold. The count is read here, in the write
	// transaction, so state added after the offer is still seen. A socket fold has no Local row
	// and so nothing to lose. The host's socket and the names the audit entry carries come from
	// the Local row a fold retires; a socket fold has no such row.
	socketPath, localName, localEnvID := p.socket, "", ""
	if p.source == foldSourceLocal {
		row, err := q.CountIngressStateByEnv(ctx, p.localID)
		if err != nil {
			return fmt.Errorf("count the local environment's ingress state: %w", err)
		}
		if ia := ingressActive(row); ia != nil {
			return ia
		}
		local, err := q.GetEnvironment(ctx, p.localID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFoldNotOffered
		}
		if err != nil {
			return fmt.Errorf("get the local environment: %w", err)
		}
		socketPath, localName, localEnvID = local.SocketPath, local.Name, p.localID
	} else {
		// D-04: the socket was probed before this transaction began, so a Local environment
		// added or another environment folded since is seen here, and the loser changes nothing.
		locals, err := q.CountEnvironmentsOfKind(ctx, KindLocal)
		if err != nil {
			return fmt.Errorf("count local environments: %w", err)
		}
		folded, err := q.CountFoldedEnvironments(ctx)
		if err != nil {
			return fmt.Errorf("count folded environments: %w", err)
		}
		if locals != 0 || folded != 0 {
			return ErrFoldNotOffered
		}
	}
	swarm, err := q.GetEnvironment(ctx, swarmEnvID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("get the swarm environment: %w", err)
	}
	now := s.now().UTC().UnixNano()
	n, err := q.FoldLocalHost(ctx, sqlc.FoldLocalHostParams{
		LocalNodeID:     sql.NullString{String: p.nodeID, Valid: true},
		LocalSocketPath: sql.NullString{String: socketPath, Valid: true},
		UpdatedAt:       now, ID: swarmEnvID,
		SwarmClusterID: sql.NullString{String: p.clusterID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("record the folded host: %w", err)
	}
	if n != 1 {
		// Folded, rebound or removed since it was looked at.
		return ErrFoldNotOffered
	}
	var grants int64
	if p.source == foldSourceLocal {
		if _, err := q.DeleteEnvironment(ctx, p.localID); err != nil {
			return fmt.Errorf("retire the local environment: %w", err)
		}
		// The retired environment's ID is never used again, so what was granted on it
		// goes with it rather than staying behind pointing at nothing.
		if grants, err = deleteEnvironmentGrants(ctx, q, p.localID); err != nil {
			return err
		}
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentLocalFolded, TargetType: targetEnvironment, TargetID: swarmEnvID,
		EnvID: swarmEnvID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{
			"swarm": swarm.Name, "local": localName, "local_environment": localEnvID,
			"node": p.nodeID, "socket": socketPath, "grants": strconv.FormatInt(grants, 10),
			"source": p.source, "role": p.role,
		},
	}); err != nil {
		return fmt.Errorf("audit local host folded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	if p.source == foldSourceLocal {
		s.mon.Stop(p.localID)
		if s.clusters != nil {
			s.clusters.Forget(p.localID)
		}
	}
	if err := s.mon.Start(s.ctx(), SocketTarget(swarmEnvID), engine.LocalSocketTransport{Path: socketPath}); err != nil {
		// The fold is recorded; the socket target is started again with the server.
		s.log.Error("monitor the folded local host", "environment", swarmEnvID, "error", err)
	}
	if s.clusters != nil {
		s.clusters.notify(swarmEnvID)
	}
	return nil
}

// FoldedNode is the local host as a node of the swarm environment it was folded into.
type FoldedNode struct {
	// ID is the node's target key (SocketTarget), the ID its row in a node list carries.
	ID string
	// Node is the host name its engine reports; NodeID its swarm node ID.
	Node, NodeID string
	// Role is manager or worker.
	Role string
	// Online is true while the socket's engine answers and is supported.
	Online bool
	// EngineVersion is what its last probe reported; empty before the first.
	EngineVersion string
}

// FoldedNode is the folded local host of an agent environment, and false for an
// environment that has none or does not exist.
func (s *Service) FoldedNode(ctx context.Context, envID string) (FoldedNode, bool) {
	row, err := s.st.RQ.GetEnvironment(ctx, envID)
	if err != nil || row.Kind != KindAgent || !row.LocalNodeID.Valid {
		return FoldedNode{}, false
	}
	v := s.socketView(row, s.now())
	return FoldedNode{
		ID: v.ID, Node: v.Node, NodeID: v.SwarmNodeID, Role: v.Role,
		Online: v.Target.State == StateOnline, EngineVersion: v.Target.Probe.EngineVersion,
	}, true
}
