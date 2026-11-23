// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"fmt"
	"slices"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// CodeHostUnreachable is the refusal of a standalone environment whose host is not connected: nothing can be created
// or removed on it (D-12, D-21).
const CodeHostUnreachable = "ingress_host_unreachable"

// hostConnected is true when the standalone environment's one node is connected, so its engine can be read.
func hostConnected(info EnvInfo) bool {
	return len(info.Nodes) > 0 && info.Nodes[0].Connected
}

// checkHostReachable refuses a standalone environment whose one node is not connected.
func checkHostReachable(_ context.Context, _ *Service, in checkInput) ([]Finding, error) {
	if hostConnected(in.Info) {
		return nil, nil
	}
	return []Finding{{
		Code:     CodeHostUnreachable,
		Message:  "The host is not connected to TierPlan, so the ingress proxy cannot be created or removed on it. Reconnect it and try again.",
		Severity: SeverityError,
	}}, nil
}

// load reads the host through the node its one engine answers on, once; every later call answers from the first read.
// The caller has checked that the host is connected.
func (v *hostView) load(ctx context.Context, s *Service, info EnvInfo) error {
	if v.read {
		return v.err
	}
	v.read = true
	v.err = s.d.Engine.OnNode(ctx, info.ID, info.Nodes[0].AgentID, func(c *dockerx.Client) (err error) {
		v.published, err = c.ContainersPublishing(ctx, proxyPorts...)
		return err
	})
	if v.err != nil {
		v.err = fmt.Errorf("read the host: %w", v.err)
	}
	return v.err
}

// checkStandalonePorts refuses a host where any container, running, created or exited, binds host TCP 80 or 443 (D-12,
// D-31). A stopped container with a restart policy takes its port back when the engine starts, so a check of the running
// ones alone would let the proxy lose its port after a reboot. The proxy's own container never counts, so an enable that
// is run again after a failed one does not refuse itself. A UDP binding does not clash. A host that is not connected is
// refused by checkHostReachable.
func checkStandalonePorts(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !hostConnected(in.Info) {
		return nil, nil
	}
	if err := in.host.load(ctx, s, in.Info); err != nil {
		return nil, err
	}
	var items []string
	for _, p := range in.host.published {
		if ingress.OwnsProxyContainer(p.Labels) {
			continue
		}
		items = append(items, fmt.Sprintf("%s publishes %d (%s)", p.Name, p.Port, p.State))
	}
	if len(items) == 0 {
		return nil, nil
	}
	slices.Sort(items)
	return []Finding{{
		Code:     CodePortInUse,
		Message:  "Ports 80 and 443 must be free on this host. These containers publish them, stopped ones included, because a container with a restart policy takes its port back when the host restarts:",
		Items:    slices.Compact(items),
		Severity: SeverityError,
	}}, nil
}

// standaloneChecks are the checks of a standalone host, in the order their findings are listed. A later plan adds to
// the end.
var standaloneChecks = []check{checkPaused, checkHostReachable, checkStandalonePorts, checkURL}

// preflightStandalone runs every check of a standalone host and returns every finding, so a person sees all that is
// wrong at once. The stacks to redeploy are listed as the swarm's preflight does.
func (s *Service) preflightStandalone(ctx context.Context, info EnvInfo, req EnableRequest) ([]Finding, []StackRef, error) {
	var out []Finding
	var redeploy []StackRef
	in := checkInput{Info: info, Req: req, cl: &clusterView{}, host: &hostView{}, redeploy: &redeploy}
	for _, c := range standaloneChecks {
		fs, err := c(ctx, s, in)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, fs...)
	}
	return out, redeploy, nil
}
