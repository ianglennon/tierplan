// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"strings"
)

// The codes of the preflight refusals this file owns. Every other check of the list adds its own beside
// its function.
const (
	// CodeStandaloneUnavailable is the refusal of a standalone environment: ingress is for swarms (D-34).
	CodeStandaloneUnavailable = "ingress_standalone_unavailable"
	// CodeNoManager is the refusal of an environment no manager agent is connected to (D-07).
	CodeNoManager = "ingress_no_manager"
)

// The severities of a Finding.
const (
	// SeverityError refuses the operation.
	SeverityError = "error"
	// SeverityWarning tells the person something and lets the operation go on.
	SeverityWarning = "warning"
)

// ErrStandaloneUnavailable means ingress is not available on a standalone environment. Nothing was written.
var ErrStandaloneUnavailable = errors.New("ingress is not yet available on a standalone environment")

// Finding is one thing the preflight found: a refusal, or a warning.
type Finding struct {
	// Code is the stable name of the finding.
	Code string
	// Message is the sentence the person reads.
	Message string
	// Items name what the finding is about (a port, a stack, a hostname), empty for none.
	Items    []string
	Severity string
}

// PreflightError is an enable the preflight refused: at least one finding of severity error. Nothing was
// written.
type PreflightError struct {
	Findings []Finding
}

// Error lists the codes of the refusals.
func (e *PreflightError) Error() string {
	var codes []string
	for _, f := range e.Findings {
		if f.Severity == SeverityError {
			codes = append(codes, f.Code)
		}
	}
	return "ingress was not enabled: " + strings.Join(codes, ", ")
}

// hasErrors reports whether any finding refuses.
func hasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// checkInput is what a preflight check reads.
type checkInput struct {
	Info EnvInfo
	Req  EnableRequest
}

// check is one step of the preflight. It returns what it found, or an error when it could not look.
type check func(ctx context.Context, s *Service, in checkInput) ([]Finding, error)

// preflightChecks are the checks, in the order their findings are listed. A later plan adds to the end.
var preflightChecks = []check{checkManager}

// preflight runs every check and returns every finding, so a person sees all that is wrong at once.
func (s *Service) preflight(ctx context.Context, info EnvInfo, req EnableRequest) ([]Finding, error) {
	var out []Finding
	for _, c := range preflightChecks {
		fs, err := c(ctx, s, checkInput{Info: info, Req: req})
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	return out, nil
}

// checkManager refuses an environment no manager agent is connected to: the cluster cannot be written to
// (D-07).
func checkManager(_ context.Context, _ *Service, in checkInput) ([]Finding, error) {
	if in.Info.ManagerConnected {
		return nil, nil
	}
	return []Finding{{
		Code:     CodeNoManager,
		Message:  "No manager node of this swarm is connected, so TierPlan cannot create the ingress objects. Reconnect a manager node's agent and try again.",
		Severity: SeverityError,
	}}, nil
}
