// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// The service as it runs (plan 07-22, STK-06, STK-09, Pitfall 5): a container's inspect document is read back
// as a compose service and compared with the one the file prepares. The two sides go through one normaliser,
// so what an image or the engine fills in is no difference, and a difference in a setting the engine keeps
// is exactly one row.

const webImage = "nginx:1.27-alpine"

// engineImage is the image the containers of these tests are made from: its own environment, command,
// entrypoint, labels, user, working directory and exposed port, which the engine merges into a container.
var engineImage = dockertest.FakeImage{
	ID: "sha256:" + strings.Repeat("1", 64), RepoTags: []string{webImage},
	Env: []string{"PATH=/usr/local/sbin:/usr/local/bin", "NGINX_VERSION=1.27"},
	Cmd: []string{"nginx", "-g", "daemon off;"}, Entrypoint: []string{"/docker-entrypoint.sh"},
	Labels: map[string]string{"maintainer": "NGINX Docker Maintainers"}, WorkingDir: "/", StopSignal: "SIGQUIT",
	ExposedPorts: []string{"80/tcp"},
}

// engineDefaults is what ImageDefaultsOf reads from engineImage's inspect document.
func engineDefaults(t *testing.T) stacks.ImageDefaults {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"Config": map[string]any{
		"Env": engineImage.Env, "Cmd": engineImage.Cmd, "Entrypoint": engineImage.Entrypoint, "Labels": engineImage.Labels,
		"WorkingDir": engineImage.WorkingDir, "StopSignal": engineImage.StopSignal,
		"ExposedPorts": map[string]struct{}{"80/tcp": {}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var in image.InspectResponse
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	return stacks.ImageDefaultsOf(in)
}

// preparedProject loads a compose file as a standalone stack and prepares it the way a deploy does.
func preparedProject(t *testing.T, compose string, files ...stacks.File) *types.Project {
	t.Helper()
	p := loadProject(t, compose)
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, p, stacks.NewFileSet(files), nil)
	if err != nil {
		t.Fatalf("ResolveObjectsFor: %v", err)
	}
	pins := map[string]string{}
	for name, svc := range p.Services {
		if svc.Image != "" {
			pins[name] = svc.Image + "@sha256:" + strings.Repeat("a", 64)
		}
	}
	if err := stacks.PrepareStandalone(p, "stack-1", pins, objs); err != nil {
		t.Fatalf("PrepareStandalone: %v", err)
	}
	return p
}

// replica is the container the SDK makes for a service of the prepared project, as the engine documents it.
func replica(t *testing.T, p *types.Project, service, id string) container.InspectResponse {
	t.Helper()
	nets, vols := map[string]string{}, map[string]string{}
	for k, n := range p.Networks {
		nets[k] = n.Name
	}
	for k, v := range p.Volumes {
		vols[k] = v.Name
	}
	return dockertest.ComposeInspect(p.Services[service], dockertest.ComposeOptions{
		Project: p.Name, ID: id, Networks: nets, Volumes: vols, Image: engineImage,
	})
}

// compareService is how the review compares one service: the prepared service and the one rebuilt from its
// container, both normalised with the image's defaults.
func compareService(t *testing.T, p *types.Project, service string, c container.InspectResponse, peers map[string]string) (desired, running types.ServiceConfig) {
	t.Helper()
	rc := stacks.RunningContextOf(p, peers)
	img := engineDefaults(t)
	svc := p.Services[service]
	desired = stacks.NormalizeComposeService(svc, img, rc)
	running = stacks.AlignDaemonDefaults(desired, stacks.NormalizeComposeService(stacks.RunningService(c, img, &svc, rc), img, rc))
	return desired, running
}

const richCompose = `services:
  web:
    image: nginx:1.27-alpine
    command: ["nginx", "-g", "daemon off;", "-c", "/etc/web.conf"]
    environment:
      MODE: prod
      EMPTY: ""
      NGINX_VERSION: "1.27"
    labels:
      team: platform
    ports:
      - "8080:80"
      - "127.0.0.1:8443:443/tcp"
      - "5353:53/udp"
    expose: ["9000", "9100-9101"]
    volumes:
      - data:/data
      - /srv/site:/site:ro
      - type: tmpfs
        target: /scratch
    networks:
      front:
        aliases: [www]
        ipv4_address: 172.30.0.10
      back: {}
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    user: "1000:1000"
    working_dir: /app
    hostname: web-host
    domainname: example.test
    stop_grace_period: 15s
    stop_signal: SIGINT
    cap_add: [NET_ADMIN]
    cap_drop: [ALL]
    read_only: true
    tty: true
    tmpfs: [/run]
    extra_hosts: ["db.local:10.0.0.5"]
    dns: [1.1.1.1]
    dns_search: [example.test]
    sysctls:
      net.core.somaxconn: "1024"
    mem_limit: 256m
    cpus: 0.5
    devices: ["/dev/null:/dev/null"]
    security_opt: ["no-new-privileges:true"]
    group_add: ["audio"]
    depends_on: [db]
  db:
    image: postgres:16
    restart: always
    deploy:
      restart_policy:
        condition: on-failure
        max_attempts: 3
      resources:
        limits:
          memory: 512M
          cpus: "0.25"
networks:
  front:
    ipam:
      config:
        - subnet: 172.30.0.0/24
  back: {}
volumes:
  data: {}
`

func TestAContainerMadeFromTheServiceReadsBackAsTheSameService(t *testing.T) {
	t.Parallel()
	p := preparedProject(t, richCompose)
	for _, name := range []string{"web", "db"} {
		desired, running := compareService(t, p, name, replica(t, p, name, strings.Repeat("c", 64)), nil)
		if !stacks.SameComposeService(desired, running) {
			want, _ := stacks.ServiceJSON(desired)
			got, _ := stacks.ServiceJSON(running)
			t.Errorf("service %s reads back differently\nfile:    %s\nrunning: %s", name, want, got)
		}
	}
}

// An engine older than 26 (API below 1.45) also lists the container's hostname among the aliases of each
// network it is on, though the compose file named no such alias. It is no difference.
func TestTheHostnameAnOlderEngineAddsAsAnAliasIsNoDifference(t *testing.T) {
	t.Parallel()
	p := preparedProject(t, richCompose)
	c := replica(t, p, "web", strings.Repeat("c", 64))
	for _, ep := range c.NetworkSettings.Networks {
		ep.Aliases = append(ep.Aliases, "web-host")
	}
	desired, running := compareService(t, p, "web", c, nil)
	if !stacks.SameComposeService(desired, running) {
		want, _ := stacks.ServiceJSON(desired)
		got, _ := stacks.ServiceJSON(running)
		t.Errorf("the hostname alias is listed\nfile:    %s\nrunning: %s", want, got)
	}
}

func TestTheImagesOwnValuesAreNoDifference(t *testing.T) {
	t.Parallel()
	// Neither command nor entrypoint nor environment is written: the container gets the image's.
	p := preparedProject(t, "services:\n  web:\n    image: nginx:1.27-alpine\n")
	desired, running := compareService(t, p, "web", replica(t, p, "web", strings.Repeat("d", 64)), nil)
	if !stacks.SameComposeService(desired, running) {
		got, _ := stacks.ServiceJSON(running)
		t.Fatalf("the image's command, environment, labels and exposed port are listed: %s", got)
	}
	for _, field := range []string{`"environment"`, `"labels"`, `"expose"`, `"user"`, `"working_dir"`} {
		if got, _ := stacks.ServiceJSON(running); strings.Contains(string(got), field) {
			t.Errorf("the normalised running service still holds %s: %s", field, got)
		}
	}
}

func TestTheComposeLabelsAndTierPlansOwnAreNotTheFilesLabels(t *testing.T) {
	t.Parallel()
	p := preparedProject(t, "services:\n  web:\n    image: nginx:1.27-alpine\n    labels:\n      team: a\n")
	c := replica(t, p, "web", strings.Repeat("e", 64))
	if c.Config.Labels["com.docker.compose.project"] == "" || c.Config.Labels["tierplan.stack.id"] == "" {
		t.Fatalf("the container lacks the labels of the SDK and TierPlan: %v", c.Config.Labels)
	}
	_, running := compareService(t, p, "web", c, nil)
	if !slices.Equal(labelKeys(running), []string{"team"}) {
		t.Errorf("labels = %v, want only team", labelKeys(running))
	}
}

func labelKeys(s types.ServiceConfig) []string {
	var out []string
	for k := range s.Labels {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// settingPaths are the paths of the settings that differ between two normalised services, as the change row
// of a review lists them.
func settingPaths(t *testing.T, desired, running types.ServiceConfig) (settings int, current, next string) {
	t.Helper()
	ch := stacks.ChangedComposeService("web", running, desired, stacks.NewMasker(nil, nil))
	return ch.Settings, string(ch.Current), string(ch.Next)
}

func TestEachSettingTheEngineKeepsShowsAsOneChangeWhenItDiffers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		edit   func(*container.InspectResponse)
		cur    string // what the running side shows
		next   string // what the file says
		anchor string
	}{
		{"restart", func(c *container.InspectResponse) {
			c.HostConfig.RestartPolicy = container.RestartPolicy{Name: "always"}
		}, `"restart":"always"`, `"restart":"unless-stopped"`, "restart"},
		{"a published port", func(c *container.InspectResponse) {
			for p, b := range c.HostConfig.PortBindings {
				if p.Num() == 80 {
					b[0].HostPort = "9090"
				}
			}
		}, `"published":"9090"`, `"published":"8080"`, "ports"},
		{"an environment value", func(c *container.InspectResponse) { c.Config.Env = append(c.Config.Env, "MODE=dev") }, `"MODE":"dev"`, `"MODE":"prod"`, "environment"},
		{"the user", func(c *container.InspectResponse) { c.Config.User = "root" }, `"user":"root"`, `"user":"1000:1000"`, "user"},
		{"a capability", func(c *container.InspectResponse) { c.HostConfig.CapAdd = []string{"SYS_ADMIN"} }, `"SYS_ADMIN"`, `"NET_ADMIN"`, "cap_add"},
		{"the memory limit", func(c *container.InspectResponse) { c.HostConfig.Memory = 128 << 20 }, `"mem_limit":"134217728"`, `"mem_limit":"268435456"`, "mem_limit"},
		{"a network alias", func(c *container.InspectResponse) {
			for _, ep := range c.NetworkSettings.Networks {
				if slices.Contains(ep.Aliases, "www") {
					ep.Aliases = slices.DeleteFunc(ep.Aliases, func(a string) bool { return a == "www" })
				}
			}
		}, `"front":{"ipv4_address":"172.30.0.10"}`, `"aliases":["www"]`, "networks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := preparedProject(t, richCompose)
			c := replica(t, p, "web", strings.Repeat("f", 64))
			tc.edit(&c)
			desired, running := compareService(t, p, "web", c, nil)
			if stacks.SameComposeService(desired, running) {
				t.Fatalf("%s was changed on the container and the services read the same", tc.name)
			}
			n, cur, next := settingPaths(t, desired, running)
			if n != 1 {
				t.Errorf("settings = %d, want 1 (%s)\ncurrent: %s\nnext:    %s", n, tc.name, cur, next)
			}
			if !strings.Contains(cur, tc.cur) || !strings.Contains(next, tc.next) {
				t.Errorf("current/next do not show %s and %s\ncurrent: %s\nnext:    %s", tc.cur, tc.next, cur, next)
			}
		})
	}
}

func TestAHandChangedRestartPolicyShowsAtRestartOnly(t *testing.T) {
	t.Parallel()
	p := preparedProject(t, "services:\n  web:\n    image: nginx:1.27-alpine\n    restart: unless-stopped\n")
	c := replica(t, p, "web", strings.Repeat("a", 64))
	c.HostConfig.RestartPolicy = container.RestartPolicy{Name: "always"}
	desired, running := compareService(t, p, "web", c, nil)
	ch := stacks.DiffComposeServices(map[string]types.ServiceConfig{"web": desired}, map[string]types.ServiceConfig{"web": running}, stacks.NewMasker(nil, nil))
	if len(ch) != 1 || ch[0].Change != stacks.ChangeChanged || ch[0].Settings != 1 {
		t.Fatalf("changes = %+v, want web changed at one setting", ch)
	}
	if !strings.Contains(string(ch[0].Current), `"restart":"always"`) || !strings.Contains(string(ch[0].Next), `"restart":"unless-stopped"`) {
		t.Errorf("current = %s, next = %s", ch[0].Current, ch[0].Next)
	}
}

func TestASecretEnvironmentValueThatChangedIsAHiddenChangeAndNeitherSideShowsIt(t *testing.T) {
	t.Parallel()
	p := preparedProject(t, "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      API_KEY: new-secret-value\n")
	c := replica(t, p, "web", strings.Repeat("b", 64))
	c.Config.Env = []string{"API_KEY=old-secret-value"}
	desired, running := compareService(t, p, "web", c, nil)
	m := stacks.NewMasker([]stacks.Var{{Name: "API_KEY", Value: "new-secret-value", Secret: true}}, nil)
	ch := stacks.DiffComposeServices(map[string]types.ServiceConfig{"web": desired}, map[string]types.ServiceConfig{"web": running}, m)
	if len(ch) != 1 || len(ch[0].HiddenChanges) == 0 {
		t.Fatalf("changes = %+v, want a hidden change", ch)
	}
	if !slices.Contains(ch[0].HiddenChanges, "environment.API_KEY") {
		t.Errorf("hidden = %v, want environment.API_KEY", ch[0].HiddenChanges)
	}
	for _, side := range []string{string(ch[0].Current), string(ch[0].Next)} {
		if strings.Contains(side, "secret-value") {
			t.Errorf("a side shows a secret value: %s", side)
		}
	}
}

func TestAServiceSharingAnothersNamespaceAndVolumesReadsBackAsTheService(t *testing.T) {
	t.Parallel()
	p := preparedProject(t, `services:
  web:
    image: nginx:1.27-alpine
    volumes: [data:/data]
  sidecar:
    image: nginx:1.27-alpine
    network_mode: service:web
    volumes_from: [web]
volumes:
  data: {}
`)
	webID, sideID := strings.Repeat("1", 64), strings.Repeat("2", 64)
	ids := map[string][]string{"web": {webID}}
	resolved := stacks.ResolveServiceRefs(p.Services["sidecar"], ids)
	if resolved.NetworkMode != "container:"+webID || !slices.Equal(resolved.VolumesFrom, []string{webID}) {
		t.Fatalf("resolved = %q %v", resolved.NetworkMode, resolved.VolumesFrom)
	}
	// The container records the resolved references, as the SDK sends them.
	prepared := *p
	prepared.Services = types.Services{"web": p.Services["web"], "sidecar": resolved}
	prepared.Services["sidecar"] = withName(resolved, "sidecar")
	c := replica(t, &prepared, "sidecar", sideID)
	peers := map[string]string{webID: "web", sideID: "sidecar"}
	_, running := compareService(t, p, "sidecar", c, peers)
	// The desired side is resolved before it is normalised, as the review does.
	rc := stacks.RunningContextOf(p, peers)
	desired := stacks.NormalizeComposeService(resolved, engineDefaults(t), rc)
	running = stacks.AlignDaemonDefaults(desired, running)
	if !stacks.SameComposeService(desired, running) {
		want, _ := stacks.ServiceJSON(desired)
		got, _ := stacks.ServiceJSON(running)
		t.Errorf("the sidecar reads back differently\nfile:    %s\nrunning: %s", want, got)
	}
	if desired.NetworkMode != "service:web" || !slices.Equal(desired.VolumesFrom, []string{"web"}) {
		t.Errorf("the diff names %q %v, want the service web, not an ID", desired.NetworkMode, desired.VolumesFrom)
	}
}

func withName(s types.ServiceConfig, name string) types.ServiceConfig {
	s.Name = name
	return s
}

func TestWhatTheEngineDoesNotRecordComesFromTheStoredRevision(t *testing.T) {
	t.Parallel()
	p := preparedProject(t, "services:\n  db:\n    image: postgres:16\n  web:\n    image: nginx:1.27-alpine\n    depends_on: [db]\n    pull_policy: missing\n")
	c := replica(t, p, "web", strings.Repeat("a", 64))
	rc := stacks.RunningContextOf(p, nil)
	stored := p.Services["web"]
	with := stacks.NormalizeComposeService(stacks.RunningService(c, engineDefaults(t), &stored, rc), engineDefaults(t), rc)
	without := stacks.NormalizeComposeService(stacks.RunningService(c, engineDefaults(t), nil, rc), engineDefaults(t), rc)
	if _, ok := with.DependsOn["db"]; !ok || with.PullPolicy != "missing" {
		t.Errorf("with a stored revision: depends_on %v pull_policy %q", with.DependsOn, with.PullPolicy)
	}
	if len(without.DependsOn) != 0 || without.PullPolicy != "" {
		t.Errorf("without one the running side has depends_on %v pull_policy %q, want none", without.DependsOn, without.PullPolicy)
	}
}

func TestResolveServiceRefsMirrorsTheSDK(t *testing.T) {
	t.Parallel()
	ids := map[string][]string{"web": {"web-1-id", "web-2-id"}, "db": {"db-1-id"}}
	cases := []struct {
		name    string
		in      types.ServiceConfig
		network string
		ipc     string
		pid     string
		from    []string
	}{
		{"nothing to resolve", types.ServiceConfig{NetworkMode: "host", VolumesFrom: []string{"container:other"}}, "host", "", "", []string{"other"}},
		{"namespaces take the first container by name", types.ServiceConfig{NetworkMode: "service:web", Ipc: "service:db", Pid: "service:web"}, "container:web-1-id", "container:db-1-id", "container:web-1-id", nil},
		{"volumes_from drops the access mode", types.ServiceConfig{VolumesFrom: []string{"web:ro", "db"}}, "", "", "", []string{"web-1-id", "db-1-id"}},
		{"a missing service stays raw and stops the rest of volumes_from and the namespaces", types.ServiceConfig{NetworkMode: "service:web", VolumesFrom: []string{"db", "gone", "web"}}, "service:web", "", "", []string{"db-1-id", "gone", "web"}},
		{"a missing namespace parent stays raw and the ones before it resolve", types.ServiceConfig{NetworkMode: "service:web", Ipc: "service:gone", Pid: "service:db"}, "container:web-1-id", "service:gone", "service:db", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			orig := slices.Clone(tc.in.VolumesFrom)
			got := stacks.ResolveServiceRefs(tc.in, ids)
			if got.NetworkMode != tc.network || got.Ipc != tc.ipc || got.Pid != tc.pid || !slices.Equal(got.VolumesFrom, tc.from) {
				t.Errorf("resolved = %q %q %q %v, want %q %q %q %v", got.NetworkMode, got.Ipc, got.Pid, got.VolumesFrom, tc.network, tc.ipc, tc.pid, tc.from)
			}
			if !slices.Equal(tc.in.VolumesFrom, orig) {
				t.Errorf("the caller's volumes_from changed to %v", tc.in.VolumesFrom)
			}
		})
	}
}

func TestDiffComposeServicesListsAddedChangedAndRemovedInOrder(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker(nil, nil)
	web := types.ServiceConfig{Name: "web", Restart: "always"}
	desired := map[string]types.ServiceConfig{
		"web": {Name: "web", Restart: "unless-stopped"}, "api": {Name: "api"}, "same": {Name: "same"},
	}
	running := map[string]types.ServiceConfig{"web": web, "same": {Name: "same"}, "old": {Name: "old"}}
	var got []string
	for _, c := range stacks.DiffComposeServices(desired, running, m) {
		got = append(got, c.Change+" "+c.Name)
	}
	if want := []string{"added api", "changed web", "removed old"}; !slices.Equal(got, want) {
		t.Errorf("changes = %v, want %v", got, want)
	}
}

func TestExpandPortsKeepsARangeOverTheBudgetAsText(t *testing.T) {
	t.Parallel()
	// A review never expands a range a load would refuse (plan 07-36, CR-02). The ranges here are
	// bounded, so a regression fails instead of hanging.
	for _, expose := range []string{"1-200000", "1-30000", "9000-8000"} {
		got := stacks.NormalizeComposeService(types.ServiceConfig{Name: "web", Image: webImage, Expose: types.StringOrNumberList{expose}}, stacks.ImageDefaults{}, stacks.RunningContext{})
		if !slices.Equal(got.Expose, []string{expose}) {
			t.Errorf("expose %q normalised to %d entries, want the one entry kept as its text", expose, len(got.Expose))
		}
	}
	got := stacks.NormalizeComposeService(types.ServiceConfig{Name: "web", Image: webImage, Expose: types.StringOrNumberList{"10-12/udp"}}, stacks.ImageDefaults{}, stacks.RunningContext{})
	if want := []string{"10/udp", "11/udp", "12/udp"}; !slices.Equal(got.Expose, want) {
		t.Errorf("expose 10-12/udp = %v, want %v", got.Expose, want)
	}
}

// TestRunningKeepsKeepLabel: a service whose file carries tierplan.keep=true reads back from its container with the
// label, so a stack adopted or compared as running keeps the user's opt-out (JAN-07), and removing it from the
// container shows as a difference. tierplan.stack.id is still not the file's.
func TestRunningKeepsKeepLabel(t *testing.T) {
	t.Parallel()
	p := preparedProject(t, "services:\n  web:\n    image: nginx:1.27-alpine\n    labels:\n      team: a\n      tierplan.keep: \"true\"\n")
	c := replica(t, p, "web", strings.Repeat("e", 64))
	if c.Config.Labels["tierplan.stack.id"] == "" {
		t.Fatalf("the container lacks the labels of TierPlan: %v", c.Config.Labels)
	}

	desired, running := compareService(t, p, "web", c, nil)

	if want := []string{"team", stacks.LabelKeep}; !slices.Equal(labelKeys(running), slices.Sorted(slices.Values(want))) {
		t.Errorf("running labels = %v, want %v", labelKeys(running), want)
	}
	if !slices.Equal(labelKeys(desired), labelKeys(running)) {
		t.Errorf("desired labels = %v, running = %v, want them to agree", labelKeys(desired), labelKeys(running))
	}

	delete(c.Config.Labels, stacks.LabelKeep)
	_, lost := compareService(t, p, "web", c, nil)
	if slices.Contains(labelKeys(lost), stacks.LabelKeep) {
		t.Errorf("labels after removal = %v, want tierplan.keep gone from the running service", labelKeys(lost))
	}
}
