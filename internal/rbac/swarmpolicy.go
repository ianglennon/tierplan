// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package rbac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/rbac/compose"
	"github.com/ianglennon/coaming/internal/rbac/swarmspec"
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
	// Allowed is false when the spec may not be applied by this caller.
	Allowed bool
	// Reason is ReasonPrivilegedService or ReasonServiceSpecUnreadable when
	// Allowed is false.
	Reason string
	// Findings are the host-escalation features found, returned whether or not
	// the caller may have them, so a review can show them to an Admin.
	Findings []compose.Finding
	// Message names each offending setting, one per line, as {path}: {value}. It
	// is empty when Allowed, and holds the decoder's words when the spec is
	// unreadable.
	Message string
}

// CheckServiceSpec is the one function the service phases call before they
// review or apply a service spec (D-13, AUTH-10). Like CheckComposeDeploy it
// judges only host escalation: a spec with none is allowed here, and the caller
// still checks its own permission. A spec with any finding needs env Admin on
// envID, or instance admin; for a system principal acting on behalf of an owner
// the owner's current access decides, so an automation never applies what its
// owner could not.
//
// raw is the spec exactly as it will be forwarded. It is decoded strictly
// (unknown fields and trailing data are refused), so what is judged is what
// Docker would act on, never editor text. A spec that does not decode is refused
// for everyone, instance admins included, because a spec that cannot be read
// cannot be shown safe. resolve lets a network given by ID be recognised as the
// host network; it may be nil. A database error comes back as err with a
// refusing decision.
func (c *Checker) CheckServiceSpec(ctx context.Context, p *Principal, envID string, raw []byte, resolve swarmspec.NetworkResolver) (ServiceDecision, error) {
	var spec swarm.ServiceSpec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&spec)
	if err == nil {
		if _, more := dec.Token(); !errors.Is(more, io.EOF) {
			err = errors.New("unexpected data after the spec")
		}
	}
	if err != nil {
		return ServiceDecision{Reason: ReasonServiceSpecUnreadable, Message: "The service spec can't be read: " + err.Error()}, nil
	}
	findings := swarmspec.Check(spec, resolve)
	if len(findings) == 0 {
		return ServiceDecision{Allowed: true}, nil
	}
	a, err := c.Access(ctx, p)
	if err != nil {
		return ServiceDecision{Reason: ReasonPrivilegedService, Findings: findings}, fmt.Errorf("rbac: check service spec: %w", err)
	}
	if a.Active && a.For(envID).Role >= RoleAdmin {
		return ServiceDecision{Allowed: true, Findings: findings}, nil
	}
	lines := make([]string, len(findings))
	for i, f := range findings {
		lines[i] = swarmspec.FormatSpec(f)
	}
	return ServiceDecision{Reason: ReasonPrivilegedService, Findings: findings, Message: strings.Join(lines, "\n")}, nil
}
