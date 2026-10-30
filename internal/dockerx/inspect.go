// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// PortRow is one published port of a container: the container side and the host
// binding it is reachable on.
type PortRow struct {
	ContainerPort uint16
	Protocol      string
	HostIP        string
	HostPort      string
}

// NetAttachment is one network a container is attached to, with the aliases the
// container answers to on it.
type NetAttachment struct {
	ContainerNet
	Aliases []string
}

// LogConfig is the container's log driver and its options. The option values can
// carry credentials (a syslog address, a Splunk token), so a browser sees them only
// through redact.Options.
type LogConfig struct {
	Type    string
	Options map[string]string
}

// ContainerDetail is one container as its detail page shows it. Env and
// LogConfig.Options hold values that may be secrets: the API layer must send them
// through the redact package and never copy them into a response as they are.
type ContainerDetail struct {
	ID    string
	Name  string
	Image string
	// ImageID is the ID of the image the container was created from.
	ImageID string
	Created string
	// Status is the engine's state word: created, running, paused, restarting,
	// removing, exited or dead.
	Status     string
	Running    bool
	Paused     bool
	Restarting bool
	OOMKilled  bool
	Dead       bool
	Pid        int
	ExitCode   int
	// Error is the engine's last start error, empty when there is none.
	Error      string
	StartedAt  string
	FinishedAt string
	// Health is none, starting, healthy or unhealthy.
	Health       string
	RestartCount int

	Cmd        []string
	Entrypoint []string
	WorkingDir string
	User       string
	Tty        bool
	// Env is the "NAME=value" strings as the engine reports them.
	Env    []string
	Labels map[string]string
	// StopTimeout is the container's own stop timeout in seconds, nil when it sets none.
	StopTimeout *int
	StopSignal  string

	// RestartPolicy is the policy name: no, always, on-failure or unless-stopped
	// ("no" when the engine reports none). RestartMaxRetries is the on-failure maximum,
	// 0 when it is not set.
	RestartPolicy     string
	RestartMaxRetries int
	LogConfig         LogConfig
	// MemoryLimit is the container's own memory limit in bytes and NanoCPUs its CPU limit in
	// billionths of a CPU; both are 0 when the container has none. They come from the
	// container's host config, never from the stats, whose limit is the host's RAM when
	// the container has none (RESEARCH E21).
	MemoryLimit, NanoCPUs int64

	Ports    []PortRow
	Mounts   []MountRow
	Networks []NetAttachment
}

// ContainerDetail inspects one container and returns the typed detail and the raw
// inspect document. The raw bytes are for the Inspect tab, which sends them through
// the same masking; they must not be passed on as they are.
func (c *Client) ContainerDetail(ctx context.Context, id string) (ContainerDetail, []byte, error) {
	res, err := c.cur().ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerDetail{}, nil, fmt.Errorf("inspect container on %s: %w", c.t.Describe(), err)
	}
	return detailOf(res.Container), res.Raw, nil
}

// detailOf maps the engine's inspect document. Every slice and map in the result is
// non-nil, so it serialises as [] and {}.
func detailOf(in container.InspectResponse) ContainerDetail {
	d := ContainerDetail{
		ID: in.ID, Name: strings.TrimPrefix(in.Name, "/"), ImageID: in.Image, Created: in.Created,
		RestartCount: in.RestartCount, RestartPolicy: "no", Health: HealthNone,
		Cmd: []string{}, Entrypoint: []string{}, Env: []string{}, Labels: map[string]string{},
		LogConfig: LogConfig{Options: map[string]string{}},
		Ports:     []PortRow{}, Mounts: mountRows(in.Mounts), Networks: []NetAttachment{},
	}
	if s := in.State; s != nil {
		d.Status, d.Running, d.Paused, d.Restarting = string(s.Status), s.Running, s.Paused, s.Restarting
		d.OOMKilled, d.Dead, d.Pid, d.ExitCode, d.Error = s.OOMKilled, s.Dead, s.Pid, s.ExitCode, s.Error
		d.StartedAt, d.FinishedAt = s.StartedAt, s.FinishedAt
		if s.Health != nil && s.Health.Status != "" {
			d.Health = string(s.Health.Status)
		}
	}
	if cfg := in.Config; cfg != nil {
		d.Image, d.WorkingDir, d.User, d.Tty = cfg.Image, cfg.WorkingDir, cfg.User, cfg.Tty
		d.StopTimeout, d.StopSignal = cfg.StopTimeout, cfg.StopSignal
		d.Cmd, d.Entrypoint = append(d.Cmd, cfg.Cmd...), append(d.Entrypoint, cfg.Entrypoint...)
		d.Env = append(d.Env, cfg.Env...)
		for k, v := range cfg.Labels {
			d.Labels[k] = v
		}
	}
	if hc := in.HostConfig; hc != nil {
		if hc.RestartPolicy.Name != "" {
			d.RestartPolicy = string(hc.RestartPolicy.Name)
		}
		d.RestartMaxRetries = hc.RestartPolicy.MaximumRetryCount
		d.MemoryLimit, d.NanoCPUs = hc.Memory, hc.NanoCPUs
		d.LogConfig.Type = hc.LogConfig.Type
		for k, v := range hc.LogConfig.Config {
			d.LogConfig.Options[k] = v
		}
	}
	if ns := in.NetworkSettings; ns != nil {
		d.Ports = portRows(ns)
		d.Networks = attachments(ns)
	}
	return d
}

// portRows flattens the published ports, one row per host binding, sorted by
// container port, protocol, host address and host port. A port that is exposed and
// not published has no binding and no row.
func portRows(ns *container.NetworkSettings) []PortRow {
	out := []PortRow{}
	for port, bindings := range ns.Ports {
		for _, b := range bindings {
			row := PortRow{ContainerPort: port.Num(), Protocol: string(port.Proto()), HostPort: b.HostPort}
			if b.HostIP.IsValid() {
				row.HostIP = b.HostIP.String()
			}
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(a, b PortRow) int {
		return cmp.Or(cmp.Compare(a.ContainerPort, b.ContainerPort), cmp.Compare(a.Protocol, b.Protocol),
			cmp.Compare(a.HostIP, b.HostIP), cmp.Compare(a.HostPort, b.HostPort))
	})
	return out
}

// attachments maps the networks of a container, sorted by name, with the aliases of
// each.
func attachments(ns *container.NetworkSettings) []NetAttachment {
	nets := netRows(&container.NetworkSettingsSummary{Networks: ns.Networks})
	out := make([]NetAttachment, 0, len(nets))
	for _, n := range nets {
		a := NetAttachment{ContainerNet: n, Aliases: []string{}}
		if ep := ns.Networks[n.Name]; ep != nil {
			a.Aliases = append(a.Aliases, ep.Aliases...)
		}
		out = append(out, a)
	}
	return out
}
