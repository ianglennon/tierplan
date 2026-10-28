// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package convert_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
	"github.com/ianglennon/bayplan/internal/stacks/stackstest"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// objects is a fake ObjectResolver: it knows the secrets and configs listed in it, and names the
// swarm object of a reference <name>.h1 (a secret) or <name>.h2 (a config), with the ID sid_ or cid_.
type objects struct{ secrets, configs []string }

func (o objects) Secret(name string) (string, string, bool) {
	return "sid_" + name, name + ".h1", slices.Contains(o.secrets, name)
}

func (o objects) Config(name string) (string, string, bool) {
	return "cid_" + name, name + ".h2", slices.Contains(o.configs, name)
}

// goldenObjects knows every secret and config the golden corpora reference.
var goldenObjects = objects{
	secrets: []string{"db_password", "tls_key"},
	configs: []string{"app_config", "nginx_conf"},
}

func plan(t *testing.T, name, compose string, vars ...stacks.Var) (convert.Plan, error) {
	t.Helper()
	return planWith(t, name, compose, nil, vars...)
}

func planWith(t *testing.T, name, compose string, objs convert.ObjectResolver, vars ...stacks.Var) (convert.Plan, error) {
	t.Helper()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: name, Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, Vars: vars},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return convert.Convert(loaded.Project, convert.Options{StackName: name, StackID: "id-" + name, Objects: objs})
}

// TestGolden converts every compose file under testdata/compose and compares the plan with the
// file of the same name under testdata/golden. Run with -update to rewrite the goldens, and
// review the diff before committing it.
func TestGolden(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("testdata", "compose", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no compose corpora under testdata/compose (%v)", err)
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".yaml")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			content, err := os.ReadFile(file) //nolint:gosec // a corpus file in the repository
			if err != nil {
				t.Fatal(err)
			}
			got, err := planWith(t, name, string(content), goldenObjects)
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			raw, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			path := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.WriteFile(path, raw, 0o644); err != nil { //nolint:gosec // a golden file in the repository
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) //nolint:gosec // a golden file in the repository
			if err != nil {
				t.Fatalf("read golden (run with -update to write it): %v", err)
			}
			if string(want) != string(raw) {
				t.Errorf("plan differs from %s (run with -update to rewrite it)\n got: %s\nwant: %s", path, raw, want)
			}
		})
	}
}

func TestConvertTheStarterStackMatchesTheGolden(t *testing.T) {
	t.Parallel()
	got, err := plan(t, "tracer", stacks.StarterCompose)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("testdata", "starter.golden.json")
	if *update {
		if err := os.WriteFile(path, raw, 0o644); err != nil { //nolint:gosec // a golden file in the repository
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a golden file in the repository
	if err != nil {
		t.Fatalf("read golden (run with -update to write it): %v", err)
	}
	if string(want) != string(raw) {
		t.Errorf("plan differs from %s (run with -update to rewrite it)\n got: %s\nwant: %s", path, raw, want)
	}
}

func TestConvertNamesTheServiceAndLabelsItAndItsDefaultNetwork(t *testing.T) {
	t.Parallel()
	got, err := plan(t, "shop", stacks.StarterCompose)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(got.Services) != 1 || len(got.Networks) != 1 {
		t.Fatalf("plan = %+v, want one service and one network", got)
	}
	svc := got.Services[0]
	if svc.Name != "shop_web" || svc.Short != "web" || svc.ImageAsWritten != "nginx:1.27-alpine" {
		t.Errorf("service = %s / %s / %s", svc.Name, svc.Short, svc.ImageAsWritten)
	}
	for k, v := range map[string]string{
		"com.docker.stack.namespace": "shop", "bayplan.stack.id": "id-shop", "com.docker.stack.image": "nginx:1.27-alpine",
	} {
		if svc.Spec.Labels[k] != v {
			t.Errorf("service label %s = %q, want %q", k, svc.Spec.Labels[k], v)
		}
	}
	net := got.Networks[0]
	if net.Name != "shop_default" || net.Driver != "overlay" || net.Attachable || net.External {
		t.Errorf("network = %+v, want overlay shop_default that is not attachable, as docker stack deploy makes it", net)
	}
	if net.Labels["com.docker.stack.namespace"] != "shop" || net.Labels["bayplan.stack.id"] != "id-shop" {
		t.Errorf("network labels = %v", net.Labels)
	}
	att := svc.Spec.TaskTemplate.Networks
	if len(att) != 1 || att[0].Target != "shop_default" || !slices.Equal(att[0].Aliases, []string{"web"}) {
		t.Errorf("attachments = %+v, want shop_default with alias web", att)
	}
}

func TestConvertReplicasEnvironmentNetworksAndPorts(t *testing.T) {
	t.Parallel()
	const compose = `services:
  api:
    image: registry.example.com/api:2
    environment:
      ZED: last
      ALPHA: first
      UNSET:
    deploy:
      replicas: 3
      labels:
        team: core
    networks:
      back:
        aliases: [backend]
      outside: {}
    ports:
      - target: 8080
        published: 9000
        mode: host
  agent:
    image: agent:1
    deploy:
      mode: global
networks:
  back:
    driver_opts:
      com.docker.network.driver.mtu: "1400"
    ipam:
      config:
        - subnet: 10.9.0.0/24
  outside:
    external: true
    name: shared-net
  unused: {}
`
	got, err := plan(t, "s", compose)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(got.Services) != 2 || got.Services[0].Short != "agent" || got.Services[1].Short != "api" {
		t.Fatalf("services = %+v, want agent then api", got.Services)
	}
	if got.Services[0].Spec.Mode.Global == nil {
		t.Errorf("agent mode = %+v, want global", got.Services[0].Spec.Mode)
	}
	api := got.Services[1].Spec
	if r := api.Mode.Replicated; r == nil || r.Replicas == nil || *r.Replicas != 3 {
		t.Errorf("api mode = %+v, want 3 replicas", api.Mode)
	}
	if api.Labels["team"] != "core" {
		t.Errorf("api service labels = %v, want deploy.labels kept", api.Labels)
	}
	if env := api.TaskTemplate.ContainerSpec.Env; !slices.Equal(env, []string{"ALPHA=first", "ZED=last"}) {
		t.Errorf("env = %v, want sorted with the value-less variable left out", env)
	}
	if p := api.EndpointSpec.Ports; len(p) != 1 || p[0].TargetPort != 8080 || p[0].PublishedPort != 9000 || p[0].PublishMode != "host" {
		t.Errorf("ports = %+v", p)
	}
	byKey := map[string]convert.NetworkPlan{}
	for _, n := range got.Networks {
		byKey[n.Key] = n
	}
	if _, unused := byKey["unused"]; unused {
		t.Error("a declared network no service uses was planned")
	}
	if n := byKey["outside"]; !n.External || n.Name != "shared-net" || n.Labels != nil {
		t.Errorf("external network = %+v, want referenced by name with no labels", n)
	}
	if n := byKey["back"]; n.Name != "s_back" || n.DriverOpts["com.docker.network.driver.mtu"] != "1400" || len(n.Pools) != 1 || n.Pools[0].Subnet != "10.9.0.0/24" {
		t.Errorf("back network = %+v", n)
	}
	var aliases []string
	for _, a := range api.TaskTemplate.Networks {
		if a.Target == "s_back" {
			aliases = a.Aliases
		}
	}
	if !slices.Equal(aliases, []string{"backend", "api"}) {
		t.Errorf("aliases on back = %v, want the file's alias then the service name, as docker stack deploy writes them", aliases)
	}
}

func TestConvertRefusesAServiceWithNoImage(t *testing.T) {
	t.Parallel()
	_, err := plan(t, "s", "services:\n  web:\n    build: .\n")
	if !errors.Is(err, convert.ErrNotConverted) && !errors.Is(err, convert.ErrNoImage) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestConvertIsStableBetweenRuns(t *testing.T) {
	t.Parallel()
	a, err := plan(t, "s", stacks.StarterCompose)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := plan(t, "s", stacks.StarterCompose)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("two conversions differ:\n%s\n%s", ja, jb)
	}
}

func TestConvertRefusesANetworkDriverThatReachesTheHost(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1
    networks: [lan]
networks:
  lan:
    driver: macvlan
    driver_opts:
      parent: eth0
`
	_, err := plan(t, "s", compose)
	var nc *convert.NotConvertedError
	if !errors.As(err, &nc) || nc.Service != "network lan" || !slices.Equal(nc.Fields, []string{"driver: macvlan"}) {
		t.Fatalf("err = %v, want a refusal of the macvlan driver", err)
	}
}

func TestConvertOverwritesAnOwnershipLabelTheFileTriesToSet(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1
    labels:
      com.docker.stack.namespace: someone-elses
      bayplan.stack.id: not-mine
    deploy:
      labels:
        com.docker.stack.namespace: someone-elses
        bayplan.stack.id: not-mine
networks:
  default:
    labels:
      bayplan.stack.id: not-mine
`
	got, err := plan(t, "mine", compose)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	svc := got.Services[0].Spec
	for name, labels := range map[string]map[string]string{
		"service": svc.Labels, "container": svc.TaskTemplate.ContainerSpec.Labels, "network": got.Networks[0].Labels,
	} {
		if labels["bayplan.stack.id"] != "id-mine" {
			t.Errorf("%s label bayplan.stack.id = %q, want the stack's own", name, labels["bayplan.stack.id"])
		}
		if ns, ok := labels["com.docker.stack.namespace"]; ok && ns != "mine" {
			t.Errorf("%s label com.docker.stack.namespace = %q, want mine", name, ns)
		}
	}
}

func TestConvertAddsTheServiceNameAsAliasOnUserDefinedNetworksOnly(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1
    networks:
      zeta: {}
      onhost:
        aliases: [front]
networks:
  zeta: {}
  onhost:
    external: true
    name: host
`
	got, err := plan(t, "s", compose)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	att := got.Services[0].Spec.TaskTemplate.Networks
	if len(att) != 2 || att[0].Target != "host" || att[1].Target != "s_zeta" {
		t.Fatalf("attachments = %+v, want host then s_zeta (sorted by target)", att)
	}
	if !slices.Equal(att[0].Aliases, []string{"front"}) {
		t.Errorf("aliases on the built-in host network = %v, want only the file's alias", att[0].Aliases)
	}
	if !slices.Equal(att[1].Aliases, []string{"web"}) {
		t.Errorf("aliases on the user network = %v, want the service name", att[1].Aliases)
	}
}

func TestConvertRefusesAServiceOnANetworkTheFileDoesNotDeclare(t *testing.T) {
	t.Parallel()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: "s", Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services:\n  web:\n    image: nginx:1\n")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := loaded.Project.Services["web"]
	svc.Networks = map[string]*composetypes.ServiceNetworkConfig{"ghost": nil}
	loaded.Project.Services["web"] = svc
	if _, err := convert.Convert(loaded.Project, convert.Options{StackName: "s", StackID: "id"}); err == nil {
		t.Fatal("Convert accepted a network the file does not declare")
	}
}

// onlyService converts compose and returns its one service's spec.
func onlyService(t *testing.T, compose string, objs convert.ObjectResolver) convert.ServicePlan {
	t.Helper()
	got, err := planWith(t, "s", compose, objs)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(got.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(got.Services))
	}
	return got.Services[0]
}

func TestConvertNamedVolumesCarryTheStackLabelsAndTheDriver(t *testing.T) {
	t.Parallel()
	const compose = `services:
  db:
    image: postgres:16
    volumes:
      - data:/data
      - cache:/cache
volumes:
  data: {}
  cache:
    driver: local
    driver_opts:
      type: tmpfs
    labels:
      purpose: cache
      bayplan.stack.id: not-mine
`
	mounts := onlyService(t, compose, nil).Spec.TaskTemplate.ContainerSpec.Mounts
	if len(mounts) != 2 {
		t.Fatalf("mounts = %+v, want 2", mounts)
	}
	data, cache := mounts[0], mounts[1]
	if string(data.Type) != "volume" || data.Source != "s_data" || data.Target != "/data" {
		t.Errorf("data mount = %+v, want a volume s_data at /data", data)
	}
	if data.VolumeOptions == nil || data.VolumeOptions.Labels["com.docker.stack.namespace"] != "s" ||
		data.VolumeOptions.Labels["bayplan.stack.id"] != "id-s" || data.VolumeOptions.DriverConfig != nil {
		t.Errorf("data volume options = %+v, want the stack's labels and no driver", data.VolumeOptions)
	}
	if cache.Source != "s_cache" || cache.VolumeOptions == nil {
		t.Fatalf("cache mount = %+v", cache)
	}
	if cache.VolumeOptions.Labels["purpose"] != "cache" || cache.VolumeOptions.Labels["bayplan.stack.id"] != "id-s" {
		t.Errorf("cache labels = %v, want the file's label and the stack's own ID", cache.VolumeOptions.Labels)
	}
	if d := cache.VolumeOptions.DriverConfig; d == nil || d.Name != "local" || d.Options["type"] != "tmpfs" {
		t.Errorf("cache driver = %+v, want local with type tmpfs", d)
	}
}

func TestConvertExternalVolumeKeepsItsNameAndGetsNoLabels(t *testing.T) {
	t.Parallel()
	const compose = `services:
  db:
    image: postgres:16
    volumes:
      - archive:/archive:ro
volumes:
  archive:
    external: true
    name: shared-archive
`
	m := onlyService(t, compose, nil).Spec.TaskTemplate.ContainerSpec.Mounts
	if len(m) != 1 || m[0].Source != "shared-archive" || !m[0].ReadOnly {
		t.Fatalf("mounts = %+v, want the external volume read-only under its own name", m)
	}
	if m[0].VolumeOptions == nil || len(m[0].VolumeOptions.Labels) != 0 || m[0].VolumeOptions.DriverConfig != nil {
		t.Errorf("volume options = %+v, want none of the stack's labels on an external volume", m[0].VolumeOptions)
	}
}

func TestConvertBindTmpfsAndAnonymousVolumes(t *testing.T) {
	t.Parallel()
	const compose = `services:
  db:
    image: postgres:16
    volumes:
      - /etc/ssl/certs:/certs:ro
      - type: bind
        source: /srv/shared
        target: /shared
        bind:
          propagation: rshared
      - type: tmpfs
        target: /tmp
        tmpfs:
          size: 1048576
      - /scratch
`
	m := onlyService(t, compose, nil).Spec.TaskTemplate.ContainerSpec.Mounts
	if len(m) != 4 {
		t.Fatalf("mounts = %+v, want 4", m)
	}
	if string(m[0].Type) != "bind" || m[0].Source != "/etc/ssl/certs" || !m[0].ReadOnly || m[0].BindOptions != nil {
		t.Errorf("plain bind = %+v", m[0])
	}
	if m[1].BindOptions == nil || m[1].BindOptions.Propagation != "rshared" {
		t.Errorf("bind with propagation = %+v", m[1])
	}
	if string(m[2].Type) != "tmpfs" || m[2].Target != "/tmp" || m[2].TmpfsOptions == nil || m[2].TmpfsOptions.SizeBytes != 1048576 {
		t.Errorf("tmpfs = %+v", m[2])
	}
	if string(m[3].Type) != "volume" || m[3].Source != "" || m[3].Target != "/scratch" || m[3].VolumeOptions != nil {
		t.Errorf("anonymous volume = %+v", m[3])
	}
}

func TestConvertRefusesARelativeBindSourceNamingTheService(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"./data", "data/sub", "../up", "~/x"} {
		compose := "services:\n  web:\n    image: nginx:1\n    volumes:\n      - type: bind\n        source: " + src + "\n        target: /data\n"
		_, err := plan(t, "s", compose)
		if !errors.Is(err, convert.ErrRelativeBind) {
			t.Errorf("source %q: err = %v, want ErrRelativeBind", src, err)
			continue
		}
		if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), src) {
			t.Errorf("source %q: %q does not name the service and the source", src, err)
		}
	}
}

func TestConvertRefusesANamedPipeMount(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1
    volumes:
      - type: npipe
        source: \\.\pipe\docker_engine
        target: \\.\pipe\docker_engine
`
	if _, err := plan(t, "s", compose); err == nil {
		t.Fatal("Convert accepted a named pipe mount")
	}
}

func TestConvertDeployBlock(t *testing.T) {
	t.Parallel()
	const compose = `services:
  api:
    image: api:2
    deploy:
      replicas: 4
      resources:
        limits:
          cpus: "0.1"
          memory: 512M
          pids: 200
        reservations:
          cpus: "0.25"
          memory: 128M
      restart_policy:
        condition: on-failure
        delay: 5s
        max_attempts: 3
        window: 2m
      update_config:
        parallelism: 2
        delay: 10s
        failure_action: rollback
        monitor: 30s
        max_failure_ratio: 0.25
        order: start-first
      rollback_config:
        failure_action: pause
      placement:
        constraints: [node.role == worker]
        preferences:
          - spread: node.labels.zone
        max_replicas_per_node: 2
`
	spec := onlyService(t, compose, nil).Spec
	tt := spec.TaskTemplate
	if r := spec.Mode.Replicated; r == nil || *r.Replicas != 4 {
		t.Errorf("mode = %+v, want 4 replicas", spec.Mode)
	}
	if tt.Resources == nil || tt.Resources.Limits == nil || tt.Resources.Reservations == nil {
		t.Fatalf("resources = %+v", tt.Resources)
	}
	if l := tt.Resources.Limits; l.NanoCPUs != 100_000_000 || l.MemoryBytes != 512<<20 || l.Pids != 200 {
		t.Errorf("limits = %+v, want 0.1 cpus exactly, 512M and 200 pids", l)
	}
	if r := tt.Resources.Reservations; r.NanoCPUs != 250_000_000 || r.MemoryBytes != 128<<20 {
		t.Errorf("reservations = %+v", r)
	}
	rp := tt.RestartPolicy
	if rp == nil || rp.Condition != "on-failure" || *rp.Delay != 5*time.Second || *rp.MaxAttempts != 3 || *rp.Window != 2*time.Minute {
		t.Errorf("restart policy = %+v", rp)
	}
	uc := spec.UpdateConfig
	if uc == nil || uc.Parallelism != 2 || uc.Delay != 10*time.Second || uc.FailureAction != "rollback" ||
		uc.Monitor != 30*time.Second || uc.MaxFailureRatio != 0.25 || uc.Order != "start-first" {
		t.Errorf("update config = %+v", uc)
	}
	if rc := spec.RollbackConfig; rc == nil || rc.Parallelism != 1 || rc.FailureAction != "pause" {
		t.Errorf("rollback config = %+v, want parallelism 1 when unset", rc)
	}
	pl := tt.Placement
	if pl == nil || !slices.Equal(pl.Constraints, []string{"node.role == worker"}) || pl.MaxReplicas != 2 ||
		len(pl.Preferences) != 1 || pl.Preferences[0].Spread.SpreadDescriptor != "node.labels.zone" {
		t.Errorf("placement = %+v", pl)
	}
}

func TestConvertWritesAPlacementEvenWhenEmptyAndNoUpdateConfig(t *testing.T) {
	t.Parallel()
	spec := onlyService(t, "services:\n  web:\n    image: nginx:1\n", nil).Spec
	if spec.TaskTemplate.Placement == nil {
		t.Error("Placement is nil; docker stack deploy always writes one")
	}
	if spec.UpdateConfig != nil || spec.RollbackConfig != nil {
		t.Errorf("update/rollback config = %+v / %+v, want none when the file sets none", spec.UpdateConfig, spec.RollbackConfig)
	}
	if spec.TaskTemplate.RestartPolicy != nil {
		t.Errorf("restart policy = %+v, want none", spec.TaskTemplate.RestartPolicy)
	}
}

func TestConvertJobModes(t *testing.T) {
	t.Parallel()
	const compose = `services:
  migrate:
    image: app:1
    deploy:
      mode: replicated-job
      replicas: 3
  scan:
    image: scanner:1
    deploy:
      mode: global-job
`
	got, err := plan(t, "s", compose)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	byShort := map[string]convert.ServicePlan{}
	for _, s := range got.Services {
		byShort[s.Short] = s
	}
	rj := byShort["migrate"].Spec.Mode.ReplicatedJob
	if rj == nil || rj.MaxConcurrent == nil || *rj.MaxConcurrent != 3 || rj.TotalCompletions == nil || *rj.TotalCompletions != 3 {
		t.Errorf("replicated job = %+v, want 3 concurrent and 3 completions", rj)
	}
	if byShort["scan"].Spec.Mode.GlobalJob == nil {
		t.Errorf("scan mode = %+v, want a global job", byShort["scan"].Spec.Mode)
	}
}

func TestConvertRefusesReplicasOnAGlobalService(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"global", "global-job"} {
		_, err := plan(t, "s", "services:\n  a:\n    image: a:1\n    deploy:\n      mode: "+mode+"\n      replicas: 2\n")
		if err == nil || !strings.Contains(err.Error(), "replicas") || !strings.Contains(err.Error(), "a") {
			t.Errorf("mode %s: err = %v, want a refusal naming replicas", mode, err)
		}
	}
}

func TestConvertResolvesSecretsAndConfigsThroughTheResolver(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1
    secrets:
      - tls_key
      - source: db_password
        target: /etc/ssl/key.pem
        uid: "100"
        gid: "101"
        mode: 0400
    configs:
      - source: nginx_conf
        target: /etc/nginx/nginx.conf
        mode: 0440
      - app_config
secrets:
  db_password:
    file: ./db.txt
  tls_key:
    file: ./tls.txt
configs:
  app_config:
    file: ./app.conf
  nginx_conf:
    file: ./nginx.conf
`
	cs := onlyService(t, compose, goldenObjects).Spec.TaskTemplate.ContainerSpec
	if len(cs.Secrets) != 2 || len(cs.Configs) != 2 {
		t.Fatalf("secrets = %d, configs = %d, want 2 and 2", len(cs.Secrets), len(cs.Configs))
	}
	s0, s1 := cs.Secrets[0], cs.Secrets[1]
	if s0.SecretName != "db_password.h1" || s0.SecretID != "sid_db_password" || s0.File == nil ||
		s0.File.Name != "/etc/ssl/key.pem" || s0.File.UID != "100" || s0.File.GID != "101" || s0.File.Mode != 0o400 {
		t.Errorf("first secret (sorted by object name) = %+v / %+v", s0, s0.File)
	}
	if s1.SecretName != "tls_key.h1" || s1.File == nil || s1.File.Name != "tls_key" || s1.File.UID != "0" || s1.File.GID != "0" || s1.File.Mode != 0o444 {
		t.Errorf("second secret = %+v / %+v, want target tls_key, root-owned, mode 0444", s1, s1.File)
	}
	c0, c1 := cs.Configs[0], cs.Configs[1]
	if c0.ConfigName != "app_config.h2" || c0.ConfigID != "cid_app_config" || c0.File == nil || c0.File.Name != "app_config" || c0.File.Mode != 0o444 {
		t.Errorf("first config = %+v / %+v", c0, c0.File)
	}
	if c1.ConfigName != "nginx_conf.h2" || c1.File == nil || c1.File.Name != "/etc/nginx/nginx.conf" || c1.File.Mode != 0o440 {
		t.Errorf("second config = %+v / %+v", c1, c1.File)
	}
}

func TestConvertRefusesASecretOrConfigTheResolverDoesNotKnow(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1
    %s:
      - ghost
%s:
  ghost:
    file: ./x
`
	for _, kind := range []string{"secrets", "configs"} {
		src := strings.NewReplacer("%s", kind).Replace(compose)
		for name, objs := range map[string]convert.ObjectResolver{"no resolver": nil, "a resolver without it": objects{}} {
			_, err := planWith(t, "s", src, objs)
			if !errors.Is(err, convert.ErrUnknownObject) {
				t.Errorf("%s with %s: err = %v, want ErrUnknownObject", kind, name, err)
				continue
			}
			if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "ghost") {
				t.Errorf("%s with %s: %q does not name the service and the reference", kind, name, err)
			}
		}
	}
}

func TestConvertMapsTheLegacyRestartKeyLikeDockerStackDeploy(t *testing.T) {
	t.Parallel()
	for restart, want := range map[string]struct {
		cond string
		max  uint64
	}{
		"unless-stopped": {"any", 0},
		"always":         {"any", 0},
		"on-failure":     {"on-failure", 0},
		"on-failure:5":   {"on-failure", 5},
		"no":             {"", 0},
	} {
		rp := onlyService(t, "services:\n  a:\n    image: a:1\n    restart: \""+restart+"\"\n", nil).Spec.TaskTemplate.RestartPolicy
		if want.cond == "" {
			if rp != nil {
				t.Errorf("restart %q: policy = %+v, want none", restart, rp)
			}
			continue
		}
		if rp == nil || string(rp.Condition) != want.cond {
			t.Errorf("restart %q: policy = %+v, want condition %s", restart, rp, want.cond)
			continue
		}
		if (want.max == 0) != (rp.MaxAttempts == nil) || (rp.MaxAttempts != nil && *rp.MaxAttempts != want.max) {
			t.Errorf("restart %q: max attempts = %v, want %d", restart, rp.MaxAttempts, want.max)
		}
	}
	const both = "services:\n  a:\n    image: a:1\n    restart: always\n    deploy:\n      restart_policy:\n        condition: none\n"
	if rp := onlyService(t, both, nil).Spec.TaskTemplate.RestartPolicy; rp == nil || rp.Condition != "none" {
		t.Errorf("restart plus restart_policy = %+v, want deploy.restart_policy to win", rp)
	}
}

func TestConvertCapabilitiesFollowEffectiveCapAddCapDrop(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		add, drop         string
		wantAdd, wantDrop []string
	}{
		"add all beats drop all":       {"[all, net_admin]", "[ALL]", []string{"ALL"}, nil},
		"add beats drop of the same":   {"[net_admin]", "[NET_ADMIN, net_raw]", []string{"CAP_NET_ADMIN"}, []string{"CAP_NET_RAW"}},
		"normalised and sorted":        {"[sys_time, chown]", "[mknod, CAP_AUDIT_WRITE]", []string{"CAP_CHOWN", "CAP_SYS_TIME"}, []string{"CAP_AUDIT_WRITE", "CAP_MKNOD"}},
		"drop all keeps what is added": {"[chown]", "[ALL]", []string{"CAP_CHOWN"}, []string{"ALL"}},
	} {
		cs := onlyService(t, "services:\n  a:\n    image: a:1\n    cap_add: "+tc.add+"\n    cap_drop: "+tc.drop+"\n", nil).Spec.TaskTemplate.ContainerSpec
		if !slices.Equal(cs.CapabilityAdd, tc.wantAdd) || !slices.Equal(cs.CapabilityDrop, tc.wantDrop) {
			t.Errorf("%s: add %v drop %v, want add %v drop %v", name, cs.CapabilityAdd, cs.CapabilityDrop, tc.wantAdd, tc.wantDrop)
		}
	}
}

func TestConvertRuntimeFields(t *testing.T) {
	t.Parallel()
	const compose = `services:
  api:
    image: api:1
    user: "1000:1000"
    working_dir: /srv
    hostname: api-host
    init: true
    read_only: true
    tty: true
    stdin_open: true
    stop_signal: SIGINT
    stop_grace_period: 45s
    isolation: default
    group_add: ["audio", "1001"]
    extra_hosts:
      - db.local=10.0.0.9
      - cache.local=10.0.0.8
    dns: [1.1.1.1, 9.9.9.9]
    dns_search: [example.com]
    dns_opt: ["ndots:2"]
    sysctls:
      net.core.somaxconn: "1024"
    ulimits:
      nproc: 65535
      nofile:
        soft: 20000
        hard: 40000
    logging:
      driver: json-file
      options:
        max-size: 10m
    oom_score_adj: -100
`
	spec := onlyService(t, compose, nil).Spec
	cs := spec.TaskTemplate.ContainerSpec
	if cs.User != "1000:1000" || cs.Dir != "/srv" || cs.Hostname != "api-host" || cs.Init == nil || !*cs.Init ||
		!cs.ReadOnly || !cs.TTY || !cs.OpenStdin || cs.StopSignal != "SIGINT" || cs.OomScoreAdj != -100 || string(cs.Isolation) != "default" {
		t.Errorf("scalar fields = %+v", cs)
	}
	if cs.StopGracePeriod == nil || *cs.StopGracePeriod != 45*time.Second {
		t.Errorf("stop grace period = %v", cs.StopGracePeriod)
	}
	if !slices.Equal(cs.Groups, []string{"audio", "1001"}) {
		t.Errorf("groups = %v", cs.Groups)
	}
	if !slices.Equal(cs.Hosts, []string{"10.0.0.8 cache.local", "10.0.0.9 db.local"}) {
		t.Errorf("hosts = %v, want swarm notation (ip host), sorted by host", cs.Hosts)
	}
	d := cs.DNSConfig
	if d == nil || len(d.Nameservers) != 2 || d.Nameservers[0].String() != "1.1.1.1" || !slices.Equal(d.Search, []string{"example.com"}) || !slices.Equal(d.Options, []string{"ndots:2"}) {
		t.Errorf("dns = %+v", d)
	}
	if cs.Sysctls["net.core.somaxconn"] != "1024" {
		t.Errorf("sysctls = %v", cs.Sysctls)
	}
	if len(cs.Ulimits) != 2 || cs.Ulimits[0].Name != "nofile" || cs.Ulimits[0].Soft != 20000 || cs.Ulimits[0].Hard != 40000 ||
		cs.Ulimits[1].Name != "nproc" || cs.Ulimits[1].Soft != 65535 || cs.Ulimits[1].Hard != 65535 {
		t.Errorf("ulimits = %+v, want nofile then nproc, a single value for both limits", cs.Ulimits)
	}
	if l := spec.TaskTemplate.LogDriver; l == nil || l.Name != "json-file" || l.Options["max-size"] != "10m" {
		t.Errorf("log driver = %+v", l)
	}
}

func TestConvertRefusesADNSAddressThatIsNotAnIP(t *testing.T) {
	t.Parallel()
	_, err := plan(t, "s", "services:\n  web:\n    image: nginx:1\n    dns: [not-an-ip]\n")
	if err == nil || !strings.Contains(err.Error(), "not-an-ip") {
		t.Fatalf("err = %v, want a refusal naming the address", err)
	}
}

func TestConvertCredentialSpec(t *testing.T) {
	t.Parallel()
	const viaConfig = `services:
  w:
    image: w:1
    credential_spec:
      config: app_config
configs:
  app_config:
    file: ./cred.json
`
	cs := onlyService(t, viaConfig, goldenObjects).Spec.TaskTemplate.ContainerSpec
	if cs.Privileges == nil || cs.Privileges.CredentialSpec == nil || cs.Privileges.CredentialSpec.Config != "cid_app_config" {
		t.Fatalf("privileges = %+v, want the credential spec to carry the config's ID", cs.Privileges)
	}
	if len(cs.Configs) != 1 || cs.Configs[0].Runtime == nil || cs.Configs[0].File != nil || cs.Configs[0].ConfigName != "app_config.h2" {
		t.Errorf("configs = %+v, want one runtime-target reference", cs.Configs)
	}
	const viaFile = "services:\n  w:\n    image: w:1\n    credential_spec:\n      file: spec.json\n"
	if cs := onlyService(t, viaFile, nil).Spec.TaskTemplate.ContainerSpec; cs.Privileges == nil || cs.Privileges.CredentialSpec == nil || cs.Privileges.CredentialSpec.File != "spec.json" {
		t.Errorf("file credential spec = %+v", cs.Privileges)
	}
	const both = "services:\n  w:\n    image: w:1\n    credential_spec:\n      file: spec.json\n      registry: HKLM\n"
	if _, err := plan(t, "s", both); err == nil {
		t.Error("Convert accepted a credential spec with a file and a registry")
	}
}

func TestConvertRefusesAFieldDockerStackDeployForbids(t *testing.T) {
	t.Parallel()
	for field, extra := range map[string]string{
		"volumes_from": "    volumes_from: [other]\n",
		"mem_limit":    "    mem_limit: 64m\n",
		"cpu_shares":   "    cpu_shares: 512\n",
		"env_file":     "    env_file: ./app.env\n",
	} {
		compose := "services:\n  other:\n    image: o:1\n  web:\n    image: nginx:1\n" + extra
		_, err := plan(t, "s", compose)
		var nc *convert.NotConvertedError
		if !errors.Is(err, convert.ErrNotConverted) || !errors.As(err, &nc) {
			t.Errorf("%s: err = %v, want ErrNotConverted", field, err)
			continue
		}
		if nc.Service != "web" || !slices.Contains(nc.Fields, field) {
			t.Errorf("%s: refusal = %+v, want service web and the key %s", field, nc, field)
		}
	}
}

func TestConvertListsTheFieldsASwarmIgnoresAndStillConverts(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1
    container_name: fixed
    privileged: true
    security_opt: ["seccomp:unconfined"]
    build: .
`
	svc := onlyService(t, compose, nil)
	want := []string{"build", "container_name", "privileged", "security_opt"}
	if !slices.Equal(svc.Ignored, want) {
		t.Errorf("Ignored = %v, want %v", svc.Ignored, want)
	}
	raw, _ := json.Marshal(svc.Spec)
	for _, leak := range []string{"unconfined", "privileged", "fixed"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the spec carries %q, which a swarm ignores: %s", leak, raw)
		}
	}
	if clean := onlyService(t, "services:\n  web:\n    image: nginx:1\n", nil); len(clean.Ignored) != 0 {
		t.Errorf("Ignored = %v on a plain service", clean.Ignored)
	}
}

func TestConvertRefusesAServiceThatOnlyBuilds(t *testing.T) {
	t.Parallel()
	_, err := plan(t, "s", "services:\n  a:\n    build: .\n")
	var nc *convert.NotConvertedError
	if !errors.As(err, &nc) || nc.Service != "a" || !slices.Equal(nc.Fields, []string{"build"}) {
		t.Fatalf("err = %v, want a refusal of build alone", err)
	}
}

// A converter refusal caused by a field's value names the field by its dotted compose path (07-41,
// D-02), so the guard can ask whether that field uses a secret variable. Each row is built from a
// literal compose file. The error's own text and sentinels are unchanged: the FieldError wraps.
func TestEveryConverterRefusalOfAFieldNamesItsPath(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1\n"
	cases := []struct {
		name, compose, path string
		objs                convert.ObjectResolver
		// is is a sentinel the error must still answer, nil for none.
		is error
		// notConverted is true when errors.As must still reach a *NotConvertedError.
		notConverted bool
	}{
		{name: "restart", compose: head + "    restart: sometimes\n", path: "services.web.restart"},
		{name: "endpoint_mode", compose: head + "    deploy:\n      endpoint_mode: x\n", path: "services.web.deploy.endpoint_mode"},
		{name: "a published range", compose: head + "    ports:\n      - \"8000-8001:80\"\n", path: "services.web.ports"},
		{name: "dns", compose: head + "    dns: [\"8.8.8.x\"]\n", path: "services.web.dns"},
		{name: "global mode with replicas", compose: head + "    deploy:\n      mode: global\n      replicas: 2\n", path: "services.web.deploy.mode"},
		{name: "a deploy mode the converter refuses", compose: head + "    deploy:\n      mode: weird\n", path: "services.web.deploy.mode", is: convert.ErrNotConverted, notConverted: true},
		{
			name: "a relative bind", compose: head + "    volumes:\n      - type: bind\n        source: ./data\n        target: /data\n",
			path: "services.web.volumes", is: convert.ErrRelativeBind,
		},
		{
			name: "a named pipe", compose: head + "    volumes:\n      - type: npipe\n        source: '\\\\.\\pipe\\docker_engine'\n        target: '\\\\.\\pipe\\docker_engine'\n",
			path: "services.web.volumes",
		},
		{
			name: "a host-reaching network driver", compose: head + "    networks: [front]\nnetworks:\n  front:\n    driver: macvlan\n",
			path: "networks.front.driver", is: convert.ErrNotConverted, notConverted: true,
		},
		{
			name: "an unknown secret", compose: head + "    secrets: [ghost]\nsecrets:\n  ghost:\n    file: ./x\n",
			path: "services.web.secrets", is: convert.ErrUnknownObject,
		},
		{
			name: "an unknown config", compose: head + "    configs: [ghost]\nconfigs:\n  ghost:\n    file: ./x\n",
			path: "services.web.configs", is: convert.ErrUnknownObject,
		},
		{
			name: "an unknown credential spec config", compose: head + "    credential_spec:\n      config: ghost\nconfigs:\n  ghost:\n    file: ./x\n",
			path: "services.web.credential_spec", is: convert.ErrUnknownObject,
		},
		{
			name: "a healthcheck with a test and disable", compose: head + "    healthcheck:\n      test: [\"CMD\", \"true\"]\n      disable: true\n",
			path: "services.web.healthcheck",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := planWith(t, "s", tc.compose, tc.objs)
			var fe *convert.FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v, want a *FieldError", err)
			}
			if fe.Path != tc.path {
				t.Errorf("path = %q, want %q", fe.Path, tc.path)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("err = %v, want it to still answer %v", err, tc.is)
			}
			var nc *convert.NotConvertedError
			if tc.notConverted != errors.As(err, &nc) {
				t.Errorf("errors.As(*NotConvertedError) = %v, want %v for %v", !tc.notConverted, tc.notConverted, err)
			}
		})
	}

	t.Run("a refusal that lists keys only is not a field error", func(t *testing.T) {
		t.Parallel()
		_, err := plan(t, "s", "services:\n  other:\n    image: o:1\n  web:\n    image: nginx:1\n    volumes_from: [other]\n")
		var fe *convert.FieldError
		var nc *convert.NotConvertedError
		if errors.As(err, &fe) || !errors.As(err, &nc) {
			t.Errorf("err = %v, want a *NotConvertedError and no *FieldError", err)
		}
	})
	t.Run("a service with no image is not a field error", func(t *testing.T) {
		t.Parallel()
		// The loader refuses a service with no image, so the project is built by hand.
		p := &composetypes.Project{Services: composetypes.Services{"web": {Name: "web"}}}
		_, err := convert.Convert(p, convert.Options{StackName: "s", StackID: "id-s"})
		var fe *convert.FieldError
		if errors.As(err, &fe) || !errors.Is(err, convert.ErrNoImage) {
			t.Errorf("err = %v, want ErrNoImage and no *FieldError", err)
		}
	})
}

// secretUseOf is the SecretUse of a compose file with one secret variable, PW.
func secretUseOf(compose, secret string) stacks.SecretUse {
	return stacks.NewSecretUse(stacks.WorkingCopy{
		Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}},
		Vars:  []stacks.Var{{Name: "PW", Value: secret, Secret: true}},
	})
}

// The guard withholds a converter error only when its field uses a secret variable, or when it names
// no field and the file uses one. The decision is the field's path, never the error's text, and what
// the guard returns hides its cause from errors.As (07-41, 07-43, T-07-177, T-07-178, T-07-183).
func TestGuardSecretsHidesOnlyWhatComesFromASecret(t *testing.T) {
	t.Parallel()
	const secret = "Password2024!"
	const compose = "services:\n  web:\n    image: nginx:1\n    restart: ${PW}\n    dns: [\"8.8.8.8\"]\n"
	s := secretUseOf(compose, secret)

	t.Run("a field that uses a secret variable", func(t *testing.T) {
		t.Parallel()
		err := convert.GuardSecrets(&convert.FieldError{Path: "services.web.restart", Err: errors.New(`convert: service web: unknown policy "sometimes"`)}, s)
		var sf *convert.SecretFieldError
		if !errors.As(err, &sf) || err.Error() != stacks.SecretValueMessage("services.web.restart") {
			t.Fatalf("err = %v, want the fixed sentence for services.web.restart", err)
		}
		var fe *convert.FieldError
		if errors.As(err, &fe) {
			t.Errorf("errors.As reached the field error %+v, want the cause out of reach", fe)
		}
	})
	t.Run("a not-converted cause stays out of reach but keeps its class", func(t *testing.T) {
		t.Parallel()
		cause := &convert.NotConvertedError{Service: "web", Fields: []string{"deploy.mode: sometimes"}}
		err := convert.GuardSecrets(&convert.FieldError{Path: "services.web.restart", Err: cause}, s)
		var nc *convert.NotConvertedError
		if !errors.Is(err, convert.ErrNotConverted) || errors.As(err, &nc) {
			t.Errorf("err = %v, want errors.Is(ErrNotConverted) true and errors.As(*NotConvertedError) false", err)
		}
		if strings.Contains(err.Error(), "sometimes") {
			t.Errorf("err = %q, want no word of the cause", err)
		}
	})
	t.Run("a plain field that quotes nothing of the secret", func(t *testing.T) {
		t.Parallel()
		in := &convert.FieldError{Path: "services.web.dns", Err: errors.New(`convert: service web: dns "8.8.8.x" is not an IP address`)}
		if got := convert.GuardSecrets(in, s); got != error(in) {
			t.Errorf("err = %v, want it unchanged", got)
		}
	})
	t.Run("a plain field keeps its words even when its value is part of the secret", func(t *testing.T) {
		t.Parallel()
		in := &convert.FieldError{Path: "services.web.dns", Err: errors.New(`convert: service web: dns "Password2024" is not an IP address`)}
		if got := convert.GuardSecrets(in, s); got != error(in) {
			t.Errorf("err = %v, want it unchanged: the decision is the field, never the text", got)
		}
	})
	t.Run("a refusal that lists keys only, and a service with no image", func(t *testing.T) {
		t.Parallel()
		for _, in := range []error{
			&convert.NotConvertedError{Service: "web", Fields: []string{"volumes_from"}},
			fmt.Errorf("%w: %s", convert.ErrNoImage, "web"),
		} {
			if got := convert.GuardSecrets(in, s); got != in {
				t.Errorf("err = %v, want %v unchanged", got, in)
			}
		}
	})
	t.Run("an error with no field fails closed whatever its text", func(t *testing.T) {
		t.Parallel()
		for _, text := range []string{"convert: service web: odd Password2024 text", "convert: service web: odd text"} {
			got := convert.GuardSecrets(errors.New(text), s)
			var sf *convert.SecretFieldError
			if !errors.As(got, &sf) || got.Error() != stacks.SecretValueMessage("") {
				t.Errorf("GuardSecrets(%q) = %v, want a SecretFieldError with the general sentence", text, got)
			}
		}
	})
	t.Run("a field the file does not have gets the file's sentence", func(t *testing.T) {
		t.Parallel()
		got := convert.GuardSecrets(&convert.FieldError{Path: "services.web.no_such_key", Err: errors.New("convert: service web: odd")}, s)
		var sf *convert.SecretFieldError
		if !errors.As(got, &sf) || got.Error() != stacks.SecretFileMessage("services.web.no_such_key") {
			t.Errorf("err = %v, want a SecretFieldError with %q", got, stacks.SecretFileMessage("services.web.no_such_key"))
		}
	})
	t.Run("a file that uses no secret", func(t *testing.T) {
		t.Parallel()
		none := secretUseOf("services:\n  web:\n    image: nginx:1\n", secret)
		in := errors.New("convert: service web: odd Password2024 text")
		if got := convert.GuardSecrets(in, none); got != in {
			t.Errorf("err = %v, want it unchanged", got)
		}
	})
	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := convert.GuardSecrets(nil, s); got != nil {
			t.Errorf("err = %v, want nil", got)
		}
	})
}

// A converter refusal for a field fed by a secret variable holds no run of that secret, whatever the structure
// of the file between the variable and the field: an anchor, a merge key, a nested default, or a plain
// reference (07-44, D-02, T-07-187). Each value is one the loader accepts and the converter refuses.
func TestAGuardedConverterRefusalHoldsNoRunOfItsSecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1\n"
	cases := map[string]struct{ compose, secret string }{
		"a restart policy through an anchor": {"x-r: &r ${PW}\n" + head + "    restart: *r\n", "sometimes:3"},
		"a published port through a merge key": {
			"x-p: &p\n  ports: [\"${PW}\"]\nservices:\n  web:\n    <<: *p\n    image: nginx:1\n", "127.0.0.1:8000-8001:80",
		},
		"a dns server through a nested default": {head + "    dns: [\"${A:-${PW}}\"]\n", "8.8.8.xkwm"},
		"an endpoint mode":                      {head + "    deploy:\n      endpoint_mode: ${PW}\n", "hunter2secretvalue"},
		"a restart policy set again in a later document": {
			head + "    restart: \"no\"\n---\nservices:\n  web:\n    restart: ${PW}\n", "sometimes:3",
		},
		"dns servers across two documents": {
			head + "    dns: [\"1.1.1.1\"]\n---\nservices:\n  web:\n    dns: [\"${PW}\"]\n", "8.8.8.xkwm",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := plan(t, "s", tc.compose, stacks.Var{Name: "PW", Value: tc.secret, Secret: true})
			if err == nil {
				t.Fatalf("Convert accepted %q, want a refusal", tc.compose)
			}
			guarded := convert.GuardSecrets(err, secretUseOf(tc.compose, tc.secret))
			var sf *convert.SecretFieldError
			if !errors.As(guarded, &sf) {
				t.Fatalf("GuardSecrets(%v) = %v, want a *SecretFieldError", err, guarded)
			}
			stackstest.NoSecretRun(t, tc.secret, guarded.Error())
		})
	}
}

func TestConvertUnusualStackName(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27-alpine
    volumes:
      - data:/data
  api:
    image: nginx:1.27-alpine
    networks:
      - back
networks:
  back: {}
volumes:
  data: {}
`
	p, err := plan(t, "MyApp", compose)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	var services, networks []string
	for _, s := range p.Services {
		services = append(services, s.Name)
	}
	for _, n := range p.Networks {
		networks = append(networks, n.Name)
	}
	slices.Sort(services)
	slices.Sort(networks)
	if want := []string{"MyApp_api", "MyApp_web"}; !slices.Equal(services, want) {
		t.Errorf("services = %v, want %v", services, want)
	}
	if want := []string{"MyApp_back", "MyApp_default"}; !slices.Equal(networks, want) {
		t.Errorf("networks = %v, want %v", networks, want)
	}
	for _, s := range p.Services {
		if s.Short != "web" {
			continue
		}
		m := s.Spec.TaskTemplate.ContainerSpec.Mounts
		if len(m) != 1 || m[0].Source != "MyApp_data" {
			t.Errorf("web mounts = %+v, want the volume MyApp_data", m)
		}
		if s.Spec.Labels["com.docker.stack.namespace"] != "MyApp" {
			t.Errorf("web labels = %v, want the namespace MyApp", s.Spec.Labels)
		}
	}
}
