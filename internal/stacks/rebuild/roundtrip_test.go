// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package rebuild_test

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/convert"
	"github.com/ianglennon/coaming/internal/stacks/rebuild"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The offline oracle of the swarm rebuilder (plan 08-11, D-20): converting a compose file, rebuilding the compose
// file from the specs it gave, and converting that gives the same specs. Nothing here needs an engine, so the
// proof holds on every run; the real-engine oracle is plan 08-26.

// plannedObjects knows the secrets and configs the corpus references, as the converter's own tests do: the swarm
// object of a reference <name> is <name>.h1 (a secret) or <name>.h2 (a config), with the ID sid_ or cid_.
type plannedObjects struct{ secrets, configs []string }

func (o plannedObjects) Secret(name string) (string, string, bool) {
	return "sid_" + name, name + ".h1", slices.Contains(o.secrets, name)
}

func (o plannedObjects) Config(name string) (string, string, bool) {
	return "cid_" + name, name + ".h2", slices.Contains(o.configs, name)
}

var corpusObjects = plannedObjects{
	secrets: []string{"db_password", "tls_key"},
	configs: []string{"app_config", "nginx_conf"},
}

// rebuiltObjects resolves the references of a rebuilt file: every secret and config is external, so its swarm name
// is the name the file gives it, and its ID is the one the swarm holds for that name.
type rebuiltObjects struct {
	project          *types.Project
	secrets, configs []rebuild.ObjectInfo
}

func (o rebuiltObjects) Secret(key string) (string, string, bool) {
	decl, ok := o.project.Secrets[key]
	if !ok {
		return "", "", false
	}
	return idOf(o.secrets, decl.Name)
}

func (o rebuiltObjects) Config(key string) (string, string, bool) {
	decl, ok := o.project.Configs[key]
	if !ok {
		return "", "", false
	}
	return idOf(o.configs, decl.Name)
}

func idOf(objs []rebuild.ObjectInfo, name string) (string, string, bool) {
	i := slices.IndexFunc(objs, func(o rebuild.ObjectInfo) bool { return o.Name == name })
	if i < 0 {
		return "", name, false
	}
	return objs[i].ID, name, true
}

func load(t *testing.T, name string, content []byte) *types.Project {
	t.Helper()
	loaded, err := stacks.Load(testsupport.Context(t), stacks.LoadInput{
		Name: name, Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: content}}},
	})
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, content)
	}
	return loaded.Project
}

// inputOf is what the engine would hold after a deploy of the plan: the services as the plan made them, the
// networks it made or referenced, and the secrets and configs under the names the plan gave them.
func inputOf(name string, plan convert.Plan, objs plannedObjects) rebuild.SwarmInput {
	in := rebuild.SwarmInput{Namespace: name}
	for _, sp := range plan.Services {
		in.Services = append(in.Services, rebuild.SwarmService{ID: "id-" + sp.Name, Name: sp.Name, Spec: sp.Spec})
	}
	for _, n := range plan.Networks {
		info := rebuild.NetworkInfo{
			ID: "net-" + n.Name, Name: n.Name, Driver: n.Driver, Attachable: n.Attachable, Internal: n.Internal,
			Labels: n.Labels, Options: n.DriverOpts, IPAMDriver: n.IPAMDriver,
		}
		for _, p := range n.Pools {
			info.Pools = append(info.Pools, rebuild.Pool{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
		}
		in.Networks = append(in.Networks, info)
	}
	for _, s := range objs.secrets {
		in.Secrets = append(in.Secrets, rebuild.ObjectInfo{ID: "sid_" + s, Name: s + ".h1"})
	}
	for _, c := range objs.configs {
		in.Configs = append(in.Configs, rebuild.ObjectInfo{ID: "cid_" + c, Name: c + ".h2"})
	}
	return in
}

// withoutOwnLabels returns the spec as the review compares it, without the labels Coaming puts on what it makes.
func withoutOwnLabels(s swarm.ServiceSpec) swarm.ServiceSpec {
	s = stacks.NormalizeSpec(s)
	for _, labels := range []map[string]string{s.Labels, s.TaskTemplate.ContainerSpec.Labels} {
		for k := range labels {
			if strings.HasPrefix(k, "coaming.") {
				delete(labels, k)
			}
		}
	}
	return s
}

// networkOf is a planned network as the round trip compares it: its key is the file's, not the engine's, and the
// engine reads an IPAM driver of "default" and none alike.
func networkOf(n convert.NetworkPlan) convert.NetworkPlan {
	n.Key = ""
	if n.IPAMDriver == "default" {
		n.IPAMDriver = ""
	}
	return n
}

// roundTrip converts a compose file, rebuilds it and converts the rebuilt file, and fails on any difference.
func roundTrip(t *testing.T, name, compose string) rebuild.Result {
	t.Helper()
	first, err := convert.Convert(load(t, name, []byte(compose)), convert.Options{StackName: name, StackID: "id-" + name, Objects: corpusObjects})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	in := inputOf(name, first, corpusObjects)
	res, err := rebuild.Swarm(in)
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}
	project := load(t, name, res.YAML)
	second, err := convert.Convert(project, convert.Options{
		StackName: name, StackID: "id-" + name,
		Objects: rebuiltObjects{project: project, secrets: in.Secrets, configs: in.Configs},
	})
	if err != nil {
		t.Fatalf("Convert of the rebuilt file: %v\n%s", err, res.YAML)
	}

	if len(first.Services) != len(second.Services) {
		t.Fatalf("services = %d after the round trip, want %d\n%s", len(second.Services), len(first.Services), res.YAML)
	}
	for i, want := range first.Services {
		got := second.Services[i]
		if got.Name != want.Name {
			t.Errorf("service %d is %s, want %s", i, got.Name, want.Name)
			continue
		}
		if g, w := withoutOwnLabels(got.Spec), withoutOwnLabels(want.Spec); !reflect.DeepEqual(g, w) {
			t.Errorf("service %s differs after the round trip\n got: %s\nwant: %s\nfile:\n%s", want.Name, asJSON(g), asJSON(w), res.YAML)
		}
	}
	norm := func(plan convert.Plan) []convert.NetworkPlan {
		out := make([]convert.NetworkPlan, 0, len(plan.Networks))
		for _, n := range plan.Networks {
			out = append(out, networkOf(n))
		}
		slices.SortFunc(out, func(a, b convert.NetworkPlan) int { return cmp.Compare(a.Name, b.Name) })
		return out
	}
	if g, w := norm(second), norm(first); !reflect.DeepEqual(g, w) {
		t.Errorf("networks differ after the round trip\n got: %s\nwant: %s\nfile:\n%s", asJSON(g), asJSON(w), res.YAML)
	}
	return res
}

// asJSON prints a value so a failure shows what the pointers hold.
func asJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "convert", "testdata", "compose", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no compose corpus under convert/testdata/compose (%v)", err)
	}
	slices.Sort(files)
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".yaml")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			content, err := os.ReadFile(file) //nolint:gosec // a corpus file in the repository
			if err != nil {
				t.Fatal(err)
			}
			roundTrip(t, name, string(content))
		})
	}
}

// TestRoundTripHandWritten covers what the corpus does not: the settings it never sets, and the values a file
// writes that a rebuild must not change.
func TestRoundTripHandWritten(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"tmpfs mode and size":          "services:\n  web:\n    image: web:1\n    volumes:\n      - type: tmpfs\n        target: /run\n        tmpfs:\n          size: 1048576\n          mode: 1777\n",
		"image mount":                  "services:\n  web:\n    image: web:1\n    volumes:\n      - type: image\n        source: assets:1\n        target: /assets\n        image:\n          subpath: dist\n",
		"job with concurrency":         "services:\n  batch:\n    image: batch:1\n    deploy:\n      mode: replicated-job\n      replicas: 4\n      restart_policy:\n        condition: none\n",
		"dnsrr with host ports":        "services:\n  db:\n    image: db:1\n    ports:\n      - target: 5432\n        published: 5432\n        mode: host\n      - target: 5433\n        published: 5433\n        protocol: udp\n        mode: host\n    deploy:\n      endpoint_mode: dnsrr\n",
		"values with dollar signs":     "services:\n  web:\n    image: web:1\n    environment:\n      PW: pa$$word\n      REF: $${X}\n      MIXED: a$$$$b\n    command: [\"sh\", \"-c\", \"echo $$HOME\"]\n",
		"scaled to zero":               "services:\n  web:\n    image: web:1\n    deploy:\n      replicas: 0\n",
		"all at once":                  "services:\n  web:\n    image: web:1\n    deploy:\n      update_config:\n        parallelism: 0\n      rollback_config:\n        parallelism: 0\n        order: start-first\n",
		"cpus and memory":              "services:\n  web:\n    image: web:1\n    deploy:\n      resources:\n        limits:\n          cpus: \"0.1\"\n          memory: 1G\n        reservations:\n          cpus: \"1.5\"\n          memory: 100M\n",
		"restart condition only":       "services:\n  web:\n    image: web:1\n    deploy:\n      restart_policy:\n        condition: any\n",
		"two networks and the default": "services:\n  web:\n    image: web:1\n    networks: [default, back]\nnetworks:\n  back: {}\n",
		"a label on every level":       "services:\n  web:\n    image: web:1\n    labels:\n      a.b/c: \"1\"\n    deploy:\n      labels:\n        d.e/f: \"2\"\n",
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		compose := cases[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			roundTrip(t, "hand", compose)
		})
	}
}

// TestRebuildKeepsANamedPipeMount: a Windows named pipe has no meaning on a swarm of Linux nodes, so convert
// refuses it; a service that holds one is still written as it is, and the check says what the pipe means.
func TestRebuildKeepsANamedPipeMount(t *testing.T) {
	t.Parallel()
	svc := coreService()
	svc.Spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: "npipe", Source: `\\.\pipe\docker_engine`, Target: `\\.\pipe\docker_engine`}}

	res, err := rebuild.Swarm(rebuild.SwarmInput{Namespace: "shop", Services: []rebuild.SwarmService{svc}})
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}

	if !strings.Contains(string(res.YAML), "type: npipe") {
		t.Errorf("the pipe mount is not in the file:\n%s", res.YAML)
	}
	vols := loadYAML(t, "shop", res.YAML).Project.Services["web"].Volumes
	if len(vols) != 1 || vols[0].Type != "npipe" {
		t.Errorf("volumes = %+v, want the one npipe mount", vols)
	}
}
