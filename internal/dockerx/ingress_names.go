// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// The engine keeps a container's endpoint under the name of the network it joined and, until the container has started
// on it, looks that name up again at start: by name, then by ID prefix. A name that is the start of the ingress
// network's ID therefore reaches the ingress network once no network carries it, however the endpoint was made (a stack
// deploy, TierPlan's network connect, or a connect made before ingress was switched on). These are the checks outside a
// stack deploy that keep such an endpoint from putting a chosen name on the shared bridge (T-25-47).

// ingressStartRefusal is ErrIngressName when starting container id would put a name on the ingress network other than
// the ones a stack deploy gives: an endpoint or network mode stored under the ingress network's name or the start of its
// ID, on a container whose name, hostname or aliases there are not its own. It is nil for any other container, and when
// the host has no ingress network.
func ingressStartRefusal(ctx context.Context, api client.APIClient, id string) error {
	res, err := api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if IsNotFound(err) {
		return nil // nothing to start: the start itself says so
	}
	if err != nil {
		return fmt.Errorf("dockerx: read container %s before starting it: %w", id, err)
	}
	insp := res.Container
	ingressID, err := guardClient{APIClient: api}.ingressResolver().ingressID(ctx)
	if err != nil {
		return err
	}
	if ingressID == "" {
		return nil
	}
	reaches := func(name string) bool { return name == ingress.NetworkName || prefixOf(ingressID, name) }
	on := false
	var aliases []string
	if insp.HostConfig != nil && reaches(string(insp.HostConfig.NetworkMode)) {
		on = true
	}
	if insp.NetworkSettings != nil {
		for name, ep := range insp.NetworkSettings.Networks {
			if !reaches(name) {
				continue
			}
			on = true
			if ep != nil {
				aliases = append(aliases, ep.Aliases...)
			}
		}
	}
	if !on {
		return nil
	}
	var labels map[string]string
	var hostname string
	if insp.Config != nil {
		labels, hostname = insp.Config.Labels, insp.Config.Hostname
	}
	ownID := insp.ID
	if len(ownID) > 12 {
		ownID = ownID[:12]
	}
	return ingressNamesAllowed(insp.Name, hostname, ownID, labels, aliases)
}

// IngressLaunchRefusal is ingressStartRefusal on this client's engine (a read: the start itself is ContainerAction), for TierPlan's own start and restart of a
// container: an error wrapping ErrIngressName refuses the action.
func (c *Client) IngressLaunchRefusal(ctx context.Context, id string) error {
	return ingressStartRefusal(ctx, c.cur(), id)
}

// ShadowsIngressNetwork reports whether name, a network's name on this client's engine, is the start of the ingress
// network's ID (and not the ingress network's own name): a container that joins it would reach the ingress network once
// it is gone. It is false when the engine has no ingress network.
func (c *Client) ShadowsIngressNetwork(ctx context.Context, name string) (bool, error) {
	if name == ingress.NetworkName {
		return false, nil
	}
	id, err := guardClient{APIClient: c.cur()}.ingressResolver().ingressID(ctx)
	if err != nil {
		return false, fmt.Errorf("on %s: %w", c.t.Describe(), err)
	}
	return prefixOf(id, strings.TrimSpace(name)), nil
}
