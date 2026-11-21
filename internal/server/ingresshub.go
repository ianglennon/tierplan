// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/buildinfo"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingressrecon"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
)

// ingressHubDeps are what wireIngressHub reads from the rest of the server.
type ingressHubDeps struct {
	Log   *slog.Logger
	Store *store.Store
	Audit *audit.Writer
	// Instance is the server's settings: the audience of every node handshake is the host of its agent base.
	Instance *instance.Instance
	// Pins are the certificate pins a welcome carries, read when asked.
	Pins func() []string
	// Recon is the reconciler whose current bundle the hub delivers.
	Recon *ingressrecon.Reconciler
	// Ingress is the store whose switch decides whether an environment's nodes are sent bundles, and Changes the notifier
	// it tells when the switch moves, so the sessions of that environment look at the current bundle again (D-12).
	Ingress *ingressstore.Store
	Changes *ingress.Notifier
	// Nodes is the swarm node list an enrolment into a swarm environment is checked against.
	Nodes agents.NodeLists
	// Refusals records the refused handshakes. It is the hub's own throttle, apart from the agent hub's.
	Refusals auth.AuditRecorder
	// UpgradeFailure hears of a TLS request to /ingress/connect that is no WebSocket upgrade: the proxy observer's
	// websocket_failing signal, as for the agent endpoint. Nil hears nothing.
	UpgradeFailure func(peer string)
}

// wireIngressHub mints the instance ID, which must exist before the hub serves a node (D-23), and builds the ingress node
// service and the hub that serves /ingress/connect (D-29). Every clock is time.Now, passed explicitly (D-36), although
// each constructor would treat nil the same way. The service's kicker is the hub, so a revoke that commits closes the
// node's session. The caller starts Run on the hub and closes it on every path.
func wireIngressHub(ctx context.Context, d ingressHubDeps) (*ingresshub.Service, *ingresshub.Hub, error) {
	if _, err := instance.EnsureInstanceID(ctx, d.Store, time.Now); err != nil {
		return nil, nil, fmt.Errorf("start the ingress channel: %w", err)
	}
	svc := ingresshub.NewService(d.Store, d.Audit, time.Now).WithNodeLists(d.Nodes)
	hub := ingresshub.NewHub(svc, d.Recon, d.Log, time.Now).
		WithPublicURL(d.Instance.AgentBase).
		WithPins(d.Pins).
		WithInstanceID(func(ctx context.Context) (string, error) { return instance.InstanceID(ctx, d.Store) }).
		WithServerVersion(buildinfo.Get().Version).
		WithRefusalAudit(d.Refusals).
		OnUpgradeFailure(d.UpgradeFailure).
		WithDeliveryGate(deliveryGate(d.Log, d.Ingress)).
		WithEnvChanges(d.Changes)
	svc.WithKicker(hub)
	return svc, hub, nil
}

// deliveryGate says whether an environment's nodes are sent bundles: only when its ingress is switched on. A switch that
// cannot be read answers false and is logged, so a failure sends nothing (D-12).
func deliveryGate(log *slog.Logger, store *ingressstore.Store) func(context.Context, string) bool {
	return func(ctx context.Context, envID string) bool {
		set, err := store.Settings(ctx, envID)
		if err != nil {
			log.Warn("could not read an environment's ingress switch; no bundle is sent to its nodes", "environment_id", envID, "error", err)
			return false
		}
		return set.Enabled
	}
}

// joinDialHosts is the dial hosts of every source together, in order. A nil source is skipped, and a source that has
// none adds none.
func joinDialHosts(sources ...func() []string) func() []string {
	return func() []string {
		var hosts []string
		for _, src := range sources {
			if src == nil {
				continue
			}
			hosts = append(hosts, src()...)
		}
		return hosts
	}
}
