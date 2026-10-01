// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"net"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/engine"
)

func dockerFacts(version string) dockerx.VersionFacts {
	return dockerx.VersionFacts{
		Version:      version,
		PlatformName: "Docker Engine - Community",
		Components:   []dockerx.ComponentFacts{{Name: "Engine", Version: version}},
	}
}

func TestClassifyEngines(t *testing.T) {
	podman := dockerx.VersionFacts{
		Version:      "5.4.0",
		PlatformName: "Podman",
		Components:   []dockerx.ComponentFacts{{Name: "Podman Engine", Version: "5.4.0"}},
	}
	desktopPlatform := dockerFacts("29.1.0")
	desktopPlatform.PlatformName = "Docker Desktop 4.49.0 (208003)"

	cases := []struct {
		name       string
		api        string
		ver        dockerx.VersionFacts
		info       dockerx.InfoFacts
		supported  bool
		reason     string
		engineName string
	}{
		{"docker 1.12 era engine", "1.39", dockerx.VersionFacts{}, dockerx.InfoFacts{}, false, dockerx.ReasonAPITooOld, ""},
		{"docker 23", "1.42", dockerFacts("23.0.6"), dockerx.InfoFacts{}, false, dockerx.ReasonAPITooOld, "Docker Engine"},
		{"docker 24 is the floor", "1.43", dockerFacts("24.0.0"), dockerx.InfoFacts{}, true, "", "Docker Engine"},
		{"docker 29", "1.52", dockerFacts("29.1.0"), dockerx.InfoFacts{}, true, "", "Docker Engine"},
		{"newer than this client", "1.99", dockerFacts("31.0.0"), dockerx.InfoFacts{}, true, "", "Docker Engine"},
		{"three digit minor sorts numerically", "1.100", dockerFacts("40.0.0"), dockerx.InfoFacts{}, true, "", "Docker Engine"},
		{"single digit minor sorts numerically", "1.9", dockerFacts("1.0.0"), dockerx.InfoFacts{}, false, dockerx.ReasonAPITooOld, "Docker Engine"},
		{"garbage version", "abc", dockerx.VersionFacts{}, dockerx.InfoFacts{}, false, dockerx.ReasonUnknownAPI, ""},
		{"no version at all", "", dockerx.VersionFacts{}, dockerx.InfoFacts{}, false, dockerx.ReasonUnknownAPI, ""},
		{"patch component is not an API version", "1.43.1", dockerFacts("24.0.0"), dockerx.InfoFacts{}, false, dockerx.ReasonUnknownAPI, "Docker Engine"},
		{"podman by component", "1.41", podman, dockerx.InfoFacts{}, false, dockerx.ReasonPodman, "Podman"},
		{"podman by platform even at a new API", "1.52", dockerx.VersionFacts{PlatformName: "Podman"}, dockerx.InfoFacts{}, false, dockerx.ReasonPodman, "Podman"},
		{"podman with a garbage version is still named", "abc", podman, dockerx.InfoFacts{}, false, dockerx.ReasonPodman, "Podman"},
		{"rootless", "1.52", dockerFacts("29.1.0"),
			dockerx.InfoFacts{SecurityOptions: []string{"name=seccomp,profile=builtin", "name=rootless"}},
			false, dockerx.ReasonRootless, "Docker Engine"},
		{"rootless in a combined option", "1.52", dockerFacts("29.1.0"),
			dockerx.InfoFacts{SecurityOptions: []string{"name=cgroupns,name=rootless"}},
			false, dockerx.ReasonRootless, "Docker Engine"},
		{"a rootful engine with other options is fine", "1.52", dockerFacts("29.1.0"),
			dockerx.InfoFacts{SecurityOptions: []string{"name=apparmor,profile=default", "name=seccomp,profile=builtin", "name=cgroupns"}},
			true, "", "Docker Engine"},
		{"desktop by operating system", "1.52", dockerFacts("29.1.0"),
			dockerx.InfoFacts{OperatingSystem: "Docker Desktop"}, false, dockerx.ReasonDockerDesktop, "Docker Desktop"},
		{"desktop by platform name", "1.52", desktopPlatform, dockerx.InfoFacts{}, false, dockerx.ReasonDockerDesktop, "Docker Desktop"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			supported, reason, name, _ := dockerx.Classify(c.api, c.ver, c.info)
			if supported != c.supported || reason != c.reason || name != c.engineName {
				t.Errorf("Classify(%q) = supported %v, reason %q, engine %q; want %v, %q, %q",
					c.api, supported, reason, name, c.supported, c.reason, c.engineName)
			}
		})
	}
}

func TestClassifySwarmRole(t *testing.T) {
	cases := []struct {
		state   string
		control bool
		want    string
	}{
		{"active", true, dockerx.TypeSwarmManager},
		{"active", false, dockerx.TypeSwarmWorker},
		{"locked", false, dockerx.TypeSwarmWorker},
		{"locked", true, dockerx.TypeSwarmWorker},
		{"inactive", false, dockerx.TypeStandalone},
		{"", false, dockerx.TypeStandalone},
		{"pending", false, dockerx.TypeStandalone},
		{"error", false, dockerx.TypeStandalone},
	}
	for _, c := range cases {
		_, _, _, typ := dockerx.Classify("1.52", dockerFacts("29.1.0"),
			dockerx.InfoFacts{SwarmNodeState: c.state, ControlAvailable: c.control})
		if typ != c.want {
			t.Errorf("Classify(swarm %q, control %v) type = %q, want %q", c.state, c.control, typ, c.want)
		}
	}
}

func newClient(t *testing.T, tr engine.Transport) *dockerx.Client {
	t.Helper()
	c, err := dockerx.New(tr)
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestProbeReportsASupportedEngine(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "1.52", Version: "29.1.0", Hostname: "node-1"})
	res, err := newClient(t, fake).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Reachable || !res.Supported {
		t.Fatalf("Reachable=%v Supported=%v reason=%q, want both true", res.Reachable, res.Supported, res.Reason)
	}
	if res.EngineVersion != "29.1.0" || res.Hostname != "node-1" || res.APIVersion != "1.52" {
		t.Errorf("got version %q hostname %q api %q", res.EngineVersion, res.Hostname, res.APIVersion)
	}
	if res.NegotiatedAPI != "1.52" {
		t.Errorf("NegotiatedAPI = %q, want 1.52", res.NegotiatedAPI)
	}
	if res.Type != dockerx.TypeStandalone || res.EngineName != "Docker Engine" {
		t.Errorf("type %q engine %q", res.Type, res.EngineName)
	}
	if res.NCPU != 4 || res.MemTotal != 8<<30 || res.StorageDriver != "overlayfs" || res.OS != "linux" || res.Arch != "amd64" {
		t.Errorf("host facts not carried over: %+v", res)
	}
	if res.ProbedAt.IsZero() {
		t.Error("ProbedAt is zero")
	}
}

func TestProbeNamesAnEngineBelowTheFloorWithoutNegotiating(t *testing.T) {
	// The fake refuses any request newer than its own API, as Docker 23 does.
	// A client that negotiated or defaulted to 1.56 would fail here.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "1.41", Version: "20.10.24", Hostname: "old"})
	res, err := newClient(t, fake).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Reachable || res.Supported || res.Reason != dockerx.ReasonAPITooOld {
		t.Fatalf("Reachable=%v Supported=%v reason=%q", res.Reachable, res.Supported, res.Reason)
	}
	if res.EngineVersion != "20.10.24" || res.APIVersion != "1.41" || res.EngineName != "Docker Engine" {
		t.Errorf("unsupported engine not named: version %q api %q engine %q", res.EngineVersion, res.APIVersion, res.EngineName)
	}
	if res.NegotiatedAPI != "" {
		t.Errorf("NegotiatedAPI = %q, want empty: nothing is negotiated for an unsupported engine", res.NegotiatedAPI)
	}
	for _, p := range fake.Requests() {
		if strings.HasPrefix(p, "/v1.5") || p == "/info" || strings.HasSuffix(p, "/info") {
			t.Errorf("an unsupported engine was sent %s; only the raw ping and a version pinned to its own API are allowed", p)
		}
	}
	if !slices.Contains(fake.Requests(), "/v1.41/version") {
		t.Errorf("requests %v do not include /v1.41/version", fake.Requests())
	}
}

func TestProbeNamesPodman(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		APIVersion:   "1.41",
		Version:      "5.4.0",
		PlatformName: "Podman",
		Components:   []dockertest.Component{{Name: "Podman Engine", Version: "5.4.0"}},
	})
	res, err := newClient(t, fake).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Supported || res.Reason != dockerx.ReasonPodman || res.EngineName != "Podman" || res.EngineVersion != "5.4.0" {
		t.Errorf("got supported=%v reason=%q engine=%q version=%q", res.Supported, res.Reason, res.EngineName, res.EngineVersion)
	}
}

func TestProbeKeepsThePingDataWhenTheVersionCallFails(t *testing.T) {
	// API 1.30 is below the fake's minimum 1.40: /v1.30/version is refused, so
	// only the raw ping data is left.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "1.30", MinAPIVersion: "1.40"})
	res, err := newClient(t, fake).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Reachable || res.Supported || res.Reason != dockerx.ReasonAPITooOld || res.APIVersion != "1.30" {
		t.Errorf("got reachable=%v supported=%v reason=%q api=%q", res.Reachable, res.Supported, res.Reason, res.APIVersion)
	}
	if res.EngineVersion != "" {
		t.Errorf("EngineVersion = %q, want empty when /version could not be read", res.EngineVersion)
	}
}

func TestProbeTreatsAGarbageAPIVersionAsUnsupported(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "abc"})
	res, err := newClient(t, fake).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Reachable || res.Supported || res.Reason != dockerx.ReasonUnknownAPI || res.APIVersion != "abc" {
		t.Errorf("got reachable=%v supported=%v reason=%q api=%q", res.Reachable, res.Supported, res.Reason, res.APIVersion)
	}
}

func TestProbeFlagsRootlessAndDockerDesktop(t *testing.T) {
	rootless := dockertest.NewFake(t, dockertest.FakeConfig{SecurityOptions: []string{"name=seccomp,profile=builtin", "name=rootless"}})
	res, err := newClient(t, rootless).Probe(context.Background())
	if err != nil || res.Supported || res.Reason != dockerx.ReasonRootless {
		t.Errorf("rootless: err=%v supported=%v reason=%q", err, res.Supported, res.Reason)
	}

	desktop := dockertest.NewFake(t, dockertest.FakeConfig{OperatingSystem: "Docker Desktop"})
	res, err = newClient(t, desktop).Probe(context.Background())
	if err != nil || res.Supported || res.Reason != dockerx.ReasonDockerDesktop || res.EngineName != "Docker Desktop" {
		t.Errorf("desktop: err=%v supported=%v reason=%q engine=%q", err, res.Supported, res.Reason, res.EngineName)
	}
}

func TestProbeLabelsTheSwarmRole(t *testing.T) {
	cases := []struct {
		name    string
		state   string
		control bool
		want    string
	}{
		{"manager", "active", true, dockerx.TypeSwarmManager},
		{"worker", "active", false, dockerx.TypeSwarmWorker},
		{"locked", "locked", false, dockerx.TypeSwarmWorker},
		{"standalone", "inactive", false, dockerx.TypeStandalone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := dockertest.NewFake(t, dockertest.FakeConfig{SwarmState: c.state, SwarmControl: c.control})
			res, err := newClient(t, fake).Probe(context.Background())
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if res.Type != c.want || res.SwarmNodeState != c.state {
				t.Errorf("type %q swarm state %q, want %q and %q", res.Type, res.SwarmNodeState, c.want, c.state)
			}
		})
	}
}

type failingTransport struct{ path string }

func (f failingTransport) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, &net.OpError{Op: "dial", Net: "unix", Err: errSocketGone}
}
func (f failingTransport) Describe() string { return "unix://" + f.path }

type socketGone struct{}

func (socketGone) Error() string { return "no such file or directory" }

var errSocketGone = socketGone{}

func TestProbeReportsAnUnreachableEngineAndNamesTheSocket(t *testing.T) {
	c := newClient(t, failingTransport{path: "/run/missing/docker.sock"})
	res, err := c.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "/run/missing/docker.sock") {
		t.Fatalf("Probe error = %v, want one naming the socket path", err)
	}
	if res.Reachable || res.Supported || res.Reason != dockerx.ReasonUnreachable {
		t.Errorf("got reachable=%v supported=%v reason=%q", res.Reachable, res.Supported, res.Reason)
	}
	if res.Detail == "" {
		t.Error("Detail is empty, want the underlying dial error for the UI")
	}
	if res.ProbedAt.IsZero() {
		t.Error("ProbedAt is zero")
	}
}

func TestProbeReportsAReachableEngineWhoseInfoFails(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{FailInfo: true})
	res, err := newClient(t, fake).Probe(context.Background())
	if err == nil {
		t.Fatal("Probe error = nil, want the /info failure")
	}
	if !res.Reachable || res.Supported || res.Reason != dockerx.ReasonProbeFailed {
		t.Errorf("got reachable=%v supported=%v reason=%q", res.Reachable, res.Supported, res.Reason)
	}
}

func TestProbeRenegotiatesTheAPIVersionOnEveryCall(t *testing.T) {
	// D-26 and Pitfall 2: the daemon is upgraded, downgraded and upgraded
	// again between probes; the negotiated version must follow it each way.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "1.43", Version: "24.0.0"})
	c := newClient(t, fake)

	steps := []struct{ daemon, version, want string }{
		{"1.43", "24.0.0", "1.43"},
		{"1.52", "29.1.0", "1.52"},
		{"1.45", "26.0.0", "1.45"},
		{"1.99", "31.0.0", "1.56"}, // a daemon newer than this client is capped at the client's maximum
	}
	for _, s := range steps {
		fake.Reconfigure(dockertest.FakeConfig{APIVersion: s.daemon, Version: s.version})
		res, err := c.Probe(context.Background())
		if err != nil {
			t.Fatalf("daemon %s: Probe: %v", s.daemon, err)
		}
		if res.NegotiatedAPI != s.want || res.EngineVersion != s.version {
			t.Errorf("daemon %s: negotiated %q engine %q, want %q and %q", s.daemon, res.NegotiatedAPI, res.EngineVersion, s.want, s.version)
		}
	}
}

// countingTransport proves the moby client dials through the Transport and not
// through the host string (RESEARCH A14).
type countingTransport struct {
	engine.Transport
	dials atomic.Int64
}

func (c *countingTransport) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c.dials.Add(1)
	return c.Transport.DialContext(ctx, network, addr)
}

func TestTheTransportDecidesWhereTheBytesGo(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "via-transport"})
	ct := &countingTransport{Transport: fake}
	res, err := newClient(t, ct).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// The client's host string is unix:///var/run/docker.sock. If the dialer
	// override did not win, a machine with a real Docker would answer instead.
	if res.Hostname != "via-transport" {
		t.Errorf("Hostname = %q: the request did not go through the transport", res.Hostname)
	}
	if ct.dials.Load() == 0 {
		t.Error("the transport was never dialled")
	}
}

func TestNewIgnoresDockerEnvironmentVariables(t *testing.T) {
	// Pitfall 1: none of these may change how Bayplan talks to Docker.
	t.Setenv("DOCKER_API_VERSION", "1.20")
	t.Setenv("DOCKER_HOST", "tcp://203.0.113.9:2375")
	t.Setenv("DOCKER_CERT_PATH", "/nonexistent")
	t.Setenv("DOCKER_TLS_VERIFY", "1")

	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "1.52"})
	res, err := newClient(t, fake).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Supported || res.NegotiatedAPI != "1.52" {
		t.Errorf("supported=%v negotiated=%q: the environment changed the client", res.Supported, res.NegotiatedAPI)
	}
}

func TestConcurrentProbesDoNotRace(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{APIVersion: "1.52"})
	c := newClient(t, fake)

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := range 10 {
		wg.Go(func() {
			// Alternate the daemon's version so the renegotiation really
			// replaces the client while other probes are queued.
			if i%3 == 0 {
				fake.Reconfigure(dockertest.FakeConfig{APIVersion: "1.45", Version: "26.0.0"})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := c.Probe(ctx); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Probe: %v", err)
	}
}

// allowedMethods is the exported method set of *dockerx.Client. Later plans add
// their read-only methods here, deliberately and one at a time (D-52).
var allowedMethods = []string{"Close", "Probe"}

var forbiddenName = regexp.MustCompile(`Plugin|Swarm(Init|Join|Leave|Unlock)|Create|Remove|Start|Stop|Kill|Exec|Update|Prune|Raw|Do$|Passthrough`)

func TestTheClientExportsOnlyTheAllowedReadOnlyMethods(t *testing.T) {
	typ := reflect.TypeOf(&dockerx.Client{})
	var got []string
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	want := slices.Clone(allowedMethods)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("exported methods of *dockerx.Client = %v, want exactly %v", got, want)
	}
	for _, name := range got {
		if forbiddenName.MatchString(name) {
			t.Errorf("method %s matches a forbidden state-changing or passthrough name (D-52)", name)
		}
	}
}

func TestNoExportedMethodTakesOrReturnsAMobyType(t *testing.T) {
	typ := reflect.TypeOf(&dockerx.Client{})
	for i := range typ.NumMethod() {
		m := typ.Method(i)
		for j := range m.Type.NumIn() {
			if pkg := m.Type.In(j).PkgPath(); strings.Contains(pkg, "moby") {
				t.Errorf("%s takes a type from %s", m.Name, pkg)
			}
		}
		for j := range m.Type.NumOut() {
			if pkg := m.Type.Out(j).PkgPath(); strings.Contains(pkg, "moby") {
				t.Errorf("%s returns a type from %s", m.Name, pkg)
			}
		}
	}
}
