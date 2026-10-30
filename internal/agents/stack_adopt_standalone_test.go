// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build integration

package agents_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The real-engine oracles of plan 08-28 for a standalone host (STK-13, STK-12, STK-08, STK-14, D-20, D-46): a
// Compose project made by the real `docker compose up` is listed, adopted and deployed by Coaming with its volume
// data and network intact; a roll back puts the older image back; a container added outside Coaming is drift.

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
// with the extra flags given. It is the only way these tests make a project that Coaming did not (D-20), and
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

// adopt adopts the unmanaged project as the check read it and returns the ID of the stack Coaming holds now.
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

// waitUnmanaged waits until the environment's stack list shows the project as one Coaming did not make. The
// list reads the host through the environment's cache, so it is refreshed on every look.
func (f *saFix) waitUnmanaged(t *testing.T, name string) {
	t.Helper()
	testsupport.Poll(t, 2*time.Minute, "Coaming to list "+name+" as unmanaged", func() (bool, string) {
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
//   - listed by Coaming as unmanaged and adoptable;
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

	// The first deploy: Coaming takes ownership. Each container is recreated once and runs; the volume and the
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
		if got := strings.TrimSpace(sh(t, f.host, `docker inspect -f '{{index .Config.Labels "coaming.stack.id"}}' `+after[svc].ID)); got != id {
			t.Errorf("%s carries coaming.stack.id %q after the first deploy, want %s", svc, got, id)
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

// waitJob waits until the stack job ends and fails the test unless it ended done.
func (f *saFix) waitJob(t *testing.T, stackID, jobID, what string) saJob {
	t.Helper()
	var job saJob
	testsupport.Poll(t, standaloneDeployWait, "the "+what+" job "+jobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.api()+"/"+stackID+"/deploys/"+jobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = saJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the %s of stack %s ended %s, want done: %s", what, stackID, job.State, raw)
	}
	return job
}

// rollback sends the roll back route's body and returns the job it started and the number of the revision it took.
func (f *saFix) rollback(t *testing.T, stackID string, body map[string]any) (jobID string, revision int64) {
	t.Helper()
	code, raw := f.srv.Do(http.MethodPost, f.api()+"/"+stackID+"/rollback", body)
	var started struct {
		JobID    string `json:"jobId"`
		Revision int64  `json:"revision"`
	}
	if code != http.StatusAccepted || json.Unmarshal(raw, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the roll back %v = %d %s", body, code, raw)
	}
	return started.JobID, started.Revision
}

// TestStandaloneRollbackThroughAnAgent proves a roll back on a real standalone host (STK-08, D-02, D-03): a stack
// deployed at the images of two tags (revisions #1 and #2) is reviewed for a roll back to #1 and rolled back, and
// its container runs the image of tag one again, by the digest revision #1 stored. History then lists #3, a
// roll back of #1, as the revision the stack stands at.
func TestStandaloneRollbackThroughAnAgent(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})

	// Two tags of one image, each with a digest of its own, removed from the host so the deploy has to fetch.
	const name = "srb"
	refs := []string{f.registry + "/hist/app:a", f.registry + "/hist/app:b"}
	digests := make([]string, len(refs))
	for i, ref := range refs {
		digests[i] = dockertest.PushVariant(t, f.host, dockertest.ServiceTestImage, ref, "tag-"+string(rune('a'+i)), nil)
	}
	if digests[0] == digests[1] {
		t.Fatalf("the two tags have the same digest %s", digests[0])
	}
	sh(t, f.host, "docker image rm "+strings.Join(refs, " "))
	composeOf := func(image string) string {
		return "services:\n  app:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n"
	}

	// Two deploys, two images: revisions #1 and #2, each good.
	id := f.create(t, name, composeOf(refs[0]), nil)
	ran := make([]saState, len(refs))
	for i, ref := range refs {
		if i > 0 {
			f.save(t, id, composeOf(ref), nil)
		}
		f.deployOK(t, id)
		states := f.states(t, name)
		if len(states) != 1 || !strings.HasSuffix(states["app"].ImageRef, "@"+digests[i]) {
			t.Fatalf("after deploy %d the container is %+v, want one running the image %s@%s", i+1, states, ref, digests[i])
		}
		ran[i] = states["app"]
	}
	if ran[0].ImageID == ran[1].ImageID {
		t.Fatalf("both tags give the image %s: the roll back would prove nothing", ran[0].ImageID)
	}

	// The review of the roll back to #1: the service is pinned to the digest revision #1 stored.
	code, body := f.srv.Do(http.MethodPost, f.api()+"/"+id+"/review", map[string]any{"mode": "rollback", "revision": 1})
	var rv struct {
		Revision    int64 `json:"revision"`
		DeployedSeq int64 `json:"deployedSeq"`
		Images      []struct {
			Service, Digest, Current, State string
		} `json:"images"`
		Problems []struct {
			Severity, Code, Message string
		} `json:"problems"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil {
		t.Fatalf("POST the review of the roll back to #1 = %d %s", code, body)
	}
	for _, p := range rv.Problems {
		if p.Severity == "error" {
			t.Errorf("the roll back review found an error: %s (%s)", p.Message, p.Code)
		}
	}
	if rv.Revision != 1 || rv.DeployedSeq != 2 {
		t.Errorf("the roll back review is of revision %d against deployed %d, want 1 against 2", rv.Revision, rv.DeployedSeq)
	}
	if len(rv.Images) != 1 {
		t.Fatalf("the roll back review lists %d images, want the one service's: %s", len(rv.Images), body)
	}
	if r := rv.Images[0]; r.Service != "app" || r.State != "pinned" || r.Digest != digests[0] || r.Current != digests[1] {
		t.Errorf("the roll back review's image row = %+v, want app pinned to %s (from %s)", r, digests[0], digests[1])
	}

	// The roll back, as the review sends it.
	jobID, revision := f.rollback(t, id, map[string]any{"revision": 1, "expectedDeployed": rv.DeployedSeq})
	if revision != 3 {
		t.Errorf("the roll back took revision %d, want 3", revision)
	}
	f.waitJob(t, id, jobID, "roll back")

	// The container runs tag one's image again, the digest revision #1 stored.
	states := f.states(t, name)
	if got := states["app"]; len(states) != 1 || got.ImageID != ran[0].ImageID || !strings.HasSuffix(got.ImageRef, "@"+digests[0]) {
		t.Errorf("after the roll back the container is %+v, want one running image %s (%s@%s)", states, ran[0].ImageID, refs[0], digests[0])
	}
	if got := f.containers(t, name)["app"]; len(got) != 1 || !strings.HasSuffix(got[0], " running") {
		t.Errorf("containers of app after the roll back = %v, want one running", got)
	}

	// History: #3 is a roll back of #1, good, and the one the stack stands at.
	code, body = f.srv.Do(http.MethodGet, f.api()+"/"+id+"/revisions", nil)
	var hist struct {
		Items []struct {
			Seq          int64  `json:"seq"`
			Origin       string `json:"origin"`
			RestoredFrom int64  `json:"restoredFrom"`
			Outcome      string `json:"outcome"`
			Deployed     bool   `json:"deployed"`
		} `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &hist) != nil {
		t.Fatalf("GET the revisions = %d %s", code, body)
	}
	if len(hist.Items) != 3 || hist.Items[0].Seq != 3 {
		t.Fatalf("History lists %+v, want revisions 3, 2 and 1 newest first", hist.Items)
	}
	if r := hist.Items[0]; r.Origin != "rollback" || r.RestoredFrom != 1 || r.Outcome != "good" || !r.Deployed {
		t.Errorf("revision #3 = %+v, want a good roll back of #1, deployed", r)
	}
	for _, r := range hist.Items[1:] {
		if r.Deployed || r.Outcome != "good" {
			t.Errorf("revision #%d = %+v, want good and no longer the deployed one", r.Seq, r)
		}
	}
	f.noDenials(t)
}

// saDrift is the part of the answer of GET .../drift these tests read.
type saDrift struct {
	State    string `json:"state"`
	Revision int64  `json:"revision"`
	Count    int    `json:"count"`
	Services []struct {
		Short, Change string
		Settings      int
	} `json:"services"`
}

// driftNow reads the stack again and returns the drift check.
func (f *saFix) driftNow(t *testing.T, stackID string) (saDrift, string) {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, f.api()+"/"+stackID+"/drift?fresh=true", nil)
	var d saDrift
	if code != http.StatusOK || json.Unmarshal(body, &d) != nil {
		t.Fatalf("GET the drift of stack %s = %d %s", stackID, code, body)
	}
	return d, string(body)
}

// TestStandaloneDriftThroughAnAgent proves drift on a real standalone host (STK-14, D-21, D-22, D-23, G-08-6): a
// stack deployed by Coaming reads no drift; a second container of its service made from the person's own file with
// the Compose CLI alone (up --scale web=2 --no-recreate, no label of Coaming's on it) is the stack's: drift, a second
// container of web on the stack's page, gone again after a restore. After a second such scale, a Remove through
// Coaming lists that container, waits for it and leaves no container of the project.
func TestStandaloneDriftThroughAnAgent(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	image := f.registry + "/bp/app:1"
	f.pushBusybox(t, image, nil)

	const name = "sdr"
	compose := func(extra string) string {
		return "services:\n  web:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n" + extra
	}
	id := f.create(t, name, compose(""), nil)
	f.deployOK(t, id)
	first := f.states(t, name)
	if len(first) != 1 {
		t.Fatalf("containers after the deploy = %v, want web", first)
	}
	if d, raw := f.driftNow(t, id); d.State != "none" || d.Revision != 1 {
		t.Fatalf("drift right after the deploy = %s, want none against revision 1", raw)
	}

	// A person adds a second container of web from their own file with the Compose CLI alone, as UAT test 6 Part G
	// does: the file carries no label of Coaming's, and the first container is left as it is.
	composeUp(t, f, name, compose(""), "--scale", "web=2", "--no-recreate")
	if got := f.containers(t, name)["web"]; len(got) != 2 {
		t.Fatalf("containers of web after up --scale web=2 = %v, want two", got)
	}
	if out := strings.TrimSpace(sh(t, f.host, `docker ps -a --filter label=com.docker.compose.project=`+name+
		` --filter label=coaming.stack.id --format '{{.Names}}'`)); out != name+"-web-1" {
		t.Fatalf("containers of the project that carry coaming.stack.id = %q, want only %s-web-1: the added one carries none", out, name)
	}
	testsupport.Poll(t, 2*time.Minute, "a fresh drift check to find web changed", func() (bool, string) {
		d, raw := f.driftNow(t, id)
		if d.State != "drifted" {
			return false, raw
		}
		for _, s := range d.Services {
			if s.Short == "web" && s.Change == "changed" && s.Settings > 0 {
				return true, raw
			}
		}
		return false, raw
	})
	if got := f.overviewContainers(t, id, "web"); len(got) != 2 {
		t.Errorf("the stack's page lists %v under web, want the two containers", got)
	}

	// A restore puts the deployed revision back: the second container goes, the first is the one it was.
	jobID, revision := f.rollback(t, id, map[string]any{"mode": "restore", "revision": 1, "expectedDeployed": 1})
	if revision != 2 {
		t.Errorf("the restore took revision %d, want 2", revision)
	}
	f.waitJob(t, id, jobID, "restore")
	testsupport.Poll(t, 2*time.Minute, "a fresh drift check to read none after the restore", func() (bool, string) {
		d, raw := f.driftNow(t, id)
		return d.State == "none", raw
	})
	if got := f.containers(t, name)["web"]; len(got) != 1 || !strings.HasSuffix(got[0], " running") {
		t.Errorf("containers of web after the restore = %v, want one running", got)
	}
	if after := f.states(t, name); after["web"].ID != first["web"].ID {
		t.Errorf("web after the restore is container %s, want the first one, %s, left alone", after["web"].ID, first["web"].ID)
	}

	// Scaled again from the person's file, the stack is removed through Coaming: the dialog lists the added container,
	// and nothing of the project is left.
	composeUp(t, f, name, compose(""), "--scale", "web=2", "--no-recreate")
	if got := f.containers(t, name)["web"]; len(got) != 2 {
		t.Fatalf("containers of web after the second up --scale web=2 = %v, want two", got)
	}
	code, body := f.srv.Do(http.MethodGet, f.api()+"/"+id+"/removal-preview", nil)
	var preview struct {
		Containers []string `json:"containers"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &preview) != nil {
		t.Fatalf("GET the removal preview = %d %s", code, body)
	}
	if !slices.Equal(preview.Containers, []string{name + "-web-1", name + "-web-2"}) {
		t.Errorf("the removal preview lists %v, want both containers of web, the added one too", preview.Containers)
	}
	code, body = f.srv.Do(http.MethodPost, f.api()+"/"+id+"/remove", map[string]any{
		"confirmName": name, "keepDefinition": false, "volumes": []map[string]string{},
	})
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the removal = %d %s", code, body)
	}
	f.waitJob(t, id, started.JobID, "removal")
	testsupport.Poll(t, 2*time.Minute, "the host to hold no container of the removed project", func() (bool, string) {
		out := strings.TrimSpace(sh(t, f.host, `docker ps -a --filter label=com.docker.compose.project=`+name+` --format '{{.Names}} {{.State}}'`))
		return out == "", out
	})
	f.noDenials(t)
}

// overviewContainers are the names of the containers the stack's page lists under one service.
func (f *saFix) overviewContainers(t *testing.T, stackID, service string) []string {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, f.api()+"/"+stackID+"/services", nil)
	var out struct {
		Items []struct {
			Short      string `json:"short"`
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		} `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("GET the services of stack %s = %d %s", stackID, code, body)
	}
	var names []string
	for _, it := range out.Items {
		if it.Short == service {
			for _, c := range it.Containers {
				names = append(names, c.Name)
			}
		}
	}
	return names
}
