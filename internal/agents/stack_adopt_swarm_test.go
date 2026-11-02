// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The adoption oracle (plan 08-26, STK-13, D-20, ROADMAP SC2 and SC3): a stack made by the real
// `docker stack deploy` is listed by TierPlan as unmanaged, adopted without a write to the engine, and
// then deployed and removed by TierPlan with nothing but its ownership labels changed.

// adoptCheckBody is the part of the answer of POST .../unmanaged-stacks/{name}/adopt-check these tests read.
type adoptCheckBody struct {
	Fingerprint string `json:"fingerprint"`
	Clean       bool   `json:"clean"`
	GapCount    int    `json:"gapCount"`
	Gaps        []struct {
		Name, Short   string
		Current, Next map[string]any
		HiddenChanges []string `json:"hiddenChanges"`
	} `json:"gaps"`
	OwnershipOnly []string `json:"ownershipOnly"`
}

// adoptCheck asks what adopting the unmanaged stack would change on its first deploy.
func adoptCheck(t *testing.T, f *swarmFix, name string) adoptCheckBody {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/unmanaged-stacks/"+name+"/adopt-check", map[string]any{})
	var out adoptCheckBody
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("POST the adopt check of %s = %d %s", name, code, body)
	}
	return out
}

// adopt adopts the unmanaged stack as the check read it and returns the ID of the stack TierPlan holds now.
func adopt(t *testing.T, f *swarmFix, name, fingerprint string) string {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/unmanaged-stacks/"+name+"/adopt", map[string]any{"fingerprint": fingerprint})
	var st struct {
		ID      string `json:"id"`
		Managed bool   `json:"managed"`
		Source  string `json:"source"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &st) != nil || st.ID == "" {
		t.Fatalf("POST the adoption of %s = %d %s", name, code, body)
	}
	if st.Source != "adopted" {
		t.Errorf("the adopted stack %s has source %q, want adopted", name, st.Source)
	}
	return st.ID
}

// waitUnmanaged waits until the environment's stack list shows the stack as one TierPlan did not make, and
// returns once it does. The list reads the engine through the environment's cache, so it is refreshed first.
func waitUnmanaged(t *testing.T, f *swarmFix, name string) {
	t.Helper()
	testsupport.Poll(t, stackGoneWait, "TierPlan to list "+name+" as unmanaged", func() (bool, string) {
		f.srv.RefreshCluster(f.env.ID)
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"?kind=unmanaged", nil)
		var list struct {
			Items []struct {
				Name      string `json:"name"`
				Managed   bool   `json:"managed"`
				Source    string `json:"source"`
				Unmanaged *struct {
					Adoptable bool   `json:"adoptable"`
					Reason    string `json:"reason"`
				} `json:"unmanaged"`
			} `json:"items"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
			return false, fmt.Sprintf("GET the unmanaged stacks = %d %s", code, body)
		}
		for _, it := range list.Items {
			if it.Name != name {
				continue
			}
			if it.Managed || it.Source != "unmanaged" || it.Unmanaged == nil {
				t.Fatalf("the list shows %s as %+v, want an unmanaged stack", name, it)
			}
			if !it.Unmanaged.Adoptable {
				t.Fatalf("the list shows %s as not adoptable: %s", name, it.Unmanaged.Reason)
			}
			return true, ""
		}
		return false, string(body)
	})
}

// serviceVersions is the version index the engine holds for each service of the stack, by service name. A
// service's version moves on every write to its spec, so a map that is the same before and after proves
// nothing was written.
func serviceVersions(ctx context.Context, mgr *dockertest.Node, stack string) (map[string]string, error) {
	names, err := stackServiceNames(ctx, mgr, stack)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, n := range names {
		v, err := mgr.Sh(ctx, "docker service inspect --format '{{.Version.Index}}' "+n)
		if err != nil {
			return nil, err
		}
		out[n] = strings.TrimSpace(v)
	}
	return out, nil
}

// objectIDs is the ID of each network, secret or config that carries the stack's namespace label, by name.
func objectIDs(ctx context.Context, mgr *dockertest.Node, noun, stack string) (map[string]string, error) {
	trunc := ""
	if noun == "network" {
		trunc = " --no-trunc" // only a network's ID is shortened by ls
	}
	out, err := mgr.Sh(ctx, "docker "+noun+" ls"+trunc+" --filter label=com.docker.stack.namespace="+stack+" --format '{{.Name}} {{.ID}}'")
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, id, ok := strings.Cut(line, " "); ok {
			ids[name] = id
		}
	}
	return ids, nil
}

// stackNetworkIDs, stackSecretIDs and stackConfigIDs are objectIDs of one kind.
func stackNetworkIDs(ctx context.Context, mgr *dockertest.Node, stack string) (map[string]string, error) {
	return objectIDs(ctx, mgr, "network", stack)
}

func stackSecretIDs(ctx context.Context, mgr *dockertest.Node, stack string) (map[string]string, error) {
	return objectIDs(ctx, mgr, "secret", stack)
}

func stackConfigIDs(ctx context.Context, mgr *dockertest.Node, stack string) (map[string]string, error) {
	return objectIDs(ctx, mgr, "config", stack)
}

// engineObjects is what the engine holds for a stack that adoption and the first deploy must not change.
type engineObjects struct {
	versions, networks, secrets, configs map[string]string
}

// readEngineObjects reads the service versions and the IDs of the networks, secrets and configs of the stack.
func readEngineObjects(t *testing.T, mgr *dockertest.Node, stack string) engineObjects {
	t.Helper()
	ctx := execCtx(t, 2*time.Minute)
	var o engineObjects
	var err error
	if o.versions, err = serviceVersions(ctx, mgr, stack); err != nil {
		t.Fatal(err)
	}
	if o.networks, err = stackNetworkIDs(ctx, mgr, stack); err != nil {
		t.Fatal(err)
	}
	if o.secrets, err = stackSecretIDs(ctx, mgr, stack); err != nil {
		t.Fatal(err)
	}
	if o.configs, err = stackConfigIDs(ctx, mgr, stack); err != nil {
		t.Fatal(err)
	}
	return o
}

// requireSame fails the test for each of the maps in want that is not the same in got. The versions are
// compared only when versions is set: the first deploy changes them on purpose (the ownership labels).
func (got engineObjects) requireSame(t *testing.T, when string, want engineObjects, versions bool) {
	t.Helper()
	for _, c := range []struct {
		what      string
		got, want map[string]string
		skip      bool
	}{
		{"service versions", got.versions, want.versions, !versions},
		{"network IDs", got.networks, want.networks, false},
		{"secret IDs", got.secrets, want.secrets, false},
		{"config IDs", got.configs, want.configs, false},
	} {
		if !c.skip && !maps.Equal(c.got, c.want) {
			t.Errorf("%s: the %s changed:\n before: %v\n after:  %v", when, c.what, c.want, c.got)
		}
	}
}

// adoptReviewBody is the part of the answer of POST .../review these tests read of an adopted stack's first review.
type adoptReviewBody struct {
	reviewBody
	Summary struct {
		Services, Networks, Volumes, Secrets, Configs struct{ Added, Changed, Removed int }
		OwnershipOnly                                 int `json:"ownershipOnly"`
	} `json:"summary"`
	Services []struct {
		Name, Change  string
		Current, Next map[string]any
		OwnershipOnly bool `json:"ownershipOnly"`
	} `json:"services"`
	Adoption *struct {
		Pending       bool `json:"pending"`
		OwnershipOnly int  `json:"ownershipOnly"`
	} `json:"adoption"`
}

// reviewAdopted reviews the adopted stack's draft through the API.
func reviewAdopted(t *testing.T, f *swarmFix, stackID string) adoptReviewBody {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+stackID+"/review", map[string]any{})
	var rv adoptReviewBody
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil {
		t.Fatalf("POST the review of stack %s = %d %s", stackID, code, body)
	}
	for _, p := range rv.Problems {
		if p.Severity == "error" {
			t.Errorf("the review of %s found an error: %s (%s, line %d)", stackID, p.Message, p.Code, p.Line)
		}
	}
	return rv
}

// waitJob waits until the stack job ends and returns it. It fails the test unless every step is done.
func waitJob(t *testing.T, f *swarmFix, stackID, jobID, what string) {
	t.Helper()
	var job stackJob
	testsupport.Poll(t, stackDeployWait, "the "+what+" job "+jobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID+"/deploys/"+jobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = stackJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the %s of stack %s ended %s, want done: %s", what, stackID, job.State, raw)
	}
}

// waitNamespaceEmpty waits until nothing carries the stack's namespace label on the engine but the secrets and
// configs in kept: no service, no network and no container on any node. A secret or config an adopted stack only
// referenced is external to it (D-13, T-08-62): TierPlan never removes it, so kept names those, by name and ID, and
// the wait ends only when exactly they are left.
func waitNamespaceEmpty(t *testing.T, f *swarmFix, stack string, kept engineObjects) {
	t.Helper()
	mgr := f.sw.Managers[0]
	testsupport.Poll(t, stackGoneWait, "nothing but the stack's external secrets and configs to be left under the namespace "+stack, func() (bool, string) {
		ctx := execCtx(t, time.Minute)
		var left []string
		for _, what := range []string{
			"docker service ls -q --filter label=com.docker.stack.namespace=" + stack,
			"docker network ls -q --filter label=com.docker.stack.namespace=" + stack,
		} {
			out, err := mgr.Sh(ctx, what)
			if err != nil {
				return false, err.Error()
			}
			if s := strings.TrimSpace(out); s != "" {
				left = append(left, what+": "+strings.ReplaceAll(s, "\n", " "))
			}
		}
		for _, o := range []struct {
			noun string
			want map[string]string
		}{{"secret", kept.secrets}, {"config", kept.configs}} {
			got, err := objectIDs(ctx, mgr, o.noun, stack)
			if err != nil {
				return false, err.Error()
			}
			if !maps.Equal(got, o.want) {
				left = append(left, fmt.Sprintf("%ss %v, want %v", o.noun, got, o.want))
			}
		}
		for _, n := range f.sw.Nodes() {
			out, err := n.Sh(ctx, "docker ps -aq --filter label=com.docker.stack.namespace="+stack)
			if err != nil {
				return false, err.Error()
			}
			if s := strings.TrimSpace(out); s != "" {
				left = append(left, n.Hostname+" containers "+strings.ReplaceAll(s, "\n", " "))
			}
		}
		return len(left) == 0, strings.Join(left, "; ")
	})
}

// removeStackTierPlan removes the stack through the API with no volume ticked, waits for the job and then until
// nothing is left under the stack's namespace but the secrets and configs in kept (see waitNamespaceEmpty), and
// shows that TierPlan no longer holds the stack.
func removeStackTierPlan(t *testing.T, f *swarmFix, stackID, stack string, kept engineObjects) {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+stackID+"/remove", map[string]any{
		"confirmName": stack, "volumes": []map[string]string{}, "keepDefinition": false,
	})
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the removal of %s = %d %s", stack, code, body)
	}
	waitJob(t, f, stackID, started.JobID, "removal")
	waitNamespaceEmpty(t, f, stack, kept)
	if code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID, nil); code != http.StatusNotFound {
		t.Errorf("GET the removed stack %s = %d %s, want 404", stack, code, body)
	}
}

// sharedStackAdoptRoundTrip is the real-engine oracle of adoption (D-20, ROADMAP SC2 and SC3, the prohibition
// STK-13: adopting never changes a service version, a network ID or a secret ID before the first deploy).
// Each file of the parity corpus is deployed by the real docker stack deploy, then, on a real swarm:
//
//   - TierPlan lists it as a stack it did not make;
//   - its adopt check finds no difference but the ownership labels, in every service;
//   - adopting writes nothing to the engine: the service versions and the network, secret and config IDs are
//     the same as before;
//   - the first review shows the ownership labels and nothing else;
//   - the first deploy keeps every network, secret and config ID and gives every service TierPlan's label;
//   - the second review lists no change at all;
//   - removing the stack through TierPlan takes its services and the networks it owns and leaves nothing under
//     its namespace but the secrets and configs it only referenced, which are external to it (D-13, T-08-62):
//     those are untouched, with the IDs they had, and the case then removes them itself.
func sharedStackAdoptRoundTrip(t *testing.T, f *swarmFix) {
	f.installPrivileged(t)
	f.waitSwarmOnline(t, 4)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/adopt/app:1", nil)

	for i, c := range parityCorpus {
		stack := fmt.Sprintf("adopt%d", i+1)
		t.Run(c.file, func(t *testing.T) {
			values := map[string]string{"IMAGE": addr + "/adopt/app@" + digest, "EXTERNAL_NETWORK": c.external}
			if c.external != "" {
				sh(t, mgr, "docker network create --driver overlay --attachable "+c.external)
				t.Cleanup(func() { removeNetwork(t, f, c.external) })
			}
			// Whatever happens below, the stack does not outlive the case.
			t.Cleanup(func() { removeStackCLI(t, f, stack) })
			src := paritySource{Compose: readCorpus(t, c.file, values), Files: c.files}

			// The stack as an admin makes it, and as it settles.
			if err := deployWithCLI(execCtx(t, 5*time.Minute), mgr, stack, src); err != nil {
				t.Fatal(err)
			}
			waitStackConverged(t, f, stack)
			before := readEngineObjects(t, mgr, stack)
			services := len(before.versions)
			if services == 0 {
				t.Fatalf("docker stack deploy made no service for %s", c.file)
			}

			waitUnmanaged(t, f, stack)

			// The check: the first deploy changes nothing but TierPlan's ownership labels, in every service.
			chk := adoptCheck(t, f, stack)
			if !chk.Clean || chk.GapCount != 0 || len(chk.Gaps) != 0 {
				for _, g := range chk.Gaps {
					var diffs []string
					leafDiff("", g.Current, g.Next, &diffs)
					t.Errorf("the adopt check finds a gap in %s:\n  %s\n  hidden changes: %v", g.Name, strings.Join(diffs, "\n  "), g.HiddenChanges)
				}
				t.Fatalf("the adopt check of %s: clean %v, %d gaps; want clean with none", c.file, chk.Clean, chk.GapCount)
			}
			if len(chk.OwnershipOnly) != services {
				t.Errorf("the adopt check lists %d services as ownership-only (%v), want all %d of the stack's services", len(chk.OwnershipOnly), chk.OwnershipOnly, services)
			}
			if chk.Fingerprint == "" {
				t.Fatalf("the adopt check of %s answered no fingerprint", c.file)
			}
			readEngineObjects(t, mgr, stack).requireSame(t, "after the adopt check", before, true)

			// Adopting: the stack is stored; nothing is sent to the engine.
			id := adopt(t, f, stack, chk.Fingerprint)
			readEngineObjects(t, mgr, stack).requireSame(t, "after adopting", before, true)

			// The first review: every service differs in its ownership labels alone.
			rv := reviewAdopted(t, f, id)
			if rv.Adoption == nil || !rv.Adoption.Pending || rv.Adoption.OwnershipOnly != services {
				t.Errorf("the first review's adoption = %+v, want pending with %d services recreated for the labels", rv.Adoption, services)
			}
			if rv.Summary.OwnershipOnly != services {
				t.Errorf("the first review's summary counts %d ownership-only services, want %d", rv.Summary.OwnershipOnly, services)
			}
			if n := rv.Summary.Services; n.Added+n.Changed+n.Removed != 0 {
				t.Errorf("the first review's summary counts services %+v besides the ownership labels, want none", n)
			}
			if len(rv.Services) != services {
				t.Errorf("the first review lists %d services, want the stack's %d", len(rv.Services), services)
			}
			for _, sc := range rv.Services {
				if sc.Change != "changed" || !sc.OwnershipOnly {
					var diffs []string
					leafDiff("", sc.Current, sc.Next, &diffs)
					t.Errorf("the first review lists %s %s (ownership only: %v):\n  %s", sc.Change, sc.Name, sc.OwnershipOnly, strings.Join(diffs, "\n  "))
				}
			}
			for _, o := range rv.Objects {
				if o.Change != "external" {
					t.Errorf("the first review lists %s %s %s: %s", o.Change, o.Kind, o.Name, o.Detail)
				}
			}
			for kind, n := range map[string]struct{ Added, Changed, Removed int }{
				"networks": rv.Summary.Networks, "volumes": rv.Summary.Volumes, "secrets": rv.Summary.Secrets, "configs": rv.Summary.Configs,
			} {
				if n.Added+n.Changed+n.Removed != 0 {
					t.Errorf("the first review's summary counts %s %+v, want none", kind, n)
				}
			}

			// The first deploy: TierPlan takes ownership. The networks, secrets and configs are the very objects
			// that were there, and every service carries TierPlan's label.
			f.deployStackTierPlan(t, id)
			waitStackConverged(t, f, stack)
			readEngineObjects(t, mgr, stack).requireSame(t, "after the first deploy", before, false)
			for name := range before.versions {
				if got := serviceField(t, f, name, `{{index .Spec.Labels "tierplan.stack.id"}}`); got != id {
					t.Errorf("%s carries tierplan.stack.id %q after the first deploy, want %s", name, got, id)
				}
			}

			// The second review: nothing is left to change.
			rv = reviewAdopted(t, f, id)
			if rv.Adoption != nil && rv.Adoption.Pending {
				t.Errorf("the second review still says the stack's first deploy is to come: %+v", rv.Adoption)
			}
			for _, sc := range rv.Services {
				var diffs []string
				leafDiff("", sc.Current, sc.Next, &diffs)
				t.Errorf("the second review lists %s %s:\n  %s", sc.Change, sc.Name, strings.Join(diffs, "\n  "))
			}
			for _, o := range rv.Objects {
				if o.Change != "external" {
					t.Errorf("the second review lists %s %s %s: %s", o.Change, o.Kind, o.Name, o.Detail)
				}
			}
			for kind, n := range map[string]struct{ Added, Changed, Removed int }{
				"services": rv.Summary.Services, "networks": rv.Summary.Networks, "volumes": rv.Summary.Volumes,
				"secrets": rv.Summary.Secrets, "configs": rv.Summary.Configs,
			} {
				if n.Added+n.Changed+n.Removed != 0 {
					t.Errorf("the second review's summary counts %s %+v, want none", kind, n)
				}
			}
			if rv.Summary.OwnershipOnly != 0 {
				t.Errorf("the second review counts %d ownership-only services, want none", rv.Summary.OwnershipOnly)
			}

			// Removal through TierPlan takes the services and the networks the stack owns. The secrets and configs it
			// referenced are external (D-13), so they stay as they were; a network the file only names stays too.
			removeStackTierPlan(t, f, id, stack, engineObjects{secrets: before.secrets, configs: before.configs})
			for noun, ids := range map[string]map[string]string{"secret": before.secrets, "config": before.configs} {
				for name := range ids {
					sh(t, mgr, "docker "+noun+" rm "+name)
				}
			}
			if c.external != "" {
				if ok, err := networkNamed(execCtx(t, time.Minute), mgr, c.external); err != nil || !ok {
					t.Errorf("the external network %s was removed with the stack (%v)", c.external, err)
				}
			}
		})
	}
}

// mountLabels are the labels on the volume options of each mount of a service, by mount target.
func mountLabels(t *testing.T, f *swarmFix, service string) map[string]map[string]string {
	t.Helper()
	var mounts []struct {
		Target        string
		VolumeOptions *struct{ Labels map[string]string }
	}
	raw := serviceField(t, f, service, "{{json .Spec.TaskTemplate.ContainerSpec.Mounts}}")
	if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
		t.Fatalf("read the mounts of %s from %q: %v", service, raw, err)
	}
	out := map[string]map[string]string{}
	for _, m := range mounts {
		if m.VolumeOptions != nil {
			out[m.Target] = m.VolumeOptions.Labels
		}
	}
	return out
}

// servicePlatforms is the JSON of a service's placement platforms, "" when it has none.
func servicePlatforms(t *testing.T, f *swarmFix, service string) string {
	t.Helper()
	got := serviceField(t, f, service, "{{with .Spec.TaskTemplate.Placement}}{{json .Platforms}}{{end}}")
	if got == "null" || got == "[]" {
		return ""
	}
	return got
}

// removeVolumeOn removes a named volume of the node and waits until it is gone. A volume that is not there is
// done, so the case and its cleanup can both call it.
func removeVolumeOn(t *testing.T, node *dockertest.Node, name string) {
	t.Helper()
	testsupport.Poll(t, stackGoneWait, "the volume "+name+" to be removed from "+node.Hostname, func() (bool, string) {
		out, err := node.Sh(execCtx(t, time.Minute), "docker volume rm "+name+" 2>&1")
		if err == nil || strings.Contains(out, "No such volume") || strings.Contains(out, "not found") { // the wording differs by engine
			return true, ""
		}
		return false, strings.TrimSpace(out)
	})
}

// sharedStackAdoptResolvedRoundTrip is the real-engine oracle of the stack the UAT adopted (G-08-4, D-20, D-18,
// ROADMAP SC3): `docker stack deploy` with its default --resolve-image=always, which writes each service's
// platforms into its placement, and a named volume, which writes the namespace label on the mount's volume
// options. On a real swarm:
//
//   - the precondition holds, or the case fails: the CLI wrote platforms on every service, and no label of
//     TierPlan's is on the volume mount;
//   - the adopt check (rebuild) is clean, with every service ownership-only;
//   - the stack's own file, pasted with ${TAG} in the image, matches what runs;
//   - adopting writes nothing to the engine and drift reads none;
//   - the first review lists the ownership labels alone, nothing under the placement;
//   - the first deploy keeps every service's platforms and the volume (its CreatedAt does not move), gives every
//     service and the volume's mount TierPlan's stack ID, and the second review is empty;
//   - a roll back to the adopted revision keeps the platforms;
//   - removal through TierPlan leaves nothing under the namespace, and the case removes the volume.
func sharedStackAdoptResolvedRoundTrip(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	// The image is pushed as a manifest list, as an image of Docker Hub is: only for a list does the engine learn
	// the image's platforms and write them into a service's placement under --resolve-image=always.
	image := addr + "/adoptr/app:1"
	dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/adoptr/app:single", nil)
	sh(t, mgr, "docker manifest create --insecure "+image+" "+addr+"/adoptr/app:single && docker manifest push --insecure --purge "+image)

	const stack, volume = "adoptr", "adoptr_data"
	services := []string{"adoptr_web", "adoptr_api"}
	// The stack and its volume do not outlive the case on any path. Cleanups run last in, first out.
	t.Cleanup(func() { removeVolumeOn(t, mgr, volume) })
	t.Cleanup(func() { removeStackCLI(t, f, stack) })

	// The stack as an admin makes it with the CLI's defaults, and as it settles.
	cliFile := readCorpus(t, "adopt-resolved.yaml", map[string]string{"IMAGE": image, "NODE": mgr.Hostname})
	if err := deployWithCLIResolving(execCtx(t, 5*time.Minute), mgr, stack, paritySource{Compose: cliFile}); err != nil {
		t.Fatal(err)
	}
	waitStackConverged(t, f, stack)

	// The precondition the oracle stands on: the CLI wrote platforms, and TierPlan has written nothing on the mount.
	platforms := map[string]string{}
	for _, svc := range services {
		if platforms[svc] = servicePlatforms(t, f, svc); platforms[svc] == "" {
			t.Fatalf("docker stack deploy --resolve-image=always wrote no placement platforms on %s; its image %q, placement %s", svc,
				serviceField(t, f, svc, "{{.Spec.TaskTemplate.ContainerSpec.Image}}"), serviceField(t, f, svc, "{{json .Spec.TaskTemplate.Placement}}"))
		}
	}
	for target, labels := range mountLabels(t, f, "adoptr_api") {
		for k := range labels {
			if strings.HasPrefix(k, "tierplan.") {
				t.Fatalf("the CLI-made mount %s carries %s before adoption", target, k)
			}
		}
	}
	before := readEngineObjects(t, mgr, stack)
	if len(before.versions) != len(services) {
		t.Fatalf("docker stack deploy made services %v, want %v", before.versions, services)
	}
	createdAt := strings.TrimSpace(sh(t, mgr, "docker volume inspect "+volume+" --format '{{.CreatedAt}}'"))

	waitUnmanaged(t, f, stack)

	// The rebuild check is clean: the first deploy changes the ownership labels and nothing else.
	chk := adoptCheck(t, f, stack)
	if !chk.Clean || chk.GapCount != 0 || len(chk.Gaps) != 0 {
		for _, g := range chk.Gaps {
			var diffs []string
			leafDiff("", g.Current, g.Next, &diffs)
			t.Errorf("the adopt check finds a gap in %s:\n  %s\n  hidden changes: %v", g.Name, strings.Join(diffs, "\n  "), g.HiddenChanges)
		}
		t.Fatalf("the adopt check of %s: clean %v, %d gaps; want clean with none", stack, chk.Clean, chk.GapCount)
	}
	if len(chk.OwnershipOnly) != len(services) {
		t.Errorf("the adopt check lists %d services as ownership-only (%v), want all %d", len(chk.OwnershipOnly), chk.OwnershipOnly, len(services))
	}
	if chk.Fingerprint == "" {
		t.Fatalf("the adopt check of %s answered no fingerprint", stack)
	}

	// The stack's own file, pasted with ${TAG}, matches what runs.
	pasted := readCorpus(t, "adopt-resolved.yaml", map[string]string{"IMAGE": addr + "/adoptr/app:${TAG}", "NODE": mgr.Hostname})
	code, body := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/unmanaged-stacks/"+stack+"/adopt-check", map[string]any{
		"source": "paste", "compose": pasted, "vars": []map[string]any{{"name": "TAG", "value": "1", "secret": false}},
	})
	var paste struct {
		Matches bool `json:"matches"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &paste) != nil {
		t.Fatalf("POST the paste check of %s = %d %s", stack, code, body)
	}
	if !paste.Matches {
		t.Errorf("the paste check of the stack's own file does not match what runs: %s", body)
	}
	readEngineObjects(t, mgr, stack).requireSame(t, "after the checks", before, true)

	// Adopting writes nothing; drift reads none.
	id := adopt(t, f, stack, chk.Fingerprint)
	removed := false
	t.Cleanup(func() {
		if !removed {
			removeStackTierPlan(t, f, id, stack, engineObjects{})
		}
	})
	readEngineObjects(t, mgr, stack).requireSame(t, "after adopting", before, true)
	if d := driftOf(t, f, id, true); d.Count != 0 || len(d.Services) != 0 {
		t.Errorf("the freshly adopted stack drifts: %+v", d)
	}

	// The first review: the ownership labels alone, nothing under the placement.
	rv := reviewAdopted(t, f, id)
	if rv.Adoption == nil || !rv.Adoption.Pending || rv.Adoption.OwnershipOnly != len(services) {
		t.Errorf("the first review's adoption = %+v, want pending with %d services recreated for the labels", rv.Adoption, len(services))
	}
	if len(rv.Services) != len(services) {
		t.Errorf("the first review lists %d services, want %d", len(rv.Services), len(services))
	}
	for _, sc := range rv.Services {
		var diffs []string
		leafDiff("", sc.Current, sc.Next, &diffs)
		if sc.Change != "changed" || !sc.OwnershipOnly {
			t.Errorf("the first review lists %s %s (ownership only: %v):\n  %s", sc.Change, sc.Name, sc.OwnershipOnly, strings.Join(diffs, "\n  "))
		}
		for _, d := range diffs {
			if strings.Contains(d, "Placement") || strings.Contains(d, "latform") {
				t.Errorf("the first review of %s changes the placement: %s", sc.Name, d)
			}
		}
	}

	// The first deploy keeps the platforms and the volume, and takes ownership of both.
	f.deployStackTierPlan(t, id)
	waitStackConverged(t, f, stack)
	readEngineObjects(t, mgr, stack).requireSame(t, "after the first deploy", before, false)
	for _, svc := range services {
		if got := servicePlatforms(t, f, svc); got != platforms[svc] {
			t.Errorf("%s has the platforms %s after the first deploy, want %s", svc, got, platforms[svc])
		}
		if got := serviceField(t, f, svc, `{{index .Spec.Labels "tierplan.stack.id"}}`); got != id {
			t.Errorf("%s carries tierplan.stack.id %q after the first deploy, want %s", svc, got, id)
		}
	}
	for target, labels := range mountLabels(t, f, "adoptr_api") {
		if labels["tierplan.stack.id"] != id {
			t.Errorf("the mount %s has the labels %v after the first deploy, want tierplan.stack.id %s", target, labels, id)
		}
	}
	if got := strings.TrimSpace(sh(t, mgr, "docker volume inspect "+volume+" --format '{{.CreatedAt}}'")); got != createdAt {
		t.Errorf("the volume %s was created at %s, now %s: the first deploy replaced it", volume, createdAt, got)
	}

	// The second review lists no change.
	rv = reviewAdopted(t, f, id)
	for _, sc := range rv.Services {
		var diffs []string
		leafDiff("", sc.Current, sc.Next, &diffs)
		t.Errorf("the second review lists %s %s:\n  %s", sc.Change, sc.Name, strings.Join(diffs, "\n  "))
	}
	for kind, n := range map[string]struct{ Added, Changed, Removed int }{
		"services": rv.Summary.Services, "networks": rv.Summary.Networks, "volumes": rv.Summary.Volumes,
		"secrets": rv.Summary.Secrets, "configs": rv.Summary.Configs,
	} {
		if n.Added+n.Changed+n.Removed != 0 {
			t.Errorf("the second review's summary counts %s %+v, want none", kind, n)
		}
	}

	// A roll back to the adopted revision keeps the platforms.
	code, body = f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/rollback", map[string]any{"revision": 1, "expectedDeployed": 2})
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the roll back to #1 = %d %s", code, body)
	}
	waitJob(t, f, id, started.JobID, "roll back")
	waitStackConverged(t, f, stack)
	for _, svc := range services {
		if got := servicePlatforms(t, f, svc); got != platforms[svc] {
			t.Errorf("%s has the platforms %s after the roll back, want %s", svc, got, platforms[svc])
		}
	}

	// Removal leaves nothing under the namespace; the named volume is the case's to remove.
	removed = true
	removeStackTierPlan(t, f, id, stack, engineObjects{})
	removeVolumeOn(t, mgr, volume)
}
