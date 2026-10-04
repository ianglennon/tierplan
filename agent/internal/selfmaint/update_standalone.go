// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package selfmaint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/ianglennon/bayplan/proto/wire"
)

// updateStandalone updates an agent that runs as a plain container. The agent
// cannot replace itself: it would stop its own process halfway. So it pulls the new
// image, starts a short-lived helper from it with the agent's own hardening, and the
// helper (RecreateAgent) recreates the agent and rolls back if the new one never
// connects. The agent answers as soon as the helper has started; the new version
// connecting is the confirmation. It returns an update_result error code, or "".
func (m *Maint) updateStandalone(ctx context.Context, cli *client.Client, self container.InspectResponse, image string) string {
	helpers, err := cli.ContainerList(ctx, client.ContainerListOptions{Filters: client.Filters{}.Add("label", HelperLabel+"=true")})
	if err != nil {
		m.log.Error("self-update: cannot look for a running helper", "error", err)
		return wire.UpdateErrFailed
	}
	if len(helpers.Items) > 0 {
		m.log.Warn("self-update: a helper is already running")
		return wire.UpdateErrBusy
	}

	// The image is pulled by the agent, which has the registry access the host has.
	// A pull that fails is no reason to stop when the image is already here.
	pull, err := cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err == nil {
		err = pull.Wait(ctx)
	}
	if err != nil {
		if _, ierr := cli.ImageInspect(ctx, image); ierr != nil {
			m.log.Error("self-update: cannot pull the image and it is not on this host", "image", image, "error", err)
			return wire.UpdateErrFailed
		}
		m.log.Warn("self-update: the pull failed, using the image already on this host", "image", image, "error", err)
	}

	mounts, ok := m.helperMounts(self)
	if !ok {
		m.log.Error("self-update: the agent's own container has no mount for the Docker socket or the state directory")
		return wire.UpdateErrFailed
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return wire.UpdateErrFailed
	}
	env := []string{"BAYPLAN_AGENT_SOCKET=" + m.cfg.SocketPath, "BAYPLAN_AGENT_STATE_DIR=" + m.cfg.StateDir}
	for _, e := range self.Config.Env {
		if strings.HasPrefix(e, "BAYPLAN_AGENT_DOCKER_GID=") {
			env = append(env, e)
		}
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "bayplan-agent-helper-" + hex.EncodeToString(suffix),
		Config: &container.Config{
			Image:  image,
			Cmd:    []string{"helper", "recreate", "--target", self.ID},
			Env:    env,
			Labels: map[string]string{AgentLabel: "true", HelperLabel: "true"},
		},
		HostConfig: &container.HostConfig{
			AutoRemove:     true,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{"SETUID", "SETGID"},
			SecurityOpt:    []string{"no-new-privileges"},
			// The helper talks to Docker through the socket only.
			NetworkMode: "none",
			Mounts:      mounts,
		},
	})
	if err != nil {
		m.log.Error("self-update: cannot create the helper", "error", err)
		return wire.UpdateErrFailed
	}
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		m.log.Error("self-update: cannot start the helper", "error", err)
		// A helper that never ran never removes itself.
		if _, rerr := cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil {
			m.log.Error("self-update: cannot remove the helper that did not start", "id", created.ID, "error", rerr)
		}
		return wire.UpdateErrFailed
	}
	m.log.Info("self-update: the helper was started; it replaces this agent", "helper", created.ID[:12], "image", image)
	return ""
}

// helperMounts is the Docker socket and the state directory, as the agent's own
// container has them, for the helper. The state directory is read-only: the helper
// only reads the connected marker. ok is false when either is missing.
func (m *Maint) helperMounts(self container.InspectResponse) ([]mount.Mount, bool) {
	var sock, state *mount.Mount
	for _, mp := range self.Mounts {
		var mt mount.Mount
		switch mp.Type {
		case mount.TypeBind:
			mt = mount.Mount{Type: mount.TypeBind, Source: mp.Source, Target: mp.Destination}
		case mount.TypeVolume:
			mt = mount.Mount{Type: mount.TypeVolume, Source: mp.Name, Target: mp.Destination}
		default:
			continue
		}
		switch mp.Destination {
		case m.cfg.SocketPath:
			sock = &mt
		case m.cfg.StateDir:
			mt.ReadOnly = true
			state = &mt
		}
	}
	if sock == nil || state == nil {
		return nil, false
	}
	return []mount.Mount{*sock, *state}, true
}
