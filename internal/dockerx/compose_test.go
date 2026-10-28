// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The Compose SDK over Bayplan's own client (plan 07-20, STK-02, RESEARCH Q1 and Q6).

// initCompose prepares the SDK's process-wide state under a directory of the test's own.
func initCompose(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	if err := InitCompose(data, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	return data
}

// The calls plan 07-20 names as refused in the first place: build, save, load, push, import, export,
// commit, copy out, exec, attach, every swarm call and a plugin install. The test after this one closes the
// list against the whole of the client interface.
var mustRefuse = []string{
	"ImageBuild", "ImageSave", "ImageLoad", "ImagePush", "ImageImport",
	"ContainerExport", "ContainerCommit", "CopyFromContainer", "ContainerAttach",
	"ExecCreate", "ExecStart", "ExecAttach", "ExecInspect", "ExecResize",
	"SwarmInit", "SwarmJoin", "SwarmInspect", "SwarmUpdate", "SwarmLeave", "SwarmGetUnlockKey", "SwarmUnlock",
	"NodeInspect", "NodeList", "NodeUpdate", "NodeRemove",
	"ServiceCreate", "ServiceInspect", "ServiceList", "ServiceUpdate", "ServiceRemove", "ServiceLogs",
	"TaskInspect", "TaskList", "TaskLogs",
	"SecretCreate", "SecretInspect", "SecretList", "SecretUpdate", "SecretRemove",
	"ConfigCreate", "ConfigInspect", "ConfigList", "ConfigUpdate", "ConfigRemove",
	"PluginInstall",
}

// callWithZeroes calls a method of the guard with a live context and zero values for the rest, and returns
// its last result, the error. The guard answers before it looks at an argument.
func callWithZeroes(t *testing.T, g any, name string) error {
	t.Helper()
	m := reflect.ValueOf(g).MethodByName(name)
	if !m.IsValid() {
		t.Fatalf("the guard has no method %s", name)
	}
	typ := m.Type()
	args := make([]reflect.Value, 0, typ.NumIn())
	for i := range typ.NumIn() {
		if typ.IsVariadic() && i == typ.NumIn()-1 {
			break // an empty list of options
		}
		in := typ.In(i)
		if in == reflect.TypeFor[context.Context]() {
			args = append(args, reflect.ValueOf(context.Background()))
			continue
		}
		args = append(args, reflect.Zero(in))
	}
	out := m.Call(args)
	if len(out) == 0 {
		t.Fatalf("%s returns nothing", name)
	}
	last := out[len(out)-1]
	if !last.Type().Implements(reflect.TypeFor[error]()) {
		t.Fatalf("%s does not end in an error", name)
	}
	if last.IsNil() {
		return nil
	}
	err, _ := last.Interface().(error)
	return err
}

func TestTheGuardRefusesTheCallsAStackDeployMustNotMake(t *testing.T) {
	t.Parallel()
	g := guardClient{}
	for _, name := range mustRefuse {
		if err := callWithZeroes(t, g, name); !errors.Is(err, ErrComposeForbidden) {
			t.Errorf("%s through the guard = %v, want ErrComposeForbidden", name, err)
		}
	}
}

func TestTheGuardNeverClosesTheClientItWraps(t *testing.T) {
	t.Parallel()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	g := guardClient{APIClient: c.cur()}
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Probe(testsupport.Context(t)); err != nil {
		t.Errorf("the client was closed by the guard: %v", err)
	}
}

func TestInitComposeMakesAPrivateEmptyConfigDirectory(t *testing.T) {
	data := t.TempDir()
	// The test makes the directory wider than the SDK may have it, to see it narrowed.
	if err := os.MkdirAll(filepath.Join(data, composeConfigDirName), 0o755); err != nil { //nolint:gosec // see above
		t.Fatal(err)
	}
	if err := InitCompose(data, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	dir := filepath.Join(data, composeConfigDirName)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Errorf("%s: mode %v, want a directory with 0700 (an existing wider one is narrowed)", dir, st.Mode())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("the configuration directory holds %v (err %v), want it empty: no login, no helper, no context", entries, err)
	}
	if err := InitCompose("", nil); err == nil {
		t.Error("InitCompose without a data directory succeeded")
	}
}

func TestTheSDKsLoggingGoesToSlog(t *testing.T) {
	var buf bytes.Buffer
	data := t.TempDir()
	if err := InitCompose(data, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))); err != nil {
		t.Fatal(err)
	}
	logrus.WithField("service", "web").Warn("the Compose SDK warns")
	out := buf.String()
	if !strings.Contains(out, "the Compose SDK warns") || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "source=compose-sdk") {
		t.Errorf("slog got %q, want the warning at WARN with source=compose-sdk", out)
	}
}

func TestComposeUpNeedsInitComposeFirst(t *testing.T) {
	composeMu.Lock()
	saved := composeDir
	composeDir = ""
	composeMu.Unlock()
	t.Cleanup(func() {
		composeMu.Lock()
		composeDir = saved
		composeMu.Unlock()
	})
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.ComposeUp(testsupport.Context(t), &types.Project{Name: "demo"}, ComposeUpOptions{}, nil); !errors.Is(err, ErrComposeNotReady) {
		t.Errorf("ComposeUp before InitCompose = %v, want ErrComposeNotReady", err)
	}
	if got := len(fake.Requests()); got != 0 {
		t.Errorf("%d requests reached the engine before InitCompose", got)
	}
}

func TestComposeUpTalksToTheEngineThroughTheGuard(t *testing.T) {
	initCompose(t)
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := &types.Project{Name: "demo", Services: types.Services{"web": types.ServiceConfig{Name: "web", Image: "nginx:1.27"}}}
	var events []ComposeEvent
	// The fake engine cannot create a container, so the up fails; what is asserted is that it was the
	// engine that answered, over the client the guard wraps.
	err = c.ComposeUp(testsupport.Context(t), p, ComposeUpOptions{}, func(ev ComposeEvent) { events = append(events, ev) })
	if err == nil {
		t.Fatal("an up against a fake engine that cannot create containers succeeded")
	}
	if errors.Is(err, ErrComposeForbidden) || errors.Is(err, ErrComposeNotReady) {
		t.Fatalf("the up was refused before it reached the engine: %v", err)
	}
	sawList := slices.ContainsFunc(fake.Requests(), func(r string) bool { return strings.HasSuffix(r, "/containers/json") || r == "/containers/json" })
	if !sawList {
		t.Errorf("requests = %v, want the up to list the project's containers", fake.Requests())
	}
}

func TestComposeStopStartAndDownAskTheEngineForTheProjectsContainers(t *testing.T) {
	initCompose(t)
	p := &types.Project{Name: "demo", Services: types.Services{"web": types.ServiceConfig{Name: "web", Image: "nginx:1.27"}}}
	calls := map[string]func(*Client) error{
		"stop":  func(c *Client) error { return c.ComposeStop(testsupport.Context(t), p) },
		"start": func(c *Client) error { return c.ComposeStart(testsupport.Context(t), p) },
		"down":  func(c *Client) error { return c.ComposeDown(testsupport.Context(t), "demo") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			fake := dockertest.NewFake(t, dockertest.FakeConfig{})
			c, err := New(fake)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			// A host with no container of the project: nothing to do, and the SDK found that out by asking
			// the engine, over the client the guard wraps.
			err = call(c)
			if errors.Is(err, ErrComposeForbidden) || errors.Is(err, ErrComposeNotReady) {
				t.Fatalf("%s was refused before it reached the engine: %v", name, err)
			}
			if !slices.ContainsFunc(fake.Requests(), func(r string) bool { return strings.HasSuffix(r, "/containers/json") }) {
				t.Errorf("requests = %v, want %s to list the project's containers", fake.Requests(), name)
			}
		})
	}
}

func TestServiceOfContainerReadsTheNamesComposeGives(t *testing.T) {
	t.Parallel()
	p := &types.Project{Name: "demo", Services: types.Services{
		"web":   {Name: "web"},
		"web-2": {Name: "web-2"},
		"db":    {Name: "db", ContainerName: "the-db"},
	}}
	for name, want := range map[string]string{
		"demo-web-1":                  "web",
		"demo-web-2-1":                "web-2",
		"demo_web_3":                  "web",
		"the-db":                      "db",
		"0123456789ab_demo-web-1":     "web",
		"demo-cache-1":                "",
		"other-web-1":                 "",
		"demo-web-":                   "",
		"0123456789ab_the-db":         "db",
		"0123456789ab_demo-web-2-1":   "web-2",
		"0123456789xz_demo-web-1":     "",
		"demo-web-1-extra-0123456789": "",
	} {
		if got := serviceOfContainer(p, name); got != want {
			t.Errorf("serviceOfContainer(%q) = %q, want %q", name, got, want)
		}
	}
	if serviceOfContainer(nil, "demo-web-1") != "" {
		t.Error("a nil project names a service")
	}
}

func TestTranslateEventReadsTheSDKsResources(t *testing.T) {
	t.Parallel()
	p := &types.Project{Name: "demo", Services: types.Services{"web": {Name: "web"}}}
	for _, tc := range []struct {
		in   api.Resource
		want ComposeEvent
	}{
		{api.Resource{ID: "Container demo-web-1", Status: api.Working, Text: api.StatusCreating},
			ComposeEvent{Kind: ComposeKindContainer, Name: "demo-web-1", Container: "demo-web-1", Service: "web", Status: "Creating"}},
		{api.Resource{ID: "Container demo-web-1", Status: api.Done, Text: api.StatusHealthy},
			ComposeEvent{Kind: ComposeKindContainer, Name: "demo-web-1", Container: "demo-web-1", Service: "web", Status: "Healthy", Done: true}},
		{api.Resource{ID: "Network demo_default", Status: api.Done, Text: api.StatusCreated},
			ComposeEvent{Kind: ComposeKindNetwork, Name: "demo_default", Status: "Created", Done: true}},
		{api.Resource{ID: "Volume demo_data", Status: api.Working, Text: api.StatusCreating},
			ComposeEvent{Kind: ComposeKindVolume, Name: "demo_data", Status: "Creating"}},
		{api.Resource{ID: "Image nginx:1.27", Status: api.Working, Text: api.StatusPulling, Details: "Pulling"},
			ComposeEvent{Kind: ComposeKindImage, Name: "nginx:1.27", Status: "Pulling", Text: "Pulling"}},
		{api.Resource{ID: "Container demo-web-1", Status: api.Error, Text: api.StatusError, Details: "port is already allocated"},
			ComposeEvent{Kind: ComposeKindContainer, Name: "demo-web-1", Container: "demo-web-1", Service: "web", Status: "Error", Text: "port is already allocated", Failed: true}},
		{api.Resource{ID: api.ResourceCompose, Status: api.Working, Text: "Running"},
			ComposeEvent{Status: "Running"}},
	} {
		if got := translateEvent(p, tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("translateEvent(%+v) =\n%+v\nwant\n%+v", tc.in, got, tc.want)
		}
	}
}

func TestComposeEventsAreDeliveredOneAtATime(t *testing.T) {
	t.Parallel()
	var inside, worst int
	b := &eventBridge{project: &types.Project{Name: "demo"}, on: func(ComposeEvent) {
		inside++
		worst = max(worst, inside)
		inside--
	}}
	done := make(chan struct{})
	for range 8 {
		go func() {
			for range 200 {
				b.On(api.Resource{ID: "Container demo-web-1", Text: api.StatusRunning})
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	if worst != 1 {
		t.Errorf("%d callbacks ran at once, want one at a time", worst)
	}
}

func TestServiceHashIsTheSDKsAndNotTheCustomLabels(t *testing.T) {
	t.Parallel()
	svc := types.ServiceConfig{Name: "web", Image: "nginx:1.27@sha256:" + strings.Repeat("a", 64)}
	h1, err := ServiceHash(svc)
	if err != nil || len(h1) != 64 {
		t.Fatalf("ServiceHash = %q, %v", h1, err)
	}
	svc.CustomLabels = types.Labels{"bayplan.stack.id": "stack-1"}
	if h2, _ := ServiceHash(svc); h2 != h1 {
		t.Error("a custom label moved the hash")
	}
	svc.Labels = types.Labels{"team": "web"}
	if h3, _ := ServiceHash(svc); h3 == h1 {
		t.Error("a label of the service did not move the hash")
	}
}

// keep the moby import for the interface assertion below.
var _ client.APIClient = guardClient{}

func TestComposeUpForcesTheRecreateOfTheServicesItIsGiven(t *testing.T) {
	t.Parallel()
	p := &types.Project{Name: "demo"}

	forced := composeUpOptions(p, ComposeUpOptions{ForceRecreate: []string{"web"}})
	if !slices.Equal(forced.Create.Services, []string{"web"}) || forced.Create.Recreate != api.RecreateForce || forced.Create.RecreateDependencies != api.RecreateDiverged {
		t.Errorf("with web forced: services %v recreate %q dependencies %q, want [web] force diverged",
			forced.Create.Services, forced.Create.Recreate, forced.Create.RecreateDependencies)
	}

	plain := composeUpOptions(p, ComposeUpOptions{})
	if len(plain.Create.Services) != 0 || plain.Create.Recreate != api.RecreateDiverged || plain.Create.RecreateDependencies != api.RecreateDiverged {
		t.Errorf("with none forced: services %v recreate %q dependencies %q, want every service diverged",
			plain.Create.Services, plain.Create.Recreate, plain.Create.RecreateDependencies)
	}

	for name, o := range map[string]api.UpOptions{"forced": forced, "plain": plain} {
		if !o.Create.RemoveOrphans || !o.Create.QuietPull || !o.Start.Wait || o.Start.Project != p {
			t.Errorf("%s: orphans removed %v quiet pull %v wait %v project %v, want the 07-20 options kept",
				name, o.Create.RemoveOrphans, o.Create.QuietPull, o.Start.Wait, o.Start.Project == p)
		}
	}
}

// TestComposePromptKeepsVolumes holds the answer to every question the Compose SDK can ask during an up: no,
// with no error, so a deploy never removes a volume to apply a changed setting (plan 08-07, D-46).
func TestComposePromptKeepsVolumes(t *testing.T) {
	initCompose(t)
	for _, tc := range []struct {
		name, message string
		fallback      bool
	}{
		{"the reconcile question", `Volume "shop_data" exists but doesn't match configuration in compose file. Recreate (data will be lost)?`, false},
		{"the same question with a yes default", `Volume "shop_data" exists but doesn't match configuration in compose file. Recreate (data will be lost)?`, true},
		{"any other question", "Remove everything?", true},
		{"no message", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := keepVolumes(tc.message, tc.fallback)
			if err != nil || got {
				t.Errorf("keepVolumes(%q, %v) = %v, %v, want false and no error", tc.message, tc.fallback, got, err)
			}
		})
	}
}

// The hashes the research probe read from a real engine with Compose 5.6.0 (08-RESEARCH Pattern 9): the label
// of a network is the sha256 of the JSON of its config, and the SDK's own functions give the same.
func TestComposeObjectHashesMatchTheResearchProbe(t *testing.T) {
	t.Parallel()
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	net := func(driver string) string {
		got, err := ComposeNetworkHash(&types.NetworkConfig{Name: "bpresearchprobe_back", Driver: driver})
		if err != nil {
			t.Fatalf("ComposeNetworkHash: %v", err)
		}
		return got
	}
	if got, want := net(""), sum(`{"name":"bpresearchprobe_back","ipam":{}}`); got != want || !strings.HasPrefix(got, "22fa6b37") {
		t.Errorf("minimal network hash = %s, want %s (the probe's 22fa6b37...)", got, want)
	}
	if got := net("bridge"); got == net("") || !strings.HasPrefix(got, "d3215973") {
		t.Errorf("a network with driver bridge hashes to %s, want another hash than the minimal one, d3215973...", got)
	}
	vol, err := ComposeVolumeHash(types.VolumeConfig{Name: "bpresearchprobe_data"})
	if err != nil {
		t.Fatalf("ComposeVolumeHash: %v", err)
	}
	if want := sum(`{"name":"bpresearchprobe_data","driver":"local"}`); vol != want || !strings.HasPrefix(vol, "ba5a7ea5") {
		t.Errorf("volume hash = %s, want %s (the probe's ba5a7ea5...)", vol, want)
	}
	if same, _ := ComposeVolumeHash(types.VolumeConfig{Name: "bpresearchprobe_data", Driver: "local"}); same != vol {
		t.Errorf("an explicit driver local hashes to %s, want the same as no driver (%s)", same, vol)
	}
	// A label is part of the hash; a custom label (Bayplan's own) is not.
	with, _ := ComposeVolumeHash(types.VolumeConfig{Name: "bpresearchprobe_data", Labels: types.Labels{"a": "b"}})
	custom, _ := ComposeVolumeHash(types.VolumeConfig{Name: "bpresearchprobe_data", CustomLabels: types.Labels{"bayplan.stack.id": "x"}})
	if with == vol || custom != vol {
		t.Errorf("label hash %s custom-label hash %s, want a label to move the hash and a custom label not to (%s)", with, custom, vol)
	}
}
