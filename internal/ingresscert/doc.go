// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingresscert keeps one DNS-01 certificate per ingress hostname (Phase 20), beside TierPlan's
// own certificate, which stays in internal/acme and its tls_certs row "acme" exactly as it was.
//
// A certificate is a row of ingress_certs. Live rows are keyed by hostname for the whole install (a
// partial unique index), carry the environment the hostname belongs to, and keep their private key
// sealed under the row's own id, so a hostname that moves to another environment keeps its row and
// its key. Every certificate write raises ingress_env.certs_version of the environments it touches
// and never config_version: a changed certificate is not a changed route.
//
// The package implements the acme.OrderStore and acme.AccountStore seams over its own tables, so the
// ACME order machinery of internal/acme runs unchanged against it.
//
// The package must not import internal/stacks or internal/server; the server composes it, not the
// other way round.
package ingresscert
