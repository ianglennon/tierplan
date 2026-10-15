// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/moby/moby/api/types/swarm"

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

func TestProbeReportsTheEngineIDTheSwarmNodeIDAndOnAManagerTheClusterID(t *testing.T) {
	cases := []struct {
		name                     string
		cfg                      dockertest.FakeConfig
		wantEngine, wantNode     string
		wantCluster, description string
	}{
		{"standalone", dockertest.FakeConfig{EngineID: "ENG:solo"}, "ENG:solo", "", "", "a host outside any swarm has an engine ID and nothing else"},
		{"manager", dockertest.FakeConfig{EngineID: "ENG:m", SwarmState: "active", SwarmControl: true, SwarmNodeID: "node-m", SwarmClusterID: "cluster-1"}, "ENG:m", "node-m", "cluster-1", "a manager reports its node and its cluster"},
		{"worker", dockertest.FakeConfig{EngineID: "ENG:w", SwarmState: "active", SwarmNodeID: "node-w", SwarmClusterID: "cluster-1"}, "ENG:w", "node-w", "", "a worker's cluster ID is never kept, as the agent drops it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := newClient(t, dockertest.NewFake(t, c.cfg)).Probe(context.Background())
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if res.EngineID != c.wantEngine || res.SwarmNodeID != c.wantNode || res.SwarmClusterID != c.wantCluster {
				t.Errorf("engine %q node %q cluster %q, want %q %q %q (%s)", res.EngineID, res.SwarmNodeID, res.SwarmClusterID, c.wantEngine, c.wantNode, c.wantCluster, c.description)
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
// their methods here, deliberately and one at a time (D-52). ContainerAction (Phase
// 4) is the first typed write: it takes a closed set of verbs, never a path, and the
// forbidden-name pattern below still refuses any raw or passthrough shape.
// StartExec (Phase 4, OBS-03) is the typed interactive shell: it takes a container
// ID and an argv and sets only the TTY, size, user, working directory and command.
// ContainerRemove (Phase 4) removes one container by ID with two booleans, force and
// anonymous volumes; it never sets links and takes no path.
// ContainerDetail (Phase 4, RES-01) is a read: one inspect by ID, typed.
// NetworkCreate (Phase 4, RES-06) takes a typed spec of a name, one of four drivers and
// its addressing; it cannot express an ingress, config-only or config-from network.
// ServiceRefs (Phase 4, RES-06) is a read: the networks and volumes each swarm service spec mentions.
// NetworkDetail (Phase 4, RES-06) is a read: one network inspect by ID, typed, with the raw document.
// NetworkRemove (Phase 4, RES-06) removes one network by ID, once; it takes no path and
// no options, and the API refuses a built-in or an in-use network before it is called.
// NetworkConnect (Phase 4, RES-06) takes a network ID, a container ID and an EndpointSpec of an
// alias and two addresses; it cannot express links, driver options or a MAC address.
// NetworkDisconnect (Phase 4, RES-06) takes a network ID and a container ID and is never forced.
// VolumeCreate (Phase 4, RES-07) takes a typed spec of a name, a driver, driver options and labels;
// it cannot express a cluster volume. VolumeExists (Phase 4, RES-07) is a read: one inspect by name.
// VolumeDriver (Phase 4, RES-07) is a read: the same inspect, answering the driver.
// VolumeUsers (Phase 4, RES-07) is a read: the names of the containers that mount one volume.
// VolumeDetail (Phase 4, RES-07) is a read: one inspect by name, with the raw document.
// VolumeSizes (Phase 4, RES-07) is a read: the engine's disk usage of its volumes, which takes no
// options, so the caller can ask for nothing else.
// VolumeRemove (Phase 4, RES-07) removes one volume by name, once; it takes no options, so force
// is never sent, and the API refuses a volume that is in use before it is called.
// ImagePull (Phase 5, D-25) pulls an image by a full reference and reports its progress; it takes
// the reference, an optional encoded credential and platform, and nothing that picks a path or a
// registry address, and it is never retried. It is the only method whose name carries Pull.
// DistributionInspect (Phase 5, D-13) is a read: it asks the engine to read one manifest from its registry with
// an optional encoded credential, to say whether the node can pull; it takes a reference and nothing
// that picks a path or a registry address, and it is never retried.
// ImageDetail (Phase 5, RES-09) is a read: one image inspect by ID, typed, with the raw document; it asks
// for no manifests and no platform.
// ImageHistory (Phase 5, RES-09) is a read: the layers of one image by ID, with no platform option.
// ImageRemove (Phase 5, RES-09) removes one image by ID and untags the names it is given first, once; it has
// no force option, so an image a container uses is refused by the engine. ImageUntag (Phase 5, RES-09)
// removes one name of an image and never sends force. ImageUsers (Phase 5, RES-09) is a read: the names of
// the containers created from one image.
// ServiceInspect (Phase 6, RES-02) is a read: one service inspect by ID or name, typed, with the raw
// document; it asks for no defaults. SwarmNetworkNames (Phase 6, RES-02) is a read: it only lists the
// swarm networks, to name a service's attachments. ServiceScale (Phase 6, RES-02) changes one service's
// replica count, once, with the version it just read; it takes an ID, a count and an optional credential, cannot express
// any other change to the spec, and is never retried.
// ResolveImage (Phase 6, RES-02) is a read: it asks the engine's registry for the digest a tag points at now, with
// an optional credential. ServiceForceUpdate (Phase 6, RES-02) bumps ForceUpdate on one service and optionally
// pins its image, once, with the version it just read; it takes an ID, a version, a pinned reference, platforms and
// a credential, takes no spec from the caller and cannot express any other change. ServiceRollback (Phase 6,
// RES-02) rolls one service back to its previous spec, once, with the version it just read.
// ServiceRemove (Phase 6, RES-02) removes one service by ID, once; it takes no options, and the API refuses a
// service a stack owns and a Bayplan agent service nobody confirmed before it is called.
// NodeUpdate (Phase 6, RES-05) changes one node's availability, role or labels, once, with the version the
// caller read: it reads the node, sends its whole spec with only those fields changed and never retries; it
// takes an ID and a NodeChange and cannot express any other change.
// NodeRemove (Phase 6, RES-05, D-22) removes one node by ID, once, and never forced: it takes an ID and no
// options, and the API refuses a node that is not Down or is a manager before it is called.
// Tasks (Phase 6, RES-04) is a read: swarm tasks by service, node, task ID or desired state, typed; a
// filter that names none is refused before any call. TaskHistoryLimit (Phase 6, RES-04) is a read: the
// cluster's task history retention limit, from the engine's info.
// ContainerLogs (Phase 4, OBS-01) is a read: one inspect, then the engine's log of the container
// with stdout, stderr and timestamps always on; it takes only the time range, the tail and follow.
var allowedMethods = []string{"Close", "Cluster", "ContainerAction", "ContainerDetail", "ContainerLogs", "ContainerOwnership", "ContainerRemove", "DistributionInspect", "Events", "ImageDetail", "ImageHistory", "ImagePull", "ImageRemove", "ImageUntag", "ImageUsers", "NetworkConnect", "NetworkCreate", "NetworkDetail", "NetworkDisconnect", "NetworkRemove", "NodeInspect", "NodeRemove", "NodeUpdate", "Probe", "RestartPolicies", "ResolveImage", "ServiceForceUpdate", "ServiceInspect", "ServiceRefs", "ServiceRemove", "ServiceRollback", "ServiceScale", "Snapshot", "StartExec", "SwarmNetworkNames", "TaskHistoryLimit", "Tasks", "VolumeCreate", "VolumeDetail", "VolumeDriver", "VolumeExists", "VolumeRemove", "VolumeSizes", "VolumeUsers"}

// typedMethodsWithForbiddenWords are allowed methods whose names contain a word the
// pattern below refuses, each reviewed one by one.
var typedMethodsWithForbiddenWords = []string{"ServiceForceUpdate", "ServiceRemove", "StartExec", "ContainerRemove", "NetworkCreate", "NetworkRemove", "VolumeCreate", "VolumeRemove", "ImageRemove", "NodeRemove", "NodeUpdate"}

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
		if slices.Contains(typedMethodsWithForbiddenWords, name) {
			continue
		}
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

// The resource snapshot (plan 18, ENV-07, D-55, D-56, Pitfall 22). The fake
// daemon serves the list endpoints, so the real client code runs.

func fakeEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{
			{ID: "c3", Names: []string{"/db"}, Image: "postgres:17", State: "exited", Status: "Exited (0) 2 hours ago", Created: 1700000300},
			{ID: "c1", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 3 hours", Created: 1700000100,
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 80, Public: 8080, Type: "tcp"}}},
			{ID: "c2", Names: []string{"/cache"}, Image: "redis:8", State: "running", Status: "Up 3 hours", Created: 1700000200,
				Ports: []dockertest.FakePort{{Private: 6379, Type: "tcp"}}},
		},
		Images: []dockertest.FakeImage{
			{ID: "sha256:aaaa", RepoTags: []string{"nginx:1.27"}, Size: 1000, Created: 1700000000},
			{ID: "sha256:bbbb", Size: 2000, Created: 1700000500},
		},
		Volumes: []dockertest.FakeVolume{{Name: "data", Driver: "local", CreatedAt: "2026-03-04T05:06:07Z"}},
		Networks: []dockertest.FakeNetwork{
			{ID: "n4", Name: "none", Driver: "null", Scope: "local"},
			{ID: "n1", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "n3", Name: "host", Driver: "host", Scope: "local"},
			{ID: "n2", Name: "app", Driver: "bridge", Scope: "local", Subnets: []string{"172.20.0.0/16", "fd00::/64"}},
		},
	}
}

func snapshot(t *testing.T, cfg dockertest.FakeConfig, swarmManager bool) (dockerx.Snapshot, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, cfg)
	snap, err := newClient(t, fake).Snapshot(context.Background(), swarmManager)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snap, fake
}

func TestSnapshotCountsEqualTheLengthOfTheListsTheyLinkTo(t *testing.T) {
	snap, _ := snapshot(t, fakeEngine(), false)
	c := snap.Counts
	if c.ContainersRunning != 2 || c.ContainersStopped != 1 || c.Containers != 3 || c.Images != 2 || c.Volumes != 1 || c.Networks != 4 {
		t.Errorf("counts = %+v", c)
	}
	if c.Containers != len(snap.Containers) || c.Images != len(snap.Images) || c.Volumes != len(snap.Volumes) || c.Networks != len(snap.Networks) {
		t.Errorf("counts %+v do not match the list lengths %d/%d/%d/%d (Pitfall 22)",
			c, len(snap.Containers), len(snap.Images), len(snap.Volumes), len(snap.Networks))
	}
	if c.Nodes != nil || c.Services != nil {
		t.Errorf("a standalone engine has no node or service counts: %+v", c)
	}
	if snap.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero")
	}
}

func TestSnapshotListsStoppedContainersToo(t *testing.T) {
	// Without all=1 a real daemon omits exited containers; the fake does too.
	snap, _ := snapshot(t, fakeEngine(), false)
	found := false
	for _, c := range snap.Containers {
		found = found || c.Name == "db"
	}
	if !found {
		t.Errorf("the exited container is missing: %+v", snap.Containers)
	}
}

func TestSnapshotRowsAreFormattedForTheLists(t *testing.T) {
	snap, _ := snapshot(t, fakeEngine(), false)

	byName := map[string]dockerx.ContainerRow{}
	for _, c := range snap.Containers {
		byName[c.Name] = c
	}
	web, ok := byName["web"]
	if !ok {
		t.Fatalf("no container named web (the leading slash must go): %+v", snap.Containers)
	}
	if web.State != "running" || web.Status != "Up 3 hours" || web.Image != "nginx:1.27" || web.ID != "c1" {
		t.Errorf("web = %+v", web)
	}
	if want := time.Unix(1700000100, 0).UTC(); !web.Created.Equal(want) || web.Created.Location() != time.UTC {
		t.Errorf("web.Created = %v, want %v in UTC", web.Created, want)
	}
	if !slices.Equal(web.Ports, []string{"0.0.0.0:8080->80/tcp"}) {
		t.Errorf("web ports = %q", web.Ports)
	}
	if !slices.Equal(byName["cache"].Ports, []string{"6379/tcp"}) {
		t.Errorf("an unpublished port reads %q, want 6379/tcp", byName["cache"].Ports)
	}

	var untagged, tagged *dockerx.ImageRow
	for i := range snap.Images {
		switch snap.Images[i].ID {
		case "sha256:bbbb":
			untagged = &snap.Images[i]
		case "sha256:aaaa":
			tagged = &snap.Images[i]
		}
	}
	if untagged == nil || tagged == nil {
		t.Fatalf("images = %+v", snap.Images)
	}
	if !slices.Equal(untagged.RepoTags, []string{"<none>"}) {
		t.Errorf("the untagged image has repoTags %q, want [<none>]", untagged.RepoTags)
	}
	if !slices.Equal(tagged.RepoTags, []string{"nginx:1.27"}) || tagged.Size != 1000 {
		t.Errorf("tagged image = %+v", tagged)
	}

	if v := snap.Volumes[0]; v.Name != "data" || v.Driver != "local" || !v.Created.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) {
		t.Errorf("volume = %+v", v)
	}

	nets := map[string]dockerx.NetworkRow{}
	for _, n := range snap.Networks {
		nets[n.Name] = n
	}
	if n := nets["app"]; !slices.Equal(n.Subnets, []string{"172.20.0.0/16", "fd00::/64"}) || n.Driver != "bridge" || n.Scope != "local" {
		t.Errorf("app network = %+v", n)
	}
	if n := nets["host"]; n.Subnets == nil || len(n.Subnets) != 0 {
		t.Errorf("a network with no subnet has Subnets %#v, want an empty non-nil slice", n.Subnets)
	}
}

func TestSnapshotOfAnEmptyEngineHasEmptyNonNilSlicesAndZeroCounts(t *testing.T) {
	snap, _ := snapshot(t, dockertest.FakeConfig{}, false)
	if snap.Containers == nil || snap.Images == nil || snap.Volumes == nil || snap.Networks == nil {
		t.Errorf("a nil slice would serialise as null: %#v %#v %#v %#v", snap.Containers, snap.Images, snap.Volumes, snap.Networks)
	}
	if len(snap.Containers)+len(snap.Images)+len(snap.Volumes)+len(snap.Networks) != 0 {
		t.Errorf("an empty engine listed something: %+v", snap)
	}
	if c := snap.Counts; c.Containers != 0 || c.ContainersRunning != 0 || c.ContainersStopped != 0 || c.Images != 0 || c.Volumes != 0 || c.Networks != 0 {
		t.Errorf("counts = %+v", c)
	}
}

func TestSnapshotOrdersEqualSortKeysByID(t *testing.T) {
	cfg := dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{
			{ID: "cz", Names: []string{"/web"}, State: "running"},
			{ID: "ca", Names: []string{"/web"}, State: "running"},
			{ID: "cm", Names: []string{"/api"}, State: "running"},
		},
		Images: []dockertest.FakeImage{
			{ID: "sha256:2", Created: 100},
			{ID: "sha256:1", Created: 100},
			{ID: "sha256:3", Created: 300},
		},
		Volumes: []dockertest.FakeVolume{{Name: "b"}, {Name: "a"}},
		Networks: []dockertest.FakeNetwork{
			{ID: "n2", Name: "x"}, {ID: "n1", Name: "x"}, {ID: "n0", Name: "w"},
		},
	}
	// The fake lists them in an order that is not the sorted one, so an
	// ordering that merely mirrored the daemon's would fail.
	snap, _ := snapshot(t, cfg, false)

	var cs []string
	for _, c := range snap.Containers {
		cs = append(cs, c.Name+"/"+c.ID)
	}
	if want := []string{"api/cm", "web/ca", "web/cz"}; !slices.Equal(cs, want) {
		t.Errorf("containers = %v, want %v (name, then ID)", cs, want)
	}
	var is []string
	for _, i := range snap.Images {
		is = append(is, i.ID)
	}
	if want := []string{"sha256:3", "sha256:1", "sha256:2"}; !slices.Equal(is, want) {
		t.Errorf("images = %v, want %v (newest first, then ID)", is, want)
	}
	if snap.Volumes[0].Name != "a" || snap.Volumes[1].Name != "b" {
		t.Errorf("volumes = %+v, want by name", snap.Volumes)
	}
	var ns []string
	for _, n := range snap.Networks {
		ns = append(ns, n.Name+"/"+n.ID)
	}
	if want := []string{"w/n0", "x/n1", "x/n2"}; !slices.Equal(ns, want) {
		t.Errorf("networks = %v, want %v (name, then ID)", ns, want)
	}
}

func TestSnapshotCountsEveryStateButRunningAsStopped(t *testing.T) {
	var cs []dockertest.FakeContainer
	for i, state := range []string{"running", "created", "paused", "restarting", "removing", "exited", "dead"} {
		cs = append(cs, dockertest.FakeContainer{ID: "c" + string(rune('a'+i)), Names: []string{"/" + state}, State: state})
	}
	snap, _ := snapshot(t, dockertest.FakeConfig{Containers: cs}, false)
	if c := snap.Counts; c.ContainersRunning != 1 || c.ContainersStopped != 6 || c.Containers != 7 {
		t.Errorf("counts = %+v, want 1 running, 6 stopped (Pitfall 21: removing is stopped)", c)
	}
}

func TestSnapshotOnASwarmManagerAlsoCountsNodesAndServices(t *testing.T) {
	cfg := fakeEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.Nodes, cfg.Services = "active", true, 3, 5
	snap, _ := snapshot(t, cfg, true)
	if snap.Counts.Nodes == nil || *snap.Counts.Nodes != 3 || snap.Counts.Services == nil || *snap.Counts.Services != 5 {
		t.Errorf("nodes/services = %v/%v, want 3/5", snap.Counts.Nodes, snap.Counts.Services)
	}
}

func TestSnapshotOnAWorkerNeverAsksForNodesOrServices(t *testing.T) {
	cfg := fakeEngine()
	cfg.SwarmState, cfg.SwarmControl = "active", false
	snap, fake := snapshot(t, cfg, false)
	if snap.Counts.Nodes != nil || snap.Counts.Services != nil {
		t.Errorf("a worker has no node or service counts: %+v", snap.Counts)
	}
	for _, p := range fake.Requests() {
		if strings.HasSuffix(p, "/nodes") || strings.HasSuffix(p, "/services") {
			t.Errorf("asked a worker for %s, which fails there (Pattern 5)", p)
		}
	}
}

func TestSnapshotRowsCannotCarryEnvironmentVariablesLabelsOrMounts(t *testing.T) {
	// T-01-77. A field that could hold these must not exist on a row type.
	// The one deliberate exception (plan 04-06): a ContainerRow keeps its labels, mounts
	// and network attachments for the search and the usage rules of networks and
	// volumes. It never has a variable or a command. The API's container list sends
	// an allowlist and never these fields; TestListsNeverCarryLabelsMountsOrVariables
	// proves it (T-04-29). A NetworkRow keeps its labels (plan 04-13) for the same
	// reason: the owner column and the search read the stack and project labels, and the
	// list sends only the owner. A VolumeRow keeps its labels (plan 04-17) for the stack and
	// project columns and the search; the list sends only those two and a flag, and
	// the option values of a volume (which can hold a share password) are never on a row.
	banned := regexp.MustCompile(`(?i)^(labels?|env|environment|mounts?|binds?|command|cmd|entrypoint|options|attachments|containers)$`)
	internal := map[string]bool{"ContainerRow.Labels": true, "ContainerRow.Mounts": true, "NetworkRow.Labels": true, "VolumeRow.Labels": true}
	for _, v := range []any{dockerx.ContainerRow{}, dockerx.ImageRow{}, dockerx.VolumeRow{}, dockerx.NetworkRow{}} {
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			if f := typ.Field(i); banned.MatchString(f.Name) && !internal[typ.Name()+"."+f.Name] {
				t.Errorf("%s has field %s, which can hold secrets", typ.Name(), f.Name)
			}
		}
	}
}

func TestSnapshotContainerRowsKeepLabelsMountsAndNetworksForTheServer(t *testing.T) {
	cfg := dockertest.FakeConfig{Containers: []dockertest.FakeContainer{{
		ID: "c1", Names: []string{"/web"}, State: "running",
		Labels: map[string]string{"com.docker.compose.project": "shop"},
		Mounts: []string{"data", "/srv/x"},
		Networks: map[string]dockertest.FakeEndpoint{
			"zeta":  {NetworkID: "nz", IPv4: "10.0.1.5"},
			"alpha": {NetworkID: "na", IPv4: "10.0.0.5", IPv6: "fd00::5", MAC: "02:42:0a:00:00:05"},
		},
	}}}
	snap, _ := snapshot(t, cfg, false)
	row := snap.Containers[0]
	if row.Labels["com.docker.compose.project"] != "shop" {
		t.Errorf("labels = %v", row.Labels)
	}
	if len(row.Mounts) != 2 || row.Mounts[0].Type != "volume" || row.Mounts[0].Name != "data" || row.Mounts[1].Type != "bind" || row.Mounts[1].Source != "/srv/x" {
		t.Errorf("mounts = %+v", row.Mounts)
	}
	if len(row.Networks) != 2 || row.Networks[0].Name != "alpha" || row.Networks[0].NetworkID != "na" || row.Networks[0].IPv4 != "10.0.0.5" ||
		row.Networks[0].IPv6 != "fd00::5" || row.Networks[0].MAC != "02:42:0a:00:00:05" || row.Networks[1].Name != "zeta" {
		t.Errorf("networks = %+v, want alpha then zeta", row.Networks)
	}
}

func TestSnapshotContainerHealthComesFromTheSummaryOrTheStatusText(t *testing.T) {
	for _, tc := range []struct {
		name, health, status, want string
	}{
		{"summary unhealthy", "unhealthy", "Up 2 minutes", "unhealthy"},
		{"summary healthy", "healthy", "Up 2 minutes", "healthy"},
		{"summary starting", "starting", "Up 2 seconds", "starting"},
		{"summary wins over the text", "healthy", "Up 2 minutes (unhealthy)", "healthy"},
		{"text unhealthy", "", "Up 2 minutes (unhealthy)", "unhealthy"},
		{"text starting", "", "Up 5 seconds (health: starting)", "starting"},
		{"text healthy", "", "Up 1 hour (healthy)", "healthy"},
		{"no healthcheck", "", "Up 1 hour", "none"},
		{"summary none", "none", "Up 1 hour", "none"},
		{"exited", "", "Exited (1) 2 hours ago", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := dockertest.FakeConfig{Containers: []dockertest.FakeContainer{
				{ID: "c1", Names: []string{"/web"}, State: "running", Status: tc.status, Health: tc.health},
			}}
			snap, _ := snapshot(t, cfg, false)
			if got := snap.Containers[0].Health; got != tc.want {
				t.Errorf("health = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSnapshotFailsWhenTheEngineIsDown(t *testing.T) {
	fake := dockertest.NewFake(t, fakeEngine())
	cli := newClient(t, fake)
	fake.Stop()
	if _, err := cli.Snapshot(context.Background(), false); err == nil {
		t.Fatal("Snapshot succeeded against a stopped engine")
	}
}

// The events stream (plan 21, ENV-07, D-57, A12, Pitfall 7). The fake daemon
// serves /events over a real socket, so the real client code runs.

// waitFor polls cond until it holds, failing after ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// recvRow waits for the next row, failing when none comes.
func recvRow(t *testing.T, rows <-chan dockerx.EventRow) dockerx.EventRow {
	t.Helper()
	select {
	case r, ok := <-rows:
		if !ok {
			t.Fatal("the events channel closed while a row was expected")
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event row")
	}
	return dockerx.EventRow{}
}

func TestEventsKeepsContainerImageNetworkAndVolumeEventsAndDropsExecNoise(t *testing.T) {
	base := time.Now().Add(-time.Minute).UnixNano()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "container", Action: "create", ActorID: "c1", TimeNano: base + 1, Attributes: map[string]string{"name": "web", "image": "nginx:1.27"}},
		{Type: "container", Action: "exec_start: sh -c 'echo $SECRET'", ActorID: "c1", TimeNano: base + 2, Attributes: map[string]string{"name": "web"}},
		{Type: "image", Action: "pull", ActorID: "nginx:1.27", TimeNano: base + 4, Attributes: map[string]string{"name": "nginx"}},
		{Type: "network", Action: "connect", ActorID: "n1", TimeNano: base + 5, Attributes: map[string]string{"name": "app", "container": "c1"}},
		{Type: "volume", Action: "create", ActorID: "data", TimeNano: base + 6, Attributes: map[string]string{"driver": "local"}},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rows, _ := newClient(t, fake).Events(ctx, time.Now().Add(-time.Hour))
	var got []dockerx.EventRow
	for range 4 {
		got = append(got, recvRow(t, rows))
	}

	wantTypes := []string{"container", "image", "network", "volume"}
	wantActions := []string{"create", "pull", "connect", "create"}
	for i, r := range got {
		if r.Type != wantTypes[i] || r.Action != wantActions[i] {
			t.Errorf("row %d = %s %s, want %s %s", i, r.Type, r.Action, wantTypes[i], wantActions[i])
		}
		if r.ID == "" || r.TimeNano != base+int64(1+[]int{0, 3, 4, 5}[i]) || r.Time.IsZero() || r.Time.Location() != time.UTC {
			t.Errorf("row %d has ID %q, TimeNano %d, Time %v", i, r.ID, r.TimeNano, r.Time)
		}
	}
	if got[0].ActorID != "c1" || got[0].Name != "web" {
		t.Errorf("the container row = %+v", got[0])
	}
	if got[3].Name != "data" {
		t.Errorf("a volume's name is its actor ID, got %q", got[3].Name)
	}
	select {
	case r := <-rows:
		t.Errorf("a fifth row arrived: %+v (exec events must be dropped)", r)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestEventsCarryTheDaemonsScopeSoAClusterEventCanBeToldFromANodeOne(t *testing.T) {
	base := time.Now().Add(-time.Minute).UnixNano()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "container", Action: "start", ActorID: "c1", TimeNano: base + 1},
		{Type: "network", Action: "create", ActorID: "ovl", Scope: "swarm", TimeNano: base + 2},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, _ := newClient(t, fake).Events(ctx, time.Now().Add(-time.Hour))
	if r := recvRow(t, rows); r.Type != "container" || r.Scope != "local" {
		t.Errorf("container row = %+v, want scope local", r)
	}
	if r := recvRow(t, rows); r.Type != "network" || r.Scope != "swarm" {
		t.Errorf("network row = %+v, want scope swarm", r)
	}
}

func TestEventsDropsAttachDetachResizeAndTopToo(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, _ := newClient(t, fake).Events(ctx, time.Time{})
	waitFor(t, "the stream to open", func() bool { return fake.OpenEventStreams() == 1 })

	for _, a := range []string{"attach", "detach", "resize", "top", "exec_die", "exec_create: sh"} {
		fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: a, ActorID: "c1"})
	}
	fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "c1"})
	if r := recvRow(t, rows); r.Action != "die" {
		t.Errorf("first kept row = %+v, want the die event", r)
	}
}

func TestEventsCopyOnlyTheAllowlistedAttributes(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, _ := newClient(t, fake).Events(ctx, time.Time{})
	waitFor(t, "the stream to open", func() bool { return fake.OpenEventStreams() == 1 })

	fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "c1", Attributes: map[string]string{
		"name": "web", "image": "nginx:1.27", "exitCode": "137", "signal": "9",
		"com.docker.compose.project": "shop", "com.docker.stack.namespace": "prod",
		"secret.token": "hunter2", "API_TOKEN": "hunter2", "execID": "abc", "com.example.owner": "ian",
		"com.docker.compose.service": "web", "PASSWORD=hunter2": "", "traefik.http.routers.x.rule": "Host(`x`)",
	}})
	r := recvRow(t, rows)
	want := map[string]string{
		"name": "web", "image": "nginx:1.27", "exitCode": "137", "signal": "9",
		"com.docker.compose.project": "shop", "com.docker.stack.namespace": "prod",
	}
	if !reflect.DeepEqual(r.Attributes, want) {
		t.Errorf("Attributes = %v, want only %v (D-61, T-01-88)", r.Attributes, want)
	}
}

func TestEventsDeliverSwarmServiceNodeSecretAndConfigEvents(t *testing.T) {
	// D-48: a swarm manager's events are in the feed. The fake applies the type
	// filter the client sent, so a type missing from the filter would never arrive.
	base := time.Now().Add(-time.Minute).UnixNano()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "service", Action: "update", ActorID: "svc1", Scope: "swarm", TimeNano: base + 1, Attributes: map[string]string{"name": "web", "com.docker.stack.namespace": "prod", "updatestate.new": "updating"}},
		{Type: "node", Action: "update", ActorID: "n1", Scope: "swarm", TimeNano: base + 2, Attributes: map[string]string{"name": "worker-1"}},
		{Type: "secret", Action: "create", ActorID: "s1", Scope: "swarm", TimeNano: base + 3, Attributes: map[string]string{"name": "db-password"}},
		{Type: "config", Action: "remove", ActorID: "cfg1", Scope: "swarm", TimeNano: base + 4, Attributes: map[string]string{"name": "nginx.conf"}},
		{Type: "plugin", Action: "install", ActorID: "p1", TimeNano: base + 5},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, _ := newClient(t, fake).Events(ctx, time.Now().Add(-time.Hour))
	for i, want := range []struct{ typ, name string }{{"service", "web"}, {"node", "worker-1"}, {"secret", "db-password"}, {"config", "nginx.conf"}} {
		r := recvRow(t, rows)
		if r.Type != want.typ || r.Name != want.name || r.Scope != "swarm" {
			t.Errorf("row %d = %+v, want a swarm %s named %s", i, r, want.typ, want.name)
		}
		if _, leaked := r.Attributes["updatestate.new"]; leaked {
			t.Errorf("row %d leaks an attribute outside the allowlist: %v", i, r.Attributes)
		}
	}
	select {
	case r := <-rows:
		t.Errorf("a plugin event arrived: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestEventsTurnAHealthStatusChangeIntoAnInvalidateOnlyRowWithoutAttributes(t *testing.T) {
	// RES-14: a list must show Unhealthy at once, but a feed for people has no use
	// for every check, and the attributes of the event can name the check command.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, _ := newClient(t, fake).Events(ctx, time.Time{})
	waitFor(t, "the stream to open", func() bool { return fake.OpenEventStreams() == 1 })

	fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "health_status: unhealthy", ActorID: "c1", Attributes: map[string]string{
		"name": "web", "image": "nginx:1.27", "healthcheck": "curl -u admin:hunter2 localhost", "API_TOKEN": "hunter2",
	}})
	r := recvRow(t, rows)
	if !r.InvalidateOnly || r.Type != "container" || r.ActorID != "c1" || r.ID == "" {
		t.Fatalf("row = %+v, want an invalidate-only container row for c1", r)
	}
	if len(r.Attributes) != 0 || r.Attributes == nil || r.Name != "" {
		t.Errorf("Attributes = %#v, Name = %q, want an empty, non-nil map and no name", r.Attributes, r.Name)
	}

	// A kept event next to it is a normal row.
	fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "c1"})
	if r := recvRow(t, rows); r.InvalidateOnly || r.Action != "die" {
		t.Errorf("next row = %+v, want a normal die row", r)
	}
}

func TestEventsAskForTheEightTypesAndPassSinceAsSecondsOrSecondsAndNanoseconds(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	cli := newClient(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _ = cli.Events(ctx, time.Unix(1700000000, 0))
	_, _ = cli.Events(ctx, time.Unix(1700000000, 123456789))
	_, _ = cli.Events(ctx, time.Time{})
	waitFor(t, "three requests", func() bool { return len(fake.EventQueries()) == 3 })

	qs := fake.EventQueries()
	sinces := []string{qs[0].Get("since"), qs[1].Get("since"), qs[2].Get("since")}
	slices.Sort(sinces)
	want := []string{"", "1700000000", "1700000000.123456789"}
	if !slices.Equal(sinces, want) {
		t.Errorf("since values = %q, want %q", sinces, want)
	}
	var f map[string]map[string]bool
	if err := json.Unmarshal([]byte(qs[0].Get("filters")), &f); err != nil {
		t.Fatalf("filters %q: %v", qs[0].Get("filters"), err)
	}
	if !reflect.DeepEqual(f, map[string]map[string]bool{"type": {"container": true, "image": true, "network": true, "volume": true, "service": true, "node": true, "secret": true, "config": true}}) {
		t.Errorf("filters = %v, want the four resource types and the four swarm ones", f)
	}
}

func TestEventsEndsWithAnErrorWhenTheStreamDrops(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, errc := newClient(t, fake).Events(ctx, time.Time{})
	waitFor(t, "the stream to open", func() bool { return fake.OpenEventStreams() == 1 })

	fake.DropEventStreams()
	select {
	case _, ok := <-rows:
		if ok {
			t.Fatal("a row arrived on a dropped stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rows channel did not close after the drop")
	}
	if err := <-errc; err == nil {
		t.Error("a dropped stream reported no error; the caller cannot tell it ended")
	}
}

func TestEventsStopsWhenTheContextEnds(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	rows, errc := newClient(t, fake).Events(ctx, time.Time{})
	waitFor(t, "the stream to open", func() bool { return fake.OpenEventStreams() == 1 })

	cancel()
	select {
	case _, ok := <-rows:
		if ok {
			t.Fatal("a row arrived after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rows channel did not close after the context ended")
	}
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	waitFor(t, "the daemon to see the stream end", func() bool { return fake.OpenEventStreams() == 0 })
}

func TestEventsDoesNotHoldTheClientLockSoProbeStillRuns(t *testing.T) {
	// A stream lives for hours. If it held the read lock Probe, which takes the
	// write lock every 30 seconds, would wait forever and the monitor could
	// never see the environment go offline.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	cli := newClient(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = cli.Events(ctx, time.Time{})
	waitFor(t, "the stream to open", func() bool { return fake.OpenEventStreams() == 1 })

	pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pcancel()
	if res, err := cli.Probe(pctx); err != nil || !res.Reachable {
		t.Fatalf("Probe while a stream is open: %+v, %v", res, err)
	}
}

func TestEventRowIDIsStableAndTellsEventsApart(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "container", Action: "start", ActorID: "c1", TimeNano: 1_700_000_000_000_000_001},
		{Type: "container", Action: "start", ActorID: "c2", TimeNano: 1_700_000_000_000_000_001},
		{Type: "container", Action: "die", ActorID: "c1", TimeNano: 1_700_000_000_000_000_001},
	}})
	read := func() []string {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rows, _ := newClient(t, fake).Events(ctx, time.Unix(1_700_000_000, 0))
		var ids []string
		for range 3 {
			ids = append(ids, recvRow(t, rows).ID)
		}
		return ids
	}
	first, second := read(), read()
	if !slices.Equal(first, second) {
		t.Errorf("IDs change between reads: %v then %v", first, second)
	}
	if first[0] == first[1] || first[0] == first[2] || first[1] == first[2] {
		t.Errorf("distinct events share an ID: %v", first)
	}
}

func TestProbeIsNotDelayedByASlowSnapshot(t *testing.T) {
	// WR-06: a snapshot used to hold the client lock for its whole run, so a
	// probe that started meanwhile waited for it, timed out and reported a
	// healthy engine offline.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{ListDelay: 2 * time.Second})
	cli := newClient(t, fake)

	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	done := make(chan error, 1)
	go func() {
		_, err := cli.Snapshot(sctx, false)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)

	pctx, pcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer pcancel()
	start := time.Now()
	res, err := cli.Probe(pctx)
	if err != nil || !res.Supported {
		t.Fatalf("Probe during a slow snapshot: Supported=%v err=%v", res.Supported, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Probe took %s while a snapshot ran, want well under 1s", d)
	}
	scancel()
	<-done
}

func TestASnapshotOnTheOldClientSurvivesARenegotiation(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{ListDelay: 500 * time.Millisecond})
	cli := newClient(t, fake)

	type result struct {
		snap dockerx.Snapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, err := cli.Snapshot(context.Background(), false)
		done <- result{snap, err}
	}()
	time.Sleep(50 * time.Millisecond)
	// Probe swaps in a freshly negotiated client and closes the old one.
	if _, err := cli.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("the snapshot that began before the renegotiation failed: %v", r.err)
	}
}

func TestAProbeWaitingForAnotherGivesUpWhenItsContextEnds(t *testing.T) {
	// Probes are serialised, and a queued one must be abandonable.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	cli := newClient(t, fake)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			_, _ = cli.Probe(ctx)
		})
	}
	wg.Wait()
	if res, err := cli.Probe(context.Background()); err != nil || !res.Reachable {
		t.Fatalf("Probe after abandoned probes: %+v, %v", res, err)
	}
}

// The cluster view (plan 02-20, ENV-09, D-34, D-43). The fake manager serves
// /nodes, /services?status=true and /tasks, so the real client code runs.

func clusterManager() dockertest.FakeConfig {
	old := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	return dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		NodeRows: []dockertest.FakeNode{
			{ID: "n3", Hostname: "worker-b", Role: "worker", State: "down", EngineVersion: "23.0.1", OS: "linux"},
			{ID: "n2", Hostname: "mgr-b", Role: "manager", EngineVersion: "29.1.0", OS: "linux", Reachability: "unreachable"},
			{ID: "n1", Hostname: "mgr-a", Role: "manager", EngineVersion: "29.1.0", OS: "linux", Leader: true},
			{ID: "n4", Hostname: "worker-a", Role: "worker", Availability: "drain", EngineVersion: "28.5.1", OS: "windows"},
		},
		ServiceRows: []dockertest.FakeService{
			{ID: "s2", Name: "web", Mode: "replicated", Running: 2, Desired: 3, UpdateState: "completed", UpdatedAt: old},
			{ID: "s1", Name: "bayplan-agent", Mode: "global", Running: 4, Desired: 4},
			{ID: "s3", Name: "backup", Mode: "replicated-job", Running: 0, Desired: 1},
		},
		AgentTasks: []dockertest.FakeAgentTask{
			{ID: "t1", NodeID: "n1", State: "running", Created: old},
			{ID: "t2", NodeID: "n3", State: "rejected", Error: "no such image", Created: old},
			{ID: "t3", NodeID: "n3", State: "failed", Error: "task: non-zero exit (1)", Created: old.Add(time.Minute)},
		},
	}
}

func TestClusterReadsNodesServicesAndTheAgentTasksFromAManager(t *testing.T) {
	fake := dockertest.NewFake(t, clusterManager())
	got, err := newClient(t, fake).Cluster(context.Background())
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}

	var hosts []string
	leaders := 0
	for _, n := range got.Nodes {
		hosts = append(hosts, n.Hostname)
		if n.Leader {
			leaders++
		}
	}
	if want := []string{"mgr-a", "mgr-b", "worker-a", "worker-b"}; !slices.Equal(hosts, want) {
		t.Errorf("nodes are %v, want %v sorted by hostname", hosts, want)
	}
	if leaders != 1 {
		t.Errorf("%d nodes lead, want exactly one", leaders)
	}
	byHost := map[string]dockerx.NodeRow{}
	for _, n := range got.Nodes {
		byHost[n.Hostname] = n
	}
	if n := byHost["mgr-a"]; !n.Leader || n.Role != "manager" || n.Reachability != "reachable" || n.State != "ready" || n.Availability != "active" || n.EngineVersion != "29.1.0" || n.OS != "linux" || n.ID != "n1" {
		t.Errorf("mgr-a = %+v", n)
	}
	if n := byHost["mgr-b"]; n.Leader || n.Reachability != "unreachable" {
		t.Errorf("mgr-b = %+v, want a follower that is unreachable", n)
	}
	if n := byHost["worker-b"]; n.Role != "worker" || n.State != "down" || n.Reachability != "" {
		t.Errorf("worker-b = %+v, want a worker that is down and has no reachability", n)
	}
	if n := byHost["worker-a"]; n.Availability != "drain" || n.OS != "windows" {
		t.Errorf("worker-a = %+v", n)
	}

	if len(got.Services) != 3 {
		t.Fatalf("services = %+v, want 3", got.Services)
	}
	svc := map[string]dockerx.ServiceRow{}
	for _, s := range got.Services {
		svc[s.Name] = s
	}
	if s := svc["web"]; s.Running != 2 || s.Desired != 3 || s.Mode != "replicated" || s.UpdateState != "completed" || !s.UpdatedAt.Equal(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)) || s.UpdatedAt.Location() != time.UTC {
		t.Errorf("web = %+v, want 2 of 3 replicated, completed, updated at 09:00 UTC", s)
	}
	if s := svc["bayplan-agent"]; s.Mode != "global" || s.Running != 4 || s.Desired != 4 || s.UpdateState != "" {
		t.Errorf("bayplan-agent = %+v", s)
	}
	if s := svc["backup"]; s.Mode != "replicated-job" {
		t.Errorf("backup = %+v, want mode replicated-job", s)
	}
	for i := 1; i < len(got.Services); i++ {
		if got.Services[i-1].Name > got.Services[i].Name {
			t.Errorf("services are not sorted by name: %+v", got.Services)
		}
	}

	// One row per node: the newest task is the current one.
	tasks := map[string]dockerx.AgentTaskRow{}
	for _, tk := range got.AgentTasks {
		tasks[tk.NodeID] = tk
	}
	if len(got.AgentTasks) != 2 || tasks["n1"].State != "running" || tasks["n3"].State != "failed" || tasks["n3"].Error != "task: non-zero exit (1)" {
		t.Errorf("agent tasks = %+v, want n1 running and n3's newest task, failed", got.AgentTasks)
	}
}

func TestClusterNodeRowsCarryTheFactsAConstraintReads(t *testing.T) {
	cfg := dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		NodeRows: []dockertest.FakeNode{
			{
				ID: "n1", Hostname: "mgr-a", Role: "manager", Leader: true, OS: "linux", Arch: "aarch64",
				Address: "10.0.0.5", ManagerAddr: "10.0.0.5:2377", NanoCPUs: 4_000_000_000, MemoryBytes: 8 << 30,
				Labels: map[string]string{"disk": "ssd"}, EngineLabels: map[string]string{"type": "gpu"},
			},
			{ID: "n2", Hostname: "wrk-a", OS: "linux"},
		},
		ServiceRows: []dockertest.FakeService{
			{ID: "s1", Name: "web", Mode: "replicated", Desired: 1, Constraints: []string{"node.role==worker", "node.labels.disk==ssd"}},
			{ID: "s2", Name: "plain", Mode: "global", Desired: 1},
		},
	}
	got, err := newClient(t, dockertest.NewFake(t, cfg)).Cluster(context.Background())
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	byHost := map[string]dockerx.NodeRow{}
	for _, n := range got.Nodes {
		byHost[n.Hostname] = n
	}
	m := byHost["mgr-a"]
	if m.Address != "10.0.0.5" || m.Arch != "aarch64" || m.NanoCPUs != 4_000_000_000 || m.MemoryBytes != 8<<30 || m.ManagerAddr != "10.0.0.5:2377" {
		t.Errorf("mgr-a = %+v, want its address, architecture, resources and manager address", m)
	}
	if m.Labels["disk"] != "ssd" || m.EngineLabels["type"] != "gpu" {
		t.Errorf("mgr-a labels = %v, engine labels = %v", m.Labels, m.EngineLabels)
	}
	w := byHost["wrk-a"]
	if w.Labels == nil || w.EngineLabels == nil || len(w.Labels) != 0 || w.ManagerAddr != "" {
		t.Errorf("wrk-a = %+v, want empty non-nil label maps and no manager address", w)
	}
	svc := map[string]dockerx.ServiceRow{}
	for _, s := range got.Services {
		svc[s.Name] = s
	}
	if want := []string{"node.role==worker", "node.labels.disk==ssd"}; !slices.Equal(svc["web"].Constraints, want) {
		t.Errorf("web constraints = %v, want %v", svc["web"].Constraints, want)
	}
	if svc["plain"].Constraints == nil || len(svc["plain"].Constraints) != 0 {
		t.Errorf("plain constraints = %#v, want empty and not nil", svc["plain"].Constraints)
	}
}

func TestClusterServiceRowsCarryPortsUpdateMessagePreviousSpecAndCreatedAt(t *testing.T) {
	created := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	cfg := clusterManager()
	cfg.ServiceRows = []dockertest.FakeService{
		{
			ID: "s-web", Name: "web", Mode: "replicated", Running: 2, Desired: 2, UpdateState: "paused",
			UpdateMessage: "update paused due to failure or early termination of task t1", CreatedAt: created,
			Labels: map[string]string{"com.docker.stack.namespace": "shop", "team": "web-ops"},
			Ports: []swarm.PortConfig{
				{Protocol: "udp", TargetPort: 53, PublishedPort: 5353, PublishMode: "host"},
				{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: "ingress"},
				{Protocol: "tcp", TargetPort: 443, PublishedPort: 8443, PublishMode: "ingress"},
			},
			PreviousSpec: &swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "web"}},
		},
		{ID: "s-plain", Name: "plain", Mode: "global", Running: 1, Desired: 1},
	}
	got, err := newClient(t, dockertest.NewFake(t, cfg)).Cluster(context.Background())
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	svc := map[string]dockerx.ServiceRow{}
	for _, s := range got.Services {
		svc[s.Name] = s
	}
	web := svc["web"]
	want := []dockerx.ServicePort{
		{Published: 5353, Target: 53, Protocol: "udp", Mode: "host"},
		{Published: 8080, Target: 80, Protocol: "tcp", Mode: "ingress"},
		{Published: 8443, Target: 443, Protocol: "tcp", Mode: "ingress"},
	}
	if !reflect.DeepEqual(web.Ports, want) {
		t.Errorf("web ports = %+v, want %+v", web.Ports, want)
	}
	if web.UpdateMessage != "update paused due to failure or early termination of task t1" || !web.HasPreviousSpec || !web.CreatedAt.Equal(created) || web.CreatedAt.Location() != time.UTC {
		t.Errorf("web = %+v, want the update message, a previous spec and the creation time in UTC", web)
	}
	if web.Labels["team"] != "web-ops" || web.Labels["com.docker.stack.namespace"] != "shop" {
		t.Errorf("web labels = %v, want the spec's labels kept for the server-side search", web.Labels)
	}
	plain := svc["plain"]
	if plain.Ports == nil || len(plain.Ports) != 0 {
		t.Errorf("plain ports = %#v, want an empty, non-nil list", plain.Ports)
	}
	if plain.UpdateMessage != "" || plain.HasPreviousSpec {
		t.Errorf("plain = %+v, want no update message and no previous spec", plain)
	}
}

func TestClusterAsksForServiceStatusAndOnlyTheAgentServiceTasks(t *testing.T) {
	fake := dockertest.NewFake(t, clusterManager())
	if _, err := newClient(t, fake).Cluster(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.Requests(), " ")
	for _, want := range []string{"/nodes", "/services", "/tasks"} {
		if !strings.Contains(got, want) {
			t.Errorf("the manager was never asked %s: %s", want, got)
		}
	}
	for _, bad := range []string{"/swarm", "/secrets", "/configs", "POST"} {
		if strings.Contains(got, bad) {
			t.Errorf("the cluster read touched %s: %s", bad, got)
		}
	}
}

func TestClusterOnAWorkerFailsWithTheMessageThatMapsToNotAManager(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{SwarmState: "active", SwarmControl: false})
	_, err := newClient(t, fake).Cluster(context.Background())
	if err == nil {
		t.Fatal("a worker answered a cluster read")
	}
	if !strings.Contains(err.Error(), "This node is not a swarm manager") {
		t.Errorf("error = %v, want the daemon's worker message so the reason maps to not_a_manager", err)
	}
}

func TestClusterWithQuorumLostFailsWithTheMessageThatMapsToNoQuorum(t *testing.T) {
	cfg := clusterManager()
	cfg.QuorumLost = true
	fake := dockertest.NewFake(t, cfg)
	cli := newClient(t, fake)
	_, err := cli.Cluster(context.Background())
	if err == nil {
		t.Fatal("a manager without quorum answered a cluster read")
	}
	if !strings.Contains(err.Error(), "The swarm does not have a leader") {
		t.Errorf("error = %v, want the daemon's quorum message so the reason maps to no_quorum", err)
	}
	// Node-local calls keep working, as on a real manager.
	if _, err := cli.Snapshot(context.Background(), false); err != nil {
		t.Errorf("a node-local snapshot failed while quorum is lost: %v", err)
	}
}

func TestRestartPoliciesReturnsOnlyTheNameAndTheRetryCount(t *testing.T) {
	cfg := dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/a"}, State: "running"},
			{ID: "c2", Names: []string{"/b"}, State: "running"},
			{ID: "c3", Names: []string{"/c"}, State: "exited"},
		},
		RestartPolicies: map[string]string{"c1": "always", "c2": "on-failure:3"},
	}
	fake := dockertest.NewFake(t, cfg)
	got, err := newClient(t, fake).RestartPolicies(context.Background(), []string{"c1", "c2", "c3"})
	if err != nil {
		t.Fatalf("RestartPolicies: %v", err)
	}
	want := map[string]dockerx.RestartPolicy{
		"c1": {Name: "always"},
		"c2": {Name: "on-failure", MaxRetry: 3},
		"c3": {Name: "no"},
	}
	if len(got) != len(want) {
		t.Fatalf("policies = %+v, want %+v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("policy of %s = %+v, want %+v", id, got[id], w)
		}
	}
	// The type holds nothing but the two fields: the inspect body also carries
	// the container's environment variables, which can hold secrets.
	if n := reflect.TypeFor[dockerx.RestartPolicy]().NumField(); n != 2 {
		t.Errorf("RestartPolicy has %d fields, want only Name and MaxRetry", n)
	}
	for _, p := range fake.Requests() {
		if p == "/_ping" {
			continue // the client negotiates the API version first
		}
		if !strings.HasSuffix(p, "/json") || !strings.Contains(p, "/containers/") {
			t.Errorf("RestartPolicies asked %s, want only container inspects", p)
		}
	}
}

func TestRestartPoliciesOfNoContainersAsksNothingAndAnUnknownIDIsAnError(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	cli := newClient(t, fake)
	got, err := cli.RestartPolicies(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("RestartPolicies(nil) = %v, %v, want an empty map", got, err)
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("an empty request made %d calls", n)
	}
	if _, err := cli.RestartPolicies(context.Background(), []string{"gone"}); err == nil {
		t.Error("an unknown container was not an error")
	}
}

func TestSnapshotOfAManagerThatLostItsQuorumKeepsTheNodeLocalListsAndLeavesTheClusterCountsOut(t *testing.T) {
	cfg := clusterManager()
	cfg.QuorumLost = true
	cfg.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/web"}, State: "running", Status: "Up 1 hour"}}
	fake := dockertest.NewFake(t, cfg)
	snap, err := newClient(t, fake).Snapshot(context.Background(), true)
	if err != nil {
		t.Fatalf("Snapshot: %v, want the node-local lists to survive a lost quorum (D-43)", err)
	}
	if len(snap.Containers) != 1 || snap.Counts.Containers != 1 {
		t.Errorf("containers = %+v, want the one container", snap.Containers)
	}
	if snap.Counts.Nodes != nil || snap.Counts.Services != nil {
		t.Errorf("counts = %+v, want no node or service counts without a quorum", snap.Counts)
	}
}

func TestSnapshotStillFailsOnAnyOtherClusterError(t *testing.T) {
	// A worker asked as a manager is a caller bug, not a lost quorum: say so loudly.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{SwarmState: "active", SwarmControl: false})
	if _, err := newClient(t, fake).Snapshot(context.Background(), true); err == nil {
		t.Fatal("a worker answered a manager's snapshot without an error")
	}
}

// The agent service is found by its label, not only by the name the install
// command gives it: a stack file deployed as `docker stack deploy ... <stack>`
// names it <stack>_bayplan-agent, and the stack name is the user's.

func TestClusterFindsTheAgentServiceWhateverItIsNamed(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	label := map[string]string{"bayplan.agent": "true"}
	cases := []struct {
		name     string
		services []dockertest.FakeService
		wantNode string // the one node whose agent task must be reported, "" for none
	}{
		{"stack file install, named by the stack", []dockertest.FakeService{
			{ID: "sa", Name: "bayplan_bayplan-agent", Mode: "global", Labels: label},
			{ID: "sw", Name: "web"},
		}, "n1"},
		{"stack file install under another stack name", []dockertest.FakeService{
			{ID: "sa", Name: "monitoring_bayplan-agent", Mode: "global", Labels: label},
		}, "n1"},
		{"command install carrying the label", []dockertest.FakeService{
			{ID: "sa", Name: "bayplan-agent", Mode: "global", Labels: label},
		}, "n1"},
		{"command install from before the label", []dockertest.FakeService{
			{ID: "sa", Name: "bayplan-agent", Mode: "global"},
		}, "n1"},
		{"a service that merely looks like the agent", []dockertest.FakeService{
			{ID: "sa", Name: "my-bayplan-agent-fork", Mode: "global"},
			{ID: "sw", Name: "web", Labels: map[string]string{"bayplan.agent": "false"}},
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := dockertest.FakeConfig{
				SwarmState: "active", SwarmControl: true,
				NodeRows:    []dockertest.FakeNode{{ID: "n1", Hostname: "mgr", Role: "manager", Leader: true}},
				ServiceRows: c.services,
				AgentTasks: []dockertest.FakeAgentTask{
					// The task is the agent's when its service is the first row, and is
					// the unrelated service's in the last case.
					{ID: "t1", NodeID: "n1", State: "running", Created: now, Service: c.services[0].ID},
				},
			}
			if len(c.services) > 0 && c.services[0].Name == "bayplan-agent" {
				cfg.AgentTasks[0].Service = "" // the install command's own default
			}
			got, err := newClient(t, dockertest.NewFake(t, cfg)).Cluster(context.Background())
			if err != nil {
				t.Fatalf("Cluster: %v", err)
			}
			if c.wantNode == "" {
				if len(got.AgentTasks) != 0 {
					t.Errorf("agent tasks = %+v, want none: no service here is the agent", got.AgentTasks)
				}
				return
			}
			if len(got.AgentTasks) != 1 || got.AgentTasks[0].NodeID != c.wantNode || got.AgentTasks[0].State != "running" {
				t.Errorf("agent tasks = %+v, want %s running", got.AgentTasks, c.wantNode)
			}
		})
	}
}

func TestClusterDoesNotReportAnotherServicesTasksAsTheAgents(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	cfg := dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		NodeRows: []dockertest.FakeNode{{ID: "n1", Hostname: "a", Role: "manager", Leader: true}, {ID: "n2", Hostname: "b"}},
		ServiceRows: []dockertest.FakeService{
			{ID: "sa", Name: "bayplan_bayplan-agent", Mode: "global", Labels: map[string]string{"bayplan.agent": "true"}},
			{ID: "sw", Name: "web"},
		},
		AgentTasks: []dockertest.FakeAgentTask{
			{ID: "t1", NodeID: "n1", State: "running", Created: now, Service: "sa"},
			{ID: "t2", NodeID: "n2", State: "failed", Created: now, Service: "sw"},
		},
	}
	got, err := newClient(t, dockertest.NewFake(t, cfg)).Cluster(context.Background())
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if len(got.AgentTasks) != 1 || got.AgentTasks[0].NodeID != "n1" {
		t.Errorf("agent tasks = %+v, want only n1's", got.AgentTasks)
	}
}

// Image usage inputs (plan 05-15, RES-09, D-28): a container row names its image by ID, an
// image row carries its repo digests, and a service ref the image its spec names.

func TestSnapshotRowsCarryTheImageIDOfAContainerAndTheRepoDigestsOfAnImage(t *testing.T) {
	cfg := dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{{ID: "c1", Names: []string{"/web"}, Image: "nginx:1.27", ImageID: "sha256:aaaa", State: "exited"}},
		Images: []dockertest.FakeImage{
			{ID: "sha256:aaaa", RepoTags: []string{"nginx:1.27"}, RepoDigests: []string{"nginx@sha256:cc", "<none>@<none>"}, Created: 5},
			{ID: "sha256:bbbb", Created: 4},
		},
	}
	snap, _ := snapshot(t, cfg, false)
	if got := snap.Containers[0].ImageID; got != "sha256:aaaa" {
		t.Errorf("container ImageID = %q, want sha256:aaaa (a stopped container too)", got)
	}
	if got := snap.Images[0].RepoDigests; !slices.Equal(got, []string{"nginx@sha256:cc"}) {
		t.Errorf("repo digests = %q, want the one real digest", got)
	}
	if got := snap.Images[1].RepoDigests; got == nil || len(got) != 0 {
		t.Errorf("an image with no digest has %#v, want an empty list and not nil", got)
	}
}

func TestServiceRefsNameTheImageOfTheSpec(t *testing.T) {
	cfg := dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{
			{ID: "s1", Name: "web", Image: "nginx:1.27@sha256:cc", Mounts: []dockertest.FakeServiceMount{{Type: "volume", Source: "data", Target: "/d"}}},
			{ID: "s2", Name: "bare"},
		},
	}
	got, err := newClient(t, dockertest.NewFake(t, cfg)).ServiceRefs(context.Background())
	if err != nil {
		t.Fatalf("ServiceRefs: %v", err)
	}
	if len(got) != 2 || got[1].Image != "nginx:1.27@sha256:cc" || got[0].Image != "" {
		t.Fatalf("ServiceRefs = %+v, want the image of web and none for bare", got)
	}
}
