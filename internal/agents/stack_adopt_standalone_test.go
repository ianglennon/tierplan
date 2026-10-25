// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package agents_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The real-engine oracles of plan 08-28 for a standalone host (STK-13, STK-12, STK-08, STK-14, D-20, D-46): a
// Compose project made by the real `docker compose up` is listed, adopted and deployed by Bayplan with its volume
// data and network intact; a roll back puts the older image back; a container added outside Bayplan is drift.

// requireComposeCLI returns the version of the Compose plugin of the dind host, and fails the test when the host
// has none. Every docker:<N>-dind image from 24 to 29 ships it, so a host without it is a broken fixture: the
// oracle never skips and never falls back to making the project another way (D-20).
func requireComposeCLI(t *testing.T, host *dockertest.Node) string {
	t.Helper()
	out, err := host.Sh(execCtx(t, time.Minute), "docker compose version --short")
	if err != nil {
		t.Fatalf("the dind host has no docker compose plugin: %v", err)
	}
	return strings.TrimSpace(out)
}

// composeUp writes the compose file into the host and runs the real `docker compose -p {project} up -d` on it,
// with the extra flags given. It is the only way these tests make a project that Bayplan did not (D-20), and
// it fails the test when the CLI does. The Compose version is logged, so a run on an older Engine records a
// project made by an older Compose.
func composeUp(t *testing.T, f *saFix, project, compose string, flags ...string) {
	t.Helper()
	version := requireComposeCLI(t, f.host)
	t.Logf("docker compose %s makes project %s %v", version, project, flags)
	dir := "/tmp/compose-" + project
	ctx := execCtx(t, 5*time.Minute)
	sh(t, f.host, "mkdir -p "+dir)
	if err := f.host.WriteFile(ctx, dir+"/compose.yaml", []byte(compose)); err != nil {
		t.Fatal(err)
	}
	sh(t, f.host, "docker compose -p "+project+" -f "+dir+"/compose.yaml up -d "+strings.Join(flags, " "))
}

// saAdoptCheck is the part of the answer of POST .../unmanaged-stacks/{name}/adopt-check these tests read.
type saAdoptCheck struct {
	Fingerprint string `json:"fingerprint"`
	Clean       bool   `json:"clean"`
	GapCount    int    `json:"gapCount"`
	Gaps        []struct {
		Name          string
		Current, Next map[string]any
		HiddenChanges []string `json:"hiddenChanges"`
	} `json:"gaps"`
	OwnershipOnly []string `json:"ownershipOnly"`
	Source        string   `json:"source"`
}

// adoptCheck asks what adopting the unmanaged project would change on its first deploy.
func (f *saFix) adoptCheck(t *testing.T, name string) saAdoptCheck {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/unmanaged-stacks/"+name+"/adopt-check", map[string]any{})
	var out saAdoptCheck
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("POST the adopt check of %s = %d %s", name, code, body)
	}
	return out
}

// adopt adopts the unmanaged project as the check read it and returns the ID of the stack Bayplan holds now.
func (f *saFix) adopt(t *testing.T, name, fingerprint string) string {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/unmanaged-stacks/"+name+"/adopt", map[string]any{"fingerprint": fingerprint})
	var st struct {
		ID     string `json:"id"`
		Source string `json:"source"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &st) != nil || st.ID == "" {
		t.Fatalf("POST the adoption of %s = %d %s", name, code, body)
	}
	if st.Source != "adopted" {
		t.Errorf("the adopted stack %s has source %q, want adopted", name, st.Source)
	}
	return st.ID
}

// waitUnmanaged waits until the environment's stack list shows the project as one Bayplan did not make. The
// list reads the host through the environment's cache, so it is refreshed on every look.
func (f *saFix) waitUnmanaged(t *testing.T, name string) {
	t.Helper()
	testsupport.Poll(t, 2*time.Minute, "Bayplan to list "+name+" as unmanaged", func() (bool, string) {
		f.srv.RefreshCluster(f.env.ID)
		code, body := f.srv.Do(http.MethodGet, f.api()+"?kind=unmanaged", nil)
		var list struct {
			Items []struct {
				Name      string `json:"name"`
				Target    string `json:"target"`
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
			if it.Managed || it.Source != "unmanaged" || it.Unmanaged == nil || it.Target != "standalone" {
				t.Fatalf("the list shows %s as %+v, want an unmanaged stack of a standalone host", name, it)
			}
			if !it.Unmanaged.Adoptable {
				t.Fatalf("the list shows %s as not adoptable: %s", name, it.Unmanaged.Reason)
			}
			return true, ""
		}
		return false, string(body)
	})
}

// saAdoptReview is the part of the answer of POST .../review these tests read of an adopted stack.
type saAdoptReview struct {
	Problems []struct {
		Severity, Code, Message string
	} `json:"problems"`
	Summary struct {
		Services, Networks, Volumes, Secrets, Configs struct{ Added, Changed, Removed int }
		OwnershipOnly                                 int `json:"ownershipOnly"`
	} `json:"summary"`
	Services []struct {
		Name, Short, Change, Because string
		Current, Next                map[string]any
		OwnershipOnly                bool `json:"ownershipOnly"`
	} `json:"services"`
	Objects []struct {
		Change, Kind, Name, Detail string
	} `json:"objects"`
	Adoption *struct {
		Pending       bool `json:"pending"`
		OwnershipOnly int  `json:"ownershipOnly"`
	} `json:"adoption"`
}

// reviewAdopted reviews the adopted stack's draft through the API. An error problem fails the test.
func (f *saFix) reviewAdopted(t *testing.T, stackID string) saAdoptReview {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.api()+"/"+stackID+"/review", map[string]any{})
	var rv saAdoptReview
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil {
		t.Fatalf("POST the review of stack %s = %d %s", stackID, code, body)
	}
	for _, p := range rv.Problems {
		if p.Severity == "error" {
			t.Fatalf("the review of %s found an error: %s (%s): %s", stackID, p.Message, p.Code, body)
		}
	}
	return rv
}

// requireNothingBesides fails the test for each object row that is not an external one and for each service,
// network, volume, secret and config count in the summary that is not zero, except the services in the summary
// when services is true. what names the review in the failure text.
func (rv saAdoptReview) requireNothingBesides(t *testing.T, what string, services bool) {
	t.Helper()
	for _, o := range rv.Objects {
		if o.Change != "external" {
			t.Errorf("%s lists %s %s %s: %s", what, o.Change, o.Kind, o.Name, o.Detail)
		}
	}
	counts := map[string]struct{ Added, Changed, Removed int }{
		"networks": rv.Summary.Networks, "volumes": rv.Summary.Volumes, "secrets": rv.Summary.Secrets, "configs": rv.Summary.Configs,
	}
	if services {
		counts["services"] = rv.Summary.Services
	}
	for kind, n := range counts {
		if n.Added+n.Changed+n.Removed != 0 {
			t.Errorf("%s counts %s %+v, want none", what, kind, n)
		}
	}
}

// volumeCreatedAt is when the engine says the volume was made, which only a removal and a new create can change.
func (f *saFix) volumeCreatedAt(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(sh(t, f.host, "docker volume inspect -f '{{.CreatedAt}}' "+name))
}

// networkID is the ID of the network of this name.
func (f *saFix) networkID(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(sh(t, f.host, "docker network inspect -f '{{.Id}}' "+name))
}

// TestStandaloneAdoptThroughAnAgent is the real-engine oracle of adoption on a standalone host (STK-13, STK-12,
// D-20, D-46, D-18, ROADMAP SC3). A Compose project made by the real `docker compose up -d` (two services with
// depends_on, a named volume holding data, a user network) is, through a real agent:
//
//   - listed by Bayplan as unmanaged and adoptable;
//   - checked: the first deploy differs from what runs in nothing but the ownership labels, in both services;
//   - adopted without a write to the host: the containers are the very ones they were;
//   - reviewed: both services are ownership-only changes, and no network or volume is listed;
//   - deployed: each container is recreated once and runs, the data in the volume is still there, the volume
//     and the network are the objects that were there (the prohibition of STK-13: no data lost);
//   - reviewed again: nothing is left to change, and a second deploy would touch no container.
func TestStandaloneAdoptThroughAnAgent(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	image := f.registry + "/bp/app:1"
	f.pushBusybox(t, image, nil)

	const project, volume, network = "legacy", "legacy_data", "legacy_back"
	composeUp(t, f, project, saCorpus(t, "adopt-compose.yaml", map[string]string{"IMAGE": image}))

	// The project as the person had it: both containers run, and the volume holds data.
	before := f.states(t, project)
	if len(before) != 2 {
		t.Fatalf("containers after docker compose up = %v, want db and web", before)
	}
	for _, svc := range []string{"db", "web"} {
		if got := f.containers(t, project)[svc]; len(got) != 1 || !strings.HasSuffix(got[0], " running") {
			t.Fatalf("containers of %s after docker compose up = %v, want one running", svc, got)
		}
	}
	sh(t, f.host, "docker run --rm -v "+volume+":/data "+image+" sh -c 'echo kept > /data/marker'")
	createdAt, networkID := f.volumeCreatedAt(t, volume), f.networkID(t, network)

	f.waitUnmanaged(t, project)

	// The check: the first deploy changes nothing but the ownership labels, in both services.
	chk := f.adoptCheck(t, project)
	if !chk.Clean || chk.GapCount != 0 || len(chk.Gaps) != 0 {
		for _, g := range chk.Gaps {
			cur, _ := json.Marshal(g.Current)
			next, _ := json.Marshal(g.Next)
			t.Errorf("the adopt check finds a gap in %s:\n  now:  %s\n  next: %s\n  hidden changes: %v", g.Name, cur, next, g.HiddenChanges)
		}
		t.Fatalf("the adopt check: clean %v, %d gaps; want clean with none", chk.Clean, chk.GapCount)
	}
	if len(chk.OwnershipOnly) != 2 {
		t.Errorf("the adopt check lists %v as ownership-only, want db and web", chk.OwnershipOnly)
	}
	if chk.Fingerprint == "" {
		t.Fatal("the adopt check answered no fingerprint")
	}
	if got := f.states(t, project); !maps.Equal(got, before) {
		t.Errorf("the adopt check changed the containers:\n before: %v\n after:  %v", before, got)
	}

	// Adopting stores the stack and writes nothing to the host.
	id := f.adopt(t, project, chk.Fingerprint)
	if got := f.states(t, project); !maps.Equal(got, before) {
		t.Errorf("adopting changed the containers:\n before: %v\n after:  %v", before, got)
	}
	if got := f.volumeCreatedAt(t, volume); got != createdAt {
		t.Errorf("adopting changed the volume: created %s, was %s", got, createdAt)
	}
	if got := f.networkID(t, network); got != networkID {
		t.Errorf("adopting changed the network: %s, was %s", got, networkID)
	}

	// The first review: both services differ in their ownership labels alone, and no network or volume is listed.
	rv := f.reviewAdopted(t, id)
	if rv.Adoption == nil || !rv.Adoption.Pending || rv.Adoption.OwnershipOnly != 2 {
		t.Errorf("the first review's adoption = %+v, want pending with 2 services recreated for the labels", rv.Adoption)
	}
	if rv.Summary.OwnershipOnly != 2 {
		t.Errorf("the first review's summary counts %d ownership-only services, want 2", rv.Summary.OwnershipOnly)
	}
	if len(rv.Services) != 2 {
		t.Errorf("the first review lists %d services, want db and web", len(rv.Services))
	}
	for _, sc := range rv.Services {
		if sc.Change != "changed" || !sc.OwnershipOnly {
			cur, _ := json.Marshal(sc.Current)
			next, _ := json.Marshal(sc.Next)
			t.Errorf("the first review lists %s %s (ownership only: %v):\n  now:  %s\n  next: %s", sc.Change, sc.Short, sc.OwnershipOnly, cur, next)
		}
	}
	if n := rv.Summary.Services; n.Added+n.Changed+n.Removed != 0 {
		t.Errorf("the first review's summary counts services %+v besides the ownership labels, want none", n)
	}
	rv.requireNothingBesides(t, "the first review", false)
	for _, o := range rv.Objects {
		if o.Kind == "network" || o.Kind == "volume" {
			t.Errorf("the first review lists the %s %s (%s): adopting must leave it alone", o.Kind, o.Name, o.Change)
		}
	}

	// The first deploy: Bayplan takes ownership. Each container is recreated once and runs; the volume and the
	// network are the very objects that were there, so the data is.
	job := f.deployOK(t, id)
	after := f.states(t, project)
	if len(after) != 2 {
		t.Fatalf("containers after the first deploy = %v, want db and web and nothing left over", after)
	}
	if all := strings.Fields(sh(t, f.host, "docker ps -aq --filter label=com.docker.compose.project="+project)); len(all) != 2 {
		t.Errorf("containers of the project after the first deploy = %v, want two", all)
	}
	lines := job.outputText()
	for _, svc := range []string{"db", "web"} {
		if after[svc].ID == before[svc].ID {
			t.Errorf("%s keeps its container %s after the first deploy, want it recreated for the ownership labels", svc, before[svc].ID)
		}
		if got := f.containers(t, project)[svc]; len(got) != 1 || !strings.HasSuffix(got[0], " running") {
			t.Errorf("containers of %s after the first deploy = %v, want one running", svc, got)
		}
		if got := strings.TrimSpace(sh(t, f.host, `docker inspect -f '{{index .Config.Labels "bayplan.stack.id"}}' `+after[svc].ID)); got != id {
			t.Errorf("%s carries bayplan.stack.id %q after the first deploy, want %s", svc, got, id)
		}
		if n := strings.Count(lines, "Recreated container "+project+"-"+svc+"-1"); n != 1 {
			t.Errorf("the deploy's output says %d times that %s-%s-1 was recreated, want once:\n%s", n, project, svc, lines)
		}
	}
	if got := strings.TrimSpace(sh(t, f.host, "docker run --rm -v "+volume+":/data "+image+" cat /data/marker")); got != "kept" {
		t.Errorf("the marker file in %s reads %q after the first deploy, want kept: the volume's data must survive", volume, got)
	}
	if got := f.volumeCreatedAt(t, volume); got != createdAt {
		t.Errorf("the volume %s was made again by the first deploy: created %s, was %s", volume, got, createdAt)
	}
	if got := f.networkID(t, network); got != networkID {
		t.Errorf("the network %s was made again by the first deploy: %s, was %s", network, got, networkID)
	}

	// The second review lists nothing, and a deploy nobody edited touches no container.
	rv = f.reviewAdopted(t, id)
	if rv.Adoption != nil && rv.Adoption.Pending {
		t.Errorf("the second review still says the first deploy is to come: %+v", rv.Adoption)
	}
	for _, sc := range rv.Services {
		t.Errorf("the second review lists %s %s", sc.Change, sc.Short)
	}
	if rv.Summary.OwnershipOnly != 0 {
		t.Errorf("the second review counts %d ownership-only services, want none", rv.Summary.OwnershipOnly)
	}
	rv.requireNothingBesides(t, "the second review", true)
	f.deployOK(t, id)
	if again := f.states(t, project); !maps.Equal(again, after) {
		t.Errorf("a deploy nobody edited changed the containers:\n before: %v\n after:  %v", after, again)
	}
	f.noDenials(t)
}
