// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"

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
