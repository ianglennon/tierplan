// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

// The codes of the preflight refusals this file owns. Every other check of the list adds its own beside
// its function.
const (
	// CodeStandaloneUnavailable is the refusal of a standalone environment: ingress is for swarms (D-34).
	CodeStandaloneUnavailable = "ingress_standalone_unavailable"
	// CodeNoManager is the refusal of an environment no manager agent is connected to (D-07).
	CodeNoManager = "ingress_no_manager"
	// CodePortInUse is the refusal of an environment where something already publishes TCP port 80 or 443 (D-07, D-43).
	CodePortInUse = "ingress_port_in_use"
)

// The ports the proxy takes on every node.
var proxyPorts = []uint32{80, 443}

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
	// cl is read once and shared by the checks that look at the swarm.
	cl *clusterView
}

// clusterView is what one manager says of the swarm, read the first time a check asks for it.
type clusterView struct {
	read bool
	err  error
	svcs []dockerx.ServiceRow
}

// load reads the swarm through the engine once; every later call answers from the first read.
func (v *clusterView) load(ctx context.Context, s *Service, envID string) error {
	if v.read {
		return v.err
	}
	v.read = true
	v.err = s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		cr, err := c.Cluster(ctx)
		if err != nil {
			return err
		}
		v.svcs = cr.Services
		return nil
	})
	if v.err != nil {
		v.err = fmt.Errorf("read the swarm: %w", v.err)
	}
	return v.err
}

// check is one step of the preflight. It returns what it found, or an error when it could not look.
type check func(ctx context.Context, s *Service, in checkInput) ([]Finding, error)

// preflightChecks are the checks, in the order their findings are listed. A later plan adds to the end.
var preflightChecks = []check{checkManager, checkServicePorts}

// preflight runs every check and returns every finding, so a person sees all that is wrong at once.
func (s *Service) preflight(ctx context.Context, info EnvInfo, req EnableRequest) ([]Finding, error) {
	var out []Finding
	in := checkInput{Info: info, Req: req, cl: &clusterView{}}
	for _, c := range preflightChecks {
		fs, err := c(ctx, s, in)
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

// checkServicePorts refuses a swarm where a service, of a stack or of none, publishes TCP port 80 or 443 in either
// mode: the proxy publishes both on every node (D-07). The proxy's own service never counts, and a UDP port does not
// clash with it (D-43).
func checkServicePorts(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !in.Info.ManagerConnected {
		return nil, nil
	}
	if err := in.cl.load(ctx, s, in.Info.ID); err != nil {
		return nil, err
	}
	var items []string
	for _, row := range in.cl.svcs {
		if row.Ingress {
			continue
		}
		for _, p := range row.Ports {
			if p.Protocol == "tcp" && slices.Contains(proxyPorts, p.Published) {
				items = append(items, fmt.Sprintf("%s publishes %d (%s mode)", row.Name, p.Published, p.Mode))
			}
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	slices.Sort(items)
	return []Finding{{
		Code:     CodePortInUse,
		Message:  "Ports 80 and 443 must be free on every node. These publish them:",
		Items:    slices.Compact(items),
		Severity: SeverityError,
	}}, nil
}
