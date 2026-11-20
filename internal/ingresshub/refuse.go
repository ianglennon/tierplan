// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import "time"

// RefuseError is what the service returns to turn a handshake away. Code is one of the ingresswire refusal
// constants and goes to the node with Detail; the outward codes are few and stable. AuditReason stays on the
// server and is one of the D-27 and D-35 reasons, so a disabled token and a wrong token look alike from
// outside while the audit log tells them apart. RetryAfter, when set, goes to the node as the time to wait
// before it tries again, for a refusal that passes.
type RefuseError struct {
	Code        string
	Detail      string
	AuditReason string
	RetryAfter  time.Duration
}

// Error names the code and the audit reason.
func (e *RefuseError) Error() string {
	if e.AuditReason != "" {
		return "ingresshub: handshake refused: " + e.Code + " (" + e.AuditReason + ")"
	}
	return "ingresshub: handshake refused: " + e.Code
}
