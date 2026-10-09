// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

var _ = registerFakeHandler((*Fake).serveInspect)

// inspectContainer matches /containers/{id}/json.
var inspectContainer = regexp.MustCompile(`^/containers/([^/]+)/json$`)

// FakeInspect is what a FakeContainer reports only when it is inspected. The zero
// value is a container with no variables, no command and the default log driver.
type FakeInspect struct {
	// Env is the "NAME=value" strings of Config.Env. A container whose Env is nil
	// reports DB_PASSWORD=hunter2, so a test that must never see a value can prove it.
	Env        []string
	Cmd        []string
	Entrypoint []string
	WorkingDir string
	User       string
	Tty        bool
	// ExitCode, StartedAt, FinishedAt, OOMKilled, Error and RestartCount are State and
	// RestartCount of the inspect document.
	ExitCode     int
	StartedAt    string
	FinishedAt   string
	OOMKilled    bool
	Error        string
	RestartCount int
	// LogType and LogOptions are HostConfig.LogConfig.
	LogType    string
	LogOptions map[string]string
	// MountModes maps a Mounts entry to its Mode ("ro", "z").
	MountModes map[string]string
}

// serveInspect answers GET /containers/{id}/json with the restart policy, the
// variables (a secret by default, to prove it is never read unmasked), labels,
// mounts, networks with their aliases, the log config and the health. An ID the
// daemon does not know, and that has no restart policy either, is the 404 a real one
// gives.
func (f *Fake) serveInspect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	m := inspectContainer.FindStringSubmatch(p)
	if m == nil || m[1] == "json" || r.Method != http.MethodGet {
		return false
	}
	id := m[1]
	found, known := cfg.resolveContainer(id)
	if known {
		id = found.ID
	}
	policy, hasPolicy := cfg.RestartPolicies[id]
	if !known && !hasPolicy {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + id})
		return true
	}
	name, retries, _ := strings.Cut(policy, ":")
	max, _ := strconv.Atoi(retries)
	resp := container.InspectResponse{
		ID:   id,
		Name: "/" + id,
		Config: &container.Config{
			Env: []string{"DB_PASSWORD=hunter2"},
		},
		HostConfig: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(name), MaximumRetryCount: max},
		},
		State: &container.State{},
	}
	if known {
		in := found.Inspect
		resp.Image = "sha256:" + strings.Repeat("0", 12) + found.ID
		resp.Config.Image = found.Image
		resp.Config.Labels = found.Labels
		resp.Config.StopTimeout = found.StopTimeout
		resp.Config.StopSignal = found.StopSignal
		if in.Env != nil {
			resp.Config.Env = in.Env
		}
		resp.Config.Cmd, resp.Config.Entrypoint = in.Cmd, in.Entrypoint
		resp.Config.WorkingDir, resp.Config.User, resp.Config.Tty = in.WorkingDir, in.User, in.Tty
		resp.RestartCount = in.RestartCount
		resp.State.Running = found.State == "running"
		resp.State.Paused = found.State == "paused"
		resp.State.Status = container.ContainerState(found.State)
		resp.State.ExitCode, resp.State.StartedAt, resp.State.FinishedAt = in.ExitCode, in.StartedAt, in.FinishedAt
		resp.State.OOMKilled, resp.State.Error = in.OOMKilled, in.Error
		if found.Health != "" {
			resp.State.Health = &container.Health{Status: container.HealthStatus(found.Health)}
		}
		if len(found.Names) > 0 {
			resp.Name = found.Names[0]
		}
		if in.LogType != "" {
			resp.HostConfig.LogConfig = container.LogConfig{Type: in.LogType, Config: in.LogOptions}
		}
		for _, mn := range found.Mounts {
			mp := container.MountPoint{Type: mount.TypeVolume, Name: mn, Source: "/var/lib/docker/volumes/" + mn + "/_data", Destination: "/mnt", RW: true}
			if strings.HasPrefix(mn, "/") {
				mp = container.MountPoint{Type: mount.TypeBind, Source: mn, Destination: "/mnt", RW: true}
			}
			mp.Mode = in.MountModes[mn]
			resp.Mounts = append(resp.Mounts, mp)
		}
		resp.NetworkSettings = &container.NetworkSettings{Ports: network.PortMap{}, Networks: map[string]*network.EndpointSettings{}}
		for _, fp := range found.Ports {
			if fp.Public == 0 {
				continue
			}
			port, err := network.ParsePort(strconv.Itoa(int(fp.Private)) + "/" + fp.Type)
			if err != nil {
				continue
			}
			b := network.PortBinding{HostPort: strconv.Itoa(int(fp.Public))}
			if fp.IP != "" {
				b.HostIP = netip.MustParseAddr(fp.IP)
			}
			resp.NetworkSettings.Ports[port] = append(resp.NetworkSettings.Ports[port], b)
		}
		for netName, ep := range found.Networks {
			es := &network.EndpointSettings{NetworkID: ep.NetworkID, Aliases: ep.Aliases}
			if ep.IPv4 != "" {
				es.IPAddress = netip.MustParseAddr(ep.IPv4)
			}
			if ep.IPv6 != "" {
				es.GlobalIPv6Address = netip.MustParseAddr(ep.IPv6)
			}
			if ep.MAC != "" {
				_ = es.MacAddress.UnmarshalText([]byte(ep.MAC))
			}
			resp.NetworkSettings.Networks[netName] = es
		}
	}
	writeJSON(w, http.StatusOK, resp)
	return true
}
