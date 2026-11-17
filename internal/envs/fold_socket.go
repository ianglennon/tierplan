// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"fmt"
	"slices"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/engine"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// detectSocket is the host behind the configured Docker socket as a fold source, for a
// TierPlan that has no Local environment (D-04). It is one when there is no Local row (enabled
// or not), no environment is folded yet, an enabled unfolded swarm environment exists that a
// fold could go into, the socket is there and its engine answers.
//
// D-15: it is called only from FoldOfferDetail and FoldLocal, on an Admin's explicit request,
// and nothing it learns is kept. The environments list, an environment's detail, the bell and
// the overview never call it, so no background or cached probe of the socket exists. The probe
// is bounded by the monitor's probe timeout. A socket that is missing, or an engine that does
// not answer or is unsupported, is "no source", never an error to the caller.
func (s *Service) detectSocket(ctx context.Context, rows []sqlc.Environment) (foldSrc, bool) {
	target := false
	for _, r := range rows {
		switch {
		case r.Kind == KindLocal:
			return foldSrc{}, false
		case r.Kind == KindAgent && r.LocalNodeID.Valid:
			return foldSrc{}, false
		case r.Kind == KindAgent && r.Enabled != 0 && r.EnvType.String == envTypeSwarm &&
			!r.ClusterChangedAt.Valid && r.SwarmClusterID.String != "":
			target = true
		}
	}
	if !target {
		return foldSrc{}, false
	}
	path, err := engine.ParseDockerHost(s.dockerHost)
	if err != nil || !engine.SocketExists(path) {
		return foldSrc{}, false
	}
	res, err := s.probe(ctx, engine.LocalSocketTransport{Path: path})
	if err != nil {
		return foldSrc{}, false
	}
	if st, _ := statusFromProbe(res, nil, s.now()); st.State != StateOnline {
		return foldSrc{}, false
	}
	return foldSrc{socket: path, probe: res, kind: foldSourceSocket}, true
}

// FoldRemovals is what a fold of a Local environment removes with it (D-16): the counts the card
// shows before an Admin confirms.
type FoldRemovals struct {
	Stacks, GitRepos, Registries int64
	// GitCredentials, ImageUpdateIgnores, JanitorKeeps and Runs count the other stored items the
	// cascade deletes: the environment's own Git credentials, image-update ignores, kept janitor
	// items, and the history of janitor runs and image-update runs together.
	GitCredentials, ImageUpdateIgnores, JanitorKeeps, Runs int64
	// Janitor is true when the Local environment has janitor settings of its own.
	Janitor bool
}

// FoldOfferDetail is what the fold card of a swarm environment shows: whether a fold is offered,
// from where, and what it would remove, or why it is refused.
type FoldOfferDetail struct {
	// Offered is true when FoldLocal would go ahead now as far as can be told without trying it.
	Offered bool
	// Source is foldSourceLocal or foldSourceSocket (empty when there is none), Role the host's
	// role in the swarm and LocalEnvID the Local environment the fold retires.
	Source, Role, LocalEnvID string
	// Refusal is nil, ErrFoldNodeNotListed or the *IngressActiveError (D-12, D-17). A host of
	// another swarm, or none, is not a refusal: the card stays hidden.
	Refusal error
	// Ingress is the same refusal as a value, set only when ingress state blocks the fold.
	Ingress *IngressActiveError
	// Removes is set only for a fold of a Local environment (D-16).
	Removes *FoldRemovals
}

// FoldOfferDetail reads the fold for one swarm environment, for an Admin who looked at it. It
// writes nothing. It is the only reader besides FoldLocal that may probe the detected socket
// (D-15), and it does so only for an enabled, unfolded swarm environment that could take a host.
// An unknown environment is ErrNotFound.
func (s *Service) FoldOfferDetail(ctx context.Context, swarmEnvID string) (FoldOfferDetail, error) {
	rows, err := s.st.RQ.ListEnvironments(ctx)
	if err != nil {
		return FoldOfferDetail{}, fmt.Errorf("list environments: %w", err)
	}
	var row sqlc.Environment
	found := false
	for _, r := range rows {
		if r.ID == swarmEnvID {
			row, found = r, true
			break
		}
	}
	if !found {
		return FoldOfferDetail{}, ErrNotFound
	}
	if row.Kind != KindAgent || row.Enabled == 0 || row.EnvType.String != envTypeSwarm || row.LocalNodeID.Valid {
		return FoldOfferDetail{}, nil
	}
	src, ok := s.localFoldSource(rows)
	if !ok {
		src, ok = s.detectSocket(ctx, rows)
	}
	if !ok {
		return FoldOfferDetail{}, nil
	}
	p, v := s.proveFold(src, row)
	switch v {
	case verdictOffered:
	case verdictNodeNotListed:
		return FoldOfferDetail{Refusal: ErrFoldNodeNotListed}, nil
	default:
		// Another swarm's host, a cold node list (the cluster loop fills it within 15 s), a
		// cluster that was rebound: nothing to say on this card.
		return FoldOfferDetail{}, nil
	}
	d := FoldOfferDetail{Offered: true, Source: p.source, Role: p.role, LocalEnvID: p.localID}
	if p.source != foldSourceLocal {
		return d, nil
	}
	// The transaction in FoldLocal stays the authority; these are read through the read pool.
	ing, err := s.st.RQ.CountIngressStateByEnv(ctx, p.localID)
	if err != nil {
		return FoldOfferDetail{}, fmt.Errorf("count the local environment's ingress state: %w", err)
	}
	rem, err := s.st.RQ.CountFoldRemovalsByEnv(ctx, p.localID)
	if err != nil {
		return FoldOfferDetail{}, fmt.Errorf("count what a fold removes: %w", err)
	}
	d.Removes = &FoldRemovals{
		Stacks: rem.Stacks, GitRepos: rem.GitRepos, Registries: rem.Registries,
		GitCredentials: rem.GitCredentials, ImageUpdateIgnores: rem.ImageUpdateIgnores,
		JanitorKeeps: rem.JanitorKeeps, Runs: rem.Runs, Janitor: rem.Janitor > 0,
	}
	if ia := ingressActive(ing); ia != nil {
		d.Offered, d.Ingress, d.Refusal = false, ia, ia
	}
	return d, nil
}

// LocalSwarm is the swarm environment the TierPlan host's own swarm node belongs to, for the
// setup panel (D-01, D-05).
type LocalSwarm struct {
	// EnvID and EnvName name the enabled, unfolded swarm environment.
	EnvID, EnvName string
	// FoldOffered is true when folding the host into it would be offered now. False with a match
	// means the panel says it is waiting: the swarm's node list does not hold the node yet.
	FoldOffered bool
}

// LocalSwarm names the unfolded swarm environment the host behind p is a node of, and whether
// the fold into it is offered. p is the probe the caller already made of the socket, so no
// Docker call is made here and nothing is cached (D-15). A manager belongs to the environment
// bound to the cluster ID it reports; a worker, which cannot report one, to the environment
// whose managers' node list holds a manager the worker reports (the same sharing the fold
// proof asks for). A host that is no swarm node, or whose swarm no environment covers (or covers
// without a node list yet), is not matched and the panel offers to add the swarm. The first
// match in list order is returned.
func (s *Service) LocalSwarm(ctx context.Context, p dockerx.ProbeResult) (LocalSwarm, bool) {
	if p.Type != dockerx.TypeSwarmManager && p.Type != dockerx.TypeSwarmWorker {
		return LocalSwarm{}, false
	}
	rows, err := s.st.RQ.ListEnvironments(ctx)
	if err != nil {
		s.log.Warn("look for the swarm of the local host", "error", err)
		return LocalSwarm{}, false
	}
	for _, r := range rows {
		if r.Kind != KindAgent || r.Enabled == 0 || r.EnvType.String != envTypeSwarm || r.LocalNodeID.Valid || r.ClusterChangedAt.Valid {
			continue
		}
		if p.Type == dockerx.TypeSwarmManager {
			if p.SwarmClusterID == "" || p.SwarmClusterID != r.SwarmClusterID.String {
				continue
			}
		} else if !sharesManager(p.SwarmRemoteManagers, s.foldNodeList(r.ID)) {
			continue
		}
		return LocalSwarm{EnvID: r.ID, EnvName: r.Name, FoldOffered: s.foldOfferedForProbe(rows, p, r)}, true
	}
	return LocalSwarm{}, false
}

// sharesManager is true when one of the manager node IDs a host reports is a manager in nodes.
func sharesManager(reported []string, nodes []dockerx.NodeRow) bool {
	for _, n := range nodes {
		if n.Role == roleManager && slices.Contains(reported, n.ID) {
			return true
		}
	}
	return false
}

// foldOfferedForProbe is whether FoldLocal would take the host behind p into r: through the
// Local environment when there is one (what its monitor holds), else through the socket p came
// from, which needs that no environment is folded yet.
func (s *Service) foldOfferedForProbe(rows []sqlc.Environment, p dockerx.ProbeResult, r sqlc.Environment) bool {
	hasLocal, folded := false, false
	for _, e := range rows {
		hasLocal = hasLocal || e.Kind == KindLocal
		folded = folded || (e.Kind == KindAgent && e.LocalNodeID.Valid)
	}
	var src foldSrc
	switch {
	case hasLocal:
		var ok bool
		if src, ok = s.localFoldSource(rows); !ok {
			return false
		}
	case folded:
		return false
	default:
		src = foldSrc{probe: p, kind: foldSourceSocket}
	}
	_, v := s.proveFold(src, r)
	return v == verdictOffered
}
