// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import "context"

// CodeHostUnreachable is the refusal of a standalone environment whose host is not connected: nothing can be created
// or removed on it (D-12, D-21).
const CodeHostUnreachable = "ingress_host_unreachable"

// checkHostReachable refuses a standalone environment whose one node is not connected.
func checkHostReachable(_ context.Context, _ *Service, in checkInput) ([]Finding, error) {
	if len(in.Info.Nodes) > 0 && in.Info.Nodes[0].Connected {
		return nil, nil
	}
	return []Finding{{
		Code:     CodeHostUnreachable,
		Message:  "The host is not connected to TierPlan, so the ingress proxy cannot be created or removed on it. Reconnect it and try again.",
		Severity: SeverityError,
	}}, nil
}

// standaloneChecks are the checks of a standalone host, in the order their findings are listed. A later plan adds to
// the end.
var standaloneChecks = []check{checkPaused, checkHostReachable, checkURL}

// preflightStandalone runs every check of a standalone host and returns every finding, so a person sees all that is
// wrong at once. The stacks to redeploy are listed as the swarm's preflight does.
func (s *Service) preflightStandalone(ctx context.Context, info EnvInfo, req EnableRequest) ([]Finding, []StackRef, error) {
	var out []Finding
	var redeploy []StackRef
	in := checkInput{Info: info, Req: req, cl: &clusterView{}, redeploy: &redeploy}
	for _, c := range standaloneChecks {
		fs, err := c(ctx, s, in)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, fs...)
	}
	return out, redeploy, nil
}
