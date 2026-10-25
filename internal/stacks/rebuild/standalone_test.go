// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rebuild_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/rebuild"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The standalone direction (plan 08-12, STK-13, D-12, D-14, D-15, D-18): a docker compose up project, read as its
// containers' inspect documents, back to a compose project. The containers are the ones stackopstest.Foreign stands
// up, made from a compose file the way the SDK makes them, so the file that comes back can be held against the
// file that went in.

// imageInspect is a fake image as the engine inspects it.
func imageInspect(t *testing.T, fi dockertest.FakeImage) image.InspectResponse {
	t.Helper()
	exposed := map[string]struct{}{}
	for _, p := range fi.ExposedPorts {
		exposed[p] = struct{}{}
	}
	raw, err := json.Marshal(map[string]any{
		"Id": fi.ID, "RepoTags": fi.RepoTags, "RepoDigests": fi.RepoDigests,
		"Config": map[string]any{"Env": fi.Env, "Cmd": fi.Cmd, "Entrypoint": fi.Entrypoint, "Labels": fi.Labels, "ExposedPorts": exposed},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out image.InspectResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// hostInputOf is what a host read gives the rebuild for the project h stands up.
func hostInputOf(t *testing.T, h *stackopstest.Hosted) rebuild.StandaloneInput {
	t.Helper()
	in := rebuild.StandaloneInput{Project: h.Name}
	for _, c := range h.Containers {
		doc := *c.Inspect.Document
		sc := rebuild.StandaloneContainer{Inspect: doc}
		for _, fi := range h.Images {
			if slices.Contains(fi.RepoTags, doc.Config.Image) {
				sc.Image = imageInspect(t, fi)
			}
		}
		in.Containers = append(in.Containers, sc)
	}
	for _, n := range h.Networks {
		info := rebuild.NetworkInfo{
			ID: n.ID, Name: n.Name, Driver: n.Driver, Attachable: n.Attachable, Internal: n.Internal, IPv6: n.IPv6,
			Labels: n.Labels, Options: n.Options,
		}
		for i, subnet := range n.Subnets {
			pool := rebuild.Pool{Subnet: subnet}
			if i < len(n.Gateways) {
				pool.Gateway = n.Gateways[i]
			}
			info.Pools = append(info.Pools, pool)
		}
		in.Networks = append(in.Networks, info)
	}
	for _, v := range h.Volumes {
		in.Volumes = append(in.Volumes, rebuild.VolumeInfo{Name: v.Name, Driver: v.Driver, Labels: v.Labels, Options: v.Options})
	}
	return in
}

// loadStandalone reads a rebuilt file as a standalone deploy does.
func loadStandalone(t *testing.T, name string, raw []byte) *types.Project {
	t.Helper()
	loaded, err := stacks.Load(testsupport.Context(t), stacks.LoadInput{
		Name: name, Target: stacks.TargetStandalone,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: raw}}},
	})
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, raw)
	}
	return loaded.Project
}

const standaloneFull = `
services:
  web:
    image: nginx:1.27
    ports:
      - "8080:80"
    environment:
      GREETING: "pa$$word"
      MODE: prod
    volumes:
      - data:/var/lib/data
      - /srv/conf:/conf:ro
    depends_on:
      db:
        condition: service_healthy
        restart: true
      cache:
        condition: service_started
    networks: [back]
    restart: unless-stopped
  db:
    image: postgres:17
    networks: [back]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready"]
      interval: 10s
  cache:
    image: redis:7
    scale: 2
networks:
  back: {}
volumes:
  data: {}
`

func TestStandaloneRebuildsServices(t *testing.T) {
	t.Parallel()
	h := stackopstest.Foreign(t, "shop", standaloneFull)

	res, err := rebuild.Standalone(hostInputOf(t, h))
	if err != nil {
		t.Fatalf("Standalone: %v", err)
	}

	p := loadStandalone(t, "shop", res.YAML)
	if got := slices.Sorted(maps.Keys(p.Services)); !slices.Equal(got, []string{"cache", "db", "web"}) {
		t.Fatalf("services = %v, want cache, db and web:\n%s", got, res.YAML)
	}
	web := p.Services["web"]
	if web.Image != "nginx:1.27" {
		t.Errorf("web image = %q, want the text the container was made from, without a digest", web.Image)
	}
	if dep := web.DependsOn["db"]; dep.Condition != "service_healthy" || !dep.Restart {
		t.Errorf("web depends on db = %+v, want service_healthy with restart", dep)
	}
	if dep := web.DependsOn["cache"]; dep.Condition != "service_started" || dep.Restart {
		t.Errorf("web depends on cache = %+v, want service_started", dep)
	}
	if pw := web.Environment["GREETING"]; pw == nil || *pw != "pa$word" {
		t.Errorf("GREETING loads as %v, want pa$word: a dollar sign is escaped in the file", pw)
	}
	if mode := web.Environment["MODE"]; mode == nil || *mode != "prod" {
		t.Errorf("MODE loads as %v, want prod", mode)
	}
	if _, image := web.Environment["IMAGE_REF"]; image {
		t.Errorf("the image's own environment is in the file:\n%s", res.YAML)
	}
	if len(web.Ports) != 1 || web.Ports[0].Published != "8080" || web.Ports[0].Target != 80 {
		t.Errorf("web ports = %+v, want 8080:80", web.Ports)
	}
	var named, bind bool
	for _, v := range web.Volumes {
		named = named || (v.Type == types.VolumeTypeVolume && v.Source == "data" && v.Target == "/var/lib/data")
		bind = bind || (v.Type == types.VolumeTypeBind && v.Source == "/srv/conf" && v.Target == "/conf" && v.ReadOnly)
	}
	if !named || !bind {
		t.Errorf("web volumes = %+v, want the named volume data and the read-only bind", web.Volumes)
	}
	if web.Restart != "unless-stopped" {
		t.Errorf("web restart = %q, want unless-stopped", web.Restart)
	}
	cache, db := p.Services["cache"], p.Services["db"]
	if got := cache.GetScale(); got != 2 {
		t.Errorf("cache scale = %d, want 2, the number of containers", got)
	}
	if got := db.GetScale(); got != 1 {
		t.Errorf("db scale = %d, want 1", got)
	}
	if n, ok := p.Networks["back"]; !ok || bool(n.External) {
		t.Errorf("network back = %+v, want a network the project owns", n)
	}
	if v, ok := p.Volumes["data"]; !ok || bool(v.External) {
		t.Errorf("volume data = %+v, want a volume the project owns", v)
	}
	if got := res.Pins["web"]; got != "nginx:1.27@"+stackopstest.Digest("a") {
		t.Errorf("web pin = %q, want the image's repo digest", got)
	}
	// The services come back as the SDK would see them: the config hash of each, which the containers carry, is the
	// hash of the service as the file now says it, so a deploy of the file recreates nothing.
	ids := map[string][]string{}
	for _, c := range h.Containers {
		ids[c.Labels[stacks.LabelComposeService]] = append(ids[c.Labels[stacks.LabelComposeService]], c.ID)
	}
	for name, svc := range p.Services {
		got, err := dockerx.ServiceHash(stacks.ResolveServiceRefs(svc, ids))
		if err != nil {
			t.Fatalf("hash %s: %v", name, err)
		}
		if got != h.Hash[name] {
			t.Errorf("service %s hashes to %s, the containers carry %s: the file would recreate it\n%s", name, got, h.Hash[name], res.YAML)
		}
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %+v, want none", res.Notes)
	}
}

func TestStandaloneOmitsDaemonDefaults(t *testing.T) {
	t.Parallel()
	h := stackopstest.Foreign(t, "shop", "services:\n  web:\n    image: nginx:1.27\n    mem_limit: 256m\n")
	// The daemon's default ulimit is on every container; one the file set is the container's own.
	doc := h.Docs["web"]
	doc.HostConfig.Ulimits = []*container.Ulimit{
		{Name: "nofile", Soft: 1048576, Hard: 1048576}, {Name: "nproc", Soft: 65535, Hard: 65535},
	}
	zero := int64(0)
	doc.HostConfig.PidsLimit = &zero

	res, err := rebuild.Standalone(hostInputOf(t, h))
	if err != nil {
		t.Fatalf("Standalone: %v", err)
	}

	text := string(res.YAML)
	for _, unwanted := range []string{"logging", "ipc", "shm_size", "memswap_limit", "pids_limit", "nofile", "container_name", "hostname"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the file has %q, which the daemon fills in:\n%s", unwanted, text)
		}
	}
	for _, want := range []string{"mem_limit: 256M", "nproc"} {
		if !strings.Contains(text, want) {
			t.Errorf("the file lacks %q, which the container sets:\n%s", want, text)
		}
	}
}

func TestStandalonePinsFromRepoDigests(t *testing.T) {
	t.Parallel()
	h := stackopstest.Foreign(t, "shop", "services:\n  web:\n    image: nginx:1.27\n  api:\n    image: shop-api\n")
	for i := range h.Images {
		if h.Images[i].RepoTags[0] == "shop-api" {
			h.Images[i].RepoDigests = nil // built on this host: no registry has it
		}
	}
	in := hostInputOf(t, h)

	res, err := rebuild.Standalone(in)
	if err != nil {
		t.Fatalf("Standalone: %v", err)
	}

	if got, want := res.Pins["web"], "nginx:1.27@"+stackopstest.Digest("a"); got != want {
		t.Errorf("web pin = %q, want %q", got, want)
	}
	var apiID string
	for _, fi := range h.Images {
		if fi.RepoTags[0] == "shop-api" {
			apiID = fi.ID
		}
	}
	if got := res.Pins["api"]; apiID == "" || got != apiID {
		t.Errorf("api pin = %q, want the image ID %q", got, apiID)
	}
	if !strings.Contains(string(res.YAML), "image: shop-api\n") {
		t.Errorf("the file lacks the image text of api:\n%s", res.YAML)
	}
	var local []string
	for _, n := range res.Notes {
		if n.Code == rebuild.NoteImageLocal {
			local = append(local, n.Service)
		}
	}
	if !slices.Equal(local, []string{"api"}) {
		t.Errorf("image_local notes for %v, want api only: %+v", local, res.Notes)
	}
}

// TestStandaloneOwnedAndExternalObjects: networks and volumes with the project's label are the project's, under
// their existing names; the others are external, with their names (D-18).
func TestStandaloneOwnedAndExternalObjects(t *testing.T) {
	t.Parallel()
	h := stackopstest.Foreign(t, "shop", `
services:
  web:
    image: nginx:1.27
    networks: [back, proxy]
    volumes:
      - data:/data
      - shared:/shared
networks:
  back: {}
  proxy:
    external: true
    name: edge-proxy
volumes:
  data: {}
  shared:
    external: true
    name: team-shared
`)

	res, err := rebuild.Standalone(hostInputOf(t, h))
	if err != nil {
		t.Fatalf("Standalone: %v", err)
	}

	p := loadStandalone(t, "shop", res.YAML)
	if n := p.Networks["back"]; bool(n.External) || n.Name != "shop_back" {
		t.Errorf("network back = %+v, want one the project owns (shop_back)", n)
	}
	proxy, ok := p.Networks["edge-proxy"]
	if !ok || !bool(proxy.External) || proxy.Name != "edge-proxy" {
		t.Errorf("networks = %+v, want edge-proxy external under its own name", p.Networks)
	}
	if v := p.Volumes["data"]; bool(v.External) || v.Name != "shop_data" {
		t.Errorf("volume data = %+v, want one the project owns (shop_data)", v)
	}
	shared, ok := p.Volumes["team-shared"]
	if !ok || !bool(shared.External) || shared.Name != "team-shared" {
		t.Errorf("volumes = %+v, want team-shared external under its own name", p.Volumes)
	}
	var objects []string
	for _, o := range res.Objects {
		objects = append(objects, o.Kind+":"+o.Name+":"+o.Treatment)
	}
	want := []string{
		"network:edge-proxy:external", "network:shop_back:owned", "volume:shop_data:owned", "volume:team-shared:external",
	}
	if !slices.Equal(objects, want) {
		t.Errorf("objects = %v, want %v", objects, want)
	}
	web := p.Services["web"]
	if _, ok := web.Networks["edge-proxy"]; !ok {
		t.Errorf("web networks = %v, want it on the external network by its key", web.Networks)
	}
}

// TestStandaloneSkipsOneOffContainers: a compose run container is not a service.
func TestStandaloneSkipsOneOffContainers(t *testing.T) {
	t.Parallel()
	h := stackopstest.Foreign(t, "shop", "services:\n  web:\n    image: nginx:1.27\n")
	oneOff := h.Containers[0]
	doc := *oneOff.Inspect.Document
	doc.ID = "oneoff"
	labels := map[string]string{}
	for k, v := range doc.Config.Labels {
		labels[k] = v
	}
	labels["com.docker.compose.oneoff"] = "True"
	cfg := *doc.Config
	cfg.Labels = labels
	cfg.Env = append(cfg.Env, "ONE_OFF=1")
	doc.Config = &cfg
	in := hostInputOf(t, h)
	in.Containers = append(in.Containers, rebuild.StandaloneContainer{Inspect: doc, Image: in.Containers[0].Image})

	res, err := rebuild.Standalone(in)
	if err != nil {
		t.Fatalf("Standalone: %v", err)
	}

	if strings.Contains(string(res.YAML), "ONE_OFF") {
		t.Errorf("the file took its settings from a one-off container:\n%s", res.YAML)
	}
}
