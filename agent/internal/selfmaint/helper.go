// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package selfmaint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/distribution/reference"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	// ConnectTimeout is how long the helper waits for the new agent to connect before
	// it puts the old one back (D-24).
	ConnectTimeout = 90 * time.Second
	// oldName is what the running agent is renamed to while its replacement is tried.
	oldName = "bayplan-agent-old"
	// rollbackTimeout bounds the rollback, which must finish even when the helper's
	// own context was cancelled.
	rollbackTimeout = time.Minute
	// defaultPoll is how often the helper looks at the new container and the marker.
	defaultPoll = time.Second
)

// ErrRolledBack is returned by RecreateAgent when the new agent did not come up
// and the old one was put back. The helper exits non-zero on it.
var ErrRolledBack = errors.New("the new agent did not connect; the old agent was restored")

// RecreateOptions is what RecreateAgent needs.
type RecreateOptions struct {
	// SocketPath is the Docker socket. Empty means /var/run/docker.sock.
	SocketPath string
	// TargetID is the container to replace: the agent that started this helper.
	TargetID string
	// SelfID is the helper's own container ID or hostname. Empty means ResolveSelfID().
	// The helper's own container tells it which image the new agent runs.
	SelfID string
	// Marker returns the time the agent's connected marker was last written, or the
	// zero time when there is none. The new agent has connected when the marker is
	// newer than the moment the helper started it.
	Marker func() (time.Time, error)
	// Timeout is how long to wait for that. Zero means ConnectTimeout.
	Timeout time.Duration
	// Poll is how often to check. Zero means one second.
	Poll time.Duration
	// Log receives one line per step. Nil means slog.Default().
	Log *slog.Logger
}

// RecreateAgent replaces the target agent container with one from the helper's own
// image, configured identically and on the same state volume, and puts the old one
// back if the new one does not connect in time (D-24). It only touches a target
// labelled bayplan.agent=true that is not itself a helper and whose image is in the
// helper's repository. The order is rename, stop, create, start, wait: the old
// agent is stopped before the new one starts because both would hold one identity
// and one state volume.
func RecreateAgent(ctx context.Context, opts RecreateOptions) error {
	return newRecreator(opts).run(ctx)
}

type recreator struct {
	opts RecreateOptions
	log  *slog.Logger
}

func newRecreator(opts RecreateOptions) *recreator {
	if opts.SocketPath == "" {
		opts.SocketPath = "/var/run/docker.sock"
	}
	if opts.SelfID == "" {
		opts.SelfID = ResolveSelfID()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = ConnectTimeout
	}
	if opts.Poll <= 0 {
		opts.Poll = defaultPoll
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &recreator{opts: opts, log: log}
}

func (r *recreator) run(ctx context.Context) error {
	if r.opts.Marker == nil {
		return errors.New("recreate: no way to read the connected marker")
	}
	cli, err := client.New(client.WithHost("unix://" + r.opts.SocketPath))
	if err != nil {
		return fmt.Errorf("recreate: build the Docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	// The helper's own container names the new image, and proves this is a helper.
	self, err := cli.ContainerInspect(ctx, r.opts.SelfID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("recreate: inspect the helper's own container: %w", err)
	}
	if self.Container.Config == nil || self.Container.Config.Labels[AgentLabel] != "true" || self.Container.Config.Labels[HelperLabel] != "true" {
		return errors.New("recreate: this container is not a Bayplan agent helper")
	}
	newImageRef := self.Container.Config.Image
	helperRepo, err := reference.ParseNormalizedNamed(newImageRef)
	if err != nil {
		return fmt.Errorf("recreate: the helper's image %q is not a reference: %w", newImageRef, err)
	}

	tr, err := cli.ContainerInspect(ctx, r.opts.TargetID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("recreate: inspect the target: %w", err)
	}
	target := tr.Container
	if target.Config == nil || target.Config.Labels[AgentLabel] != "true" {
		return errors.New("recreate: the target is not labelled bayplan.agent=true; refusing to touch it")
	}
	if target.Config.Labels[HelperLabel] == "true" {
		return errors.New("recreate: the target is a helper, not an agent; refusing to touch it")
	}
	targetRepo, err := reference.ParseNormalizedNamed(target.Config.Image)
	if err != nil || targetRepo.Name() != helperRepo.Name() {
		return fmt.Errorf("recreate: the target runs %q, which is not in the helper's repository %s; refusing to touch it", target.Config.Image, helperRepo.Name())
	}

	create := r.buildCreate(ctx, cli, target, newImageRef)
	name := strings.TrimPrefix(target.Name, "/")
	if name == "" {
		return errors.New("recreate: the target has no name")
	}

	// From here on the old agent is changed, and a failure puts it back. The rename
	// comes first because it is the cheapest step to fail (the old name may be taken).
	if _, err := cli.ContainerRename(ctx, target.ID, client.ContainerRenameOptions{NewName: oldName}); err != nil {
		return fmt.Errorf("recreate: rename the old agent to %s: %w", oldName, err)
	}
	r.log.Info("recreate: the old agent was renamed", "to", oldName)
	if _, err := cli.ContainerStop(ctx, target.ID, client.ContainerStopOptions{}); err != nil {
		return r.rollback(ctx, cli, target.ID, name, "", "stop the old agent: "+err.Error())
	}

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name, Config: create.config, HostConfig: create.host, NetworkingConfig: create.network,
	})
	if err != nil {
		return r.rollback(ctx, cli, target.ID, name, "", "create the new agent: "+err.Error())
	}
	newID := created.ID
	for _, nw := range create.extra {
		if _, err := cli.NetworkConnect(ctx, nw.name, client.NetworkConnectOptions{Container: newID, EndpointConfig: nw.ep}); err != nil {
			return r.rollback(ctx, cli, target.ID, name, newID, "connect the new agent to network "+nw.name+": "+err.Error())
		}
	}

	started := time.Now()
	if _, err := cli.ContainerStart(ctx, newID, client.ContainerStartOptions{}); err != nil {
		return r.rollback(ctx, cli, target.ID, name, newID, "start the new agent: "+err.Error())
	}
	r.log.Info("recreate: the new agent was started; waiting for it to connect", "image", newImageRef, "timeout", r.opts.Timeout)

	if reason := r.waitConnected(ctx, cli, newID, started); reason != "" {
		return r.rollback(ctx, cli, target.ID, name, newID, reason)
	}

	if _, err := cli.ContainerRemove(ctx, target.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
		// The update worked; a leftover only blocks the name for the next one.
		r.log.Warn("recreate: the new agent is up but the old container could not be removed; remove it with docker rm -f "+oldName, "error", err)
	}
	r.log.Info("recreate: the agent was updated", "image", newImageRef)
	return nil
}

// waitConnected polls until the connected marker is newer than started. It returns
// "" on success and the reason for a rollback otherwise: the new container stopped
// or keeps restarting, the context ended, or the time ran out.
func (r *recreator) waitConnected(ctx context.Context, cli *client.Client, newID string, started time.Time) string {
	deadline := time.NewTimer(r.opts.Timeout)
	defer deadline.Stop()
	tick := time.NewTicker(r.opts.Poll)
	defer tick.Stop()
	for {
		ins, err := cli.ContainerInspect(ctx, newID, client.ContainerInspectOptions{})
		switch {
		case err != nil && ctx.Err() == nil:
			return "inspect the new agent: " + err.Error()
		case err == nil && (ins.Container.State == nil || !ins.Container.State.Running || ins.Container.State.Restarting):
			code := 0
			if ins.Container.State != nil {
				code = ins.Container.State.ExitCode
			}
			return fmt.Sprintf("the new agent is not running (exit code %d)", code)
		}
		if err == nil {
			if at, merr := r.opts.Marker(); merr != nil {
				r.log.Warn("recreate: cannot read the connected marker", "error", merr)
			} else if at.After(started) {
				return ""
			}
		}
		select {
		case <-ctx.Done():
			return "the helper was stopped: " + ctx.Err().Error()
		case <-deadline.C:
			return fmt.Sprintf("the new agent did not connect within %s", r.opts.Timeout)
		case <-tick.C:
		}
	}
}

// rollback removes the new container, if there is one, gives the old agent its name
// back and starts it. It runs on its own context so that a cancelled helper still
// restores the agent. It always returns an error wrapping ErrRolledBack.
func (r *recreator) rollback(parent context.Context, cli *client.Client, oldID, name, newID, reason string) error {
	r.log.Error("recreate failed; putting the old agent back", "reason", reason)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), rollbackTimeout)
	defer cancel()
	if newID != "" {
		if _, err := cli.ContainerRemove(ctx, newID, client.ContainerRemoveOptions{Force: true}); err != nil {
			r.log.Error("rollback: cannot remove the new container", "id", newID, "error", err)
		}
	}
	if _, err := cli.ContainerRename(ctx, oldID, client.ContainerRenameOptions{NewName: name}); err != nil {
		r.log.Error("rollback: cannot give the old agent its name back", "name", name, "error", err)
	}
	if _, err := cli.ContainerStart(ctx, oldID, client.ContainerStartOptions{}); err != nil {
		r.log.Error("rollback: cannot start the old agent", "error", err)
		return fmt.Errorf("%w (and the old agent could not be started: %v): %s", ErrRolledBack, err, reason)
	}
	return fmt.Errorf("%w: %s", ErrRolledBack, reason)
}

// createSpec is the request that recreates the agent.
type createSpec struct {
	config  *container.Config
	host    *container.HostConfig
	network *network.NetworkingConfig
	// extra are the networks beyond the first. A create request carries one endpoint
	// (engines before API 1.44 refuse more), so the rest are connected before start.
	extra []namedEndpoint
}

type namedEndpoint struct {
	name string
	ep   *network.EndpointSettings
}

// buildCreate derives the create request of the new agent from the old container.
// The host and network configuration is carried over whole; the container's own
// Config is reduced to what the operator chose (see userConfig).
func (r *recreator) buildCreate(ctx context.Context, cli *client.Client, old container.InspectResponse, newImageRef string) createSpec {
	var imgCfg *dockerspec.DockerOCIImageConfig
	if old.Image != "" {
		if im, err := cli.ImageInspect(ctx, old.Image); err == nil {
			imgCfg = im.Config
		} else {
			r.log.Warn("recreate: the old agent's image is not available to compare with; keeping its configuration except image metadata labels", "error", err)
		}
	}
	spec := createSpec{config: userConfig(old.ID, old.Config, imgCfg), host: old.HostConfig}
	spec.config.Image = newImageRef

	mode := string(old.HostConfig.NetworkMode)
	if old.HostConfig == nil {
		mode = ""
	}
	if mode == "" || mode == "default" {
		mode = "bridge"
	}
	if old.NetworkSettings == nil || strings.HasPrefix(mode, "container:") || mode == "host" || mode == "none" {
		return spec
	}
	names := make([]string, 0, len(old.NetworkSettings.Networks))
	for n := range old.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	if _, ok := old.NetworkSettings.Networks[mode]; !ok {
		if len(names) == 0 {
			return spec
		}
		mode = names[0]
	}
	spec.network = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		mode: endpointConfig(old.ID, old.NetworkSettings.Networks[mode]),
	}}
	for _, n := range names {
		if n != mode {
			spec.extra = append(spec.extra, namedEndpoint{n, endpointConfig(old.ID, old.NetworkSettings.Networks[n])})
		}
	}
	return spec
}

// endpointConfig keeps what the operator configured on a network endpoint (static
// addresses, extra aliases, links, driver options) and drops what the engine filled
// in while the old container ran: its addresses, IDs and MAC, and the alias that is
// the old container's own ID.
func endpointConfig(oldID string, es *network.EndpointSettings) *network.EndpointSettings {
	if es == nil {
		return &network.EndpointSettings{}
	}
	out := &network.EndpointSettings{
		IPAMConfig: es.IPAMConfig.Copy(), Links: slices.Clone(es.Links), DriverOpts: es.DriverOpts, GwPriority: es.GwPriority,
	}
	for _, a := range es.Aliases {
		if len(a) >= 12 && strings.HasPrefix(oldID, a) {
			continue
		}
		out.Aliases = append(out.Aliases, a)
	}
	return out
}

// imageMetadataLabelPrefixes are the label namespaces an image uses to describe
// itself. A container carries the image's labels, so they show up in an inspect of
// the old container, and copying them would stamp the old version on the new one.
var imageMetadataLabelPrefixes = []string{"org.opencontainers.image.", "org.label-schema."}

// userConfig reduces the inspected Config of the old container to what the operator
// set, by removing what the old image supplied (img, when it can be read) and what
// the engine adds: the hostname that is the container's own ID, the image's labels,
// environment, entrypoint, command, working directory, user, volumes, exposed ports
// and stop signal. The new container takes those from the new image, so a changed
// default is not held back by the old one. Without img only the hostname and the
// image metadata labels are removed, and the rest is kept.
func userConfig(oldID string, c *container.Config, img *dockerspec.DockerOCIImageConfig) *container.Config {
	out := *c
	out.OnBuild, out.Shell, out.ArgsEscaped = nil, nil, false
	if len(out.Hostname) >= 12 && strings.HasPrefix(oldID, out.Hostname) {
		out.Hostname = ""
	}

	out.Labels = make(map[string]string, len(c.Labels))
	for k, v := range c.Labels {
		if img != nil {
			if iv, ok := img.Labels[k]; ok && iv == v {
				continue
			}
		}
		if hasAnyPrefix(k, imageMetadataLabelPrefixes) {
			continue
		}
		out.Labels[k] = v
	}
	if img == nil {
		return &out
	}

	out.Env = nil
	for _, e := range c.Env {
		if !slices.Contains(img.Env, e) {
			out.Env = append(out.Env, e)
		}
	}
	if slices.Equal(c.Entrypoint, img.Entrypoint) {
		out.Entrypoint = nil
	}
	if slices.Equal(c.Cmd, img.Cmd) {
		out.Cmd = nil
	}
	if c.WorkingDir == img.WorkingDir {
		out.WorkingDir = ""
	}
	if c.User == img.User {
		out.User = ""
	}
	if c.StopSignal == img.StopSignal {
		out.StopSignal = ""
	}
	out.Volumes = nil
	for v := range c.Volumes {
		if _, ok := img.Volumes[v]; !ok {
			if out.Volumes == nil {
				out.Volumes = map[string]struct{}{}
			}
			out.Volumes[v] = struct{}{}
		}
	}
	out.ExposedPorts = nil
	for p := range c.ExposedPorts {
		if _, ok := img.ExposedPorts[p.String()]; !ok {
			if out.ExposedPorts == nil {
				out.ExposedPorts = network.PortSet{}
			}
			out.ExposedPorts[p] = struct{}{}
		}
	}
	if img.Healthcheck != nil && reflect.DeepEqual(c.Healthcheck, img.Healthcheck) {
		out.Healthcheck = nil
	}
	return &out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
