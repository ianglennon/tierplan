// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/engine"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// ErrFoldNotOffered means the local host cannot be folded into that environment now:
// there is no local environment, the local host is not a manager of the swarm the
// environment is bound to, or the fold has been done already (D-48).
var ErrFoldNotOffered = errors.New("envs: folding the local host into that environment is not offered")

// foldPair is the local environment and the swarm environment it can be folded into.
type foldPair struct {
	localID, swarmID string
	// nodeID, clusterID and socket are what the local socket said and where it is.
	nodeID, clusterID, socket string
}

// findFold looks for the overlap D-48 describes, by identity and nothing else: the
// local socket's own /info must say the host is a manager of a swarm, and an agent
// environment must be bound to that very cluster ID. The binding was made by manager
// agents under the join token (D-12), so a remote swarm cannot claim the local host's
// cluster without holding the local host's cluster ID, which only a manager of it
// learns (T-02-97). Only what the local socket's monitor holds is read; nothing is
// asked of Docker.
func (s *Service) findFold(rows []sqlc.Environment) (foldPair, bool) {
	var local *sqlc.Environment
	for i := range rows {
		if rows[i].Kind == KindLocal && rows[i].Enabled != 0 {
			local = &rows[i]
			break
		}
	}
	if local == nil {
		return foldPair{}, false
	}
	st := s.mon.Status(local.ID)
	p := st.Probe
	if st.State != StateOnline || p.Type != dockerx.TypeSwarmManager || p.SwarmClusterID == "" || p.SwarmNodeID == "" {
		return foldPair{}, false
	}
	for _, r := range rows {
		if r.Kind != KindAgent || r.Enabled == 0 || r.LocalNodeID.Valid || r.ClusterChangedAt.Valid {
			continue
		}
		if r.EnvType.String != envTypeSwarm || r.SwarmClusterID.String != p.SwarmClusterID {
			continue
		}
		return foldPair{localID: local.ID, swarmID: r.ID, nodeID: p.SwarmNodeID, clusterID: p.SwarmClusterID, socket: local.SocketPath}, true
	}
	return foldPair{}, false
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
	p, ok := s.findFold(rows)
	if !ok || (envID != p.localID && envID != p.swarmID) {
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
// ErrNotFound means there is no such environment; ErrFoldNotOffered that the local
// host is not a manager of that environment's swarm, or that it was folded already.
func (s *Service) FoldLocal(ctx context.Context, actor audit.Actor, swarmEnvID, ip, ua string) error {
	if !s.Known(ctx, swarmEnvID) {
		return ErrNotFound
	}
	rows, err := s.st.RQ.ListEnvironments(ctx)
	if err != nil {
		return fmt.Errorf("list environments: %w", err)
	}
	p, ok := s.findFold(rows)
	if !ok || p.swarmID != swarmEnvID {
		return ErrFoldNotOffered
	}

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	local, err := q.GetEnvironment(ctx, p.localID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFoldNotOffered
	}
	if err != nil {
		return fmt.Errorf("get the local environment: %w", err)
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
		LocalSocketPath: sql.NullString{String: local.SocketPath, Valid: true},
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
	if _, err := q.DeleteEnvironment(ctx, p.localID); err != nil {
		return fmt.Errorf("retire the local environment: %w", err)
	}
	// The retired environment's ID is never used again, so what was granted on it
	// goes with it rather than staying behind pointing at nothing.
	grants, err := deleteEnvironmentGrants(ctx, q, p.localID)
	if err != nil {
		return err
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentLocalFolded, TargetType: targetEnvironment, TargetID: swarmEnvID,
		EnvID: swarmEnvID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{
			"swarm": swarm.Name, "local": local.Name, "local_environment": p.localID,
			"node": p.nodeID, "socket": local.SocketPath, "grants": strconv.FormatInt(grants, 10),
		},
	}); err != nil {
		return fmt.Errorf("audit local host folded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.mon.Stop(p.localID)
	if s.clusters != nil {
		s.clusters.Forget(p.localID)
	}
	if err := s.mon.Start(s.ctx(), SocketTarget(swarmEnvID), engine.LocalSocketTransport{Path: local.SocketPath}); err != nil {
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
