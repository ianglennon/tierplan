// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

import (
	"context"

	"github.com/ianglennon/bayplan/internal/rbac/compose"
	"github.com/ianglennon/bayplan/internal/rbac/swarmspec"
)

// Decision reasons of CheckServiceSpec.
const (
	// ReasonPrivilegedService is a service spec with a host-escalation finding,
	// asked for by someone below env Admin on the environment (D-13).
	ReasonPrivilegedService = "privileged_service"
	// ReasonServiceSpecUnreadable is a spec that does not decode strictly into a
	// swarm service spec. It is refused for everyone, instance admins included.
	ReasonServiceSpecUnreadable = "service_spec_unreadable"
)

// ServiceDecision is the outcome of CheckServiceSpec.
type ServiceDecision struct {
	Allowed  bool
	Reason   string
	Findings []compose.Finding
	Message  string
}

// CheckServiceSpec judges the host escalation of a service spec.
func (c *Checker) CheckServiceSpec(ctx context.Context, p *Principal, envID string, raw []byte, resolve swarmspec.NetworkResolver) (ServiceDecision, error) {
	return ServiceDecision{}, nil
}
