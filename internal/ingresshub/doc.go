// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingresshub is the server end of the ingress channel: per-node
// enrolment with a join token, connect with the node's stored Ed25519 key,
// revoke, and the read-only WebSocket that delivers an environment's current
// bundle (Phase 22).
//
// The trust model is short. The join token admits enrolment only: it lets a
// host that holds it register a public key, and it is checked against the swarm
// node list so the node it names exists. Afterwards the node proves its key
// over a fresh server nonce, the audience and the TLS exporter value, so a
// recorded exchange replays nowhere. The channel accepts no configuration from
// a node: it says hello, applied, rejected and health, and the server pushes
// every bundle.
//
// The ingress node is its own identity with its own token kind (tpit_) and its
// own table, beside the agent (internal/agents) and not a generalisation of it.
package ingresshub
