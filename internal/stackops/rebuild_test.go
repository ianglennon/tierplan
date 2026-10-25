// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/rebuild"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

const rebuildDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// foreignSpec is a service as docker stack deploy leaves it, holding a value a Viewer must not read in its
// environment, its command and its log options.
func foreignSpec(ns, short string) swarm.ServiceSpec {
	replicas := uint64(2)
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: ns + "_" + short, Labels: map[string]string{
			stacks.LabelSwarmNamespace: ns, stacks.LabelSwarmImage: "nginx:1.27",
		}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   "nginx:1.27@" + rebuildDigest,
				Labels:  map[string]string{stacks.LabelSwarmNamespace: ns},
				Env:     []string{"DB_PASSWORD=s3cret-value", "MODE=prod"},
				Command: []string{"/entry.sh", "--token", "cmd-secret"},
				Secrets: []*swarm.SecretReference{{
					SecretID: "sec-1", SecretName: ns + "_pw",
					File: &swarm.SecretReferenceFileTarget{Name: "pw", UID: "0", GID: "0", Mode: 0o444},
				}},
			},
			Networks:  []swarm.NetworkAttachmentConfig{{Target: "net-default", Aliases: []string{short}}},
			LogDriver: &swarm.Driver{Name: "splunk", Options: map[string]string{"splunk-token": "log-secret"}},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}
}

func foreignRig(t *testing.T) *swarmRig {
	t.Helper()
	spec := foreignSpec("shop", "web")
	ns := map[string]string{stacks.LabelSwarmNamespace: "shop"}
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "net-default", Name: "shop_default", Driver: "overlay", Scope: "swarm", Labels: ns}}
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-1", Name: "shop_pw", Labels: ns}}
	}, dockertest.FakeService{
		ID: "svc-shop-web", Name: "shop_web", Mode: "replicated", Running: 2, Desired: 2, Labels: spec.Labels, Spec: &spec,
	})
}

// secretOrConfigRead matches a request for one secret or config, the reads that carry data.
var secretOrConfigRead = regexp.MustCompile(`/(secrets|configs)/[^/?]+`)

func TestRebuildLiveMasksForAViewer(t *testing.T) {
	t.Parallel()
	rig := foreignRig(t)
	st := stackops.DiscoveredStack(rig.envID, "shop", stacks.TargetSwarm)

	hidden, err := rig.deps.RebuildLive(testsupport.Context(t), rig.envID, st, stackops.RebuildOptions{Reveal: false})
	if err != nil {
		t.Fatalf("RebuildLive without reveal: %v", err)
	}
	shown, err := rig.deps.RebuildLive(testsupport.Context(t), rig.envID, st, stackops.RebuildOptions{Reveal: true})
	if err != nil {
		t.Fatalf("RebuildLive with reveal: %v", err)
	}

	for _, secret := range []string{"s3cret-value", "cmd-secret", "log-secret"} {
		if strings.Contains(string(hidden.YAML), secret) {
			t.Errorf("the preview for a viewer carries %q:\n%s", secret, hidden.YAML)
		}
		if !strings.Contains(string(shown.YAML), secret) {
			t.Errorf("the preview for a caller who may reveal lacks %q:\n%s", secret, shown.YAML)
		}
	}
	if !strings.Contains(string(hidden.YAML), "<hidden>") {
		t.Errorf("the masked preview does not say what it hides:\n%s", hidden.YAML)
	}
	if !strings.Contains(string(hidden.YAML), "MODE") {
		t.Errorf("the masked preview lost the names of the environment:\n%s", hidden.YAML)
	}
	// D-13: the names of the secrets and configs are read, never their data.
	for _, req := range rig.fake.Requests() {
		if secretOrConfigRead.MatchString(req) {
			t.Errorf("the rebuild asked for %s: a secret or a config was inspected", req)
		}
	}
	if got := hidden.Pins["shop_web"]; got != "nginx:1.27@"+rebuildDigest {
		t.Errorf("pin = %q, want the full image of the running spec", got)
	}
}

// objectsSpec is a service that uses two networks, two volumes, a secret and a config: one of each kind carries
// the stack's label, the others are the swarm's own or another stack's.
func objectsSpec() swarm.ServiceSpec {
	spec := foreignSpec("shop", "web")
	cs := spec.TaskTemplate.ContainerSpec
	cs.Mounts = []mount.Mount{
		{Type: mount.TypeVolume, Source: "shop_data", Target: "/data", VolumeOptions: &mount.VolumeOptions{
			Labels: map[string]string{stacks.LabelSwarmNamespace: "shop", "purpose": "db"},
		}},
		{Type: mount.TypeVolume, Source: "shared-vol", Target: "/shared"},
		{Type: mount.TypeBind, Source: "/srv/conf", Target: "/conf", ReadOnly: true},
	}
	cs.Configs = []*swarm.ConfigReference{{
		ConfigID: "cfg-1", ConfigName: "shop_conf",
		File: &swarm.ConfigReferenceFileTarget{Name: "/etc/app.conf", UID: "1000", GID: "1000", Mode: 0o440},
	}}
	spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{
		{Target: "net-default", Aliases: []string{"web"}},
		{Target: "net-back", Aliases: []string{"web", "api"}},
		{Target: "net-shared"},
	}
	return spec
}

func objectsRig(t *testing.T) *swarmRig {
	t.Helper()
	spec := objectsSpec()
	ns := map[string]string{stacks.LabelSwarmNamespace: "shop"}
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{
			{ID: "net-default", Name: "shop_default", Driver: "overlay", Scope: "swarm", Labels: ns},
			{
				ID: "net-back", Name: "shop_back", Driver: "overlay", Scope: "swarm", Labels: ns, Attachable: true,
				Subnets: []string{"10.9.0.0/24"}, Gateways: []string{"10.9.0.1"},
				Options: map[string]string{"com.docker.network.driver.mtu": "1400", "com.docker.network.driver.overlay.vxlanid_list": "4097"},
			},
			{ID: "net-shared", Name: "shared", Driver: "overlay", Scope: "swarm"},
		}
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-1", Name: "shop_pw", Labels: ns}}
		c.Configs = []dockertest.FakeConfigObject{{ID: "cfg-1", Name: "shop_conf", Labels: ns, Data: []byte("conf-content-never-read")}}
	}, dockertest.FakeService{
		ID: "svc-shop-web", Name: "shop_web", Mode: "replicated", Running: 2, Desired: 2, Labels: spec.Labels, Spec: &spec,
	})
}

// TestRebuildLiveOwnedAndExternalObjects is D-13 and D-18: a network or a volume that carries the stack's label is the
// stack's own, written under its existing name; any other is external, with its name; a secret and a config are
// always external, by name.
func TestRebuildLiveOwnedAndExternalObjects(t *testing.T) {
	t.Parallel()
	rig := objectsRig(t)
	st := stackops.DiscoveredStack(rig.envID, "shop", stacks.TargetSwarm)

	res, err := rig.deps.RebuildLive(testsupport.Context(t), rig.envID, st, stackops.RebuildOptions{Reveal: true})
	if err != nil {
		t.Fatalf("RebuildLive: %v", err)
	}

	var got []string
	for _, o := range res.Objects {
		got = append(got, o.Kind+" "+o.Name+" "+o.Treatment)
	}
	want := []string{
		"config shop_conf external",
		"network shared external",
		"network shop_back owned",
		"network shop_default owned",
		"secret shop_pw external",
		"volume shared-vol external",
		"volume shop_data owned",
	}
	if !slices.Equal(got, want) {
		t.Errorf("objects = %v, want %v", got, want)
	}
	text := string(res.YAML)
	for _, wantText := range []string{"back:", "shared:", "external: true", "name: shop_pw", "name: shop_conf", "name: shared-vol", "10.9.0.0/24", "com.docker.network.driver.mtu", "purpose: db"} {
		if !strings.Contains(text, wantText) {
			t.Errorf("the file lacks %q:\n%s", wantText, text)
		}
	}
	for _, unwanted := range []string{"vxlanid", "conf-content-never-read", "com.docker.stack"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the file has %q:\n%s", unwanted, text)
		}
	}
	if shop := res.Project.Networks["back"]; bool(shop.External) || !shop.Attachable {
		t.Errorf("back = %+v, want the stack's own attachable network", shop)
	}
	if data := res.Project.Volumes["data"]; bool(data.External) || data.Name != "" {
		t.Errorf("data = %+v, want the stack's own volume under the default name", data)
	}
	for _, req := range rig.fake.Requests() {
		if secretOrConfigRead.MatchString(req) {
			t.Errorf("the rebuild asked for %s: a secret or a config was inspected", req)
		}
	}
}

// foreignProject is a docker compose up project whose services hold values a Viewer must not read.
const foreignProject = `
services:
  web:
    image: nginx:1.27
    environment:
      DB_PASSWORD: s3cret-value
      MODE: prod
    command: ["/entry.sh", "--token", "cmd-secret"]
    logging:
      driver: syslog
      options:
        syslog-address: tcp://log-secret:514
    networks: [back]
networks:
  back: {}
`

// TestRebuildLiveStandaloneMasksForAViewer: a standalone project is rebuilt from its containers, and a caller
// without secrets.reveal gets <hidden> for every value the containers hold (T-08-44, D-47).
func TestRebuildLiveStandaloneMasksForAViewer(t *testing.T) {
	t.Parallel()
	rig := hostRig(t, stackopstest.Foreign(t, "shop", foreignProject))
	st := stackops.DiscoveredStack(rig.envID, "shop", stacks.TargetStandalone)

	hidden, err := rig.deps.RebuildLive(testsupport.Context(t), rig.envID, st, stackops.RebuildOptions{Reveal: false})
	if err != nil {
		t.Fatalf("RebuildLive without reveal: %v", err)
	}
	shown, err := rig.deps.RebuildLive(testsupport.Context(t), rig.envID, st, stackops.RebuildOptions{Reveal: true})
	if err != nil {
		t.Fatalf("RebuildLive with reveal: %v", err)
	}

	for _, secret := range []string{"s3cret-value", "cmd-secret", "log-secret"} {
		if strings.Contains(string(hidden.YAML), secret) {
			t.Errorf("the preview for a viewer carries %q:\n%s", secret, hidden.YAML)
		}
		if !strings.Contains(string(shown.YAML), secret) {
			t.Errorf("the preview for a caller who may reveal lacks %q:\n%s", secret, shown.YAML)
		}
	}
	if !strings.Contains(string(hidden.YAML), "<hidden>") || !strings.Contains(string(hidden.YAML), "DB_PASSWORD") {
		t.Errorf("the masked preview does not say what it hides, or lost the names of the environment:\n%s", hidden.YAML)
	}
	for _, text := range []string{string(hidden.YAML), string(shown.YAML)} {
		if strings.Contains(text, "IMAGE_REF") || strings.Contains(text, "/bin/run") {
			t.Errorf("the image's own environment or command is in the file:\n%s", text)
		}
	}
	if writes := rig.fake.Writes(); len(writes) != 0 {
		t.Errorf("the rebuild wrote to the engine: %v", writes)
	}
	if got := shown.Pins["web"]; got != "nginx:1.27@"+stackopstest.Digest("a") {
		t.Errorf("pin = %q, want the repo digest of the image the container runs", got)
	}
}

// TestRebuildLiveStandaloneOfAProjectWithNoContainers is a rebuild error, not an empty file.
func TestRebuildLiveStandaloneOfAProjectWithNoContainers(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	st := stackops.DiscoveredStack(rig.envID, "shop", stacks.TargetStandalone)

	_, err := rig.deps.RebuildLive(testsupport.Context(t), rig.envID, st, stackops.RebuildOptions{})

	var failed *stackops.RebuildError
	if !errors.As(err, &failed) {
		t.Fatalf("RebuildLive of a project with no container: %v, want a *RebuildError", err)
	}
}

const configHashLabel = "com.docker.compose.config-hash"

// hashRebuild rebuilds a Foreign project after the test has changed what the host holds.
func hashRebuild(t *testing.T, h *stackopstest.Hosted) rebuild.Result {
	t.Helper()
	rig := hostRig(t, h)
	st := stackops.DiscoveredStack(rig.envID, h.Name, stacks.TargetStandalone)
	res, err := rig.deps.RebuildLive(testsupport.Context(t), rig.envID, st, stackops.RebuildOptions{Reveal: true})
	if err != nil {
		t.Fatalf("RebuildLive: %v", err)
	}
	return res
}

// preparedProject is a rebuilt file loaded and prepared the way a deploy does it.
func preparedProject(t *testing.T, name string, res rebuild.Result) *types.Project {
	t.Helper()
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: res.YAML}}}
	loaded, err := stacks.Deployable(testsupport.Context(t), stacks.CheckInput{Name: name, Target: stacks.TargetStandalone, Copy: copy})
	if err != nil {
		t.Fatalf("Deployable: %v\n%s", err, res.YAML)
	}
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, loaded.Project, stacks.NewFileSet(copy.Files), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stacks.PrepareStandalone(loaded.Project, "stack-"+name, res.Pins, objs); err != nil {
		t.Fatalf("PrepareStandalone: %v", err)
	}
	return loaded.Project
}

func labelOfNetwork(h *stackopstest.Hosted, name string) *dockertest.FakeNetwork {
	for i := range h.Networks {
		if h.Networks[i].Name == name {
			return &h.Networks[i]
		}
	}
	return nil
}

func labelOfVolume(h *stackopstest.Hosted, name string) *dockertest.FakeVolume {
	for i := range h.Volumes {
		if h.Volumes[i].Name == name {
			return &h.Volumes[i]
		}
	}
	return nil
}

// TestRebuildMatchesNetworkAndVolumeHashes: the rebuilt file declares an existing network or volume only in the
// form whose hash is the one the host holds, or references it as external, so the first deploy of an adopted
// project recreates nothing and a volume's data is never at risk (STK-13, D-46, D-18, T-08-43).
func TestRebuildMatchesNetworkAndVolumeHashes(t *testing.T) {
	t.Parallel()
	const service = "  web:\n    image: nginx:1.27\n    networks: [back]\n    volumes: [data:/data]\n"

	// ownedFormsMatch holds the rebuilt file against the host: every object it owns hashes, as the SDK hashes
	// it on a project loaded and prepared as a deploy does, to the label the host holds.
	ownedFormsMatch := func(t *testing.T, h *stackopstest.Hosted, res rebuild.Result) {
		t.Helper()
		p := preparedProject(t, h.Name, res)
		for key, n := range p.Networks {
			if bool(n.External) {
				continue
			}
			want := labelOfNetwork(h, n.Name).Labels[configHashLabel]
			if got, err := dockerx.ComposeNetworkHash(&n); err != nil || got != want {
				t.Errorf("network %s hashes to %s (%v), the host holds %s:\n%s", key, got, err, want, res.YAML)
			}
		}
		for key, v := range p.Volumes {
			if bool(v.External) {
				continue
			}
			want := labelOfVolume(h, v.Name).Labels[configHashLabel]
			if got, err := dockerx.ComposeVolumeHash(v); err != nil || got != want {
				t.Errorf("volume %s hashes to %s (%v), the host holds %s:\n%s", key, got, err, want, res.YAML)
			}
		}
	}

	t.Run("the minimal rendering is written when it is the one", func(t *testing.T) {
		t.Parallel()
		h := stackopstest.Foreign(t, "shop", "services:\n"+service+"networks:\n  back: {}\nvolumes:\n  data: {}\n")

		res := hashRebuild(t, h)

		ownedFormsMatch(t, h, res)
		p := preparedProject(t, "shop", res)
		if n := p.Networks["back"]; bool(n.External) || n.Driver != "" || len(n.DriverOpts) != 0 {
			t.Errorf("network back = %+v, want the minimal rendering", n)
		}
		for _, o := range res.Objects {
			if o.Treatment != rebuild.TreatmentOwned {
				t.Errorf("object %+v is not owned", o)
			}
		}
	})

	t.Run("an explicit driver is written when the hash says the file had it", func(t *testing.T) {
		t.Parallel()
		h := stackopstest.Foreign(t, "shop", "services:\n"+service+"networks:\n  back:\n    driver: bridge\nvolumes:\n  data: {}\n")

		res := hashRebuild(t, h)

		ownedFormsMatch(t, h, res)
		if n := preparedProject(t, "shop", res).Networks["back"]; bool(n.External) || n.Driver != "bridge" {
			t.Errorf("network back = %+v, want one that owns its driver bridge", n)
		}
	})

	t.Run("driver options and address pools are written when the hash says the file had them", func(t *testing.T) {
		t.Parallel()
		h := stackopstest.Foreign(t, "shop", "services:\n"+service+`networks:
  back:
    driver_opts:
      com.docker.network.bridge.name: br-shop
    ipam:
      config:
        - subnet: 172.28.0.0/16
          gateway: 172.28.0.1
    labels:
      team: web
volumes:
  data:
    driver_opts:
      type: tmpfs
      device: tmpfs
    labels:
      team: db
`)

		res := hashRebuild(t, h)

		ownedFormsMatch(t, h, res)
		p := preparedProject(t, "shop", res)
		if n := p.Networks["back"]; bool(n.External) || n.DriverOpts["com.docker.network.bridge.name"] != "br-shop" ||
			len(n.Ipam.Config) != 1 || n.Ipam.Config[0].Gateway != "172.28.0.1" || n.Labels["team"] != "web" {
			t.Errorf("network back = %+v, want its options, pool and label", n)
		}
		if v := p.Volumes["data"]; bool(v.External) || v.DriverOpts["type"] != "tmpfs" || v.Labels["team"] != "db" {
			t.Errorf("volume data = %+v, want its options and label", v)
		}
	})

	t.Run("an address pool with a subnet only is written with the subnet only", func(t *testing.T) {
		t.Parallel()
		h := stackopstest.Foreign(t, "shop", "services:\n"+service+"networks:\n  back:\n    ipam:\n      config:\n        - subnet: 172.28.0.0/16\nvolumes:\n  data: {}\n")

		res := hashRebuild(t, h)

		ownedFormsMatch(t, h, res)
		if n := preparedProject(t, "shop", res).Networks["back"]; bool(n.External) || len(n.Ipam.Config) != 1 || n.Ipam.Config[0].Gateway != "" {
			t.Errorf("network back = %+v, want a pool with its subnet only", n)
		}
	})

	t.Run("an object whose hash no rendering gives is external, with a note", func(t *testing.T) {
		t.Parallel()
		h := stackopstest.Foreign(t, "shop", "services:\n"+service+"networks:\n  back: {}\nvolumes:\n  data: {}\n")
		labelOfNetwork(h, "shop_back").Labels[configHashLabel] = strings.Repeat("0", 64)
		labelOfVolume(h, "shop_data").Labels[configHashLabel] = strings.Repeat("1", 64)

		res := hashRebuild(t, h)

		p := preparedProject(t, "shop", res)
		if n := p.Networks["back"]; !bool(n.External) || n.Name != "shop_back" {
			t.Errorf("network back = %+v, want an external network named shop_back", n)
		}
		if v := p.Volumes["data"]; !bool(v.External) || v.Name != "shop_data" {
			t.Errorf("volume data = %+v, want an external volume named shop_data", v)
		}
		var noted []string
		for _, n := range res.Notes {
			if n.Code == rebuild.NoteObjectExternalHash {
				noted = append(noted, n.Service)
			}
		}
		if !slices.Equal(noted, []string{"shop_back", "shop_data"}) {
			t.Errorf("object_external_hash notes for %v, want shop_back and shop_data: %+v", noted, res.Notes)
		}
		for _, o := range res.Objects {
			if o.Treatment != rebuild.TreatmentExternal {
				t.Errorf("object %+v is not external", o)
			}
		}
	})

	t.Run("an object with no hash label keeps the first rendering", func(t *testing.T) {
		t.Parallel()
		h := stackopstest.Foreign(t, "shop", "services:\n"+service+"networks:\n  back:\n    driver: bridge\nvolumes:\n  data: {}\n")
		delete(labelOfNetwork(h, "shop_back").Labels, configHashLabel)
		delete(labelOfVolume(h, "shop_data").Labels, configHashLabel)

		res := hashRebuild(t, h)

		p := preparedProject(t, "shop", res)
		if n := p.Networks["back"]; bool(n.External) || n.Driver != "" {
			t.Errorf("network back = %+v, want the first rendering: the SDK leaves an object with no hash alone", n)
		}
		if v := p.Volumes["data"]; bool(v.External) {
			t.Errorf("volume data = %+v, want an owned volume", v)
		}
		if len(res.Notes) != 0 {
			t.Errorf("notes = %+v, want none", res.Notes)
		}
	})

	t.Run("the project's default network is left implicit when it is the plain one", func(t *testing.T) {
		t.Parallel()
		h := stackopstest.Foreign(t, "shop", "services:\n  web:\n    image: nginx:1.27\n")

		res := hashRebuild(t, h)

		ownedFormsMatch(t, h, res)
		if strings.Contains(string(res.YAML), "networks") {
			t.Errorf("the file declares the network a project gets without declaring one:\n%s", res.YAML)
		}
		var owned []string
		for _, o := range res.Objects {
			owned = append(owned, o.Kind+":"+o.Name+":"+o.Treatment)
		}
		if !slices.Equal(owned, []string{"network:shop_default:owned"}) {
			t.Errorf("objects = %v, want the default network, owned", owned)
		}
	})

	t.Run("a default network the hash does not match is declared external, and the service names it", func(t *testing.T) {
		t.Parallel()
		h := stackopstest.Foreign(t, "shop", "services:\n  web:\n    image: nginx:1.27\n")
		labelOfNetwork(h, "shop_default").Labels[configHashLabel] = strings.Repeat("0", 64)

		res := hashRebuild(t, h)

		p := preparedProject(t, "shop", res)
		if n := p.Networks["default"]; !bool(n.External) || n.Name != "shop_default" {
			t.Errorf("network default = %+v, want an external network named shop_default:\n%s", n, res.YAML)
		}
		if _, on := p.Services["web"].Networks["default"]; !on {
			t.Errorf("web networks = %v, want it on the default network", p.Services["web"].Networks)
		}
	})
}
