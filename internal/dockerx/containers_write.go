// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"fmt"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// The calls below are the building blocks of a container recreate (P12 D-07): read the whole
// inspect document of a container, create its replacement beside it, connect the replacement's
// further networks and swap the names (D-12). Like every write here they send once and never
// retry, since a write whose answer was lost may have happened (P2 D-42). The errors keep the
// engine's type, so IsNotFound, IsConflict, IsRefusal and IsPolicyDenied tell the caller what
// happened; the engine's own words stay in the error and never go into an API answer.

// ContainerInspectFull inspects one container and returns the engine's whole inspect document
// as typed values: configuration, host configuration, mounts and network attachments. It is the
// input of a recreate. The document is the container's own, secrets in its environment
// included, so it stays inside the server.
func (c *Client) ContainerInspectFull(ctx context.Context, id string) (container.InspectResponse, error) {
	res, err := c.cur().ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("inspect container on %s: %w", c.t.Describe(), err)
	}
	return res.Container, nil
}

// ContainerCreate creates a container named name from cfg, host and net, without starting it,
// and returns its ID and the engine's warnings. An empty name lets the engine choose one. The
// image must already be on the node: the engine does not pull for a create. A name already in
// use is a conflict (IsConflict). Engines before API 1.44 accept one network in net; the
// caller connects the others before it starts the container (NetworkConnectEndpoint).
func (c *Client) ContainerCreate(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig, net *network.NetworkingConfig) (id string, warnings []string, err error) {
	res, err := c.cur().ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name, Config: cfg, HostConfig: host, NetworkingConfig: net,
	})
	if err != nil {
		return "", nil, fmt.Errorf("create container on %s: %w", c.t.Describe(), err)
	}
	return res.ID, res.Warnings, nil
}

// ContainerRename gives the container id the name newName. A name already in use is a
// conflict (IsConflict) and an unknown container is not found (IsNotFound).
func (c *Client) ContainerRename(ctx context.Context, id, newName string) error {
	if _, err := c.cur().ContainerRename(ctx, id, client.ContainerRenameOptions{NewName: newName}); err != nil {
		return fmt.Errorf("rename container on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// NetworkConnectEndpoint connects the container to the network with the endpoint settings es
// as they are: aliases, static addresses, MAC address, links and driver options. NetworkConnect
// is the call for a person's choices; this one carries an endpoint a recreate copied from
// another container. A nil es connects with the network's defaults.
func (c *Client) NetworkConnectEndpoint(ctx context.Context, networkID, containerID string, es *network.EndpointSettings) error {
	if _, err := c.cur().NetworkConnect(ctx, networkID, client.NetworkConnectOptions{Container: containerID, EndpointConfig: es}); err != nil {
		return fmt.Errorf("connect to network on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// IsPolicyDenied reports whether err is a node's agent refusing a call by its own policy,
// which it words "agent_policy_denied": a 403 the engine itself never sends in these words.
func IsPolicyDenied(err error) bool {
	return err != nil && cerrdefs.IsPermissionDenied(err) && strings.Contains(err.Error(), "agent_policy_denied")
}
