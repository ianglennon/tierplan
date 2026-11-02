// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// facadeFix is one dind engine that is a one-node swarm, with a registry on it and a dockerx client
// the way the product reaches a manager, over the engine's TCP port.
type facadeFix struct {
	node *dockertest.Node
	cli  *dockerx.Client
	raw  *client.Client
	// registry is how the node reaches its registry: the IPv4 loopback, since Engine 24 dials
	// "localhost" as ::1 first.
	registry string
}

// startFacadeFix starts the engine the tests of the swarm facades run against: a one-node swarm, the
// busybox image the services run and a registry holding nothing yet. The engine is TIERPLAN_TEST_ENGINE.
func startFacadeFix(t *testing.T) *facadeFix {
	t.Helper()
	node := dockertest.StartHost(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := node.Exec(ctx, "docker", "swarm", "init", "--advertise-addr", node.IP); err != nil {
		t.Fatalf("swarm init on %s: %v\n%s", node.Hostname, err, out)
	}
	dockertest.LoadImage(t, dockertest.ServiceTestImage, node)
	dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	cli, err := dockerx.New(dockertest.TCPTransportFor(node.Host()))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &facadeFix{node: node, cli: cli, raw: node.Client(t), registry: "127.0.0.1:5000"}
}

// tasksOf reads the tasks of one service the way the product does.
func (f *facadeFix) tasksOf(t *testing.T, ctx context.Context, id string) []dockerx.TaskRow {
	t.Helper()
	rows, err := f.cli.Tasks(ctx, dockerx.TaskFilter{ServiceIDs: []string{id}})
	if err != nil {
		t.Fatalf("Tasks of %s: %v", id, err)
	}
	return rows
}

// running are the tasks of the service that are running and meant to.
func running(rows []dockerx.TaskRow) []dockerx.TaskRow {
	var out []dockerx.TaskRow
	for _, r := range rows {
		if r.State == "running" && r.DesiredState == "running" {
			out = append(out, r)
		}
	}
	return out
}

// waitRunning polls the product's own task read until the service has want running tasks, all of them on
// the template the service names now, and returns them.
func (f *facadeFix) waitRunning(t *testing.T, ctx context.Context, id string, want int) []dockerx.TaskRow {
	t.Helper()
	var got []dockerx.TaskRow
	dockertest.Wait(t, 3*time.Minute, "service "+id+" to have its tasks running on the current template", func() (bool, string) {
		d, _, err := f.cli.ServiceInspect(ctx, id)
		if err != nil {
			return false, err.Error()
		}
		rows, err := f.cli.Tasks(ctx, dockerx.TaskFilter{ServiceIDs: []string{id}})
		if err != nil {
			return false, err.Error()
		}
		got = nil
		var seen []string
		for _, r := range running(rows) {
			if r.SpecHash == d.TemplateHash {
				got = append(got, r)
			}
		}
		for _, r := range rows {
			seen = append(seen, r.State+"->"+r.DesiredState+" "+r.Err)
		}
		return len(got) == want && len(running(rows)) == want, strings.Join(seen, "; ")
	})
	return got
}

// specOf is a service spec as the JSON a create or an update takes.
func specOf(t *testing.T, s swarm.ServiceSpec) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// decodeSpec reads a spec the facade returned.
func decodeSpec(t *testing.T, raw json.RawMessage) swarm.ServiceSpec {
	t.Helper()
	var s swarm.ServiceSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestServiceFacadeOnARealEngine drives every swarm service facade against a real engine (plan 06-37,
// RES-02, RES-03, RES-04, RES-05, RES-11): a service is created pinned to the digest its tag has, scaled,
// edited, force-updated after the tag was pushed again, rolled back and removed; tasks are read with the
// template hash that says which of them run the current version; the history limit is the cluster's; and
// a service log is cut per task, with the time and the task in front of every line. The fakes the unit
// tests use assume each of these answers (RESEARCH A7 on Engine 24, E1, E5, E8, E10, E16).
func TestServiceFacadeOnARealEngine(t *testing.T) {
	f := startFacadeFix(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	ref := f.registry + "/facade/app:1"
	first := dockertest.PushVariant(t, f.node, dockertest.ServiceTestImage, ref, "first", nil)

	// ResolveImage: the tag's digest as the registry holds it, the platforms the image is built for.
	res, err := f.cli.ResolveImage(ctx, ref, "")
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	if res.Digest != first || res.Ref != ref+"@"+first || res.Tag != "1" {
		t.Fatalf("ResolveImage = %+v, want the pushed digest %s pinned on %s", res, first, ref)
	}
	if _, err := f.cli.ResolveImage(ctx, f.registry+"/facade/app@"+first, ""); !errors.Is(err, dockerx.ErrDigestOnly) {
		t.Errorf("ResolveImage of a digest alone = %v, want ErrDigestOnly", err)
	}
	if _, err := f.cli.ResolveImage(ctx, f.registry+"/facade/missing:1", ""); err == nil {
		t.Error("ResolveImage of an image the registry does not have succeeded")
	} else if code := dockerx.PullFailure(err); code == "" {
		t.Errorf("ResolveImage of a missing image = %v, which PullFailure does not classify", err)
	}

	// A single-platform manifest pushed by the engine may come back with no platform list at all, which is
	// not an error: the service then has no platform constraint. Record what this engine says.
	t.Logf("ResolveImage platforms on engine %s: %v", dockertest.EngineFromEnv(), res.Platforms)

	// ServiceCreate.
	replicas := uint64(2)
	grace := time.Second
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "facade-web", Labels: map[string]string{"team": "a"}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: res.Ref, Command: []string{"sleep", "3600"}, Env: []string{"MODE=one"}, StopGracePeriod: &grace},
			Placement:     &swarm.Placement{Platforms: platformsOf(res.Platforms)},
		},
		// Sleep as PID 1 ignores SIGTERM, so without a short grace period every replaced task takes ten seconds to
		// stop; and the monitor after each task is one second, not five, so a rollout of three tasks is quick.
		UpdateConfig:   &swarm.UpdateConfig{Parallelism: 3, Monitor: time.Second},
		RollbackConfig: &swarm.UpdateConfig{Parallelism: 3, Monitor: time.Second},
		Mode:           swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}
	id, warnings, err := f.cli.ServiceCreate(ctx, specOf(t, spec), "")
	if err != nil {
		t.Fatalf("ServiceCreate: %v (warnings %v)", err, warnings)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = f.raw.ServiceRemove(c, id, client.ServiceRemoveOptions{})
	})
	if _, _, err := f.cli.ServiceCreate(ctx, specOf(t, spec), ""); !errors.Is(err, dockerx.ErrServiceNameTaken) {
		t.Errorf("ServiceCreate of a name that is taken = %v, want ErrServiceNameTaken", err)
	}
	if _, _, err := f.cli.ServiceCreate(ctx, json.RawMessage(`{"Name":"x","NoSuchField":1}`), ""); !errors.Is(err, dockerx.ErrServiceSpecInvalid) {
		t.Errorf("ServiceCreate of a spec with an unknown field = %v, want ErrServiceSpecInvalid", err)
	}
	bad := spec
	bad.Name = "facade-bad-network"
	bad.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "tp-no-such-network"}}
	var rejected *dockerx.SpecRejectedError
	if _, _, err := f.cli.ServiceCreate(ctx, specOf(t, bad), ""); !errors.As(err, &rejected) {
		t.Errorf("ServiceCreate naming a network that does not exist = %v, want a *SpecRejectedError", err)
	}

	// ServiceInspect.
	d, raw, err := f.cli.ServiceInspect(ctx, "facade-web")
	if err != nil {
		t.Fatalf("ServiceInspect by name: %v", err)
	}
	if d.ID != id || d.Name != "facade-web" || d.Mode != "replicated" || d.Image != res.Ref || d.Replicas != 2 || d.Version == 0 ||
		d.Stack != "" || d.Agent || d.Labels["team"] != "a" || d.PreviousSpec != nil || d.TemplateHash == "" || len(raw) == 0 {
		t.Fatalf("ServiceInspect = %+v", d)
	}
	if _, _, err := f.cli.ServiceInspect(ctx, "facade-nope"); !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Errorf("ServiceInspect of a missing service = %v, want ErrServiceNotFound", err)
	}

	// Tasks, and the hash that says a task runs the service's current template (RESEARCH E10).
	tasks := f.waitRunning(t, ctx, id, 2)
	slots := map[int]bool{}
	for _, tk := range tasks {
		slots[tk.Slot] = true
		if tk.ServiceID != id || tk.NodeID == "" || tk.ContainerID == "" || tk.SpecHash != d.TemplateHash {
			t.Errorf("task %+v, want a placed task with a container on the service's template %s", tk, d.TemplateHash)
		}
	}
	if len(slots) != 2 {
		t.Errorf("running slots = %v, want two", slots)
	}
	if _, err := f.cli.Tasks(ctx, dockerx.TaskFilter{}); !errors.Is(err, dockerx.ErrUnfilteredTasks) {
		t.Errorf("Tasks with no filter = %v, want ErrUnfilteredTasks", err)
	}
	if byNode, err := f.cli.Tasks(ctx, dockerx.TaskFilter{NodeIDs: []string{tasks[0].NodeID}, DesiredState: "running"}); err != nil || len(byNode) < 2 {
		t.Errorf("Tasks of the node = %d rows, %v, want at least the service's two", len(byNode), err)
	}
	if limit, err := f.cli.TaskHistoryLimit(ctx); err != nil || limit != dockerx.DefaultTaskHistoryLimit {
		t.Errorf("TaskHistoryLimit = %d, %v, want the cluster's default of %d", limit, err, dockerx.DefaultTaskHistoryLimit)
	}

	// ServiceScale: up, then the same count again writes nothing.
	prev, changed, err := f.cli.ServiceScale(ctx, id, 3, "")
	if err != nil || prev != 2 || !changed {
		t.Fatalf("ServiceScale to 3 = previous %d, changed %v, %v", prev, changed, err)
	}
	f.waitRunning(t, ctx, id, 3)
	after, _, err := f.cli.ServiceInspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Replicas != 3 || after.Version <= d.Version || after.Image != res.Ref {
		t.Errorf("after a scale: replicas %d version %d (was %d) image %s", after.Replicas, after.Version, d.Version, after.Image)
	}
	if prev, changed, err = f.cli.ServiceScale(ctx, id, 3, ""); err != nil || prev != 3 || changed {
		t.Errorf("ServiceScale to the same count = previous %d, changed %v, %v, want nothing written", prev, changed, err)
	}
	if v, _, _ := f.cli.ServiceInspect(ctx, id); v.Version != after.Version {
		t.Errorf("a scale to the same count moved the version from %d to %d", after.Version, v.Version)
	}

	// ServiceUpdate: an edited env, the stale version refused (RESEARCH E8), a spec the manager rejects.
	edited := decodeSpec(t, after.Spec)
	edited.TaskTemplate.ContainerSpec.Env = []string{"MODE=two"}
	warn, err := f.cli.ServiceUpdate(ctx, id, after.Version, specOf(t, edited), "")
	if err != nil {
		t.Fatalf("ServiceUpdate: %v (warnings %v)", err, warn)
	}
	if _, err := f.cli.ServiceUpdate(ctx, id, after.Version, specOf(t, edited), ""); !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Errorf("ServiceUpdate with a version that is no longer current = %v, want ErrServiceChanged", err)
	}
	f.waitRunning(t, ctx, id, 3)
	dockertest.WaitServiceUpdateState(t, f.node, id, "completed", 3*time.Minute)
	edit, _, err := f.cli.ServiceInspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decodeSpec(t, edit.Spec).TaskTemplate.ContainerSpec.Env, []string{"MODE=two"}) || edit.PreviousSpec == nil || edit.UpdateStatus == nil {
		t.Fatalf("after an update: env %v, previous spec %s, update status %+v", decodeSpec(t, edit.Spec).TaskTemplate.ContainerSpec.Env, edit.PreviousSpec, edit.UpdateStatus)
	}
	if !slices.Equal(decodeSpec(t, edit.PreviousSpec).TaskTemplate.ContainerSpec.Env, []string{"MODE=one"}) {
		t.Errorf("the previous spec's env = %v, want the one before the edit", decodeSpec(t, edit.PreviousSpec).TaskTemplate.ContainerSpec.Env)
	}
	var unknown *dockerx.SpecRejectedError
	broken := decodeSpec(t, edit.Spec)
	broken.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "tp-no-such-network"}}
	if _, err := f.cli.ServiceUpdate(ctx, id, edit.Version, specOf(t, broken), ""); !errors.As(err, &unknown) {
		t.Errorf("ServiceUpdate naming a network that does not exist = %v, want a *SpecRejectedError", err)
	}

	// ServiceForceUpdate after the tag moved: the new digest is pinned and every task is replaced.
	second := dockertest.PushVariant(t, f.node, dockertest.ServiceTestImage, ref, "second", nil)
	if second == first {
		t.Fatalf("pushing a different image under the tag answered the same digest %s", first)
	}
	moved, err := f.cli.ResolveImage(ctx, ref+"@"+first, "")
	if err != nil || moved.Digest != second {
		t.Fatalf("ResolveImage after the tag moved = %+v, %v, want the new digest %s (a digest on the reference is dropped)", moved, err, second)
	}
	oldTasks := map[string]bool{}
	for _, tk := range f.tasksOf(t, ctx, id) {
		oldTasks[tk.ID] = true
	}
	err = f.cli.ServiceForceUpdate(ctx, id, dockerx.ForceUpdateOptions{Version: edit.Version - 1, Image: moved.Ref, Platforms: moved.Platforms})
	if !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Errorf("ServiceForceUpdate with a stale version = %v, want ErrServiceChanged", err)
	}
	if err := f.cli.ServiceForceUpdate(ctx, id, dockerx.ForceUpdateOptions{Version: edit.Version, Image: moved.Ref, Platforms: moved.Platforms}); err != nil {
		t.Fatalf("ServiceForceUpdate: %v", err)
	}
	dockertest.WaitServiceUpdateState(t, f.node, id, "completed", 3*time.Minute)
	f.waitRunning(t, ctx, id, 3)
	forced, _, err := f.cli.ServiceInspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if forced.Image != moved.Ref || !strings.Contains(forced.Image, second) || forced.Version <= edit.Version {
		t.Errorf("after a force update the image is %s at version %d, want %s", forced.Image, forced.Version, moved.Ref)
	}
	if got := decodeSpec(t, forced.Spec).TaskTemplate.ForceUpdate; got != decodeSpec(t, edit.Spec).TaskTemplate.ForceUpdate+1 {
		t.Errorf("ForceUpdate counter = %d, want one more than before", got)
	}
	for _, tk := range running(f.tasksOf(t, ctx, id)) {
		if oldTasks[tk.ID] {
			t.Errorf("task %s survived a force update", tk.ID)
		}
	}

	// ServiceRollback: the stale version is refused, a service that was never updated has nothing to go back to,
	// and the right one returns to the image before.
	if err := f.cli.ServiceRollback(ctx, id, forced.Version-1, ""); !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Errorf("ServiceRollback with a stale version = %v, want ErrServiceChanged", err)
	}
	if err := f.cli.ServiceRollback(ctx, id, forced.Version, ""); err != nil {
		t.Fatalf("ServiceRollback: %v", err)
	}
	dockertest.WaitServiceUpdateState(t, f.node, id, "rollback_completed", 3*time.Minute)
	f.waitRunning(t, ctx, id, 3)
	back, _, err := f.cli.ServiceInspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if back.Image != res.Ref {
		t.Errorf("after a rollback the image is %s, want the one before the force update, %s", back.Image, res.Ref)
	}
	if prevSpec := decodeSpec(t, back.PreviousSpec); prevSpec.TaskTemplate.ContainerSpec.Image != moved.Ref {
		t.Errorf("after a rollback the previous spec's image is %s, want the one just replaced, %s", prevSpec.TaskTemplate.ContainerSpec.Image, moved.Ref)
	}
	single := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "facade-fresh"},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: res.Ref, Command: []string{"sleep", "3600"}},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: new(uint64)}},
	}
	freshID, _, err := f.cli.ServiceCreate(ctx, specOf(t, single), "")
	if err != nil {
		t.Fatalf("ServiceCreate of the fresh service: %v", err)
	}
	fresh, _, err := f.cli.ServiceInspect(ctx, freshID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cli.ServiceRollback(ctx, freshID, fresh.Version, ""); !errors.Is(err, dockerx.ErrNoPreviousSpec) {
		t.Errorf("ServiceRollback of a service that was never updated = %v, want ErrNoPreviousSpec", err)
	}
	if _, _, err := f.cli.ServiceScale(ctx, freshID, 0, ""); err != nil {
		t.Errorf("ServiceScale of a service already at 0 = %v", err)
	}

	// ServiceRemove, and what is left: nothing of the service, and a second remove says so.
	if err := f.cli.ServiceRemove(ctx, id); err != nil {
		t.Fatalf("ServiceRemove: %v", err)
	}
	if err := f.cli.ServiceRemove(ctx, id); !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Errorf("ServiceRemove of a service that is gone = %v, want ErrServiceNotFound", err)
	}
	if _, _, err := f.cli.ServiceInspect(ctx, id); !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Errorf("ServiceInspect after a remove = %v, want ErrServiceNotFound", err)
	}
	if err := f.cli.ServiceRemove(ctx, freshID); err != nil {
		t.Errorf("ServiceRemove of the fresh service: %v", err)
	}
}

// platformsOf turns the os/arch strings ResolveImage answers into placement platforms.
func platformsOf(in []string) []swarm.Platform {
	var out []swarm.Platform
	for _, p := range in {
		os, arch, _ := strings.Cut(p, "/")
		out = append(out, swarm.Platform{OS: os, Architecture: arch})
	}
	return out
}

// TestServiceLogsOnARealEngine reads the log of a two-task service through the facade (plan 06-37, RES-11, RES-12):
// the tail is applied to each task, not to the service (RESEARCH E1), every record leads with the engine's time
// and the ids of its task, node and service, a task is read on its own, and the stream of a service that has not
// logged anything ends clean.
func TestServiceLogsOnARealEngine(t *testing.T) {
	f := startFacadeFix(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	id := dockertest.CreateLoggingService(t, f.node, "facade-logs", 2)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = f.raw.ServiceRemove(c, id, client.ServiceRemoveOptions{})
	})
	tasks := f.waitRunning(t, ctx, id, 2)

	// Wait until each task has written enough lines for a tail of 3 to be full.
	read := func(o dockerx.ServiceLogOptions) []dockerx.ServiceLogRecord {
		t.Helper()
		rc, tty, err := f.cli.ServiceLogs(ctx, id, o)
		if err != nil {
			t.Fatalf("ServiceLogs: %v", err)
		}
		defer func() { _ = rc.Close() }()
		return records(t, rc, tty)
	}
	byTask := func(recs []dockerx.ServiceLogRecord) map[string]int {
		out := map[string]int{}
		for _, r := range recs {
			out[r.TaskID]++
		}
		return out
	}
	dockertest.Wait(t, 2*time.Minute, "each task to have written five lines", func() (bool, string) {
		counts := byTask(read(dockerx.ServiceLogOptions{}))
		for _, tk := range tasks {
			if counts[tk.ID] < 5 {
				return false, "lines per task: " + mapString(counts)
			}
		}
		return true, ""
	})

	recs := read(dockerx.ServiceLogOptions{Tail: "3"})
	counts := byTask(recs)
	for _, tk := range tasks {
		if counts[tk.ID] != 3 {
			t.Errorf("tail 3 gave task %s %d lines, want 3 each (RESEARCH E1: tail is per task); all: %s", tk.ID, counts[tk.ID], mapString(counts))
		}
	}
	for _, r := range recs {
		if r.TaskID == "" || r.NodeID == "" || r.ServiceID != id || r.T.IsZero() || !strings.HasPrefix(r.Message, "slot=") {
			t.Errorf("record %+v, want the time, the ids of task, node and service, and the task's own line", r)
		}
	}

	// One task on its own.
	rc, tty, err := f.cli.TaskLogs(ctx, tasks[0].ID, dockerx.ServiceLogOptions{Tail: "2"})
	if err != nil {
		t.Fatalf("TaskLogs: %v", err)
	}
	one := records(t, rc, tty)
	_ = rc.Close()
	if len(one) != 2 {
		t.Errorf("TaskLogs tail 2 = %d lines, want 2", len(one))
	}
	for _, r := range one {
		if r.TaskID != tasks[0].ID {
			t.Errorf("a line of task %s in the log of task %s", r.TaskID, tasks[0].ID)
		}
	}
	if _, _, err := f.cli.TaskLogs(ctx, "nosuchtaskid0000000000000", dockerx.ServiceLogOptions{}); !errors.Is(err, dockerx.ErrTaskNotFound) {
		t.Errorf("TaskLogs of a task that is not there = %v, want ErrTaskNotFound", err)
	}
	if _, _, err := f.cli.ServiceLogs(ctx, "facade-no-such-service", dockerx.ServiceLogOptions{}); !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Errorf("ServiceLogs of a service that is not there = %v, want ErrServiceNotFound", err)
	}

	// The lines of a task that was replaced stay in the service's log while the history keeps it (RESEARCH E5).
	if err := f.cli.ServiceForceUpdate(ctx, id, dockerx.ForceUpdateOptions{Version: serviceVersion(t, ctx, f.cli, id)}); err != nil {
		t.Fatalf("ServiceForceUpdate: %v", err)
	}
	newTasks := f.waitRunning(t, ctx, id, 2)
	dockertest.Wait(t, 2*time.Minute, "the replaced tasks' lines to still be in the service log", func() (bool, string) {
		counts := byTask(read(dockerx.ServiceLogOptions{}))
		for _, tk := range append(slices.Clone(tasks), newTasks...) {
			if counts[tk.ID] == 0 {
				return false, "lines per task: " + mapString(counts)
			}
		}
		return true, ""
	})
}

func serviceVersion(t *testing.T, ctx context.Context, cli *dockerx.Client, id string) uint64 {
	t.Helper()
	d, _, err := cli.ServiceInspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Version
}

// records reads a log stream to its end into parsed records.
func records(t *testing.T, r interface{ Read([]byte) (int, error) }, tty bool) []dockerx.ServiceLogRecord {
	t.Helper()
	var out []dockerx.ServiceLogRecord
	err := dockerx.ServiceLogFrames(r, tty, func(stream string, rec []byte) {
		rr, perr := dockerx.ParseServiceLogRecord(string(bytes.TrimSpace(rec)))
		if perr != nil {
			t.Errorf("a log record that does not parse: %v: %q", perr, rec)
			return
		}
		rr.Stream = stream
		out = append(out, rr)
	})
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	return out
}

func mapString(m map[string]int) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%d", k[:min(8, len(k))], v))
	}
	slices.Sort(parts)
	return strings.Join(parts, " ")
}
