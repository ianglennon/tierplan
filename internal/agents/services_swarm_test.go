// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build swarm

package agents_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/registry"
)

// The service suites (plan 06-37, RES-02 to RES-05, RES-11, D-36) run the phase's service routes through real
// agents on a swarm of three managers and a worker. The images the services run live in a fixture registry
// published on the ingress at 127.0.0.1:5000 (never localhost: Engine 24 dials that as ::1 first), because a
// new service is never created unpinned and so its tag has to resolve to a digest.

const (
	// rolloutWait is how long a rolling update or a rollback of a few tasks gets to end, pulls included.
	rolloutWait = 4 * time.Minute
	// taskWait is how long tasks get to be running, a pull on each node included.
	taskWait = 4 * time.Minute
)

// quickServiceSpec is a spec of replicas tasks of image that sleep, stop in a second when asked and roll
// quickly: a monitor of a second after each task, and all the tasks of an update at once. A shell or sleep as
// PID 1 ignores SIGTERM, so without the grace period each replaced task would take ten seconds to stop.
func quickServiceSpec(name, image string, replicas uint64) swarm.ServiceSpec {
	grace := time.Second
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: image, Command: []string{"sleep", "3600"}, StopGracePeriod: &grace},
		},
		Mode:           swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		UpdateConfig:   &swarm.UpdateConfig{Parallelism: 3, Monitor: time.Second},
		RollbackConfig: &swarm.UpdateConfig{Parallelism: 3, Monitor: time.Second},
	}
}

// specView is the answer of the spec route.
type specView struct {
	Version      uint64         `json:"version"`
	Spec         map[string]any `json:"spec"`
	PreviousSpec map[string]any `json:"previousSpec"`
}

// specOf reads the spec of a service as the editor does.
func (f *swarmFix) specOf(t *testing.T, id string) specView {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/services/"+id+"/spec", nil)
	var v specView
	if code != http.StatusOK || json.Unmarshal(body, &v) != nil {
		t.Fatalf("GET spec of %s = %d %s", id, code, body)
	}
	return v
}

// containerSpecOf is the ContainerSpec object of a spec read as JSON.
func containerSpecOf(t *testing.T, spec map[string]any) map[string]any {
	t.Helper()
	tt, _ := spec["TaskTemplate"].(map[string]any)
	cs, _ := tt["ContainerSpec"].(map[string]any)
	if cs == nil {
		t.Fatalf("the spec has no TaskTemplate.ContainerSpec: %v", spec)
	}
	return cs
}

// serviceTasks are the running-desired tasks of a service as `docker service ps` lists them on a manager:
// the node, the image the task runs (not truncated, so with its digest) and the state.
type serviceTask struct{ Node, Image, State string }

func (f *swarmFix) serviceTasks(t *testing.T, id string) ([]serviceTask, string) {
	t.Helper()
	out, err := f.sw.Managers[0].Sh(execCtx(t, time.Minute),
		"docker service ps --no-trunc --filter desired-state=running --format '{{.Node}}|{{.Image}}|{{.CurrentState}}' "+id)
	if err != nil {
		return nil, err.Error()
	}
	var tasks []serviceTask
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if p := strings.SplitN(line, "|", 3); len(p) == 3 {
			tasks = append(tasks, serviceTask{Node: p[0], Image: p[1], State: p[2]})
		}
	}
	return tasks, out
}

// waitTasksOn waits until the service has want tasks that should run, all running and all on the image whose
// digest is given, and returns the hosts they run on.
func (f *swarmFix) waitTasksOn(t *testing.T, id, digest string, want int, d time.Duration) []string {
	t.Helper()
	var hosts []string
	dockertest.Wait(t, d, "service "+id+" to have its tasks running on "+digest, func() (bool, string) {
		tasks, seen := f.serviceTasks(t, id)
		hosts = hosts[:0]
		for _, tk := range tasks {
			if !strings.HasPrefix(tk.State, "Running") || !strings.Contains(tk.Image, digest) {
				return false, seen
			}
			hosts = append(hosts, tk.Node)
		}
		return len(tasks) == want, seen
	})
	return hosts
}

// nodeNamed is the harness node with this engine hostname.
func (f *swarmFix) nodeNamed(t *testing.T, host string) *dockertest.Node {
	t.Helper()
	for _, n := range f.sw.Nodes() {
		if n.Hostname == host {
			return n
		}
	}
	t.Fatalf("no node named %s among %v", host, f.hostnames())
	return nil
}

// auditOf asserts that the action has exactly one audit row, that it is a success about this service, and returns it.
func (f *swarmFix) auditOne(t *testing.T, action, serviceID string) map[string]string {
	t.Helper()
	rows := f.srv.AuditRows(action)
	if len(rows) != 1 {
		t.Fatalf("%s audit rows = %+v, want exactly one", action, rows)
	}
	if rows[0].Result != "success" || rows[0].TargetID != serviceID {
		t.Errorf("%s audit row = %+v, want a success about service %s", action, rows[0], serviceID)
	}
	return rows[0].Details
}

// sharedServiceLifecycle runs a service through the whole of its life by the API, on a real swarm of three
// managers and a worker with an agent on each (plan 06-37, RES-02, RES-03, RES-04, RES-05, D-27, P5 D-27): it is
// created from an image in a registry that wants a sign-in, with the one stored credential and no docker login
// anywhere, and comes out pinned to tag@sha256 with the digest the push answered; scaled to three tasks, which
// land on nodes that never had the image; edited; force-updated after the tag was pushed again, which moves it
// to the new digest; rolled back to the spec before; and removed. Each action leaves one audit row.
func sharedServiceLifecycle(t *testing.T, f *swarmFix) {
	const user, password = "bot", "lifecycle-pass-0123456789"
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{Htpasswd: dockertest.HtpasswdFor(user, password)})
	ref := addr + "/private/web:1"
	login := &dockertest.Login{User: user, Password: password}
	first := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref, "first", login)
	// Signed in for the push only. Nobody has the image locally: every node that runs a task must pull it with
	// what the engine carries from the create, the scale, the update and the rollback.
	dockertest.Logout(t, mgr)
	sh(t, mgr, "docker image rm -f "+ref)
	f.install(t)
	f.waitSwarmOnline(t, 4)

	creds := registry.NewStore(f.srv.Core().Store(), f.srv.Core().Vault(), nil)
	if _, err := creds.Create(context.Background(), registry.CreateInput{
		Name: "lifecycle-bot", Type: registry.TypeGeneric, Host: addr, Prefix: "private", Username: user, Secret: password,
	}, "test", nil); err != nil {
		t.Fatal(err)
	}
	base := "/api/env/" + f.env.ID

	// Create: pinned to the digest the push answered, and running.
	code, body := f.createService(t, quickServiceSpec("life-web", ref, 1))
	if code != http.StatusCreated {
		t.Fatalf("POST services = %d %s", code, body)
	}
	var made createdService
	if err := json.Unmarshal(body, &made); err != nil {
		t.Fatal(err)
	}
	if made.ID == "" || made.Name != "life-web" || !made.Pinned || made.Digest != first || made.Image != ref+"@"+first {
		t.Fatalf("created service = %+v, want %s pinned to the pushed digest %s", made, ref, first)
	}
	if spec := strings.TrimSpace(sh(t, mgr, "docker service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "+made.ID)); spec != ref+"@"+first {
		t.Errorf("the swarm holds the image %q, want %s", spec, ref+"@"+first)
	}
	f.waitTasksOn(t, made.ID, first, 1, taskWait)
	if name := f.auditOne(t, "service.created", made.ID)["name"]; name != "life-web" {
		t.Errorf("service.created details name = %q", name)
	}

	// Scale to three: tasks on three different nodes, none of which ever signed in.
	code, body = f.srv.Do(http.MethodPost, base+"/services/scale", map[string]any{"targets": []map[string]any{{"serviceId": made.ID, "replicas": 3}}})
	var scaled struct {
		Results []struct {
			Result   string `json:"result"`
			Changed  bool   `json:"changed"`
			Previous *int64 `json:"previous"`
		} `json:"results"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &scaled) != nil || len(scaled.Results) != 1 ||
		scaled.Results[0].Result != "done" || !scaled.Results[0].Changed || scaled.Results[0].Previous == nil || *scaled.Results[0].Previous != 1 {
		t.Fatalf("POST scale = %d %s, want done, changed, from 1", code, body)
	}
	hosts := f.waitTasksOn(t, made.ID, first, 3, taskWait)
	distinct := slices.Clone(hosts)
	slices.Sort(distinct)
	if distinct = slices.Compact(distinct); len(distinct) < 2 {
		t.Errorf("the three tasks run on %v: want them spread over several nodes, each pulling the private image with the carried credential", hosts)
	}
	for _, h := range distinct {
		if got := strings.TrimSpace(sh(t, f.nodeNamed(t, h), "docker image inspect -f '{{.Id}}' "+ref+"@"+first)); !strings.HasPrefix(got, "sha256:") {
			t.Errorf("%s runs a task but does not have %s: %q", h, ref+"@"+first, got)
		}
	}
	if d := f.auditOne(t, "service.scaled", made.ID); d["replicas"] != "3" || d["previous"] != "1" {
		t.Errorf("service.scaled details = %v, want 3 from 1", d)
	}

	// Edit: the environment of the spec, applied with the version that was read.
	v := f.specOf(t, made.ID)
	containerSpecOf(t, v.Spec)["Env"] = []any{"MODE=two"}
	code, body = f.srv.Do(http.MethodPost, base+"/services/"+made.ID+"/apply", map[string]any{"version": v.Version, "spec": v.Spec})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s", code, body)
	}
	f.waitRollout(t, made.ID, "completed", rolloutWait)
	f.waitTasksOn(t, made.ID, first, 3, taskWait)
	if env := strings.TrimSpace(sh(t, mgr, "docker service inspect -f '{{json .Spec.TaskTemplate.ContainerSpec.Env}}' "+made.ID)); env != `["MODE=two"]` {
		t.Errorf("the service's env is %s after the edit", env)
	}
	f.auditOne(t, "service.updated", made.ID)

	// The tag is pushed again: same name, new digest. A force update picks it up.
	second := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref, "second", login)
	dockertest.Logout(t, mgr)
	sh(t, mgr, "docker image rm -f "+ref)
	if second == first {
		t.Fatalf("pushing another image under %s answered the same digest %s", ref, first)
	}
	code, body = f.srv.Do(http.MethodPost, base+"/services/"+made.ID+"/force-update", map[string]any{})
	var forced struct {
		Image         string `json:"image"`
		Digest        string `json:"digest"`
		Pinned        bool   `json:"pinned"`
		PreviousImage string `json:"previousImage"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &forced) != nil || !forced.Pinned || forced.Digest != second ||
		forced.Image != ref+"@"+second || forced.PreviousImage != ref+"@"+first {
		t.Fatalf("POST force-update = %d %s, want %s pinned to the new digest %s", code, body, ref, second)
	}
	f.waitRollout(t, made.ID, "completed", rolloutWait)
	f.waitTasksOn(t, made.ID, second, 3, taskWait)
	f.auditOne(t, "service.force_updated", made.ID)
	if item := f.waitService(t, "life-web", "to show a spec it can go back to", time.Minute, func(it svcItem) bool { return it.HasPreviousSpec }); item.Image != ref+"@"+second {
		t.Errorf("the list shows the image %q after the force update, want %s", item.Image, ref+"@"+second)
	}

	// Rollback: a version that is not the current one is refused, the current one goes back to the spec before.
	v = f.specOf(t, made.ID)
	if v.PreviousSpec == nil {
		t.Fatal("the spec route shows no previous spec after the updates")
	}
	if code, body = f.srv.Do(http.MethodPost, base+"/services/"+made.ID+"/rollback", map[string]any{"version": v.Version - 1}); code != http.StatusConflict || problemType(body) != "service_changed" {
		t.Errorf("POST rollback with a stale version = %d %s, want 409 service_changed", code, body)
	}
	if len(f.srv.AuditRows("service.rolled_back")) != 1 {
		t.Errorf("a refused rollback left %d audit rows, want one (the refusal)", len(f.srv.AuditRows("service.rolled_back")))
	}
	code, body = f.srv.Do(http.MethodPost, base+"/services/"+made.ID+"/rollback", map[string]any{"version": v.Version})
	var back struct {
		Image string `json:"image"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &back) != nil || back.Image != ref+"@"+first {
		t.Fatalf("POST rollback = %d %s, want 202 going back to %s", code, body, ref+"@"+first)
	}
	f.waitRollout(t, made.ID, "rollback_completed", rolloutWait)
	f.waitTasksOn(t, made.ID, first, 3, taskWait)
	var rolled int
	for _, r := range f.srv.AuditRows("service.rolled_back") {
		if r.Result == "success" && r.TargetID == made.ID {
			rolled++
		}
	}
	if rolled != 1 {
		t.Errorf("successful service.rolled_back rows = %d, want one", rolled)
	}

	// Remove: the service and its tasks go, and one audit row says so.
	if code, body = f.srv.Do(http.MethodDelete, base+"/services/"+made.ID, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE service = %d %s", code, body)
	}
	f.srv.WaitFor("the service to leave the list", 30*time.Second, func() (bool, string) {
		f.srv.RefreshCluster(f.env.ID)
		_, ok := f.service(t, "life-web")
		return !ok, "still listed"
	})
	if out := strings.TrimSpace(sh(t, mgr, "docker service ls --filter name=life-web -q")); out != "" {
		t.Errorf("docker service ls still lists the service: %s", out)
	}
	f.auditOne(t, "service.removed", made.ID)
	for _, n := range f.sw.Nodes() {
		if dockertest.HasClientConfig(t, n) {
			t.Errorf("%s has a Docker client configuration after the lifecycle: nothing may log in", n.Hostname)
		}
	}
}

// sharedServiceAutoRollback updates a service whose failure action is rollback onto a command that fails at once
// (plan 06-37, RES-03, D-23): the first replaced task fails inside the monitor window, the swarm rolls the update
// back and the rollout route reports rollback_completed, with the error of the task that failed. The service
// ends on its first spec with its tasks running, and the list counts it among the services with a problem.
func sharedServiceAutoRollback(t *testing.T, f *swarmFix) {
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	ref := addr + "/public/web:1"
	digest := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref, "rollback", nil)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	base := "/api/env/" + f.env.ID

	spec := quickServiceSpec("auto-rb", ref, 3)
	spec.UpdateConfig = &swarm.UpdateConfig{Parallelism: 1, FailureAction: swarm.UpdateFailureActionRollback, Monitor: 3 * time.Second}
	spec.RollbackConfig = &swarm.UpdateConfig{Parallelism: 3, Monitor: time.Second}
	code, body := f.createService(t, spec)
	var made createdService
	if code != http.StatusCreated || json.Unmarshal(body, &made) != nil || made.Digest != digest {
		t.Fatalf("POST services = %d %s, want 201 pinned to %s", code, body, digest)
	}
	f.waitTasksOn(t, made.ID, digest, 3, taskWait)

	// The update: the same image with a command that exits 1 at once.
	v := f.specOf(t, made.ID)
	containerSpecOf(t, v.Spec)["Command"] = []any{"sh", "-c", "echo this update fails on purpose; exit 1"}
	code, body = f.srv.Do(http.MethodPost, base+"/services/"+made.ID+"/apply", map[string]any{"version": v.Version, "spec": v.Spec})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s", code, body)
	}
	rolled := f.waitRollout(t, made.ID, "rollback_completed", rolloutWait)
	t.Logf("rollout after the failed update: %+v", rolled)
	if rolled.LatestTaskError == "" {
		t.Errorf("rollout latestTaskError is empty, want the error of the task that failed")
	}
	// The swarm calls the rollback completed while the last tasks it replaced are still starting, so what the
	// rollout counts at that moment is not asserted: the wait below is for every task on the restored template.
	// The service is back on its first command, with three tasks running on the old template.
	f.waitTasksOn(t, made.ID, digest, 3, taskWait)
	f.srv.WaitFor("every task to run the template the rollback restored", 2*time.Minute, func() (bool, string) {
		r := f.serviceRollout(t, made.ID)
		b, _ := json.Marshal(r)
		return r.State == "rollback_completed" && r.Updated == 3 && r.Total == 3, string(b)
	})
	if cmd := strings.TrimSpace(sh(t, mgr, "docker service inspect -f '{{json .Spec.TaskTemplate.ContainerSpec.Command}}' "+made.ID)); cmd != `["sleep","3600"]` {
		t.Errorf("the service's command is %s after the rollback, want its first one", cmd)
	}
	// The list is the server's own read of the cluster, taken again each time round the wait.
	f.waitService(t, "auto-rb", "to show the update state rollback_completed", time.Minute, func(it svcItem) bool { return it.UpdateState == "rollback_completed" })
	code, body = f.srv.Do(http.MethodGet, base+"/services?status=problem", nil)
	var problems struct {
		Items []svcItem `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &problems) != nil || len(problems.Items) != 1 || problems.Items[0].Name != "auto-rb" {
		t.Errorf("GET services?status=problem = %d %s, want the one service whose update was rolled back", code, body)
	}
}

// sseEvent is one event of an event stream.
type sseEvent struct{ Name, Data string }

// readSSE opens an event stream as the signed-in admin and hands each event to handle until it answers true, the
// stream ends or the context is done. It fails the test when the stream cannot be opened or ends without handle
// having answered true.
func (f *swarmFix) readSSE(t *testing.T, ctx context.Context, path string, handle func(sseEvent) bool) {
	t.Helper()
	client := *f.srv.Client() // the same trust; a stream has no whole-request timeout
	client.Timeout = 0
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(f.srv.Cookie())
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET %s = %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var ev sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			ev.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if ev.Name != "" && handle(ev) {
				return
			}
			ev = sseEvent{}
		}
	}
	t.Fatalf("the stream %s ended before the awaited event (%v)", path, sc.Err())
}

// sharedStatsFromWorker streams the stats of a service whose only task can run on the worker (plan 06-37,
// RES-11, D-29): the worker is reachable only through its own agent, and the stream names the task on the worker's
// host and sums a sample of its memory, counted and not silent.
func sharedStatsFromWorker(t *testing.T, f *swarmFix) {
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	ref := addr + "/public/load:1"
	digest := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref, "stats", nil)
	f.install(t)
	f.waitSwarmOnline(t, 4)

	spec := quickServiceSpec("stats-w", ref, 1)
	spec.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{"node.role==worker"}}
	// A task that uses a little CPU and holds some memory, so a sample has something to say.
	spec.TaskTemplate.ContainerSpec.Command = []string{"sh", "-c", "head -c 8000000 /dev/zero | tr '\\0' 'x' > /dev/shm/held; while true; do i=0; while [ $i -lt 20000 ]; do i=$((i+1)); done; sleep 0.2; done"}
	code, body := f.createService(t, spec)
	var made createdService
	if code != http.StatusCreated || json.Unmarshal(body, &made) != nil {
		t.Fatalf("POST services = %d %s", code, body)
	}
	hosts := f.waitTasksOn(t, made.ID, digest, 1, taskWait)
	if len(hosts) != 1 || hosts[0] != worker.Hostname {
		t.Fatalf("the task runs on %v, want only the worker %s", hosts, worker.Hostname)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	type taskRef struct {
		ID   string `json:"id"`
		Node string `json:"node"`
	}
	var ready struct {
		Tasks []taskRef `json:"tasks"`
	}
	var sample struct {
		Total struct {
			Mem     uint64 `json:"mem"`
			Counted int    `json:"counted"`
			Silent  int    `json:"silent"`
		} `json:"total"`
		Tasks []struct {
			ID     string  `json:"id"`
			Mem    *uint64 `json:"mem"`
			NoData bool    `json:"noData"`
		} `json:"tasks"`
	}
	f.readSSE(t, ctx, "/api/env/"+f.env.ID+"/services/"+made.ID+"/stats/stream", func(ev sseEvent) bool {
		switch ev.Name {
		case "ready":
			if err := json.Unmarshal([]byte(ev.Data), &ready); err != nil {
				t.Fatalf("ready event %q: %v", ev.Data, err)
			}
		case "sample":
			if err := json.Unmarshal([]byte(ev.Data), &sample); err != nil {
				t.Fatalf("sample event %q: %v", ev.Data, err)
			}
			// The first rounds can come before the worker's agent has answered.
			return sample.Total.Counted == 1 && sample.Total.Mem > 0
		}
		return false
	})
	if len(ready.Tasks) != 1 || ready.Tasks[0].Node != worker.Hostname {
		t.Errorf("ready event tasks = %+v, want the one task on the worker %s", ready.Tasks, worker.Hostname)
	}
	if sample.Total.Silent != 0 || len(sample.Tasks) != 1 || sample.Tasks[0].ID != ready.Tasks[0].ID || sample.Tasks[0].NoData ||
		sample.Tasks[0].Mem == nil || *sample.Tasks[0].Mem == 0 {
		t.Errorf("sample = %+v, want the worker's task counted with its memory", sample)
	}
	t.Logf("sample from the worker's agent: %+v", sample)
}

// TestSwarmHostPolicyRefused sends a service whose task would mount the whole host to managers whose agents were
// installed as the generated command says, with no opt-in (plan 06-37, RES-02, D-36): an Admin's create is refused
// 409 agent_host_policy by the agent and nothing is made, a service that already has host access cannot be scaled
// either, and the refusal is audited. With the one setting a host owner adds, the same create goes through.
func TestSwarmHostPolicyRefused(t *testing.T) {
	hostBind := func(name, image string) swarm.ServiceSpec {
		spec := quickServiceSpec(name, image, 1)
		spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/", Target: "/host", ReadOnly: true}}
		return spec
	}

	t.Run("refused without the opt-in", func(t *testing.T) {
		f := startSwarmFix(t, 3, 0)
		mgr := f.sw.Managers[0]
		addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
		ref := addr + "/public/host:1"
		dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref, "policy", nil)
		f.install(t)
		f.waitSwarmOnline(t, 3)
		base := "/api/env/" + f.env.ID

		// Control: an ordinary spec is made.
		if code, body := f.createService(t, quickServiceSpec("policy-plain", ref, 1)); code != http.StatusCreated {
			t.Fatalf("POST services of an ordinary spec = %d %s, want 201", code, body)
		}
		// A bind of / is refused by the agent, whoever the caller is.
		code, body := f.createService(t, hostBind("policy-host", ref))
		if code != http.StatusConflict || problemType(body) != "agent_host_policy" {
			t.Fatalf("POST services with a bind of / = %d %s, want 409 agent_host_policy", code, body)
		}
		if out := strings.TrimSpace(sh(t, mgr, "docker service ls --filter name=policy-host -q")); out != "" {
			t.Errorf("a refused create left the service %s", out)
		}
		var refused bool
		for _, r := range f.srv.AuditRows("service.created") {
			refused = refused || (r.Result == "failure" && r.Details["code"] == "agent_host_policy" && r.Details["name"] == "policy-host")
		}
		if !refused {
			t.Errorf("service.created rows = %+v, want one failure with the code agent_host_policy", f.srv.AuditRows("service.created"))
		}

		// A service that has host access already (made on the manager, not through Coaming) cannot be scaled,
		// because every update carries the whole spec.
		sh(t, mgr, "docker service create --name policy-existing --no-resolve-image --detach --replicas 0 --mount type=bind,src=/,dst=/host,ro "+dockertest.ServiceTestImage+" sleep 3600")
		code, body = f.srv.Do(http.MethodPost, base+"/services/scale", map[string]any{"targets": []map[string]any{{"serviceId": "policy-existing", "replicas": 1}}})
		var scaled struct {
			Results []struct{ Result, Code string } `json:"results"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &scaled) != nil || len(scaled.Results) != 1 || scaled.Results[0].Result != "failed" || scaled.Results[0].Code != "agent_host_policy" {
			t.Errorf("POST scale of a service with a bind of / = %d %s, want failed with agent_host_policy", code, body)
		}
		if n := strings.TrimSpace(sh(t, mgr, "docker service inspect -f '{{.Spec.Mode.Replicated.Replicas}}' policy-existing")); n != "0" {
			t.Errorf("the refused scale left %s replicas", n)
		}
	})

	t.Run("allowed with the opt-in", func(t *testing.T) {
		f := startSwarmFix(t, 1, 0)
		mgr := f.sw.Managers[0]
		addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
		ref := addr + "/public/host:1"
		digest := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref, "policy", nil)
		f.installPrivileged(t)
		f.waitSwarmOnline(t, 1)

		code, body := f.createService(t, hostBind("policy-host", ref))
		if code != http.StatusCreated {
			t.Fatalf("POST services with a bind of / and the opt-in set = %d %s, want 201", code, body)
		}
		var made createdService
		if err := json.Unmarshal(body, &made); err != nil {
			t.Fatal(err)
		}
		f.waitTasksOn(t, made.ID, digest, 1, taskWait)
		code, body = f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/services/scale", map[string]any{"targets": []map[string]any{{"serviceId": made.ID, "replicas": 2}}})
		var scaled struct {
			Results []struct{ Result string } `json:"results"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &scaled) != nil || len(scaled.Results) != 1 || scaled.Results[0].Result != "done" {
			t.Errorf("POST scale of a service with host access and the opt-in set = %d %s, want done", code, body)
		}
	})
}
