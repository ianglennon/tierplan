// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"errors"
	"strings"

	"github.com/ianglennon/coaming/internal/tunnel"
	"github.com/ianglennon/coaming/proto/wire"
)

// The D-40 reason codes, as the wire protocol names them. Nothing an agent says
// becomes a reason: the server maps what it observed to one of these.
const (
	ReasonAgentOffline        = wire.ReasonAgentOffline
	ReasonTLSUntrusted        = wire.ReasonTLSUntrusted
	ReasonAuthRejected        = wire.ReasonAuthRejected
	ReasonCertExpired         = wire.ReasonCertExpired
	ReasonRevoked             = wire.ReasonRevoked
	ReasonBlocked             = wire.ReasonBlocked
	ReasonVersionIncompatible = wire.ReasonVersionIncompatible
	ReasonClockSkew           = wire.ReasonClockSkew
	ReasonTimeout             = wire.ReasonTimeout
	ReasonNotAManager         = wire.ReasonNotAManager
	ReasonNoQuorum            = wire.ReasonNoQuorum
	ReasonClusterChanged      = wire.ReasonClusterChanged
	ReasonEngineUnreachable   = wire.ReasonEngineUnreachable
	ReasonUnsupportedEngine   = wire.ReasonUnsupportedEngine
	ReasonAgentPolicyDenied   = wire.ReasonAgentPolicyDenied
	// ReasonUnknownAgent is D-31: the agent holds a certificate this environment
	// issued, but the agent is not in the database, as after a restore from an
	// older backup.
	ReasonUnknownAgent = wire.ReasonUnknownAgent
)

// reasons is the fixed set of codes: the D-40 codes, unknown_agent (D-31) and the
// certificate issue.
var reasons = map[string]struct{}{
	ReasonAgentOffline: {}, ReasonTLSUntrusted: {}, ReasonAuthRejected: {}, ReasonCertExpired: {}, ReasonRevoked: {},
	ReasonBlocked: {}, ReasonVersionIncompatible: {}, ReasonClockSkew: {}, ReasonTimeout: {}, ReasonNotAManager: {},
	ReasonNoQuorum: {}, ReasonClusterChanged: {}, ReasonEngineUnreachable: {}, ReasonUnsupportedEngine: {},
	ReasonAgentPolicyDenied: {}, ReasonUnknownAgent: {}, IssueCertExpiring: {},
}

// IsReason reports whether code is one of the D-40 codes, or IssueCertExpiring.
func IsReason(code string) bool {
	_, ok := reasons[code]
	return ok
}

// The bodies a swarm daemon answers a cluster question with when it cannot (RESEARCH
// Pattern 5, read from real Engine 29 daemons): a worker, and a manager that has
// lost quorum.
const (
	workerBodyMarker = "This node is not a swarm manager"
	quorumBodyMarker = "The swarm does not have a leader"
)

// ReasonFor maps an error from a call through an agent to its D-40 code, or ""
// when the error is none of the known signatures. The answer is always one of the
// fixed codes: nothing an agent or a daemon wrote is passed on.
func ReasonFor(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(ClassifyDockerError(err), ErrAgentPolicyDenied) {
		return ReasonAgentPolicyDenied
	}
	var refusal *tunnel.RefuseError
	if errors.As(err, &refusal) {
		if IsReason(refusal.Code) {
			return refusal.Code
		}
		return ""
	}
	switch {
	case errors.Is(err, ErrNoManager):
		return ReasonNotAManager
	case errors.Is(err, tunnel.ErrAgentGone):
		return ReasonAgentOffline
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, workerBodyMarker):
		return ReasonNotAManager
	case strings.Contains(msg, quorumBodyMarker):
		return ReasonNoQuorum
	}
	return ""
}
