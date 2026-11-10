// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package acme is TierPlan's ACME client: it gets and renews TierPlan's own Let's Encrypt
// certificate through HTTP-01 (Phase 14).
//
// It drives lego's low-level acme/api package, so an order can be resumed after a restart. The pieces
// are small and separate: names and preflight (what a certificate may carry), Classify (what a failure
// means), the ledger (when a retry is allowed), the Orderer (one order, newOrder to a stored
// certificate) and the Store (everything that is persisted). A Manager puts them behind the admin's
// entry points: it runs one order at a time as a background jobs.Hub job, applies the ledger around
// every run, and hands a new certificate to an Installer, which the server implements, so this package
// never touches a listener.
//
// It must not import internal/server or internal/api, and it is never linked into the agent.
package acme
