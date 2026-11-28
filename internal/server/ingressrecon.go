// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"log/slog"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/ingressrecon"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// wireIngressRecon builds the ingress reconciler over the server's ingress model, certificate store and notifier
// (Phase 21). It does not start it: Build runs it in the background beside the certificate manager, and it keeps
// every environment's bundle current in memory for as long as the server runs. It renders whether or not ingress is
// switched on (D-11); delivering a bundle is Phase 22's.
//
// It runs on the ACME clock, so the one WithACMEClock option moves it too. A test clock is handed over through its
// Quiet view, exactly as wireIngressCerts does: the reconciler's own timers (the poll, the safety tick and the
// debounce) would otherwise be counted by a test that waits for another manager's timer and move the clock early.
// The clock may be nil, which is the wall clock.
//
// The self-route holder gives TierPlan's own route and trust (D-14) through its SelfInput; a nil holder renders none.
func wireIngressRecon(log *slog.Logger, model *ingressstore.Store, certs *ingresscert.Store, notifier *ingress.Notifier,
	clock stackops.Clock, holder *selfRoute) *ingressrecon.Reconciler {
	if q, ok := clock.(quietable); ok {
		clock = q.Quiet()
	}
	return ingressrecon.New(ingressrecon.Deps{Model: model, Certs: certs, Notifier: notifier, Clock: clock, Log: log, Self: holder.SelfInput,
		HoldFirstRender: holder.HoldFirstRender})
}
