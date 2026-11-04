//go:build swarm

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The image update suites of plan 12-35 (IMG-04, IMG-06, IMG-07, D-12, RESEARCH A2 and A5) run the phase's update routes
// through real agents on a swarm of three managers and a worker, against a registry:2 service published on the ingress. The
// server asks that registry itself (an update check is a manifest HEAD from the server, as in production), so the fixture
// registry is forwarded to this machine's 127.0.0.1:5000, the name the nodes know it by (dockertest.ForwardRegistry).

// updateWait is how long an update, or its rollback, gets to end on the swarm: a pull on each node and the wait for the
// tasks. It is below the converge timeout of the server (10 minutes) on purpose, so a wait that runs out is a failure here.
const updateWait = 8 * time.Minute

// updateOf is the update object of a service as the services list carries it now, and false when the service or its
// update object is not there.
func (f *swarmFix) updateOf(t *testing.T, name string) (updateView, bool) {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/services", nil)
	var list struct {
		Items []struct {
			Name   string      `json:"name"`
			Update *updateView `json:"update"`
		} `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET services = %d %s", code, body)
	}
	for _, it := range list.Items {
		if it.Name == name && it.Update != nil {
			return *it.Update, true
		}
	}
	return updateView{}, false
}

// waitUpdateState waits until the update state of the service reads want, reading the cluster again each time round, and
// returns the update object as it stood then.
func (f *swarmFix) waitUpdateState(t *testing.T, name, want string, d time.Duration) updateView {
	t.Helper()
	var got updateView
	f.srv.WaitFor("the update state of "+name+" to read "+want, d, func() (bool, string) {
		f.srv.RefreshCluster(f.env.ID)
		u, ok := f.updateOf(t, name)
		got = u
		b, _ := json.Marshal(u)
		if !ok {
			return false, "no update object in the services list"
		}
		return u.State == want, string(b)
	})
	return got
}

// checkNow asks the registries for every image of the environment, as Check now does, and returns how many images were
// asked about and how many could not be read.
func (f *swarmFix) checkNow(t *testing.T) (checked, failed int) {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/updates/check", nil)
	var res struct {
		Checked int `json:"checked"`
		Failed  int `json:"failed"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &res) != nil {
		t.Fatalf("POST updates/check = %d %s", code, body)
	}
	return res.Checked, res.Failed
}

// previewUpdate is the preview of the update of one workload: whether it is offered and the codes of its warnings.
func (f *swarmFix) previewUpdate(t *testing.T, kind, name string) (offered bool, items []struct{ Name, FromDigest, ToDigest string }, warnings []string) {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/updates/preview?kind="+kind+"&name="+name, nil)
	var pv struct {
		Offered bool `json:"offered"`
		Items   []struct {
			Name       string `json:"name"`
			FromDigest string `json:"fromDigest"`
			ToDigest   string `json:"toDigest"`
		} `json:"items"`
		Warnings []struct {
			Code string `json:"code"`
		} `json:"warnings"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &pv) != nil {
		t.Fatalf("GET updates/preview of %s %s = %d %s", kind, name, code, body)
	}
	for _, it := range pv.Items {
		items = append(items, struct{ Name, FromDigest, ToDigest string }{it.Name, it.FromDigest, it.ToDigest})
	}
	for _, w := range pv.Warnings {
		warnings = append(warnings, w.Code)
	}
	return pv.Offered, items, warnings
}

// updateJob is a job of an image update as the route answers it.
type updateJob struct {
	State string `json:"state"`
	Items []struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"items"`
}

// startUpdate starts the update of a service with the digest the confirm showed, and returns the status and body of the
// answer. force is the choice to try again a version that failed.
func (f *swarmFix) startUpdate(t *testing.T, kind, name, expectKey, expectDigest string, force bool) (int, []byte) {
	t.Helper()
	body := map[string]any{"kind": kind, "name": name}
	if expectKey != "" {
		body["expect"] = map[string]string{expectKey: expectDigest}
	}
	if force {
		body["force"] = true
	}
	return f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/updates/run", body)
}

// updateJobID reads the job of a started update.
func updateJobID(t *testing.T, code int, body []byte) string {
	t.Helper()
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST updates/run = %d %s, want 200 with a job", code, body)
	}
	return started.JobID
}

// waitUpdateJob waits until the job of an image update ends and returns it, and how long it ran.
func (f *swarmFix) waitUpdateJob(t *testing.T, jobID string) (updateJob, time.Duration) {
	t.Helper()
	began := time.Now()
	var job updateJob
	f.srv.WaitFor("the update job "+jobID+" to end", updateWait, func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/update-jobs/"+jobID, nil)
		if code != http.StatusOK {
			return false, string(body)
		}
		job = updateJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	return job, time.Since(began)
}

// serviceImage is the image reference in the spec of a service, from the swarm itself.
func (f *swarmFix) serviceImage(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(sh(t, f.sw.Managers[0], "docker service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "+name))
}

// removeUpdateFixture removes a service through the API and the fixture's images from every node, so the case leaves the
// swarm as it found it.
func (f *swarmFix) removeUpdateFixture(t *testing.T, serviceID, serviceName string, refs ...string) {
	t.Helper()
	if code, body := f.srv.Do(http.MethodDelete, "/api/env/"+f.env.ID+"/services/"+serviceID, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE service %s = %d %s", serviceName, code, body)
	}
	f.srv.WaitFor("the service "+serviceName+" to leave the list", 30*time.Second, func() (bool, string) {
		f.srv.RefreshCluster(f.env.ID)
		_, ok := f.service(t, serviceName)
		return !ok, "still listed"
	})
	for _, n := range f.sw.Nodes() {
		for _, ref := range refs {
			sh(t, n, "docker image rm -f $(docker image ls -q --filter reference='"+ref+"*') >/dev/null 2>&1 || true")
		}
	}
}

// sharedImageUpdate checks a plain service of an image in the fixture registry, moves the tag, and updates the service in one
// click on a real swarm (plan 12-35, IMG-04, IMG-06, D-05): the check reads up to date; after the tag is pushed again with
// another build it reads update available with the new digest, and the preview offers it and says the image defines no health
// check (read from the image on a node, by the reference the service runs); the update re-checks the registry, writes the new
// digest and ends done with both tasks running it; the list then reads up to date. The service and its images are removed.
func sharedImageUpdate(t *testing.T, f *swarmFix) {
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	dockertest.ForwardRegistry(t, mgr)
	ref := addr + "/upd/web:1"
	digestA := dockertest.PushVariantCmd(t, mgr, dockertest.ServiceTestImage, ref, "A", `["sleep","3600"]`, nil)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	addFixtureCredential(t, f.srv, addr, "upd")

	code, body := f.createService(t, quickServiceSpec("upd-web", ref, 2))
	var made createdService
	if code != http.StatusCreated || json.Unmarshal(body, &made) != nil || made.Digest != digestA {
		t.Fatalf("POST services = %d %s, want 201 pinned to %s", code, body, digestA)
	}
	f.waitTasksOn(t, made.ID, digestA, 2, taskWait)

	// The tag has not moved: the server asked the registry and reads the service as up to date.
	// The registry service of the fixture is a service of the swarm too, under a name that cannot resolve, so its check fails at once
	// and is not asserted on: only the service under test is.
	checked, failed := f.checkNow(t)
	t.Logf("the first check asked about %d images and could not read %d (the fixture registry's own service is one of the failures)", checked, failed)
	if u := f.waitUpdateState(t, "upd-web", "up_to_date", time.Minute); u.RunningDigest != digestA || u.RemoteDigest != digestA {
		t.Errorf("up to date, running %s, tag at %s, want both %s", u.RunningDigest, u.RemoteDigest, digestA)
	}

	// The tag is pushed again with another build: the next check sees it.
	digestB := dockertest.PushVariantCmd(t, mgr, dockertest.ServiceTestImage, ref, "B", `["sleep","3600"]`, nil)
	if digestB == digestA {
		t.Fatalf("the two builds have one digest %s", digestA)
	}
	f.checkNow(t)
	if u := f.waitUpdateState(t, "upd-web", "update_available", time.Minute); u.RunningDigest != digestA || u.RemoteDigest != digestB {
		t.Errorf("update available, running %s, tag at %s, want %s and %s", u.RunningDigest, u.RemoteDigest, digestA, digestB)
	}
	offered, items, warnings := f.previewUpdate(t, "service", "upd-web")
	if !offered || len(items) != 1 || items[0].FromDigest != digestA || items[0].ToDigest != digestB {
		t.Errorf("the preview offers %v with items %+v, want one item from %s to %s", offered, items, digestA, digestB)
	}
	// The image of this build defines no health check. The warning is also what a preview says when the image cannot be read, so
	// the image that does define one (sharedImageUpdateHealth) is the proof that the read by reference works.
	if !slices.Contains(warnings, "no_healthcheck") {
		t.Errorf("the preview's warnings are %v, want no_healthcheck for an image without a health check", warnings)
	}

	// The update, as the confirm sends it: the digest the person saw.
	code, body = f.startUpdate(t, "service", "upd-web", "upd-web", digestB, false)
	jobID := updateJobID(t, code, body)
	job, took := f.waitUpdateJob(t, jobID)
	raw, _ := json.Marshal(job)
	if job.State != "done" {
		t.Fatalf("the update job ended %s, want done: %s", job.State, raw)
	}
	t.Logf("the update of upd-web to %s took %s: %s", digestB, took.Round(time.Second), raw)
	if got := f.serviceImage(t, "upd-web"); got != ref+"@"+digestB {
		t.Errorf("the service runs %q, want %s@%s", got, ref, digestB)
	}
	f.waitTasksOn(t, made.ID, digestB, 2, taskWait)
	if u := f.waitUpdateState(t, "upd-web", "up_to_date", time.Minute); u.RunningDigest != digestB {
		t.Errorf("after the update the service runs %s, want %s", u.RunningDigest, digestB)
	}
	rows := f.srv.AuditRows("image.updated")
	if len(rows) != 1 || rows[0].Result != "success" {
		t.Errorf("image.updated audit rows = %+v, want one success", rows)
	}
	f.removeUpdateFixture(t, made.ID, "upd-web", addr+"/upd/web")
}
