// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// Settings reads an environment's ingress switch and counter. An environment with no ingress_env row
// gives Settings{EnvID: envID} with Exists false and ConfigVersion 0, and no error.
func (s *Store) Settings(ctx context.Context, envID string) (ingress.Settings, error) {
	row, err := s.st.RQ.GetIngressEnv(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return ingress.Settings{EnvID: envID}, nil
	}
	if err != nil {
		return ingress.Settings{}, fmt.Errorf("ingressstore: read the ingress settings of environment %q: %w", envID, err)
	}
	return ingress.Settings{
		EnvID: row.EnvID, Exists: true, Enabled: row.Enabled != 0, ConfigVersion: row.ConfigVersion,
		UpdatedAt: fromNanos(row.UpdatedAt), UpdatedBy: row.UpdatedBy,
	}, nil
}

// Snapshot reads what the renderer needs of one environment: its settings, domains, claims and routes,
// each claim and route with the name and target of the stack that holds it.
//
// Read order is the rule: the settings, and so ConfigVersion, are read first, then the rest. The reads
// are not one transaction, so a write can land between them. Because the version was read first, such a
// write is rendered again under its newer version on the next poll; an older state is never rendered
// under a newer version. Reading the version last would invert that.
func (s *Store) Snapshot(ctx context.Context, envID string) (ingress.Snapshot, error) {
	set, err := s.Settings(ctx, envID)
	if err != nil {
		return ingress.Snapshot{}, err
	}
	domains, err := s.st.RQ.ListIngressDomainsByEnv(ctx, envID)
	if err != nil {
		return ingress.Snapshot{}, fmt.Errorf("ingressstore: list the domains of environment %q: %w", envID, err)
	}
	claims, err := s.st.RQ.ListIngressClaimsByEnv(ctx, envID)
	if err != nil {
		return ingress.Snapshot{}, fmt.Errorf("ingressstore: list the claims of environment %q: %w", envID, err)
	}
	routes, err := s.st.RQ.ListIngressRoutesByEnv(ctx, envID)
	if err != nil {
		return ingress.Snapshot{}, fmt.Errorf("ingressstore: list the routes of environment %q: %w", envID, err)
	}
	snap := ingress.Snapshot{
		Settings: set,
		Domains:  make([]ingress.Domain, 0, len(domains)),
		Claims:   make([]ingress.SnapshotClaim, 0, len(claims)),
		Routes:   make([]ingress.SnapshotRoute, 0, len(routes)),
	}
	for _, d := range domains {
		snap.Domains = append(snap.Domains, domainOf(d))
	}
	for _, c := range claims {
		snap.Claims = append(snap.Claims, snapshotClaimOf(c))
	}
	for _, r := range routes {
		snap.Routes = append(snap.Routes, snapshotRouteOf(r))
	}
	return snap, nil
}

// Versions reads the config and certificate versions of every environment that has an ingress_env row,
// ordered by environment id, in one query. A stack deleted by cascade raises its environment's
// ConfigVersion in it without anybody being notified, so the reconciler's poll reads this.
func (s *Store) Versions(ctx context.Context) ([]ingress.EnvVersion, error) {
	rows, err := s.st.RQ.ListIngressEnvVersions(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the ingress versions: %w", err)
	}
	out := make([]ingress.EnvVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, ingress.EnvVersion{EnvID: r.EnvID, ConfigVersion: r.ConfigVersion, CertsVersion: r.CertsVersion})
	}
	return out, nil
}

// snapshotClaimOf converts a claim row; the stack columns are NULL for a system claim.
func snapshotClaimOf(c sqlc.ListIngressClaimsByEnvRow) ingress.SnapshotClaim {
	return ingress.SnapshotClaim{
		Claim: ingress.Claim{
			EnvID: c.EnvID, Hostname: c.Hostname, DomainID: c.DomainID, OwnerKind: ingress.OwnerKind(c.OwnerKind),
			StackID: c.StackID.String, ClaimedBy: c.ClaimedBy, ClaimedAt: fromNanos(c.ClaimedAt),
		},
		StackName: c.StackName.String,
		Target:    ingress.Target(c.StackTarget.String),
	}
}

// snapshotRouteOf converts a route row; the stack columns are NULL for a system claim's route.
func snapshotRouteOf(r sqlc.ListIngressRoutesByEnvRow) ingress.SnapshotRoute {
	return ingress.SnapshotRoute{
		Route: ingress.Route{
			ID: r.ID, EnvID: r.EnvID, Hostname: r.Hostname, Name: r.Name, PathPrefix: r.PathPrefix,
			Service: r.Service, Port: int(r.Port), Scheme: ingress.Scheme(r.Scheme), Strip: r.Strip != 0,
			CreatedAt: fromNanos(r.CreatedAt), UpdatedAt: fromNanos(r.UpdatedAt),
		},
		OwnerKind: ingress.OwnerKind(r.OwnerKind),
		StackID:   r.StackID.String,
		StackName: r.StackName.String,
		Target:    ingress.Target(r.StackTarget.String),
	}
}
