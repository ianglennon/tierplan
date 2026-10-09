// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package dockertest holds the test helpers for code that talks to Docker
// through internal/dockerx: a fake daemon that speaks just enough of the Engine
// API over a real unix socket (so the real client code runs unchanged), and
// helpers that find or start a real engine for the integration tests.
//
// It lives under internal/dockerx so the depguard rule that keeps the moby
// client inside that tree still holds. Nothing in the product imports it.
package dockertest

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ianglennon/bayplan/internal/engine"
)

const (
	// EnvDockerHost names the variable that points the integration tests at an
	// engine that is already running, such as the CI dind service.
	EnvDockerHost = "BAYPLAN_TEST_DOCKER_HOST"
	// EnvEngine names the variable that picks the dind major version.
	EnvEngine = "BAYPLAN_TEST_ENGINE"

	defaultEngine = "29"
	dindPort      = "2375/tcp"
)

// Component is one entry of the /version Components list.
type Component struct {
	Name    string
	Version string
}

// FakeConfig describes the engine a FakeDaemon pretends to be. Zero values
// become the defaults noted on each field.
type FakeConfig struct {
	// APIVersion is the daemon's maximum API version. Default "1.52".
	APIVersion string
	// MinAPIVersion is the oldest API version it still serves. Default "1.12",
	// which is old enough that no client request is refused as too old.
	MinAPIVersion string
	// OSType is the OSType ping header. Default "linux".
	OSType string
	// Version is the engine version. Default "29.1.0".
	Version string
	// PlatformName is the /version Platform.Name. Default "Docker Engine - Community".
	PlatformName string
	// Components replaces the default Engine component, for example to
	// present a Podman engine.
	Components []Component
	// Hostname is Info.Name. Default "fake-node".
	Hostname string
	// OperatingSystem is Info.OperatingSystem. Default "Ubuntu 26.04 LTS".
	OperatingSystem string
	// SecurityOptions is Info.SecurityOptions.
	SecurityOptions []string
	// SwarmState is Info.Swarm.LocalNodeState. Default "inactive".
	SwarmState string
	// SwarmControl is Info.Swarm.ControlAvailable.
	SwarmControl bool
	// EngineID is Info.ID. Default "FAKE:ID".
	EngineID string
	// SwarmNodeID is Info.Swarm.NodeID.
	SwarmNodeID string
	// SwarmClusterID is Info.Swarm.Cluster.ID; empty leaves Cluster unset, as an
	// engine outside a swarm reports it. A real worker leaves it unset too, which a
	// fake worker does not do unless this is empty.
	SwarmClusterID string
	// FailInfo makes /info answer 500, for the partial-failure path.
	FailInfo bool

	// Containers, Images, Volumes and Networks are what the list endpoints
	// answer. Nil means an empty engine: the fake answers an empty list, never
	// an error.
	Containers []FakeContainer
	Images     []FakeImage
	Volumes    []FakeVolume
	Networks   []FakeNetwork
	// Nodes and Services are how many swarm nodes and services /nodes and
	// /services list. A fake that is not a swarm manager answers both with the
	// 503 a real worker gives, so a client that asks anyway fails loudly.
	Nodes, Services int
	// ListDelay is how long /containers/json takes to answer, so a test can
	// make concurrent callers overlap.
	ListDelay time.Duration
	// PolicyDeny makes the four list endpoints and every write answer 403 with the
	// message an agent gives for a request its policy refuses: agent_policy_denied:
	// followed by the method and path.
	PolicyDeny bool
	// Events is the history the daemon has buffered when it starts, oldest
	// first, as /events replays it to a client that passes since.
	Events []FakeEvent

	// NodeRows are the nodes /nodes lists, in place of the Nodes count, so a test
	// can say who leads, who is reachable and what each node runs.
	NodeRows []FakeNode
	// ServiceRows are the services /services lists, in place of the Services count.
	// With ?status=true each carries its ServiceStatus.
	ServiceRows []FakeService
	// AgentTasks are the tasks /tasks lists. A task is listed when the request's
	// service filter names its service, by ID or by name, as a real daemon matches.
	AgentTasks []FakeAgentTask
	// QuorumLost makes /nodes, /services, /tasks and /swarm answer the 500 a
	// manager gives when the swarm has no leader. Node-local calls keep working,
	// and so does /_ping, as on a real manager (RESEARCH Pitfall 11).
	QuorumLost bool
	// RestartPolicies maps a container ID to the restart policy /containers/{id}/json
	// reports: a name such as "always", or "on-failure:3" with its maximum retry
	// count. An ID that is not here is a container with no policy.
	RestartPolicies map[string]string
}

// FakeNode is one entry of /nodes. Leader marks the one manager whose
// ManagerStatus says so; Reachability applies to managers only and defaults to
// "reachable". Zero Role is "worker", zero Availability "active", zero State
// "ready".
type FakeNode struct {
	ID            string
	Hostname      string
	Role          string
	Availability  string
	State         string
	EngineVersion string
	OS            string
	Leader        bool
	Reachability  string
}

// FakeService is one entry of /services. Mode is "replicated" (the default),
// "global", "replicated-job" or "global-job". UpdateState is the UpdateStatus
// state, empty for none. A zero UpdatedAt is the Unix epoch.
type FakeService struct {
	ID, Name    string
	Mode        string
	Running     uint64
	Desired     uint64
	UpdateState string
	UpdatedAt   time.Time
	// Labels are the service's own labels (the --label of docker service create,
	// the deploy.labels of a stack file).
	Labels map[string]string
}

// FakeAgentTask is one task of a service in ServiceRows, an agent's when that
// service is one.
type FakeAgentTask struct {
	NodeID, State, Error string
	// Service is the ID or name of the service the task belongs to. Empty means the
	// service named bayplan-agent.
	Service string
	// ID and Created order the tasks of one node: the newest is the current one.
	ID      string
	Created time.Time
}

// FakeEvent is one event of the fake daemon's /events stream. A zero TimeNano
// means the moment the event is published.
type FakeEvent struct {
	Type    string
	Action  string
	ActorID string
	// Scope is local or swarm; empty means local.
	Scope string
	// Attributes are sent as they are, so a test can prove which ones Bayplan
	// drops.
	Attributes map[string]string
	TimeNano   int64
}

// eventBuffer is how many events a real daemon keeps for a client that asks
// for history with since (RESEARCH Pitfall 7).
const eventBuffer = 256

// eventSub is one open /events stream.
type eventSub struct {
	ch       chan FakeEvent
	drop     chan struct{}
	dropOnce sync.Once
}

// FakeContainer is one entry of /containers/json. Labels and Mounts exist so a
// test can prove they never reach a Bayplan response.
type FakeContainer struct {
	ID      string
	Names   []string
	Image   string
	State   string
	Status  string
	Created int64
	Ports   []FakePort
	Labels  map[string]string
	Mounts  []string
}

// FakePort is one published or exposed container port. IP is empty for a port
// that is not published.
type FakePort struct {
	IP      string
	Private uint16
	Public  uint16
	Type    string
}

// FakeImage is one entry of /images/json. An untagged image has no RepoTags.
type FakeImage struct {
	ID       string
	RepoTags []string
	Size     int64
	Created  int64
	Labels   map[string]string
}

// FakeVolume is one entry of /volumes. CreatedAt is RFC 3339, or empty.
type FakeVolume struct {
	Name      string
	Driver    string
	CreatedAt string
	Labels    map[string]string
}

// FakeNetwork is one entry of /networks.
type FakeNetwork struct {
	ID      string
	Name    string
	Driver  string
	Scope   string
	Subnets []string
	Created time.Time
}

func (c FakeConfig) withDefaults() FakeConfig {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&c.APIVersion, "1.52")
	def(&c.MinAPIVersion, "1.12")
	def(&c.OSType, "linux")
	def(&c.Version, "29.1.0")
	def(&c.PlatformName, "Docker Engine - Community")
	def(&c.Hostname, "fake-node")
	def(&c.OperatingSystem, "Ubuntu 26.04 LTS")
	def(&c.SwarmState, "inactive")
	def(&c.EngineID, "FAKE:ID")
	if len(c.Components) == 0 {
		c.Components = []Component{{Name: "Engine", Version: c.Version}}
	}
	return c
}

// Fake is a running fake daemon. It is an engine.Transport that dials its own
// unix socket, and it records every request path it served.
type Fake struct {
	path string

	mu       sync.Mutex
	cfg      FakeConfig
	requests []string
	srv      *httptest.Server // nil while the daemon is stopped
	// states is the state a write left each container in, by container ID, and
	// writes is every write served, in order (fake_writes.go).
	states map[string]fakeState
	writes []string

	// evMu guards the events history, the open streams and the queries seen.
	evMu      sync.Mutex
	evHistory []FakeEvent
	evSubs    map[*eventSub]struct{}
	evQueries []url.Values
}

var versionPrefix = regexp.MustCompile(`^/v(\d+)\.(\d+)(/.*)$`)

// FakeDaemon starts a fake daemon on a unix socket and returns the transport
// that reaches it. The socket lives in a short temporary directory because
// unix socket paths are limited to about 100 bytes.
func FakeDaemon(t testing.TB, cfg FakeConfig) engine.Transport {
	t.Helper()
	return NewFake(t, cfg)
}

// NewFake is FakeDaemon with the concrete type, so a test can read the
// requests it served and change what it reports.
func NewFake(t testing.TB, cfg FakeConfig) *Fake {
	t.Helper()
	dir, err := os.MkdirTemp("", "bpfd")
	if err != nil {
		t.Fatalf("make the fake daemon directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	f := &Fake{path: filepath.Join(dir, "d.sock"), cfg: cfg.withDefaults(), evSubs: map[*eventSub]struct{}{}, states: map[string]fakeState{}}
	f.evHistory = append(f.evHistory, cfg.Events...)
	if err := f.listen(); err != nil {
		t.Fatalf("listen on the fake daemon socket: %v", err)
	}
	t.Cleanup(f.Stop)
	return f
}

// listen starts serving on the fake's socket path.
func (f *Fake) listen() error {
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		return err
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Listener = ln
	srv.Start()
	f.mu.Lock()
	f.srv = srv
	f.mu.Unlock()
	return nil
}

// Stop takes the daemon down: the socket is closed and removed, so a client
// sees "no such file" or "connection refused" as it would with a stopped
// engine. It is safe to call twice. Restart brings the daemon back on the same
// path, with whatever configuration it has then.
func (f *Fake) Stop() {
	f.mu.Lock()
	srv := f.srv
	f.srv = nil
	f.mu.Unlock()
	if srv != nil {
		// An open /events stream would keep Close waiting.
		f.DropEventStreams()
		srv.Close()
	}
}

// Restart starts the daemon again on the same socket path after Stop. It does
// nothing if the daemon is already running.
func (f *Fake) Restart(t testing.TB) {
	t.Helper()
	f.mu.Lock()
	running := f.srv != nil
	f.mu.Unlock()
	if running {
		return
	}
	if err := f.listen(); err != nil {
		t.Fatalf("restart the fake daemon: %v", err)
	}
}

// SetAPIVersion changes the maximum API version the daemon reports from now
// on, as an engine upgraded while it was stopped.
func (f *Fake) SetAPIVersion(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg.APIVersion = v
}

// SocketPath is the path of the fake's unix socket.
func (f *Fake) SocketPath() string { return f.path }

// DialContext implements engine.Transport: every dial reaches the fake.
func (f *Fake) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", f.path)
}

// Describe implements engine.Transport.
func (f *Fake) Describe() string { return "unix://" + f.path }

// Requests returns the request paths served so far, in order, as the daemon
// saw them (including any /vX.Y prefix).
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// Reconfigure changes what the fake reports from now on, as a daemon that was
// upgraded or downgraded between two probes.
func (f *Fake) Reconfigure(cfg FakeConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg.withDefaults()
}

func (f *Fake) config() FakeConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.withStates(f.states)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	cfg := f.config()
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()

	p := r.URL.Path
	if p == "/_ping" {
		h := w.Header()
		h.Set("API-Version", cfg.APIVersion)
		h.Set("OSType", cfg.OSType)
		h.Set("Docker-Experimental", "false")
		switch cfg.SwarmState {
		case "active":
			role := "worker"
			if cfg.SwarmControl {
				role = "manager"
			}
			h.Set("Swarm", "active/"+role)
		default:
			h.Set("Swarm", cfg.SwarmState)
		}
		h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = w.Write([]byte("OK"))
		return
	}

	// A real daemon refuses a request whose version prefix is newer than its
	// own maximum (or older than its minimum). The fake does too, so a client
	// that forgot to negotiate fails here exactly as it would against Docker 23.
	if m := versionPrefix.FindStringSubmatch(p); m != nil {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		got := fmt.Sprintf("%d.%d", major, minor)
		switch {
		case cmpAPI(got, cfg.APIVersion) > 0:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"message": fmt.Sprintf("client version %s is too new. Maximum supported API version is %s", got, cfg.APIVersion)})
			return
		case cmpAPI(got, cfg.MinAPIVersion) < 0:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"message": fmt.Sprintf("client version %s is too old. Minimum supported API version is %s", got, cfg.MinAPIVersion)})
			return
		}
		p = m[3]
	}

	// A write is answered by the first handler that claims it (fake_writes.go and
	// the files later plans add), before any read route is tried.
	for _, h := range fakeHandlers {
		if h(f, w, r, p, cfg) {
			return
		}
	}

	if cfg.PolicyDeny {
		switch p {
		case "/containers/json", "/images/json", "/volumes", "/networks":
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
			return
		}
	}

	switch p {
	case "/version":
		comps := make([]system.ComponentVersion, 0, len(cfg.Components))
		for _, c := range cfg.Components {
			comps = append(comps, system.ComponentVersion{Name: c.Name, Version: c.Version})
		}
		writeJSON(w, http.StatusOK, system.VersionResponse{
			Platform:      system.PlatformInfo{Name: cfg.PlatformName},
			Version:       cfg.Version,
			APIVersion:    cfg.APIVersion,
			MinAPIVersion: cfg.MinAPIVersion,
			Os:            "linux",
			Arch:          "amd64",
			Components:    comps,
		})
	case "/info":
		if cfg.FailInfo {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "info is down"})
			return
		}
		writeJSON(w, http.StatusOK, system.Info{
			ID:              cfg.EngineID,
			Name:            cfg.Hostname,
			ServerVersion:   cfg.Version,
			OperatingSystem: cfg.OperatingSystem,
			OSType:          cfg.OSType,
			Architecture:    "x86_64",
			KernelVersion:   "7.0.0-fake",
			Driver:          "overlayfs",
			NCPU:            4,
			MemTotal:        8 << 30,
			SecurityOptions: cfg.SecurityOptions,
			Swarm:           cfg.swarmInfo(),
		})
	case "/events":
		f.serveEvents(w, r)
	case "/containers/json":
		if cfg.ListDelay > 0 {
			select {
			case <-time.After(cfg.ListDelay):
			case <-r.Context().Done():
				return
			}
		}
		writeJSON(w, http.StatusOK, cfg.containerList(r.URL.Query().Get("all") != ""))
	case "/images/json":
		writeJSON(w, http.StatusOK, cfg.imageList())
	case "/volumes":
		writeJSON(w, http.StatusOK, cfg.volumeList())
	case "/networks":
		writeJSON(w, http.StatusOK, cfg.networkList())
	case "/nodes", "/services", "/tasks", "/swarm":
		f.serveCluster(w, r, p, cfg)
	default:
		if id, ok := inspectID(p); ok {
			f.serveInspect(w, id, cfg)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "page not found"})
	}
}

// workerRefusal and quorumRefusal are the messages a real Engine 29 daemon gives
// a cluster question it cannot answer (RESEARCH Pattern 5, read from real daemons).
const (
	workerRefusal = "This node is not a swarm manager. Worker nodes can't be used to view or modify cluster state. Please run this command on a manager node or promote the current node to a manager."
	quorumRefusal = "rpc error: code = Unknown desc = The swarm does not have a leader. It's possible that too few managers are online. Make sure more than half of the managers are online."
)

// serveCluster answers the swarm-wide lists: the worker 503, the quorum-loss 500
// or the configured rows.
func (f *Fake) serveCluster(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) {
	if !cfg.swarmManager() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": workerRefusal})
		return
	}
	if cfg.QuorumLost {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": quorumRefusal})
		return
	}
	switch p {
	case "/nodes":
		writeJSON(w, http.StatusOK, cfg.nodeList())
	case "/services":
		writeJSON(w, http.StatusOK, cfg.serviceList(r.URL.Query().Get("status") == "true"))
	case "/tasks":
		writeJSON(w, http.StatusOK, cfg.taskList(r.URL.Query().Get("filters")))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "page not found"})
	}
}

// inspectContainer matches /containers/{id}/json.
var inspectContainer = regexp.MustCompile(`^/containers/([^/]+)/json$`)

func inspectID(p string) (string, bool) {
	m := inspectContainer.FindStringSubmatch(p)
	if m == nil || m[1] == "json" {
		return "", false
	}
	return m[1], true
}

// serveInspect answers GET /containers/{id}/json with the restart policy and, to
// prove it is never read, an environment variable that holds a secret.
func (f *Fake) serveInspect(w http.ResponseWriter, id string, cfg FakeConfig) {
	found, known := cfg.resolveContainer(id)
	if known {
		id = found.ID
	}
	policy, hasPolicy := cfg.RestartPolicies[id]
	if !known && !hasPolicy {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + id})
		return
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
		resp.Config.Labels = found.Labels
		resp.State.Running = found.State == "running"
		resp.State.Status = container.ContainerState(found.State)
		if len(found.Names) > 0 {
			resp.Name = found.Names[0]
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveContainer finds a container the way a real daemon does: by its full ID,
// by its name (with or without the leading slash), or by an ID prefix that only one
// container has.
func (c FakeConfig) resolveContainer(ref string) (FakeContainer, bool) {
	for _, fc := range c.Containers {
		if fc.ID == ref {
			return fc, true
		}
	}
	for _, fc := range c.Containers {
		for _, n := range fc.Names {
			if strings.TrimPrefix(n, "/") == strings.TrimPrefix(ref, "/") {
				return fc, true
			}
		}
	}
	var hit FakeContainer
	n := 0
	for _, fc := range c.Containers {
		if ref != "" && strings.HasPrefix(fc.ID, ref) {
			hit = fc
			n++
		}
	}
	return hit, n == 1
}

// swarmInfo is Info.Swarm: the state, the node ID and the cluster ID when one is set.
func (c FakeConfig) swarmInfo() swarm.Info {
	info := swarm.Info{
		NodeID:           c.SwarmNodeID,
		LocalNodeState:   swarm.LocalNodeState(c.SwarmState),
		ControlAvailable: c.SwarmControl,
	}
	if c.SwarmClusterID != "" {
		info.Cluster = &swarm.ClusterInfo{ID: c.SwarmClusterID}
	}
	return info
}

func (c FakeConfig) swarmManager() bool { return c.SwarmState == "active" && c.SwarmControl }

// containerList is the /containers/json answer. Without all, the way a real
// daemon behaves, only running containers are listed.
func (c FakeConfig) containerList(all bool) []container.Summary {
	out := make([]container.Summary, 0, len(c.Containers))
	for _, fc := range c.Containers {
		if !all && fc.State != "running" {
			continue
		}
		s := container.Summary{
			ID: fc.ID, Names: fc.Names, Image: fc.Image, Created: fc.Created,
			State: container.ContainerState(fc.State), Status: fc.Status, Labels: fc.Labels,
		}
		for _, p := range fc.Ports {
			ps := container.PortSummary{PrivatePort: p.Private, PublicPort: p.Public, Type: p.Type}
			if p.IP != "" {
				ps.IP = netip.MustParseAddr(p.IP)
			}
			s.Ports = append(s.Ports, ps)
		}
		for _, m := range fc.Mounts {
			s.Mounts = append(s.Mounts, container.MountPoint{Source: m, Destination: "/mnt"})
		}
		out = append(out, s)
	}
	return out
}

func (c FakeConfig) imageList() []image.Summary {
	out := make([]image.Summary, 0, len(c.Images))
	for _, fi := range c.Images {
		out = append(out, image.Summary{ID: fi.ID, RepoTags: fi.RepoTags, Size: fi.Size, Created: fi.Created, Labels: fi.Labels})
	}
	return out
}

func (c FakeConfig) volumeList() volume.ListResponse {
	out := volume.ListResponse{Volumes: make([]volume.Volume, 0, len(c.Volumes))}
	for _, fv := range c.Volumes {
		out.Volumes = append(out.Volumes, volume.Volume{
			Name: fv.Name, Driver: fv.Driver, CreatedAt: fv.CreatedAt, Labels: fv.Labels,
			Mountpoint: "/var/lib/docker/volumes/" + fv.Name + "/_data", Scope: "local",
		})
	}
	return out
}

func (c FakeConfig) networkList() []network.Summary {
	out := make([]network.Summary, 0, len(c.Networks))
	for _, fn := range c.Networks {
		n := network.Summary{Network: network.Network{ID: fn.ID, Name: fn.Name, Driver: fn.Driver, Scope: fn.Scope, Created: fn.Created}}
		for _, sub := range fn.Subnets {
			n.IPAM.Config = append(n.IPAM.Config, network.IPAMConfig{Subnet: netip.MustParsePrefix(sub)})
		}
		out = append(out, n)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// cmpAPI compares two "major.minor" versions.
func cmpAPI(a, b string) int {
	pa, pb := splitAPI(a), splitAPI(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitAPI(v string) [2]int {
	maj, min, _ := strings.Cut(v, ".")
	a, _ := strconv.Atoi(maj)
	b, _ := strconv.Atoi(min)
	return [2]int{a, b}
}

// DockerHost returns the address of a real engine for the integration tests:
// BAYPLAN_TEST_DOCKER_HOST when it is set (CI points it at the dind service),
// otherwise a docker:<BAYPLAN_TEST_ENGINE or 29>-dind container started with
// testcontainers and removed when the test ends. It waits for the daemon by
// polling /_ping, never by sleeping (RESEARCH Pitfall 23).
func DockerHost(t testing.TB) string {
	t.Helper()
	if h := os.Getenv(EnvDockerHost); h != "" {
		return h
	}
	engineMajor := os.Getenv(EnvEngine)
	if engineMajor == "" {
		engineMajor = defaultEngine
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "docker:" + engineMajor + "-dind",
			ExposedPorts: []string{dindPort},
			Env:          map[string]string{"DOCKER_TLS_CERTDIR": ""},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Privileged = true
			},
			WaitingFor: wait.ForHTTP("/_ping").WithPort(dindPort).WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start docker:%s-dind: %v", engineMajor, err)
	}
	endpoint, err := ctr.PortEndpoint(ctx, dindPort, "tcp")
	if err != nil {
		t.Fatalf("find the dind port: %v", err)
	}
	return endpoint
}

// TCPTransport dials one TCP address whatever the client asks for. It exists
// for the integration tests only: the product constructs nothing but a local
// socket transport, so a tcp:// engine can never be reached by configuration
// (D-21).
type TCPTransport struct {
	// Addr is host:port, with no scheme.
	Addr string
}

// TCPTransportFor builds a TCPTransport from a DockerHost value such as
// tcp://localhost:2375.
func TCPTransportFor(host string) TCPTransport {
	return TCPTransport{Addr: strings.TrimPrefix(host, "tcp://")}
}

// DialContext implements engine.Transport.
func (t TCPTransport) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", t.Addr)
}

// Describe implements engine.Transport.
func (t TCPTransport) Describe() string { return "tcp://" + t.Addr }

// RawClient returns a moby client to host, for test setup only (swarm init,
// creating containers). It is not the product API: production code reaches
// Docker only through dockerx.
func RawClient(t testing.TB, host string) *client.Client {
	t.Helper()
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		t.Fatalf("create the test setup client for %s: %v", host, err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// PublishEvent adds an event to the daemon's buffered history and sends it to
// every open /events stream, as the daemon does when something happens on the
// host. A zero TimeNano is replaced with the current time.
func (f *Fake) PublishEvent(e FakeEvent) {
	if e.TimeNano == 0 {
		e.TimeNano = time.Now().UnixNano()
	}
	f.evMu.Lock()
	defer f.evMu.Unlock()
	f.evHistory = append(f.evHistory, e)
	if len(f.evHistory) > eventBuffer {
		f.evHistory = f.evHistory[len(f.evHistory)-eventBuffer:]
	}
	for sub := range f.evSubs {
		select {
		case sub.ch <- e:
		default: // a stream nobody reads is as good as dropped
			sub.dropOnce.Do(func() { close(sub.drop) })
		}
	}
}

// ClearEvents empties the buffered history, as a daemon restart does.
func (f *Fake) ClearEvents() {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	f.evHistory = nil
}

// DropEventStreams ends every open /events stream without ending the daemon,
// as a network blip or a proxy timeout would.
func (f *Fake) DropEventStreams() {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	for sub := range f.evSubs {
		sub.dropOnce.Do(func() { close(sub.drop) })
	}
}

// OpenEventStreams is how many /events streams are open now.
func (f *Fake) OpenEventStreams() int {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	return len(f.evSubs)
}

// EventQueries returns the query string of every /events request served, in
// order, so a test can check the since and filters a client sent.
func (f *Fake) EventQueries() []url.Values {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	return append([]url.Values(nil), f.evQueries...)
}

// serveEvents answers GET /events like a daemon: the response starts at once
// (headers flushed), history at or after since is replayed when since is given,
// then new events follow until the client leaves or the stream is dropped.
func (f *Fake) serveEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	types := eventTypeFilter(q.Get("filters"))
	since, hasSince := parseEventSince(q.Get("since"))

	sub := &eventSub{ch: make(chan FakeEvent, 2*eventBuffer), drop: make(chan struct{})}
	f.evMu.Lock()
	f.evQueries = append(f.evQueries, q)
	var replay []FakeEvent
	if hasSince {
		for _, e := range f.evHistory {
			if e.TimeNano >= since {
				replay = append(replay, e)
			}
		}
	}
	f.evSubs[sub] = struct{}{}
	f.evMu.Unlock()
	defer func() {
		f.evMu.Lock()
		delete(f.evSubs, sub)
		f.evMu.Unlock()
	}()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	enc := json.NewEncoder(w)
	send := func(e FakeEvent) bool {
		if types != nil && !types[e.Type] {
			return true
		}
		scope := cmp.Or(e.Scope, "local")
		msg := events.Message{
			Type: events.Type(e.Type), Action: events.Action(e.Action), Scope: scope,
			Actor:    events.Actor{ID: e.ActorID, Attributes: e.Attributes},
			Time:     e.TimeNano / int64(time.Second),
			TimeNano: e.TimeNano,
		}
		return enc.Encode(msg) == nil && rc.Flush() == nil
	}
	for _, e := range replay {
		if !send(e) {
			return
		}
	}
	for {
		select {
		case e := <-sub.ch:
			if !send(e) {
				return
			}
		case <-sub.drop:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// eventTypeFilter reads the type terms of the filters query parameter, which is
// JSON like {"type":{"container":true}}. Nil means no type filter.
func eventTypeFilter(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	var f map[string]map[string]bool
	if json.Unmarshal([]byte(raw), &f) != nil || len(f["type"]) == 0 {
		return nil
	}
	return f["type"]
}

// parseEventSince reads since as unix seconds or seconds.nanoseconds and
// returns nanoseconds.
func parseEventSince(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	secs, frac, _ := strings.Cut(raw, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return 0, false
	}
	var ns int64
	if frac != "" {
		frac = (frac + "000000000")[:9]
		ns, _ = strconv.ParseInt(frac, 10, 64)
	}
	return s*int64(time.Second) + ns, true
}

// nodeList is the /nodes answer: the configured rows, or Nodes bare nodes.
func (c FakeConfig) nodeList() []swarm.Node {
	if len(c.NodeRows) == 0 {
		nodes := make([]swarm.Node, c.Nodes)
		for i := range nodes {
			nodes[i].ID = fmt.Sprintf("node%02d", i)
		}
		return nodes
	}
	out := make([]swarm.Node, 0, len(c.NodeRows))
	for _, fn := range c.NodeRows {
		role := fn.Role
		if role == "" {
			role = "worker"
		}
		avail := fn.Availability
		if avail == "" {
			avail = "active"
		}
		state := fn.State
		if state == "" {
			state = "ready"
		}
		n := swarm.Node{
			ID:   fn.ID,
			Spec: swarm.NodeSpec{Role: swarm.NodeRole(role), Availability: swarm.NodeAvailability(avail)},
			Description: swarm.NodeDescription{
				Hostname: fn.Hostname,
				Platform: swarm.Platform{OS: fn.OS, Architecture: "x86_64"},
				Engine:   swarm.EngineDescription{EngineVersion: fn.EngineVersion},
			},
			Status: swarm.NodeStatus{State: swarm.NodeState(state)},
		}
		if role == "manager" {
			reach := fn.Reachability
			if reach == "" {
				reach = "reachable"
			}
			n.ManagerStatus = &swarm.ManagerStatus{Leader: fn.Leader, Reachability: swarm.Reachability(reach)}
		}
		out = append(out, n)
	}
	return out
}

// serviceList is the /services answer: the configured rows, or Services bare
// services. The status counts are sent only when the client asked for them, as a
// real daemon does.
func (c FakeConfig) serviceList(withStatus bool) []swarm.Service {
	if len(c.ServiceRows) == 0 {
		services := make([]swarm.Service, c.Services)
		for i := range services {
			services[i].ID = fmt.Sprintf("svc%02d", i)
		}
		return services
	}
	out := make([]swarm.Service, 0, len(c.ServiceRows))
	for _, fs := range c.ServiceRows {
		s := swarm.Service{ID: fs.ID}
		s.Spec.Name = fs.Name
		s.Spec.Labels = fs.Labels
		s.UpdatedAt = fs.UpdatedAt
		switch fs.Mode {
		case "global":
			s.Spec.Mode.Global = &swarm.GlobalService{}
		case "replicated-job":
			s.Spec.Mode.ReplicatedJob = &swarm.ReplicatedJob{}
		case "global-job":
			s.Spec.Mode.GlobalJob = &swarm.GlobalJob{}
		default:
			s.Spec.Mode.Replicated = &swarm.ReplicatedService{}
		}
		if fs.UpdateState != "" {
			s.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateState(fs.UpdateState)}
		}
		if withStatus {
			s.ServiceStatus = &swarm.ServiceStatus{RunningTasks: fs.Running, DesiredTasks: fs.Desired}
		}
		out = append(out, s)
	}
	return out
}

// taskList is the /tasks answer. A task is listed only when the filter names its
// service, by ID or by name, as a real daemon lists only what matches.
func (c FakeConfig) taskList(rawFilters string) []swarm.Task {
	out := []swarm.Task{}
	var f map[string]map[string]bool
	if json.Unmarshal([]byte(rawFilters), &f) != nil {
		return out
	}
	alias := map[string]string{} // service ID <-> name
	for _, fs := range c.ServiceRows {
		alias[fs.ID], alias[fs.Name] = fs.Name, fs.ID
	}
	for _, ft := range c.AgentTasks {
		svc := ft.Service
		if svc == "" {
			svc = "bayplan-agent"
		}
		if !f["service"][svc] && !f["service"][alias[svc]] {
			continue
		}
		t := swarm.Task{ID: ft.ID, NodeID: ft.NodeID, ServiceID: svc}
		t.CreatedAt = ft.Created
		t.Status = swarm.TaskStatus{State: swarm.TaskState(ft.State), Err: ft.Error}
		out = append(out, t)
	}
	return out
}
