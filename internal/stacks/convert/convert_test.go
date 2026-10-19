// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package convert_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func plan(t *testing.T, name, compose string, vars ...stacks.Var) (convert.Plan, error) {
	t.Helper()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: name, Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, Vars: vars},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return convert.Convert(loaded.Project, convert.Options{StackName: name, StackID: "id-" + name})
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
	if net.Name != "shop_default" || net.Driver != "overlay" || !net.Attachable || net.External {
		t.Errorf("network = %+v, want overlay attachable shop_default", net)
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
	if !slices.Equal(aliases, []string{"api", "backend"}) {
		t.Errorf("aliases on back = %v, want api and backend", aliases)
	}
}

func TestConvertRefusesASettingItDoesNotConvertYet(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1
    command: ["nginx", "-g", "daemon off;"]
    volumes:
      - data:/data
    deploy:
      resources:
        limits:
          memory: 64M
volumes:
  data: {}
`
	_, err := plan(t, "s", compose)
	var nc *convert.NotConvertedError
	if !errors.Is(err, convert.ErrNotConverted) || !errors.As(err, &nc) {
		t.Fatalf("err = %v, want ErrNotConverted", err)
	}
	if nc.Service != "web" || !slices.Equal(nc.Fields, []string{"Command", "Volumes", "deploy.Resources"}) {
		t.Errorf("not converted = %+v", nc)
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
