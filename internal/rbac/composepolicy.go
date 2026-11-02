// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rbac

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ianglennon/tierplan/internal/rbac/compose"
)

// Decision reasons of CheckComposeDeploy.
const (
	// ReasonPrivilegedCompose is a compose file with a host-escalation finding,
	// asked for by someone below env Admin on the environment (D-13).
	ReasonPrivilegedCompose = "privileged_compose"
	// ReasonComposeUnreadable is a compose file that is too large or does not
	// parse. It is refused for everyone, instance admins included, because a
	// file that cannot be read cannot be shown safe (D-13 fails closed).
	ReasonComposeUnreadable = "compose_unreadable"
)

// ComposeDecision is the outcome of CheckComposeDeploy.
type ComposeDecision struct {
	// Allowed is false when the file may not be deployed by this caller.
	Allowed bool
	// Reason is ReasonPrivilegedCompose or ReasonComposeUnreadable when Allowed
	// is false.
	Reason string
	// Findings are the host-escalation features found, returned whether or not
	// the caller may deploy them, so a review can show them to an Admin.
	Findings []compose.Finding
	// Message names each offending key and line, one per line, as
	// services.<name>.<key> (line N): <value>. It is empty when Allowed.
	Message string
}

// CheckComposeDeploy is the one function the stack phases call before they
// review or deploy a compose file (D-13, AUTH-10). It judges only host
// escalation: a file with none is allowed here, and the caller still checks its
// own deploy permission. A file with any finding needs env Admin on envID, or
// instance admin; for a system principal acting on behalf of an owner, the
// owner's current access decides (D-14), so an automation never deploys what its
// owner could not.
//
// It reads the raw file, which cannot see include: or extends: targets or
// environment interpolation; an unresolved ${...} is itself a finding. The
// stack phases call it twice, at review and deploy time over the raw file, and
// again over the resolved compose-go project just before deploying (RESEARCH
// A6). A database error comes back as err with a refusing decision.
func (c *Checker) CheckComposeDeploy(ctx context.Context, p *Principal, envID string, src []byte) (ComposeDecision, error) {
	findings, err := compose.Check(src)
	return c.decideCompose(ctx, p, envID, findings, err)
}

// CheckComposeDeployResolved is CheckComposeDeploy for a caller that knows the variables of the
// file: each interpolated value is judged by what lookup gives for its names (the stack's table,
// with the defaults the file writes), at the line it was written, so a bind source written as
// ${DATA} is judged by DATA's value and the findings keep the numbers of the file the person
// edits. A name with no value and no default is still a finding. The decision, the Admin rule and
// the on-behalf-of rule are CheckComposeDeploy's. The stack phases call it at review and again
// just before a deploy, beside CheckServiceSpec on each converted service.
func (c *Checker) CheckComposeDeployResolved(ctx context.Context, p *Principal, envID string, src []byte, lookup func(name string) (string, bool)) (ComposeDecision, error) {
	findings, err := compose.CheckResolved(src, lookup)
	return c.decideCompose(ctx, p, envID, findings, err)
}

// decideCompose turns the findings of a check into the decision for one caller.
func (c *Checker) decideCompose(ctx context.Context, p *Principal, envID string, findings []compose.Finding, err error) (ComposeDecision, error) {
	if err != nil {
		msg := "The compose file can't be read: it isn't valid YAML."
		if errors.Is(err, compose.ErrTooLarge) {
			msg = "The compose file is larger than 1 MiB."
		}
		return ComposeDecision{Reason: ReasonComposeUnreadable, Message: msg}, nil
	}
	if !compose.NeedsEnvAdmin(findings) {
		return ComposeDecision{Allowed: true, Findings: findings}, nil
	}
	a, err := c.Access(ctx, p)
	if err != nil {
		return ComposeDecision{Reason: ReasonPrivilegedCompose, Findings: findings}, fmt.Errorf("rbac: check compose deploy: %w", err)
	}
	if a.Active && a.For(envID).Role >= RoleAdmin {
		return ComposeDecision{Allowed: true, Findings: findings}, nil
	}
	lines := make([]string, len(findings))
	for i, f := range findings {
		lines[i] = compose.Format(f)
	}
	return ComposeDecision{Reason: ReasonPrivilegedCompose, Findings: findings, Message: strings.Join(lines, "\n")}, nil
}
