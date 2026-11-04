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

	"github.com/moby/moby/api/types/swarm"

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
	// wall-clock: the time an update took is measured for the log (RESEARCH A2 and A5); nothing waits on it
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

// imageUpdateHealthCase is the name of the shared case that needs a server with the shortest converge timeout (sharedCaseOptions);
// it is the name sharedSwarmCases lists the case by.
const imageUpdateHealthCase = "ImageUpdateHealth"

// goodBuild and badBuild are the Dockerfile instructions of a build that runs on and of one whose command exits at once. A
// service that is to follow its image's command is made without a command of its own (imageSpec), so the build decides.
const (
	goodBuild = `CMD ["sleep","3600"]`
	badBuild  = `CMD ["sh","-c","echo this build fails on purpose; exit 1"]`
)

// imageSpec is quickServiceSpec of replicas tasks with no command of its own, so the image's command decides whether a task
// runs on.
func imageSpec(name, image string, replicas uint64) swarm.ServiceSpec {
	spec := quickServiceSpec(name, image, replicas)
	spec.TaskTemplate.ContainerSpec.Command = nil
	return spec
}

// auditCount is how many audit rows the action has with the result.
func (f *swarmFix) auditCount(action, result string) int {
	n := 0
	for _, r := range f.srv.AuditRows(action) {
		if r.Result == result {
			n++
		}
	}
	return n
}

// sharedImageUpdateRollback updates a plain service onto a build whose command exits at once, on a real swarm (plan 12-35,
// IMG-07, D-12, D-14): the new tasks fail, TierPlan puts the previous digest back itself, the run ends rolled back with the
// reason, the service runs the old digest on both tasks and its badge reads rolled back with the digest that failed. An update
// to that digest is then refused without force (409 previously_failed) and nothing changes.
func sharedImageUpdateRollback(t *testing.T, f *swarmFix) {
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	dockertest.ForwardRegistry(t, mgr)
	ref := addr + "/upd/rb:1"
	digestA := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, ref, "A", nil, goodBuild)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	addFixtureCredential(t, f.srv, addr, "upd")

	code, body := f.createService(t, imageSpec("upd-rb", ref, 2))
	var made createdService
	if code != http.StatusCreated || json.Unmarshal(body, &made) != nil || made.Digest != digestA {
		t.Fatalf("POST services = %d %s, want 201 pinned to %s", code, body, digestA)
	}
	f.waitTasksOn(t, made.ID, digestA, 2, taskWait)

	// The tag moves to a build that exits at once.
	digestC := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, ref, "C", nil, badBuild)
	f.checkNow(t)
	f.waitUpdateState(t, "upd-rb", "update_available", time.Minute)

	code, body = f.startUpdate(t, "service", "upd-rb", "upd-rb", digestC, false)
	jobID := updateJobID(t, code, body)
	job, took := f.waitUpdateJob(t, jobID)
	raw, _ := json.Marshal(job)
	t.Logf("the failing update of upd-rb to %s ended %s after %s: %s", digestC, job.State, took.Round(time.Second), raw)
	if job.State == "done" {
		t.Fatalf("an update to a build that exits at once ended done: %s", raw)
	}

	// The previous digest is back on both tasks, and the badge says what happened.
	if got := f.serviceImage(t, "upd-rb"); got != ref+"@"+digestA {
		t.Errorf("after the rollback the service runs %q, want %s@%s", got, ref, digestA)
	}
	f.waitTasksOn(t, made.ID, digestA, 2, updateWait)
	u := f.waitUpdateState(t, "upd-rb", "rolled_back", time.Minute)
	if u.RunningDigest != digestA || u.RemoteDigest != digestC || u.Failure == nil || u.Failure.Digest != digestC || u.Failure.Reason != "didnt_start" || u.Failure.RollbackFailed {
		b, _ := json.Marshal(u)
		t.Errorf("the badge after the rollback = %s, want rolled_back, running %s, the tag at %s and a failure of %s (didnt_start)", b, digestA, digestC, digestC)
	}
	if n := f.auditCount("image.update_failed", "failure"); n != 1 {
		t.Errorf("image.update_failed failure rows = %d, want one", n)
	}
	if n := f.auditCount("image.update_rolled_back", "success"); n != 1 {
		t.Errorf("image.update_rolled_back success rows = %d, want one", n)
	}

	// The version that failed is not offered again, and a plain request for it changes nothing.
	code, body = f.startUpdate(t, "service", "upd-rb", "upd-rb", digestC, false)
	if code != http.StatusConflict || !strings.Contains(string(body), "previously_failed") {
		t.Errorf("an unforced update to the version that failed = %d %s, want 409 previously_failed", code, body)
	}
	if got := f.serviceImage(t, "upd-rb"); got != ref+"@"+digestA {
		t.Errorf("after the refusal the service runs %q, want %s@%s", got, ref, digestA)
	}
	f.removeUpdateFixture(t, made.ID, "upd-rb", addr+"/upd/rb")
}

// sharedImageUpdateHealth updates a service whose image has a health check onto a build whose check never passes (plan 12-35,
// IMG-06, IMG-07, RESEARCH A2 and A5): the swarm keeps the new task starting, so the wait for the service to settle does not
// end well, and TierPlan judges the update a failure within the window the server is given, puts the previous digest back and
// reads the failure as the new version not becoming healthy. The time to failure is logged for RESEARCH. The same case holds the
// read of an image's health check by the reference a service runs (12-16): the preview of an update of this service does not
// warn that there is no health check, which it would if the image could not be read.
func sharedImageUpdateHealth(t *testing.T, f *swarmFix) {
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	dockertest.ForwardRegistry(t, mgr)
	ref := addr + "/upd/hc:1"
	healthy := "HEALTHCHECK --interval=2s --timeout=2s --retries=1 CMD true"
	unhealthy := "HEALTHCHECK --interval=2s --timeout=2s --retries=1 CMD false"
	digestA := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, ref, "A", nil, goodBuild, healthy)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	addFixtureCredential(t, f.srv, addr, "upd")

	code, body := f.createService(t, imageSpec("upd-hc", ref, 1))
	var made createdService
	if code != http.StatusCreated || json.Unmarshal(body, &made) != nil || made.Digest != digestA {
		t.Fatalf("POST services = %d %s, want 201 pinned to %s", code, body, digestA)
	}
	f.waitTasksOn(t, made.ID, digestA, 1, taskWait)

	// A build that runs on but never becomes healthy.
	digestB := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, ref, "B", nil, goodBuild, unhealthy)
	f.checkNow(t)
	f.waitUpdateState(t, "upd-hc", "update_available", time.Minute)
	offered, _, warnings := f.previewUpdate(t, "service", "upd-hc")
	if !offered {
		t.Fatalf("the update of upd-hc is not offered")
	}
	if slices.Contains(warnings, "no_healthcheck") {
		t.Errorf("the preview's warnings are %v: the image the service runs defines a health check, read by its reference on a node, so there is no no_healthcheck", warnings)
	}

	code, body = f.startUpdate(t, "service", "upd-hc", "upd-hc", digestB, false)
	jobID := updateJobID(t, code, body)
	job, took := f.waitUpdateJob(t, jobID)
	raw, _ := json.Marshal(job)
	t.Logf("RESEARCH A2/A5: the update of upd-hc to a build that never turns healthy ended %s after %s (the server's converge timeout is 1m): %s", job.State, took.Round(time.Second), raw)
	if job.State == "done" {
		t.Fatalf("an update to a build that never becomes healthy ended done, so a settled service is not a healthy one: %s", raw)
	}
	if took > 3*time.Minute {
		t.Errorf("the failure took %s, want it inside the converge window of 1 minute plus the rollback", took)
	}
	if got := f.serviceImage(t, "upd-hc"); got != ref+"@"+digestA {
		t.Errorf("after the rollback the service runs %q, want %s@%s", got, ref, digestA)
	}
	f.waitTasksOn(t, made.ID, digestA, 1, updateWait)
	u := f.waitUpdateState(t, "upd-hc", "rolled_back", time.Minute)
	if u.Failure == nil || u.Failure.Digest != digestB || (u.Failure.Reason != "unhealthy" && u.Failure.Reason != "didnt_start") {
		b, _ := json.Marshal(u)
		t.Errorf("the badge after the rollback = %s, want rolled_back with a failure of %s that is unhealthy (or didnt_start)", b, digestB)
	} else {
		t.Logf("RESEARCH A2/A5: the failure reads %q", u.Failure.Reason)
	}
	f.removeUpdateFixture(t, made.ID, "upd-hc", addr+"/upd/hc")
}

// stackRevision is a revision of a stack as History lists it.
type stackRevision struct {
	Seq          int64  `json:"seq"`
	Origin       string `json:"origin"`
	RestoredFrom int64  `json:"restoredFrom"`
	Outcome      string `json:"outcome"`
	Deployed     bool   `json:"deployed"`
	ImageUpdate  bool   `json:"imageUpdate"`
}

// revisions are the revisions of the stack, newest first.
func (f *swarmFix) revisions(t *testing.T, stackID string) []stackRevision {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID+"/revisions", nil)
	var hist struct {
		Items []stackRevision `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &hist) != nil {
		t.Fatalf("GET the revisions of %s = %d %s", stackID, code, body)
	}
	return hist.Items
}

// waitStackUpdateJob waits until the deploy job of a stack's update, rollback included, ends, and returns it.
func (f *swarmFix) waitStackUpdateJob(t *testing.T, stackID, jobID string) endedStackJob {
	t.Helper()
	var job endedStackJob
	f.srv.WaitFor("the update job "+jobID+" of the stack to end", updateWait, func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID+"/deploys/"+jobID, nil)
		if code != http.StatusOK {
			return false, string(body)
		}
		job = endedStackJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	return job
}

// startStackUpdate starts an update through the update route and returns the job and the stack it belongs to.
func (f *swarmFix) startStackUpdate(t *testing.T, kind, name string, expect map[string]string) (jobID, stackID string) {
	t.Helper()
	body := map[string]any{"kind": kind, "name": name}
	if expect != nil {
		body["expect"] = expect
	}
	code, raw := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/updates/run", body)
	var started struct {
		JobID   string `json:"jobId"`
		StackID string `json:"stackId"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &started) != nil || started.JobID == "" || started.StackID == "" {
		t.Fatalf("POST updates/run of %s %s = %d %s, want 200 with a job and a stack", kind, name, code, raw)
	}
	return started.JobID, started.StackID
}

// sharedStackImageUpdate updates the services of a managed swarm stack through Update stack, then a failing build of one of them
// (plan 12-35, IMG-06, IMG-07, D-06, D-12): the first update makes revision #2, an image update that History labels so, with both
// services on the new digests; the second makes revision #3, which fails, and TierPlan puts the stack back with revision #4, a
// rollback of #2 that is the deployed one, with every service on the pin it had and the failed service's badge reading rolled back.
func sharedStackImageUpdate(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	dockertest.ForwardRegistry(t, mgr)
	addFixtureCredential(t, f.srv, addr, "upd")
	refWeb, refApp := addr+"/upd/sweb:1", addr+"/upd/sapp:1"
	webA := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, refWeb, "A", nil, goodBuild)
	appA := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, refApp, "A", nil, goodBuild)
	compose := "services:\n  web:\n    image: " + refWeb + "\n    stop_grace_period: 1s\n    deploy:\n      replicas: 1\n" +
		"  app:\n    image: " + refApp + "\n    stop_grace_period: 1s\n    deploy:\n      replicas: 1\n"
	const stack, web, app = "updstk", "updstk_web", "updstk_app"
	id := f.createStackTierPlan(t, stack, compose)
	t.Cleanup(func() { removeStackCLI(t, f, stack) })
	f.deployStackTierPlan(t, id)
	image := func(svc string) string { return serviceField(t, f, svc, "{{.Spec.TaskTemplate.ContainerSpec.Image}}") }
	if got := image(web); got != refWeb+"@"+webA {
		t.Fatalf("web runs %q, want %s@%s", got, refWeb, webA)
	}
	f.waitTasksOn(t, web, webA, 1, taskWait)
	f.waitTasksOn(t, app, appA, 1, taskWait)

	// Both tags move to good builds; Update stack takes both services.
	webB := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, refWeb, "B", nil, goodBuild)
	appB := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, refApp, "B", nil, goodBuild)
	f.checkNow(t)
	f.waitUpdateState(t, web, "update_available", time.Minute)
	f.waitUpdateState(t, app, "update_available", time.Minute)
	jobID, stackID := f.startStackUpdate(t, "stack", stack, map[string]string{web: webB, app: appB})
	if stackID != id {
		t.Fatalf("the update belongs to stack %s, want %s", stackID, id)
	}
	if job := f.waitStackUpdateJob(t, id, jobID); job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("Update stack ended %s, want done: %s", job.State, raw)
	}
	if got := image(web); got != refWeb+"@"+webB {
		t.Errorf("web runs %q after Update stack, want %s@%s", got, refWeb, webB)
	}
	if got := image(app); got != refApp+"@"+appB {
		t.Errorf("app runs %q after Update stack, want %s@%s", got, refApp, appB)
	}
	f.waitTasksOn(t, web, webB, 1, taskWait)
	f.waitTasksOn(t, app, appB, 1, taskWait)
	revs := f.revisions(t, id)
	if len(revs) != 2 || revs[0].Seq != 2 || revs[0].Origin != "update" || !revs[0].ImageUpdate || revs[0].Outcome != "good" || !revs[0].Deployed {
		t.Fatalf("History after Update stack = %+v, want revision #2 a good image update, deployed, above #1", revs)
	}

	// web's tag moves to a build that exits at once; app's does not move.
	webC := dockertest.PushVariantWith(t, mgr, dockertest.ServiceTestImage, refWeb, "C", nil, badBuild)
	f.checkNow(t)
	f.waitUpdateState(t, web, "update_available", time.Minute)
	jobID, _ = f.startStackUpdate(t, "service", web, map[string]string{web: webC})
	job := f.waitStackUpdateJob(t, id, jobID)
	raw, _ := json.Marshal(job)
	t.Logf("the failing update of the stack ended %s: %s", job.State, raw)
	if job.State == "done" {
		t.Fatalf("an update to a build that exits at once ended done: %s", raw)
	}
	f.awaitStackUnlocked(t, id)

	// The stack is back on revision #2's pins, as a revision of its own: a rollback of #2.
	if got := image(web); got != refWeb+"@"+webB {
		t.Errorf("after the rollback web runs %q, want %s@%s", got, refWeb, webB)
	}
	if got := image(app); got != refApp+"@"+appB {
		t.Errorf("after the rollback app runs %q, want %s@%s", got, refApp, appB)
	}
	f.waitTasksOn(t, web, webB, 1, updateWait)
	f.waitTasksOn(t, app, appB, 1, updateWait)
	revs = f.revisions(t, id)
	if len(revs) != 4 {
		t.Fatalf("History after the rollback = %+v, want four revisions", revs)
	}
	if r := revs[0]; r.Seq != 4 || r.Origin != "rollback" || r.RestoredFrom != 2 || r.Outcome != "good" || !r.Deployed {
		t.Errorf("revision #4 = %+v, want a good rollback of #2, deployed", r)
	}
	if r := revs[1]; r.Seq != 3 || r.Origin != "update" || r.Outcome != "failed" || r.Deployed {
		t.Errorf("revision #3 = %+v, want the failed image update, not deployed", r)
	}
	u := f.waitUpdateState(t, web, "rolled_back", time.Minute)
	if u.Failure == nil || u.Failure.Digest != webC || u.RunningDigest != webB {
		b, _ := json.Marshal(u)
		t.Errorf("web's badge after the rollback = %s, want rolled_back with a failure of %s and the service on %s", b, webC, webB)
	}
	if n := f.auditCount("image.update_rolled_back", "success"); n < 1 {
		t.Errorf("image.update_rolled_back success rows = %d, want at least one", n)
	}
	removeStackTierPlan(t, f, id, stack, engineObjects{})
}
