// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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
	// MemoryBytes and NanoCPUs are the container's own limits, HostConfig.Memory and
	// HostConfig.NanoCPUs; zero is no limit.
	MemoryBytes, NanoCPUs int64
	// LogType and LogOptions are HostConfig.LogConfig.
	LogType    string
	LogOptions map[string]string
	// MountModes maps a Mounts entry to its Mode ("ro", "z"); a mode with ro in it makes the
	// mount read-only in the list as well.
	MountModes map[string]string
	// MountTargets maps a Mounts entry to where it appears in the container; the default
	// is /mnt.
	MountTargets map[string]string
	// ImageID is the ID of the image the container was created from, as the inspect shows it in Image.
	// The default is a made-up ID that no image of the fake has.
	ImageID string
	// Document, when set, is the inspect document the container answers, as the engine would document a
	// container made from a compose service (a test builds it from the prepared service). The ID, the
	// state and the container's own labels, which the FakeContainer carries, are put into it when it
	// leaves them empty, so one fake container can stand for a whole stack.
	Document *container.InspectResponse
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
			Resources:     container.Resources{Memory: found.Inspect.MemoryBytes, NanoCPUs: found.Inspect.NanoCPUs},
		},
		State: &container.State{},
	}
	if known {
		in := found.Inspect
		resp.Image = "sha256:" + strings.Repeat("0", 12) + found.ID
		if in.ImageID != "" {
			resp.Image = in.ImageID
		}
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
			if to := in.MountTargets[mn]; to != "" {
				mp.Destination = to
			}
			mp.RW = !strings.Contains(mp.Mode, "ro")
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
	if known && found.Inspect.Document != nil {
		resp = found.Inspect.documentFor(resp)
	}
	writeJSON(w, http.StatusOK, resp)
	return true
}

// documentFor is the document a FakeInspect.Document stands for, with what the document leaves out taken
// from base (the ID, the name, the image ID, the state and the labels of the container).
func (in FakeInspect) documentFor(base container.InspectResponse) container.InspectResponse {
	doc := *in.Document
	doc.ID = base.ID
	if doc.Name == "" {
		doc.Name = base.Name
	}
	if doc.Image == "" {
		doc.Image = base.Image
	}
	if in.ImageID != "" {
		doc.Image = in.ImageID
	}
	if doc.State == nil {
		doc.State = base.State
	}
	cfg := container.Config{}
	if doc.Config != nil {
		cfg = *doc.Config
	}
	labels := map[string]string{}
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	for k, v := range base.Config.Labels {
		labels[k] = v
	}
	cfg.Labels = labels
	doc.Config = &cfg
	return doc
}
