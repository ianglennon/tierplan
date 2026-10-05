// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package selfmaint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/ianglennon/bayplan/proto/wire"
)

// tokenSecretPrefix starts the name of every generation of the join token secret the
// install creates (bayplan-agent-token-g1, g2, ...). Only a secret with this name, in
// the agent's own service, is removed with it.
const tokenSecretPrefix = "bayplan-agent-token-g"

// Uninstall removes the agent from its host (D-30). Like an update it only ever acts
// on what the agent itself is: its own container, the swarm service that container
// carries the ID of, the token secret that service names, and the state volume its own
// container mounts. Nothing is chosen by a name or an ID the server sent.
//
//   - Swarm: with msg.Service the agent, which must be a manager, removes its own
//     service and its token secret. Its helper is started before that, because
//     removing the service ends this process. Without msg.Service, and with
//     msg.RemoveData, the agent only starts the helper.
//   - Standalone: the helper removes the agent's container and then its volume.
//
// The helper never runs inside the agent's container and mounts nothing but the Docker
// socket, because a volume in use cannot be removed and the helper must not be what
// holds it. The answer says the removal was accepted and started, not that it finished.
func (m *Maint) Uninstall(ctx context.Context, msg wire.Uninstall) wire.UpdateResult {
	res := result(msg.ID)
	if !m.cfg.SelfUpdate {
		m.log.Warn("the server asked this agent to uninstall, and BAYPLAN_AGENT_SELF_UPDATE=false refuses it")
		return refuse(res, wire.UpdateErrSelfUpdateDisabled)
	}
	if !m.running.CompareAndSwap(false, true) {
		return refuse(res, wire.UpdateErrBusy)
	}
	defer m.running.Store(false)
	if m.beforeDocker != nil {
		m.beforeDocker()
	}

	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	cli, err := m.newClient()
	if err != nil {
		m.log.Error("uninstall: cannot build the Docker client", "error", err)
		return refuse(res, wire.UpdateErrFailed)
	}
	defer func() { _ = cli.Close() }()

	self, code := m.inspectSelf(ctx, cli)
	if code != "" {
		return refuse(res, code)
	}
	own, err := parseOwnImage(self.Config.Image)
	if err != nil || (m.cfg.OwnRepository != "" && own != m.cfg.OwnRepository) {
		m.log.Error("uninstall: the agent's own image is not in the agent's repository", "image", self.Config.Image)
		return refuse(res, wire.UpdateErrOwnContainerUnknown)
	}

	m.log.Info("uninstall requested", "remove_data", msg.RemoveData, "service", msg.Service)
	if svc := self.Config.Labels[swarmServiceLabel]; svc != "" {
		if code := m.uninstallSwarm(ctx, cli, self, svc, msg); code != "" {
			return refuse(res, code)
		}
	} else if code := m.uninstallStandalone(ctx, cli, self, msg); code != "" {
		return refuse(res, code)
	}
	res.OK = true
	return res
}

// uninstallSwarm handles a swarm agent. It returns an update_result error code, or "".
func (m *Maint) uninstallSwarm(ctx context.Context, cli *client.Client, self container.InspectResponse, serviceID string, msg wire.Uninstall) string {
	var secrets []string
	if msg.Service {
		info, err := cli.Info(ctx, client.InfoOptions{})
		if err != nil {
			m.log.Error("uninstall: cannot read the engine's swarm state", "error", err)
			return wire.UpdateErrFailed
		}
		if !info.Info.Swarm.ControlAvailable {
			m.log.Warn("uninstall: this agent runs on a worker, which cannot remove the service")
			return wire.UpdateErrNotAManager
		}
	}

	hc := unixHTTPClient(m.cfg.SocketPath)
	defer hc.CloseIdleConnections()
	if msg.Service {
		// Read before anything changes: it is the spec that names the secret, and the
		// secret cannot be found once the service is gone.
		inspected, err := httpDo(ctx, hc, http.MethodGet, "/services/"+url.PathEscape(serviceID), nil)
		if err != nil {
			m.log.Error("uninstall: cannot inspect the agent's own service", "service", serviceID, "error", err)
			return wire.UpdateErrFailed
		}
		secrets = tokenSecrets(inspected)
	}

	// The helper first: removing the service ends this agent.
	if msg.RemoveData {
		if code := m.startRemovalHelper(ctx, cli, self, false, false); code != "" {
			return code
		}
	}
	if !msg.Service {
		return ""
	}
	if _, err := httpDo(ctx, hc, http.MethodDelete, "/services/"+url.PathEscape(serviceID), nil); err != nil {
		m.log.Error("uninstall: cannot remove the agent's own service", "service", serviceID, "error", err)
		return wire.UpdateErrFailed
	}
	m.log.Info("uninstall: the agent's service was removed", "service", serviceID)
	for _, id := range secrets {
		// The service is gone, so the secret is no longer in use. A failure here leaves
		// one secret for the admin's manual command and is not the uninstall failing.
		if _, err := httpDo(ctx, hc, http.MethodDelete, "/secrets/"+url.PathEscape(id), nil); err != nil {
			m.log.Warn("uninstall: cannot remove the agent's token secret", "secret", id, "error", err)
			continue
		}
		m.log.Info("uninstall: the agent's token secret was removed", "secret", id)
	}
	return ""
}

// uninstallStandalone has a helper remove the agent's container and, unless the server
// asked to keep the data, its volume. It returns an update_result error code, or "".
func (m *Maint) uninstallStandalone(ctx context.Context, cli *client.Client, self container.InspectResponse, msg wire.Uninstall) string {
	return m.startRemovalHelper(ctx, cli, self, true, !msg.RemoveData)
}

// tokenSecrets returns the IDs of the join-token secrets a service's spec names: the
// secrets of GET /services/{id} whose name the install gives its token. A secret with
// any other name is somebody else's and is never returned. The service JSON is read as
// loosely as possible: anything that does not look as expected yields no secret.
func tokenSecrets(inspected []byte) []string {
	var svc struct {
		Spec struct {
			TaskTemplate struct {
				ContainerSpec struct {
					Secrets []struct {
						SecretID   string `json:"SecretID"`
						SecretName string `json:"SecretName"`
					} `json:"Secrets"`
				} `json:"ContainerSpec"`
			} `json:"TaskTemplate"`
		} `json:"Spec"`
	}
	if err := json.Unmarshal(inspected, &svc); err != nil {
		return nil
	}
	var ids []string
	for _, s := range svc.Spec.TaskTemplate.ContainerSpec.Secrets {
		if s.SecretID != "" && strings.HasPrefix(s.SecretName, tokenSecretPrefix) {
			ids = append(ids, s.SecretID)
		}
	}
	return ids
}

// startRemovalHelper starts the short-lived container that waits for this agent's
// container to go and then removes its state volume (RemoveVolumeAfter). The helper
// runs the agent's own image, which is already on the host, with the same hardening as
// an update helper and no mount but the Docker socket. removeContainer makes the helper
// remove the agent's container itself, after a moment for the agent to answer; keepData
// makes it leave the volume. It returns an update_result error code, or "".
func (m *Maint) startRemovalHelper(ctx context.Context, cli *client.Client, self container.InspectResponse, removeContainer, keepData bool) string {
	sock, ok := m.socketMount(self)
	if !ok {
		m.log.Error("uninstall: the agent's own container has no mount for the Docker socket")
		return wire.UpdateErrFailed
	}
	if !removeContainer && keepData {
		return "" // nothing for a helper to do
	}
	args := []string{"helper", "remove-volume", "--wait", self.ID}
	volume := m.stateVolume(self)
	if volume != "" {
		args = append(args, "--volume", volume)
	}
	if removeContainer {
		args = append(args, "--remove-container")
	}
	if keepData || volume == "" {
		args = append(args, "--keep-volume")
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return wire.UpdateErrFailed
	}
	env := []string{"BAYPLAN_AGENT_SOCKET=" + m.cfg.SocketPath}
	for _, e := range self.Config.Env {
		if strings.HasPrefix(e, "BAYPLAN_AGENT_DOCKER_GID=") {
			env = append(env, e)
		}
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "bayplan-agent-helper-" + hex.EncodeToString(suffix),
		Config: &container.Config{
			Image:  self.Config.Image,
			Cmd:    args,
			Env:    env,
			Labels: map[string]string{AgentLabel: "true", HelperLabel: "true"},
		},
		HostConfig: &container.HostConfig{
			AutoRemove:     true,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{"SETUID", "SETGID"},
			SecurityOpt:    []string{"no-new-privileges"},
			NetworkMode:    "none",
			Mounts:         []mount.Mount{sock},
		},
	})
	if err != nil {
		m.log.Error("uninstall: cannot create the helper", "error", err)
		return wire.UpdateErrFailed
	}
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		m.log.Error("uninstall: cannot start the helper", "error", err)
		if _, rerr := cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil {
			m.log.Error("uninstall: cannot remove the helper that did not start", "id", created.ID, "error", rerr)
		}
		return wire.UpdateErrFailed
	}
	m.log.Info("uninstall: the helper was started", "helper", created.ID[:12], "volume", volume)
	return ""
}

// socketMount is the Docker socket as the agent's own container has it.
func (m *Maint) socketMount(self container.InspectResponse) (mount.Mount, bool) {
	for _, mp := range self.Mounts {
		if mp.Destination != m.cfg.SocketPath {
			continue
		}
		switch mp.Type {
		case mount.TypeBind:
			return mount.Mount{Type: mount.TypeBind, Source: mp.Source, Target: mp.Destination}, true
		case mount.TypeVolume:
			return mount.Mount{Type: mount.TypeVolume, Source: mp.Name, Target: mp.Destination}, true
		}
	}
	return mount.Mount{}, false
}

// stateVolume is the name of the named volume the agent's own container mounts as its
// state directory, or "" when the state directory is a bind mount or not mounted: an
// operator's own directory is not the agent's to remove.
func (m *Maint) stateVolume(self container.InspectResponse) string {
	for _, mp := range self.Mounts {
		if mp.Destination == m.cfg.StateDir && mp.Type == mount.TypeVolume {
			return mp.Name
		}
	}
	return ""
}

// parseOwnImage is the repository of an image reference the agent's own container
// reports.
func parseOwnImage(ref string) (string, error) {
	n, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", err
	}
	return n.Name(), nil
}
