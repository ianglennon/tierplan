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
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"
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
	// FailInfo makes /info answer 500, for the partial-failure path.
	FailInfo bool
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

	f := &Fake{path: filepath.Join(dir, "d.sock"), cfg: cfg.withDefaults()}
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
	return f.cfg
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
			ID:              "FAKE:ID",
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
			Swarm: swarm.Info{
				LocalNodeState:   swarm.LocalNodeState(cfg.SwarmState),
				ControlAvailable: cfg.SwarmControl,
			},
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "page not found"})
	}
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
