// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import "github.com/ianglennon/tierplan/proto/ingresswire"

// This file is package ingresshub and is compiled only into the package's tests. It lets the external tests
// reach the handshake gate directly, so a test can hold slots and spend attempts on the hub's injected clock
// without opening hundreds of connections.

// GateAdmit is the hub's handshake gate admitting one handshake from ip: the release that gives its slots
// back, or the refusal a node would be sent.
func (h *Hub) GateAdmit(ip string) (release func(), refuse *ingresswire.Refuse) {
	return h.gate.admit(ip)
}

// GateEntries is how many addresses the gate's connect and enrolment buckets are tracking.
func (h *Hub) GateEntries() (connect, enrol int) {
	return h.gate.connect.Len(), h.gate.enrol.Len()
}

// EnvelopeBuilds is how many times the hub has marshalled a bundle message: once per environment, bundle ID and
// instance ID however many sessions receive it.
func (h *Hub) EnvelopeBuilds() int64 { return h.envelopes.builds.Load() }
