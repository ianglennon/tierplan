// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
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
