// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stackops/stackopstest"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/convert"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The adopt check (plan 08-14, D-16, D-20): the review of the rebuilt definition against what runs. An ownership
// label is the one difference the first deploy makes by itself, so it is counted apart; every other difference is a
// gap, shown before the person adopts.

// corpusObjects names the secrets and configs of the converter's corpus the way its own tests do: the swarm object
// of a reference <name> is <name>.h1 (a secret) or <name>.h2 (a config), with the ID sid_ or cid_.
type corpusObjects struct{}

func (corpusObjects) Secret(name string) (string, string, bool) {
	return "sid_" + name, name + ".h1", name == "db_password" || name == "tls_key"
}

func (corpusObjects) Config(name string) (string, string, bool) {
	return "cid_" + name, name + ".h2", name == "app_config" || name == "nginx_conf"
}

// withoutCoamingLabels removes Coaming's labels from a map: docker stack deploy puts none on what it makes.
func withoutCoamingLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if !strings.HasPrefix(k, "coaming.") {
			out[k] = v
		}
	}
	return out
}

// corpusRig stands the converted corpus file (internal/stacks/convert/testdata/compose/<file>) on a fake swarm as
// docker stack deploy would leave it under the namespace shop: its services, networks, secrets and configs, with the
// namespace labels and none of Coaming's. edit changes each service's spec before it is stood up.
func corpusRig(t *testing.T, file string, edit func(short string, spec *swarm.ServiceSpec)) *swarmRig {
	t.Helper()
	return corpusRigTuned(t, file, edit, nil)
}

// corpusRigTuned is corpusRig with a last say on the fake's configuration.
func corpusRigTuned(t *testing.T, file string, edit func(short string, spec *swarm.ServiceSpec), tune func(*dockertest.FakeConfig)) *swarmRig {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "stacks", "convert", "testdata", "compose", file)) //nolint:gosec // a fixed corpus path
	if err != nil {
		t.Fatal(err)
	}
	return composeRigTuned(t, content, edit, tune)
}

// composeRig stands the converted compose text on a fake swarm as docker stack deploy would leave it (see corpusRig).
func composeRig(t *testing.T, content []byte, edit func(short string, spec *swarm.ServiceSpec)) *swarmRig {
	t.Helper()
	return composeRigTuned(t, content, edit, nil)
}

// composeRigTuned is composeRig with a last say on the fake's configuration, after the stack's objects are in it.
func composeRigTuned(t *testing.T, content []byte, edit func(short string, spec *swarm.ServiceSpec), tune func(*dockertest.FakeConfig)) *swarmRig {
	t.Helper()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: "shop", Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: content}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := convert.Convert(loaded.Project, convert.Options{StackName: "shop", StackID: "gone", Objects: corpusObjects{}})
	if err != nil {
		t.Fatal(err)
	}
	ns := map[string]string{stacks.LabelSwarmNamespace: "shop"}
	var services []dockertest.FakeService
	for _, sp := range plan.Services {
		spec := sp.Spec
		spec.Labels = withoutCoamingLabels(spec.Labels)
		cs := *spec.TaskTemplate.ContainerSpec
		cs.Labels = withoutCoamingLabels(cs.Labels)
		cs.Image += "@" + rebuildDigest
		spec.TaskTemplate.ContainerSpec = &cs
		spec.TaskTemplate.Networks = slices.Clone(spec.TaskTemplate.Networks)
		for i, n := range spec.TaskTemplate.Networks {
			spec.TaskTemplate.Networks[i].Target = "net-" + n.Target
		}
		if edit != nil {
			edit(sp.Short, &spec)
		}
		mode := "replicated"
		if spec.Mode.Global != nil {
			mode = "global"
		}
		services = append(services, dockertest.FakeService{
			ID: "svc-" + sp.Name, Name: sp.Name, Mode: mode, Running: 1, Desired: 1, Labels: spec.Labels, Spec: &spec,
		})
	}
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		for _, n := range plan.Networks {
			c.Networks = append(c.Networks, dockertest.FakeNetwork{
				ID: "net-" + n.Name, Name: n.Name, Driver: n.Driver, Scope: "swarm", Labels: withoutCoamingLabels(n.Labels),
				Attachable: n.Attachable, Internal: n.Internal,
			})
		}
		for _, s := range []string{"db_password", "tls_key"} {
			c.Secrets = append(c.Secrets, dockertest.FakeSecret{ID: "sid_" + s, Name: s + ".h1", Labels: ns})
		}
		for _, s := range []string{"app_config", "nginx_conf"} {
			c.Configs = append(c.Configs, dockertest.FakeConfigObject{ID: "cid_" + s, Name: s + ".h2", Labels: ns, Data: []byte("never-read")})
		}
		if tune != nil {
			tune(c)
		}
	}, services...)
}

// checkOf runs the adopt check of the stack named shop on a rig.
func (r *swarmRig) checkOf(t *testing.T, target stacks.Target, reveal bool) (stackops.AdoptCheckResult, error) {
	t.Helper()
	managed, err := adoptRepo(t, r).Stacks(testsupport.Context(t), r.envID)
	if err != nil {
		t.Fatal(err)
	}
	return r.deps.AdoptCheck(testsupport.Context(t), stackops.AdoptInput{
		EnvID: r.envID, Name: "shop", Target: target, Caller: stackops.Caller{Actor: "ada"}, Managed: managed,
	}, reveal)
}

// TestAdoptReviewCleanRoundTrip: a stack docker stack deploy made, converted back from what it runs, differs from
// what runs only in Coaming's ownership labels: every service is ownership-only and the check is Clean (D-20).
func TestAdoptReviewCleanRoundTrip(t *testing.T) {
	t.Parallel()
	rig := corpusRig(t, "deploy.yaml", nil)

	res, err := rig.checkOf(t, stacks.TargetSwarm, true)
	if err != nil {
		t.Fatalf("AdoptCheck: %v", err)
	}

	if !res.Clean || len(res.Gaps) != 0 {
		var names []string
		for _, g := range res.Gaps {
			names = append(names, g.Short+" "+g.Change+" "+string(g.Current)+" -> "+string(g.Next))
		}
		t.Fatalf("Clean = %v with gaps %v, want a clean round trip", res.Clean, names)
	}
	if want := []string{"api", "bare", "legacy"}; !slices.Equal(res.OwnershipOnly, want) {
		t.Errorf("ownership-only services = %v, want %v", res.OwnershipOnly, want)
	}
	if res.Fingerprint == "" {
		t.Errorf("the check answers no fingerprint to adopt with")
	}
}

// strayRig is a stack with a service that is in the namespace but is not named for it, which the rebuild cannot
// write back under its own name: the file names it and the first deploy would make another service of that name.
func strayRig(t *testing.T) *swarmRig {
	t.Helper()
	spec := foreignSpec("shop", "web")
	spec.Name = "web"
	ns := map[string]string{stacks.LabelSwarmNamespace: "shop"}
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "net-default", Name: "shop_default", Driver: "overlay", Scope: "swarm", Labels: ns}}
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-1", Name: "shop_pw", Labels: ns}}
	}, dockertest.FakeService{
		ID: "svc-web", Name: "web", Mode: "replicated", Running: 2, Desired: 2, Labels: spec.Labels, Spec: &spec,
	})
}

// TestAdoptReviewListsGaps: a difference that is not an ownership label is listed, by service and kind, and the
// check is not Clean. The check still answers: gaps never block (D-20).
func TestAdoptReviewListsGaps(t *testing.T) {
	t.Parallel()
	rig := strayRig(t)

	res, err := rig.checkOf(t, stacks.TargetSwarm, true)
	if err != nil {
		t.Fatalf("AdoptCheck: %v", err)
	}

	if res.Clean {
		t.Fatalf("Clean = true with a service the first deploy would replace")
	}
	got := map[string]string{}
	for _, g := range res.Gaps {
		got[g.Name] = g.Change
	}
	if got["web"] != stacks.ChangeRemoved || got["shop_web"] != stacks.ChangeAdded {
		t.Errorf("gaps = %v, want web removed and shop_web added", got)
	}
	if len(res.OwnershipOnly) != 0 {
		t.Errorf("ownership-only = %v, want none: these services differ in more than a label", res.OwnershipOnly)
	}
}

// TestAdoptReviewStandaloneOwnershipOnly: a docker compose up project differs from its rebuilt definition only in the
// config hash its first deploy writes (Coaming's labels are in it), so its service is ownership-only.
func TestAdoptReviewStandaloneOwnershipOnly(t *testing.T) {
	t.Parallel()
	rig := hostRig(t, stackopstest.Foreign(t, "shop", foreignProject))

	res, err := rig.checkOf(t, stacks.TargetStandalone, true)
	if err != nil {
		t.Fatalf("AdoptCheck: %v", err)
	}

	if !res.Clean || len(res.Gaps) != 0 {
		t.Fatalf("Clean = %v with gaps %+v, want a clean round trip", res.Clean, res.Gaps)
	}
	if want := []string{"web"}; !slices.Equal(res.OwnershipOnly, want) {
		t.Errorf("ownership-only services = %v, want %v", res.OwnershipOnly, want)
	}
}

// legacy2 is a stack made by docker stack deploy with its default --resolve-image=always and a named volume. Its
// file is what the person wrote: the web image carries a variable, there is no label of Coaming's anywhere (G-08-4).
const legacy2Compose = `services:
  web:
    image: nginx:${TAG}
    ports:
      - "8080:80"
    networks:
      - front
  api:
    image: ghcr.io/example/legacy-api:2.3
    environment:
      LOG_LEVEL: info
    volumes:
      - apidata:/data
    networks:
      - front
networks:
  front:
volumes:
  apidata:
`

// legacy2Rig stands the stored specs of testdata/adopt/legacy2-services.json on a fake swarm: the engine's own
// /services answer for the stack legacy2, with the platforms docker stack deploy stored from the images it resolved
// and a named volume whose mount carries only the CLI's namespace label.
func legacy2Rig(t *testing.T) *swarmRig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "adopt", "legacy2-services.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored []swarm.Service
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("fixture holds %d services, want 2", len(stored))
	}
	// The fixture stays shaped like the CLI's output: none of Coaming's labels on a service, a container or a mount.
	var front string
	var services []dockertest.FakeService
	for _, s := range stored {
		spec := s.Spec
		for where, labels := range map[string]map[string]string{"service": spec.Labels, "container": spec.TaskTemplate.ContainerSpec.Labels} {
			for k := range labels {
				if strings.HasPrefix(k, "coaming.") {
					t.Fatalf("fixture %s %s label %q is one of Coaming's", spec.Name, where, k)
				}
			}
		}
		for _, m := range spec.TaskTemplate.ContainerSpec.Mounts {
			if m.VolumeOptions == nil {
				continue
			}
			for k := range m.VolumeOptions.Labels {
				if strings.HasPrefix(k, "coaming.") {
					t.Fatalf("fixture %s mount %s label %q is one of Coaming's", spec.Name, m.Target, k)
				}
			}
		}
		front = spec.TaskTemplate.Networks[0].Target
		services = append(services, dockertest.FakeService{
			ID: s.ID, Name: spec.Name, Mode: "replicated", Running: 1, Desired: 1, Labels: spec.Labels, Spec: &spec,
		})
	}
	ns := map[string]string{stacks.LabelSwarmNamespace: "legacy2"}
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: front, Name: "legacy2_front", Driver: "overlay", Scope: "swarm", Labels: ns})
	}, services...)
}

// legacy2Check runs the adopt check of legacy2: from what runs when pasted is nil, else with that file pasted back.
func (r *swarmRig) legacy2Check(t *testing.T, pasted *stacks.WorkingCopy) stackops.AdoptCheckResult {
	t.Helper()
	managed, err := adoptRepo(t, r).Stacks(testsupport.Context(t), r.envID)
	if err != nil {
		t.Fatal(err)
	}
	in := stackops.AdoptInput{
		EnvID: r.envID, Name: "legacy2", Target: stacks.TargetSwarm, Caller: stackops.Caller{Actor: "ada"}, Managed: managed,
	}
	if pasted != nil {
		in.Source, in.Pasted = stackops.AdoptSourcePaste, pasted
	}
	res, err := r.deps.AdoptCheck(testsupport.Context(t), in, true)
	if err != nil {
		t.Fatalf("AdoptCheck: %v", err)
	}
	return res
}

// gapPaths lists every differing path of the gaps, so a failure shows what is still read as a difference.
func gapPaths(t *testing.T, gaps []stacks.ServiceChange) []string {
	t.Helper()
	var out []string
	for _, g := range gaps {
		var cur, next map[string]any
		if err := json.Unmarshal(g.Current, &cur); err != nil {
			cur = nil
		}
		if err := json.Unmarshal(g.Next, &next); err != nil {
			next = nil
		}
		for _, p := range stacks.ChangedPaths(cur, next) {
			out = append(out, g.Short+": "+p.Path)
		}
	}
	return out
}

// TestAdoptCheckOfADockerStackDeployStackWithPlatformsAndANamedVolume (G-08-4): the stack docker stack deploy made
// with its default --resolve-image=always holds the platforms of each image, and its named volume's mount carries no
// label of Coaming's. Neither is something a compose file can say, so the check of what runs and the check of the
// file the stack was made from are both clean: the services differ in nothing but the ownership labels (D-12, D-20).
func TestAdoptCheckOfADockerStackDeployStackWithPlatformsAndANamedVolume(t *testing.T) {
	t.Parallel()
	rig := legacy2Rig(t)

	report := func(t *testing.T, res stackops.AdoptCheckResult) {
		t.Helper()
		if !res.Clean || len(res.Gaps) != 0 {
			var settings int
			for _, g := range res.Gaps {
				settings += g.Settings
			}
			t.Fatalf("Clean = %v with %d gap(s) of %d settings:\n%s", res.Clean, len(res.Gaps), settings, strings.Join(gapPaths(t, res.Gaps), "\n"))
		}
		if want := []string{"api", "web"}; !slices.Equal(res.OwnershipOnly, want) {
			t.Errorf("ownership-only services = %v, want %v", res.OwnershipOnly, want)
		}
	}

	t.Run("rebuilt from what runs", func(t *testing.T) {
		res := rig.legacy2Check(t, nil)
		report(t, res)
	})

	t.Run("pasted file", func(t *testing.T) {
		pasted := composeOnly(legacy2Compose, stacks.Var{Name: "TAG", Value: "1.27-alpine"})
		res := rig.legacy2Check(t, &pasted)
		report(t, res)
		if !res.Matches {
			t.Errorf("Matches = false, want the file the stack was made from to match what runs")
		}
	})
}
