// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package selfmaint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ianglennon/bayplan/proto/wire"
)

const (
	oldLong  = "aaaaaaaaaaaa" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oldShort = "aaaaaaaaaaaa"
	helperID = "cccccccccccc" + "dddddddddddddddddddddddddddddddddddddddddddddddddddd"
	stateDir = "/var/lib/bayplan-agent"
	sockPath = "/var/run/docker.sock"
)

// oldImageConfig is the config baked into the agent image the old container runs.
func oldImageConfig() *dockerspec.DockerOCIImageConfig {
	return &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{
		Env:        []string{"PATH=/usr/local/bin:/usr/bin", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"},
		Entrypoint: []string{"/usr/local/bin/bayplan-agent"},
		Labels:     map[string]string{"org.opencontainers.image.version": "0.2.0", "org.opencontainers.image.title": "Bayplan agent"},
		Volumes:    map[string]struct{}{stateDir: {}},
	}}
}

// oldAgent is a standalone agent as `docker run` would leave it and the engine
// inspect it: what the operator chose, plus what the image and the engine added.
func oldAgent(f *fakeEngine) *container.InspectResponse {
	f.images["sha256:old"] = image.InspectResponse{ID: "sha256:old", Config: oldImageConfig()}
	return f.addContainer(container.InspectResponse{
		ID: oldLong, Name: "/bayplan-agent", Image: "sha256:old",
		State: &container.State{Running: true, Status: "running"},
		Config: &container.Config{
			Hostname:   oldShort, // the engine's default: the short container ID
			Image:      ownImage,
			Env:        []string{"BAYPLAN_AGENT_URL=wss://bayplan.example/agent/connect", "BAYPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/t", "PATH=/usr/local/bin:/usr/bin", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"},
			Entrypoint: []string{"/usr/local/bin/bayplan-agent"},
			Labels: map[string]string{
				"bayplan.agent": "true", "owner": "ops",
				"org.opencontainers.image.version": "0.2.0", "org.opencontainers.image.title": "Bayplan agent",
			},
			Volumes: map[string]struct{}{stateDir: {}},
		},
		HostConfig: &container.HostConfig{
			Binds:          []string{sockPath + ":" + sockPath, "bayplan-agent:" + stateDir},
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{"SETUID", "SETGID"},
			SecurityOpt:    []string{"no-new-privileges"},
			NetworkMode:    "bridge",
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: sockPath, Destination: sockPath, RW: true},
			{Type: mount.TypeVolume, Name: "bayplan-agent", Source: "/var/lib/docker/volumes/bayplan-agent/_data", Destination: stateDir, Driver: "local", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"bridge": {
				Aliases: []string{oldShort}, NetworkID: "net1", EndpointID: "ep1",
				IPAddress: netip.MustParseAddr("172.17.0.2"), Gateway: netip.MustParseAddr("172.17.0.1"), IPPrefixLen: 16,
				DNSNames: []string{"bayplan-agent", oldShort},
			},
		}},
	})
}

// helperSelf registers the helper container: the new image, labelled as a helper.
func helperSelf(f *fakeEngine) {
	f.addContainer(container.InspectResponse{
		ID: helperID, Name: "/bayplan-agent-helper-1", State: &container.State{Running: true, Status: "running"},
		Config: &container.Config{Hostname: helperID[:12], Image: newImage, Labels: map[string]string{"bayplan.agent": "true", "bayplan.agent.helper": "true"}},
	})
}

type markerClock struct {
	mu sync.Mutex
	at time.Time
}

func (m *markerClock) set(t time.Time) { m.mu.Lock(); m.at = t; m.mu.Unlock() }
func (m *markerClock) get() (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.at, nil
}

// recreateFixture is an old agent and the helper, and a recreator that reads the
// marker from mc. The new container "connects" when it starts if connect is true.
func recreateFixture(t *testing.T, connect bool, timeout time.Duration) (*fakeEngine, *recreator, *markerClock) {
	t.Helper()
	f := newFakeEngine(t)
	oldAgent(f)
	helperSelf(f)
	mc := &markerClock{at: time.Now().Add(-time.Hour)} // the old agent's marker
	if connect {
		f.onStart = func(_ *fakeEngine, c *container.InspectResponse) {
			if c.ID != oldLong && c.ID != helperID {
				mc.set(time.Now().Add(time.Millisecond))
			}
		}
	}
	r := newRecreator(RecreateOptions{
		SocketPath: f.sock, TargetID: oldLong, SelfID: helperID[:12], Marker: mc.get,
		Timeout: timeout, Poll: 5 * time.Millisecond, Log: quietLog(),
	})
	return f, r, mc
}

func TestRecreateReplacesTheAgentWithTheSameConfigurationAndTheNewImage(t *testing.T) {
	f, r, _ := recreateFixture(t, true, 5*time.Second)
	wantHC, _ := json.Marshal(f.containers[oldLong].HostConfig)
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("recreate failed: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("want one new container, got %d", len(f.creates))
	}
	cr := f.creates[0]
	if cr.Name != "bayplan-agent" {
		t.Fatalf("the new container is named %q, want the old one's name", cr.Name)
	}
	if cr.Req.Config.Image != newImage {
		t.Fatalf("the new container runs %q, want the helper's own image %q", cr.Req.Config.Image, newImage)
	}
	// The operator's configuration is carried over.
	for _, want := range []string{"BAYPLAN_AGENT_URL=wss://bayplan.example/agent/connect", "BAYPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/t"} {
		if !slices.Contains(cr.Req.Config.Env, want) {
			t.Errorf("env %q was not carried over: %v", want, cr.Req.Config.Env)
		}
	}
	if cr.Req.Config.Labels["bayplan.agent"] != "true" || cr.Req.Config.Labels["owner"] != "ops" {
		t.Errorf("the operator's labels were not carried over: %v", cr.Req.Config.Labels)
	}
	if gotHC, _ := json.Marshal(cr.Req.HostConfig); string(gotHC) != string(wantHC) {
		t.Errorf("HostConfig changed:\n got: %s\nwant: %s", gotHC, wantHC)
	}
	// What the old image or the engine had put in is not: it would pin the old image's
	// labels and environment, and the old container's ID as the hostname, on the new one.
	if cr.Req.Config.Hostname != "" {
		t.Errorf("the old container's ID was copied as the hostname: %q", cr.Req.Config.Hostname)
	}
	if v, ok := cr.Req.Config.Labels["org.opencontainers.image.version"]; ok {
		t.Errorf("the old image's version label was pinned onto the new container: %q", v)
	}
	if slices.Contains(cr.Req.Config.Env, "PATH=/usr/local/bin:/usr/bin") {
		t.Errorf("the old image's environment was pinned onto the new container: %v", cr.Req.Config.Env)
	}
	if len(cr.Req.Config.Entrypoint) != 0 {
		t.Errorf("the old image's entrypoint was pinned: %v", cr.Req.Config.Entrypoint)
	}
	if _, ok := cr.Req.Config.Volumes[stateDir]; ok {
		t.Errorf("the old image's volume declaration was pinned: %v", cr.Req.Config.Volumes)
	}

	// The network endpoint comes over as configuration, not as the old container's address.
	ep := cr.Req.NetworkingConfig.EndpointsConfig["bridge"]
	if ep == nil {
		t.Fatalf("the bridge endpoint was not carried over: %+v", cr.Req.NetworkingConfig)
	}
	if ep.NetworkID != "" || ep.EndpointID != "" || ep.IPAddress.IsValid() || ep.Gateway.IsValid() {
		t.Errorf("runtime endpoint data was copied: %+v", ep)
	}
	if slices.Contains(ep.Aliases, oldShort) {
		t.Errorf("the old container's ID alias was copied: %v", ep.Aliases)
	}

	// The old container is gone, the new one runs.
	if f.containers[oldLong] != nil {
		t.Error("the old container was not removed after the new one connected")
	}
	if c := f.containers[cr.ID]; c == nil || !c.State.Running {
		t.Error("the new container is not running")
	}

	// The order of the writes: rename, stop, create, start, remove the old one.
	var ops []string
	for _, w := range f.writes() {
		ops = append(ops, strings.Fields(w)[0]+" "+strings.SplitN(strings.Fields(w)[1], "?", 2)[0])
	}
	want := []string{
		"POST /containers/" + oldLong + "/rename", "POST /containers/" + oldLong + "/stop",
		"POST /containers/create", "POST /containers/" + cr.ID + "/start", "DELETE /containers/" + oldLong,
	}
	if !reflect.DeepEqual(ops, want) {
		t.Fatalf("writes:\n got: %v\nwant: %v", ops, want)
	}
	if q := f.writeWithPrefix("POST /containers/" + oldLong + "/rename"); !strings.Contains(q, "name=bayplan-agent-old") {
		t.Errorf("the old container was renamed with %q, want name=bayplan-agent-old", q)
	}
}

func (f *fakeEngine) writeWithPrefix(prefix string) string {
	for _, w := range f.writes() {
		if strings.HasPrefix(w, prefix) {
			return w
		}
	}
	return ""
}

func TestRecreateConnectsTheOtherNetworksWhenThereIsMoreThanOne(t *testing.T) {
	f, r, _ := recreateFixture(t, true, 5*time.Second)
	f.containers[oldLong].NetworkSettings.Networks["backend"] = &network.EndpointSettings{
		Aliases: []string{"agent", oldShort}, NetworkID: "net2", IPAddress: netip.MustParseAddr("10.0.0.5"),
		IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.0.0.5")},
	}
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("recreate failed: %v", err)
	}
	cr := f.creates[0]
	if n := len(cr.Req.NetworkingConfig.EndpointsConfig); n != 1 || cr.Req.NetworkingConfig.EndpointsConfig["bridge"] == nil {
		t.Fatalf("a create request carries one endpoint (the mode's), got %+v", cr.Req.NetworkingConfig.EndpointsConfig)
	}
	if len(f.connects) != 1 || f.connects[0].Network != "backend" || f.connects[0].Container != cr.ID {
		t.Fatalf("connects = %+v, want the new container on backend", f.connects)
	}
	ep := f.connects[0].Endpoint
	if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address.String() != "10.0.0.5" || slices.Contains(ep.Aliases, oldShort) || ep.IPAddress.IsValid() || ep.NetworkID != "" {
		t.Fatalf("the backend endpoint = %+v, want its static address and aliases without runtime data", ep)
	}
}

func TestRecreateRollsBackWhenTheNewAgentNeverConnects(t *testing.T) {
	f, r, _ := recreateFixture(t, false, 150*time.Millisecond)
	err := r.run(context.Background())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	assertRolledBack(t, f)
}

func assertRolledBack(t *testing.T, f *fakeEngine) {
	t.Helper()
	for id, c := range f.containers {
		if id != oldLong && id != helperID {
			t.Errorf("the new container %s (%s) was not removed", id, c.Name)
		}
	}
	old := f.containers[oldLong]
	if old == nil {
		t.Fatal("the old container is gone after a rollback")
	}
	if old.Name != "/bayplan-agent" || !old.State.Running {
		t.Fatalf("the old container is %q running=%v, want /bayplan-agent running", old.Name, old.State.Running)
	}
	for _, w := range f.writes() {
		if strings.HasPrefix(w, "DELETE /containers/"+oldLong) {
			t.Fatalf("the old container was deleted: %v", f.writes())
		}
	}
}

func TestRecreateRollsBackAtOnceWhenTheNewContainerStopsRunning(t *testing.T) {
	f, r, _ := recreateFixture(t, false, time.Minute)
	f.onStart = func(_ *fakeEngine, c *container.InspectResponse) {
		if c.ID != oldLong && c.ID != helperID {
			c.State = &container.State{Status: "exited", Running: false, ExitCode: 1}
		}
	}
	start := time.Now()
	err := r.run(context.Background())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v: a container that exited must not be waited for", d)
	}
	assertRolledBack(t, f)
}

func TestRecreateRollsBackWhenTheNewContainerKeepsRestarting(t *testing.T) {
	f, r, _ := recreateFixture(t, false, time.Minute)
	f.onStart = func(_ *fakeEngine, c *container.InspectResponse) {
		if c.ID != oldLong && c.ID != helperID {
			c.State = &container.State{Status: "restarting", Running: true, Restarting: true}
		}
	}
	if err := r.run(context.Background()); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	assertRolledBack(t, f)
}

func TestRecreateRollsBackWhenTheNewContainerCannotBeStarted(t *testing.T) {
	f, r, _ := recreateFixture(t, false, time.Minute)
	f.failStart[fmt.Sprintf("%064x", 1)] = true
	if err := r.run(context.Background()); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	assertRolledBack(t, f)
}

func TestRecreateRollsBackEvenWhenItsOwnContextWasCancelled(t *testing.T) {
	f, r, _ := recreateFixture(t, false, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	f.onStart = func(_ *fakeEngine, c *container.InspectResponse) {
		if c.ID != oldLong && c.ID != helperID {
			go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		}
	}
	if err := r.run(ctx); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	assertRolledBack(t, f)
}

func TestAMarkerFromBeforeTheStartIsNotASuccess(t *testing.T) {
	f, r, mc := recreateFixture(t, false, 150*time.Millisecond)
	mc.set(time.Now().Add(-time.Second)) // the old agent's marker, however recent
	if err := r.run(context.Background()); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	assertRolledBack(t, f)
}

func TestRecreateTouchesNothingWhenTheOldNameIsTaken(t *testing.T) {
	f, r, _ := recreateFixture(t, true, time.Second)
	f.addContainer(container.InspectResponse{ID: "e" + strings.Repeat("0", 63), Name: "/bayplan-agent-old", State: &container.State{}, Config: &container.Config{}})
	if err := r.run(context.Background()); err == nil || errors.Is(err, ErrRolledBack) {
		t.Fatalf("want a plain error, got %v", err)
	}
	for _, w := range f.writes() {
		if !strings.Contains(w, "/rename") {
			t.Fatalf("something was changed after the rename failed: %v", f.writes())
		}
	}
	if old := f.containers[oldLong]; old.Name != "/bayplan-agent" || !old.State.Running {
		t.Fatalf("the old container was disturbed: %q running=%v", old.Name, old.State.Running)
	}
}

func TestRecreateRefusesATargetThatIsNotAnAgentOrNotOfTheHelpersRepository(t *testing.T) {
	for name, mutate := range map[string]func(f *fakeEngine){
		"no label":            func(f *fakeEngine) { delete(f.containers[oldLong].Config.Labels, "bayplan.agent") },
		"label false":         func(f *fakeEngine) { f.containers[oldLong].Config.Labels["bayplan.agent"] = "false" },
		"another repository":  func(f *fakeEngine) { f.containers[oldLong].Config.Image = "ghcr.io/evil/agent:1" },
		"the helper itself":   func(f *fakeEngine) { f.containers[oldLong].Config.Labels["bayplan.agent.helper"] = "true" },
		"helper not a helper": func(f *fakeEngine) { delete(f.containers[helperID].Config.Labels, "bayplan.agent.helper") },
		"helper foreign repo": func(f *fakeEngine) { f.containers[helperID].Config.Image = "ghcr.io/evil/agent:1" },
	} {
		t.Run(name, func(t *testing.T) {
			f, r, _ := recreateFixture(t, true, time.Second)
			mutate(f)
			if err := r.run(context.Background()); err == nil {
				t.Fatal("a target that is not an agent was recreated")
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("something was changed: %v", w)
			}
		})
	}
}

func TestRecreateFailsWithoutChangingAnythingWhenTheTargetDoesNotExist(t *testing.T) {
	f, r, _ := recreateFixture(t, true, time.Second)
	r.opts.TargetID = "ffffffffffff"
	if err := r.run(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("something was changed: %v", w)
	}
}

func TestRecreateKeepsEveryLabelWhenTheOldImageIsGone(t *testing.T) {
	f, r, _ := recreateFixture(t, true, 5*time.Second)
	delete(f.images, "sha256:old")
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("recreate failed: %v", err)
	}
	l := f.creates[0].Req.Config.Labels
	if l["bayplan.agent"] != "true" || l["owner"] != "ops" {
		t.Errorf("labels = %v", l)
	}
	// Without the old image to compare with, the image's own metadata labels are
	// still not carried: the new image sets its own.
	if _, ok := l["org.opencontainers.image.version"]; ok {
		t.Errorf("the old image's version label was carried: %v", l)
	}
}

// ---- the update that starts the helper ----

func standaloneFixture(t *testing.T, mut func(*Config)) (*fakeEngine, *Maint) {
	t.Helper()
	f := newFakeEngine(t)
	old := oldAgent(f)
	_ = old
	f.images[newImage] = image.InspectResponse{ID: "sha256:new", Config: oldImageConfig()}
	return f, newMaint(f, func(c *Config) {
		c.SelfID = oldShort
		c.StateDir = stateDir
		if mut != nil {
			mut(c)
		}
	})
}

func TestStandaloneUpdatePullsTheImageAndStartsAHelperThatRecreatesTheAgent(t *testing.T) {
	f, m := standaloneFixture(t, nil)
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, ID: "s1", Image: newImage})
	if !res.OK || res.ID != "s1" {
		t.Fatalf("the update was refused: %+v", res)
	}
	if len(f.pulled) != 1 || f.pulled[0] != ownRepo+":0.2.1" {
		t.Fatalf("pulled %v, want only %s:0.2.1", f.pulled, ownRepo)
	}
	if len(f.creates) != 1 {
		t.Fatalf("want one helper, got %d creates", len(f.creates))
	}
	h := f.creates[0].Req
	if h.Config.Image != newImage {
		t.Errorf("the helper runs %q, want %q", h.Config.Image, newImage)
	}
	if h.Config.Labels["bayplan.agent"] != "true" || h.Config.Labels["bayplan.agent.helper"] != "true" {
		t.Errorf("helper labels = %v", h.Config.Labels)
	}
	if want := []string{"helper", "recreate", "--target", oldLong}; !reflect.DeepEqual([]string(h.Config.Cmd), want) {
		t.Errorf("helper command = %v, want %v (the full ID of the agent's own container)", h.Config.Cmd, want)
	}
	if !slices.Contains(h.Config.Env, "BAYPLAN_AGENT_SOCKET="+sockPath) || !slices.Contains(h.Config.Env, "BAYPLAN_AGENT_STATE_DIR="+stateDir) {
		t.Errorf("helper env = %v", h.Config.Env)
	}
	hc := h.HostConfig
	if hc == nil || !hc.AutoRemove || !hc.ReadonlyRootfs || !slices.Equal(hc.CapDrop, []string{"ALL"}) ||
		!slices.Equal(hc.CapAdd, []string{"SETUID", "SETGID"}) || !slices.Contains(hc.SecurityOpt, "no-new-privileges") {
		t.Errorf("the helper does not have the agent's hardening and AutoRemove: %+v", hc)
	}
	if hc != nil && hc.NetworkMode != "none" {
		t.Errorf("the helper needs no network, got mode %q", hc.NetworkMode)
	}
	if hc != nil && hc.RestartPolicy.Name != "" && hc.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Errorf("a helper must not restart: %+v", hc.RestartPolicy)
	}
	var sock, state *mount.Mount
	for i := range hc.Mounts {
		switch hc.Mounts[i].Target {
		case sockPath:
			sock = &hc.Mounts[i]
		case stateDir:
			state = &hc.Mounts[i]
		}
	}
	if sock == nil || sock.Type != mount.TypeBind || sock.Source != sockPath {
		t.Errorf("the helper needs the Docker socket: %+v", hc.Mounts)
	}
	if state == nil || state.Type != mount.TypeVolume || state.Source != "bayplan-agent" {
		t.Errorf("the helper needs the agent's state volume by name: %+v", hc.Mounts)
	}
	if len(hc.Mounts) != 2 {
		t.Errorf("the helper has mounts beyond the socket and the state: %+v", hc.Mounts)
	}

	// The writes: pull, create the helper, start it. The agent itself is untouched
	// here; the helper does the rest.
	ops := f.writes()
	if len(ops) != 3 || !strings.HasPrefix(ops[0], "POST /images/create") || ops[1] != "POST /containers/create?name="+f.creates[0].Name ||
		ops[2] != "POST /containers/"+f.creates[0].ID+"/start" {
		t.Fatalf("writes = %v", ops)
	}
	if !strings.HasPrefix(f.creates[0].Name, "bayplan-agent-helper-") {
		t.Errorf("helper name %q", f.creates[0].Name)
	}
	if c := f.containers[oldLong]; c == nil || !c.State.Running {
		t.Error("the update disturbed the running agent itself")
	}
}

func TestStandaloneUpdateToleratesAFailedPullWhenTheImageIsAlreadyHere(t *testing.T) {
	f, m := standaloneFixture(t, nil)
	f.pullErr = true
	if res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage}); !res.OK {
		t.Fatalf("a failed pull with the image present must still update: %+v", res)
	}
	if len(f.creates) != 1 {
		t.Fatalf("no helper was created")
	}
}

func TestStandaloneUpdateFailsWhenTheImageCannotBePulledAndIsNotHere(t *testing.T) {
	f, m := standaloneFixture(t, nil)
	f.pullErr = true
	delete(f.images, newImage)
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	if res.OK || res.Error != wire.UpdateErrFailed {
		t.Fatalf("want %s: %+v", wire.UpdateErrFailed, res)
	}
	if len(f.creates) != 0 {
		t.Fatal("a helper was created without an image")
	}
}

func TestStandaloneUpdateRemovesTheHelperItCreatedWhenItCannotStartIt(t *testing.T) {
	f, m := standaloneFixture(t, nil)
	f.failStart[fmt.Sprintf("%064x", 1)] = true
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	if res.OK || res.Error != wire.UpdateErrFailed {
		t.Fatalf("want %s: %+v", wire.UpdateErrFailed, res)
	}
	if len(f.containers) != 1 || f.containers[oldLong] == nil {
		t.Fatalf("a helper that never started was left behind: %v", f.containers)
	}
}

func TestStandaloneUpdateAnswersBusyWhileAHelperIsRunning(t *testing.T) {
	f, m := standaloneFixture(t, nil)
	helperSelf(f)
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	if res.OK || res.Error != wire.UpdateErrBusy {
		t.Fatalf("want %s: %+v", wire.UpdateErrBusy, res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

func TestStandaloneUpdateOfAForeignRepositoryPullsNothing(t *testing.T) {
	f, m := standaloneFixture(t, nil)
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: "ghcr.io/evil/agent:1"})
	if res.OK || res.Error != wire.UpdateErrImageNotOwnRepository {
		t.Fatalf("%+v", res)
	}
	if c := f.callList(); len(c) != 0 {
		t.Fatalf("calls = %v", c)
	}
}

func TestOnlyOneHelperIsEverStartedForTwoUpdatesInARow(t *testing.T) {
	f, m := standaloneFixture(t, nil)
	f.onStart = func(f *fakeEngine, c *container.InspectResponse) {
		if c.Config.Labels["bayplan.agent.helper"] == "true" {
			c.State.Running = true
		}
	}
	if res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage}); !res.OK {
		t.Fatalf("first: %+v", res)
	}
	if res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage}); res.Error != wire.UpdateErrBusy {
		t.Fatalf("a second update while the helper runs must answer busy: %+v", res)
	}
	if len(f.creates) != 1 {
		t.Fatalf("%d helpers were created", len(f.creates))
	}
}

// The JSON the fake serves for a created container must stay decodable as what the
// engine would send: guard against the fixtures drifting from the API types.
func TestFixtureContainerRoundTripsThroughJSON(t *testing.T) {
	f := newFakeEngine(t)
	c := oldAgent(f)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back container.InspectResponse
	if err := json.Unmarshal(b, &back); err != nil || back.ID != oldLong || back.NetworkSettings.Networks["bridge"].IPAddress.String() != "172.17.0.2" {
		t.Fatalf("round trip: %v %+v", err, back)
	}
}
