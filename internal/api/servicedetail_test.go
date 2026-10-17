// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// The service detail and its inspect document (plan 06-06, RES-02, D-39, AUTH-07). A service
// spec can carry credentials in its environment, command line, health check and log driver
// options; a caller without secrets.reveal never receives one from either route.

const sdDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// sdSecrets are the values the fixture service hides, one per kind.
var sdSecrets = map[string]string{
	"env": "env-secret-hunter2", "command": "cmd-secret-token", "args": "args-secret-pw",
	"health": "health-secret-pw", "log": "log-secret-token",
}

func sdSpec(image string, replicas uint64) *swarm.ServiceSpec {
	delay, window, attempts := 5*time.Second, 2*time.Minute, uint64(4)
	return &swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "api", Labels: map[string]string{"com.docker.stack.namespace": "shop", "tier": "backend"}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:       image,
				Env:         []string{"DB_PASSWORD=" + sdSecrets["env"], "MODE=prod", "FLAG"},
				Command:     []string{"/bin/api", "--token", sdSecrets["command"]},
				Args:        []string{"--requirepass", sdSecrets["args"]},
				Healthcheck: &container.HealthConfig{Test: []string{"CMD-SHELL", "curl -u a:" + sdSecrets["health"] + " http://localhost/"}},
				Mounts: []mount.Mount{
					{Type: mount.TypeVolume, Source: "api-data", Target: "/data"},
					{Type: mount.TypeBind, Source: "/srv/conf", Target: "/etc/conf", ReadOnly: true},
				},
				Secrets: []*swarm.SecretReference{{SecretID: "sec1", SecretName: "db_pw", File: &swarm.SecretReferenceFileTarget{Name: "db_pw", UID: "0", GID: "0", Mode: 0o444}}},
				Configs: []*swarm.ConfigReference{{ConfigID: "cfg1", ConfigName: "app_conf", File: &swarm.ConfigReferenceFileTarget{Name: "/etc/app.conf", UID: "1000", GID: "1000", Mode: 0o440}}},
			},
			Resources: &swarm.ResourceRequirements{
				Limits:       &swarm.Limit{NanoCPUs: 500_000_000, MemoryBytes: 256 << 20},
				Reservations: &swarm.Resources{NanoCPUs: 250_000_000, MemoryBytes: 128 << 20},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionOnFailure, Delay: &delay, MaxAttempts: &attempts, Window: &window},
			Placement: &swarm.Placement{
				Constraints: []string{"node.role==worker"}, MaxReplicas: 2,
				Preferences: []swarm.PlacementPreference{{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.zone"}}},
			},
			Networks:  []swarm.NetworkAttachmentConfig{{Target: "net-front", Aliases: []string{"api", "api.internal"}}},
			LogDriver: &swarm.Driver{Name: "splunk", Options: map[string]string{"splunk-token": sdSecrets["log"]}},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism: 2, Delay: 10 * time.Second, FailureAction: swarm.UpdateFailureActionPause,
			Monitor: 5 * time.Second, MaxFailureRatio: 0.25, Order: swarm.UpdateOrderStartFirst,
		},
		EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: "ingress"}}},
	}
}

func sdService() dockertest.FakeService {
	prev := sdSpec("registry.example.com/shop/api:1.3", 2)
	return dockertest.FakeService{
		ID: "svc-api", Name: "api", Mode: "replicated", Running: 2, Desired: 3, Version: 7,
		Spec: sdSpec("registry.example.com/shop/api:1.4@"+sdDigest, 3), PreviousSpec: prev,
		UpdateState: "completed", UpdateMessage: "update completed",
		Ports:     []swarm.PortConfig{{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: "ingress"}},
		CreatedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 2, 11, 30, 0, 0, time.UTC),
	}
}

// sdEnv is a swarm environment with the sdService, its network, and the plain webService.
func sdEnv(t *testing.T) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(sdService(), webService())
	cfg.Networks = []dockertest.FakeNetwork{{ID: "net-front", Name: "shop_front", Driver: "overlay", Scope: "swarm"}}
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

type sdDetail struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	Running   uint64 `json:"running"`
	Desired   uint64 `json:"desired"`
	Stack     string `json:"stack"`
	Agent     bool   `json:"agent"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Image     struct {
		Ref    string `json:"ref"`
		Tag    string `json:"tag"`
		Digest string `json:"digest"`
		Pinned bool   `json:"pinned"`
	} `json:"image"`
	Ports []struct {
		Published uint32 `json:"published"`
		Target    uint32 `json:"target"`
		Protocol  string `json:"protocol"`
		Mode      string `json:"mode"`
	} `json:"ports"`
	Mounts []struct {
		Type     string `json:"type"`
		Source   string `json:"source"`
		Target   string `json:"target"`
		ReadOnly bool   `json:"readOnly"`
	} `json:"mounts"`
	Secrets  []sdFileRef `json:"secrets"`
	Configs  []sdFileRef `json:"configs"`
	Networks []struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
	} `json:"networks"`
	Env []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Masked bool   `json:"masked"`
	} `json:"env"`
	EnvMasked bool `json:"envMasked"`
	Resources struct {
		CPULimit          float64 `json:"cpuLimit"`
		CPUReservation    float64 `json:"cpuReservation"`
		MemoryLimit       int64   `json:"memoryLimit"`
		MemoryReservation int64   `json:"memoryReservation"`
	} `json:"resources"`
	Placement struct {
		Constraints []string `json:"constraints"`
		Preferences []string `json:"preferences"`
		MaxReplicas uint64   `json:"maxReplicas"`
	} `json:"placement"`
	RestartPolicy *struct {
		Condition   string `json:"condition"`
		Delay       string `json:"delay"`
		MaxAttempts uint64 `json:"maxAttempts"`
		Window      string `json:"window"`
	} `json:"restartPolicy"`
	UpdatePolicy *struct {
		Parallelism     uint64  `json:"parallelism"`
		Delay           string  `json:"delay"`
		Order           string  `json:"order"`
		FailureAction   string  `json:"failureAction"`
		Monitor         string  `json:"monitor"`
		MaxFailureRatio float64 `json:"maxFailureRatio"`
	} `json:"updatePolicy"`
	Labels          map[string]string `json:"labels"`
	UpdateStatus    string            `json:"updateStatus"`
	HasPreviousSpec bool              `json:"hasPreviousSpec"`
}

type sdFileRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	File string `json:"file"`
	UID  string `json:"uid"`
	GID  string `json:"gid"`
	Mode string `json:"mode"`
}

func sdNoSecrets(t *testing.T, who string, rec *httptest.ResponseRecorder) {
	t.Helper()
	for kind, token := range sdSecrets {
		if strings.Contains(rec.Body.String(), token) {
			t.Errorf("%s received the %s secret %q: %s", who, kind, token, rec.Body)
		}
	}
}

func TestServiceDetailMasksForAViewer(t *testing.T) {
	t.Parallel()
	f, e := sdEnv(t)
	get := f.asRole(t, e, "sd-viewer", "viewer")
	rec := get("services/svc-api")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	sdNoSecrets(t, "a viewer", rec)
	var d sdDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Env) != 3 || !d.EnvMasked {
		t.Fatalf("env = %+v masked = %v, want three names with envMasked", d.Env, d.EnvMasked)
	}
	for i, name := range []string{"DB_PASSWORD", "MODE", "FLAG"} {
		if d.Env[i].Name != name || !d.Env[i].Masked || d.Env[i].Value != "" {
			t.Errorf("env[%d] = %+v, want %s masked with no value", i, d.Env[i], name)
		}
	}
	if d.ID != "svc-api" || d.Name != "api" || d.Mode != "replicated" || d.Running != 2 || d.Desired != 3 || d.Stack != "shop" || d.Agent {
		t.Errorf("identity = %+v", d)
	}
	if d.CreatedAt != "2026-10-01T10:00:00Z" || d.UpdatedAt != "2026-10-02T11:30:00Z" {
		t.Errorf("times = %q, %q", d.CreatedAt, d.UpdatedAt)
	}
	if d.UpdateStatus != "completed" || !d.HasPreviousSpec {
		t.Errorf("updateStatus = %q, hasPreviousSpec = %v", d.UpdateStatus, d.HasPreviousSpec)
	}
}

func TestServiceDetailRevealsForAnAdmin(t *testing.T) {
	t.Parallel()
	f, e := sdEnv(t)
	rec := f.get(t, e, "services/svc-api")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	d := decode[sdDetail](t, rec)
	if d.EnvMasked || len(d.Env) != 3 || d.Env[0].Value != sdSecrets["env"] || d.Env[0].Masked || d.Env[1].Value != "prod" || d.Env[2].Name != "FLAG" {
		t.Fatalf("an Admin's env = %+v masked = %v, want the values", d.Env, d.EnvMasked)
	}
}

func TestServiceDetailFillsTheCardsFromTheSpec(t *testing.T) {
	t.Parallel()
	f, e := sdEnv(t)
	d := decode[sdDetail](t, f.get(t, e, "services/svc-api"))

	if d.Image.Ref != "registry.example.com/shop/api:1.4@"+sdDigest || d.Image.Tag != "1.4" || d.Image.Digest != sdDigest || !d.Image.Pinned {
		t.Errorf("image = %+v", d.Image)
	}
	if len(d.Ports) != 1 || d.Ports[0].Published != 8080 || d.Ports[0].Target != 80 || d.Ports[0].Protocol != "tcp" || d.Ports[0].Mode != "ingress" {
		t.Errorf("ports = %+v", d.Ports)
	}
	if len(d.Mounts) != 2 || d.Mounts[0].Type != "volume" || d.Mounts[0].Source != "api-data" || d.Mounts[0].Target != "/data" || d.Mounts[0].ReadOnly ||
		d.Mounts[1].Type != "bind" || d.Mounts[1].Source != "/srv/conf" || !d.Mounts[1].ReadOnly {
		t.Errorf("mounts = %+v", d.Mounts)
	}
	if len(d.Secrets) != 1 || d.Secrets[0] != (sdFileRef{ID: "sec1", Name: "db_pw", File: "db_pw", UID: "0", GID: "0", Mode: "0444"}) {
		t.Errorf("secrets = %+v", d.Secrets)
	}
	if len(d.Configs) != 1 || d.Configs[0] != (sdFileRef{ID: "cfg1", Name: "app_conf", File: "/etc/app.conf", UID: "1000", GID: "1000", Mode: "0440"}) {
		t.Errorf("configs = %+v", d.Configs)
	}
	if len(d.Networks) != 1 || d.Networks[0].ID != "net-front" || d.Networks[0].Name != "shop_front" || len(d.Networks[0].Aliases) != 2 {
		t.Errorf("networks = %+v, want the id and the network's name", d.Networks)
	}
	if d.Resources.CPULimit != 0.5 || d.Resources.CPUReservation != 0.25 || d.Resources.MemoryLimit != 256<<20 || d.Resources.MemoryReservation != 128<<20 {
		t.Errorf("resources = %+v", d.Resources)
	}
	if len(d.Placement.Constraints) != 1 || d.Placement.Constraints[0] != "node.role==worker" || len(d.Placement.Preferences) != 1 ||
		d.Placement.Preferences[0] != "node.labels.zone" || d.Placement.MaxReplicas != 2 {
		t.Errorf("placement = %+v", d.Placement)
	}
	if rp := d.RestartPolicy; rp == nil || rp.Condition != "on-failure" || rp.Delay != "5s" || rp.MaxAttempts != 4 || rp.Window != "2m0s" {
		t.Errorf("restartPolicy = %+v", rp)
	}
	if up := d.UpdatePolicy; up == nil || up.Parallelism != 2 || up.Delay != "10s" || up.Order != "start-first" || up.FailureAction != "pause" || up.Monitor != "5s" || up.MaxFailureRatio != 0.25 {
		t.Errorf("updatePolicy = %+v", up)
	}
	if d.Labels["tier"] != "backend" || d.Labels["com.docker.stack.namespace"] != "shop" {
		t.Errorf("labels = %v", d.Labels)
	}
}

func TestServiceDetailOfAServiceWithoutADigestIsNotPinned(t *testing.T) {
	t.Parallel()
	f, e := sdEnv(t)
	d := decode[sdDetail](t, f.get(t, e, "services/svc-web"))
	if d.Image.Tag != "1" || d.Image.Digest != "" || d.Image.Pinned || d.Image.Ref != scaleImage {
		t.Errorf("image = %+v", d.Image)
	}
	if d.RestartPolicy != nil || d.UpdatePolicy != nil || len(d.Env) != 0 || d.EnvMasked || len(d.Ports) != 0 || len(d.Mounts) != 0 {
		t.Errorf("a spec with no such sections must answer none of them: %+v", d)
	}
}

type sdInspect struct {
	Masked   bool `json:"masked"`
	Document struct {
		ID           string `json:"ID"`
		Spec         sdInspectSpec
		PreviousSpec sdInspectSpec
	} `json:"document"`
}

type sdInspectSpec struct {
	TaskTemplate struct {
		ContainerSpec struct {
			Env         []string
			Command     []string
			Args        []string
			Healthcheck struct{ Test []string }
		}
		LogDriver struct{ Options map[string]string }
	}
}

func TestServiceInspectMasksSpecAndPreviousSpec(t *testing.T) {
	t.Parallel()
	f, e := sdEnv(t)
	get := f.asRole(t, e, "sd-inspect-viewer", "viewer")
	rec := get("services/svc-api/inspect")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	sdNoSecrets(t, "a viewer", rec)
	var got sdInspect
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Masked || got.Document.ID != "svc-api" {
		t.Fatalf("masked = %v, id = %q", got.Masked, got.Document.ID)
	}
	for name, spec := range map[string]sdInspectSpec{"Spec": got.Document.Spec, "PreviousSpec": got.Document.PreviousSpec} {
		cs := spec.TaskTemplate.ContainerSpec
		if len(cs.Env) != 3 || cs.Env[0] != "DB_PASSWORD=<hidden>" || cs.Env[1] != "MODE=<hidden>" || cs.Env[2] != "FLAG" {
			t.Errorf("%s env = %v", name, cs.Env)
		}
		if len(cs.Command) != 1 || cs.Command[0] != "<hidden>" || len(cs.Args) != 1 || cs.Args[0] != "<hidden>" {
			t.Errorf("%s command = %v args = %v", name, cs.Command, cs.Args)
		}
		if len(cs.Healthcheck.Test) != 2 || cs.Healthcheck.Test[0] != "CMD-SHELL" || cs.Healthcheck.Test[1] != "<hidden>" {
			t.Errorf("%s health check = %v", name, cs.Healthcheck.Test)
		}
		if spec.TaskTemplate.LogDriver.Options["splunk-token"] != "<hidden>" {
			t.Errorf("%s log options = %v", name, spec.TaskTemplate.LogDriver.Options)
		}
	}
}

func TestServiceInspectRevealsForAnAdmin(t *testing.T) {
	t.Parallel()
	f, e := sdEnv(t)
	rec := f.get(t, e, "services/svc-api/inspect")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[sdInspect](t, rec)
	if got.Masked || got.Document.Spec.TaskTemplate.ContainerSpec.Env[0] != "DB_PASSWORD="+sdSecrets["env"] ||
		got.Document.PreviousSpec.TaskTemplate.LogDriver.Options["splunk-token"] != sdSecrets["log"] {
		t.Fatalf("an Admin's inspect = %s", rec.body)
	}
}

func TestServiceDetailNotFound(t *testing.T) {
	t.Parallel()
	f, e := sdEnv(t)
	for _, path := range []string{"services/ghost", "services/ghost/inspect"} {
		rec := f.get(t, e, path)
		if rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
			t.Errorf("GET %s = %d %q: %s, want 404 service_not_found", path, rec.code, rec.ptyp, rec.body)
		}
	}
}

func TestServiceDetailOfAStandaloneHostIsNotASwarm(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	for _, path := range []string{"services/web", "services/web/inspect"} {
		rec := f.get(t, e, path)
		if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
			t.Errorf("GET %s = %d %q: %s, want 409 not_a_swarm", path, rec.code, rec.ptyp, rec.body)
		}
	}
}

func TestServiceDetailNeedsAGrantOnTheEnvironment(t *testing.T) {
	t.Parallel()
	f, e := sdEnv(t)
	f.addUser(t, "sd-nobody", false)
	nobody, _ := f.signIn(t, "sd-nobody", false)
	for _, path := range []string{"services/svc-api", "services/svc-api/inspect"} {
		rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/" + path, cookie: nobody})
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "environment_not_found" {
			t.Errorf("no grant, GET %s = %d: %s", path, rec.Code, rec.Body)
		}
		if anon := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/" + path}); anon.Code != http.StatusUnauthorized {
			t.Errorf("anonymous, GET %s = %d", path, anon.Code)
		}
	}
}
