// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"github.com/ianglennon/bayplan/proto/wire"
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
)

// IsReason reports whether code is one of the D-40 codes, or IssueCertExpiring.
func IsReason(code string) bool {
	_ = code
	return false
}

// ReasonFor maps an error from a call through an agent to its D-40 code, or ""
// when the error is none of the known signatures.
func ReasonFor(err error) string {
	_ = err
	return ""
}
