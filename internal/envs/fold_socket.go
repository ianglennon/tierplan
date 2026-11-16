// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"fmt"

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
	d.Removes = &FoldRemovals{Stacks: rem.Stacks, GitRepos: rem.GitRepos, Registries: rem.Registries, Janitor: rem.Janitor > 0}
	if ia := ingressActive(ing); ia != nil {
		d.Offered, d.Ingress, d.Refusal = false, ia, ia
	}
	return d, nil
}
