// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingressrecon is TierPlan's ingress reconciler. One goroutine, started by the server, keeps a
// current bundle for every environment: it renders them all when it starts, and again whenever the ingress
// notifier reports a change, after a fixed 250 ms window armed by the first wake-up (D-19, D-32). The
// version poll and the safety tick follow in later plans.
//
// The current bundle of each environment is held in memory only. Nothing is written to the database, and
// a restart rebuilds exactly the same bundles, because a bundle ID depends on the rendered content alone
// (D-20, D-07). A render whose bundle ID equals the current one publishes and notifies nothing; it only
// refreshes the version and the skipped routes that sit beside the ID (D-32).
//
// Time comes only from the injected clock, so a test drives the debounce without sleeping.
package ingressrecon
