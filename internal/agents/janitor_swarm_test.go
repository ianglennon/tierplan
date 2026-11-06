// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// janitorShift is the janitor's clock moved forward by a fixed amount, so containers and networks made a
// moment ago are past the 24-hour grace period at once. It never fires After: the runner does not wait on it.
type janitorShift struct{ by atomic.Int64 }

func (c *janitorShift) Now() time.Time { return time.Now().Add(time.Duration(c.by.Load())) }

func (c *janitorShift) After(time.Duration) <-chan time.Time { return nil }

// janitorSwarmShift is how far past the grace period the janitor's clock and the address-space reader's
// clock are moved in the Janitor case.
const janitorSwarmShift = 48 * time.Hour

// swarmTaskBox is one container of a swarm task as the engine of its node reports it.
type swarmTaskBox struct {
	node                      *dockertest.Node
	id, state                 string
	created                   time.Time
	serviceID, serviceName    string
	taskID, taskName, slotKey string
}

// taskBoxes lists the containers of the tasks of one service on every node, from each node's own engine
// and not from TierPlan, so a check does not trust the code under test to report its own effect. The
// containers are found by their names (service.slot.task) and the labels are read from the inspect.
func taskBoxes(t *testing.T, f *swarmFix, service string) ([]swarmTaskBox, error) {
	t.Helper()
	const format = `{{.Id}}|{{.State.Status}}|{{.Created}}|` +
		`{{index .Config.Labels "com.docker.swarm.service.id"}}|{{index .Config.Labels "com.docker.swarm.service.name"}}|` +
		`{{index .Config.Labels "com.docker.swarm.task.id"}}|{{index .Config.Labels "com.docker.swarm.task.name"}}`
	line := `ids=$(docker ps -aq --no-trunc --filter name=` + service + `.); [ -z "$ids" ] || docker inspect --format '` + format + `' $ids`
	var out []swarmTaskBox
	for _, n := range f.sw.Nodes() {
		raw, err := n.Sh(execCtx(t, time.Minute), line)
		if err != nil {
			return nil, fmt.Errorf("list %s on %s: %w", service, n.Hostname, err)
		}
		for _, l := range strings.Split(strings.TrimSpace(raw), "\n") {
			parts := strings.Split(strings.TrimSpace(l), "|")
			if len(parts) != 7 {
				continue
			}
			created, err := time.Parse(time.RFC3339Nano, parts[2])
			if err != nil {
				return nil, fmt.Errorf("created %q of %s: %w", parts[2], parts[0], err)
			}
			b := swarmTaskBox{node: n, id: parts[0], state: parts[1], created: created,
				serviceID: parts[3], serviceName: parts[4], taskID: parts[5], taskName: parts[6]}
			// The task name is {service}.{slot}.{task ID}: the middle is the slot of a replicated service.
			if rest, ok := strings.CutPrefix(b.taskName, service+"."); ok {
				b.slotKey, _, _ = strings.Cut(rest, ".")
			}
			out = append(out, b)
		}
	}
	return out, nil
}

// exitedBySlot groups the exited containers of a service by replica slot, oldest first by the second the
// engine created them in and then by ID: the order the janitor ranks them in.
func exitedBySlot(boxes []swarmTaskBox) map[string][]swarmTaskBox {
	slots := map[string][]swarmTaskBox{}
	for _, b := range boxes {
		if b.state == "exited" {
			slots[b.slotKey] = append(slots[b.slotKey], b)
		}
	}
	for _, s := range slots {
		slices.SortFunc(s, func(a, b swarmTaskBox) int {
			return cmp.Or(cmp.Compare(a.created.Unix(), b.created.Unix()), strings.Compare(a.id, b.id))
		})
	}
	return slots
}

// janitorSwarmItem and janitorSwarmPreview are the parts of the preview this case reads.
type janitorSwarmItem struct {
	Type   string `json:"type"`
	NodeID string `json:"nodeId"`
	Node   string `json:"node"`
	Ref    string `json:"ref"`
	Name   string `json:"name"`
	Class  string `json:"class"`
	Reason struct {
		Code    string `json:"code"`
		Service string `json:"service"`
		Slot    string `json:"slot"`
	} `json:"reason"`
}

type janitorSwarmPreview struct {
	Items            []janitorSwarmItem `json:"items"`
	NotRemoved       []janitorSwarmItem `json:"notRemoved"`
	WithheldNodes    []string           `json:"withheldNodes"`
	NetworksWithheld bool               `json:"networksWithheld"`
}

// sharedJanitor runs the janitor on a real swarm of three managers and a worker through real agents (plan
// 11-31, JAN-02, JAN-03, JAN-04, JAN-07, RESEARCH A2 and A4). A service of two replicas whose task exits at
// once and restarts leaves exited task containers on the nodes; a second such service carries the container
// label tierplan.keep=true; an overlay network labelled with a stack that does not exist is an orphan. With the
// janitor's and the address-space reader's clocks 48 hours on:
//
//   - the task containers carry the swarm labels the planner groups by (A2), and the preview lists exactly the
//     containers beyond the newest two of each slot as removable task_beyond_keep, whatever node they are on
//   - the labelled service's containers are protected kept_label, and the orphan overlay is removable
//     network_orphaned
//   - Clean now removes exactly those on the node they are on, and the network with network.inspect_saved
//     before network.removed, and nothing else
//   - the task history limit is read through a manager's agent as Docker's 5, set to 7 through the agent's
//     swarm update route, read back as 7 and put back (A4: the /info spec goes back through a swarm update)
func sharedJanitor(t *testing.T, f *swarmFix) {
	dockertest.LoadImage(t, "busybox", f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	mgr := f.sw.Managers[0]
	base := "/api/env/" + f.env.ID + "/janitor"

	cleanupNetworks(t, mgr, "jn-gone")
	cleanupSh(t, mgr, "docker service rm jn-crash jn-keep")

	// Two services whose tasks exit at once and restart after two seconds, so each slot soon holds several exited
	// containers. The two seconds keep the containers of one slot in distinct seconds, which is how they are ranked.
	// The labelled service stops by itself after four tasks, under Docker's history limit of 5 and with no scale down, so
	// the swarm removes none of its containers and any that goes was the janitor's doing.
	sh(t, mgr, "docker service create --detach --name jn-crash --replicas 2 --restart-condition any --restart-delay 2s busybox sh -c 'exit 1'")
	sh(t, mgr, "docker service create --detach --name jn-keep --replicas 1 --restart-condition any --restart-max-attempts 3 --restart-delay 2s --container-label tierplan.keep=true busybox sh -c 'exit 1'")
	crashID := strings.TrimSpace(sh(t, mgr, "docker service inspect --format '{{.ID}}' jn-crash"))
	keepID := strings.TrimSpace(sh(t, mgr, "docker service inspect --format '{{.ID}}' jn-keep"))
	goneID := createOverlay(t, f, map[string]any{
		"name": "jn-gone", "driver": "overlay",
		"labels": map[string]string{"com.docker.stack.namespace": "jn_gone_stack"},
	})

	// Every slot of the crashing service holds at least three exited containers, so the newest two leave one beyond.
	f.srv.WaitFor("every slot of jn-crash to hold three exited containers and jn-keep four", 4*time.Minute, func() (bool, string) {
		crash, err := taskBoxes(t, f, "jn-crash")
		if err != nil {
			return false, err.Error()
		}
		keep, err := taskBoxes(t, f, "jn-keep")
		if err != nil {
			return false, err.Error()
		}
		slots := exitedBySlot(crash)
		seen := fmt.Sprintf("jn-crash slots %v, jn-keep %d containers", countBySlot(slots), len(keep))
		if len(slots) < 2 || len(keep) < 4 {
			return false, seen
		}
		for _, s := range slots {
			if len(s) < 3 {
				return false, seen
			}
		}
		return true, seen
	})

	// Stop the churn: with no task wanted running, the set of exited containers can only shrink as the swarm
	// reaps tasks past its history limit.
	sh(t, mgr, "docker service update --detach --replicas 0 jn-crash")

	// RESEARCH A2: the labels the planner groups task containers by are on the containers the engine made.
	var want []swarmTaskBox // the containers the janitor must list: beyond the newest two of their slot
	var wantKept, allCrash []swarmTaskBox
	var preview janitorSwarmPreview
	f.srv.WaitFor("a preview that matches the exited containers the engines hold", 3*time.Minute, func() (bool, string) {
		before, err := taskBoxes(t, f, "jn-crash")
		if err != nil {
			return false, err.Error()
		}
		keptBefore, err := taskBoxes(t, f, "jn-keep")
		if err != nil {
			return false, err.Error()
		}
		for _, b := range append(slices.Clone(before), keptBefore...) {
			if b.state != "exited" {
				return false, fmt.Sprintf("%s is %s, waiting for every task to stop", b.taskName, b.state)
			}
		}
		code, body := f.srv.Do(http.MethodPost, base+"/preview", nil)
		if code != http.StatusOK {
			t.Fatalf("POST preview = %d %s", code, body)
		}
		preview = janitorSwarmPreview{}
		if err := json.Unmarshal(body, &preview); err != nil {
			t.Fatalf("decode the preview %s: %v", body, err)
		}
		after, err := taskBoxes(t, f, "jn-crash")
		if err != nil {
			return false, err.Error()
		}
		if boxIDs(before) != boxIDs(after) {
			return false, "the swarm was still removing containers while the preview was read"
		}
		want, wantKept, allCrash = nil, keptBefore, after
		for _, s := range exitedBySlot(after) {
			if len(s) > 2 {
				want = append(want, s[:len(s)-2]...)
			}
		}
		var got []string
		for _, it := range preview.Items {
			if it.Type == "container" {
				got = append(got, it.Ref)
			}
		}
		var wantIDs []string
		for _, b := range want {
			wantIDs = append(wantIDs, b.id)
		}
		slices.Sort(got)
		slices.Sort(wantIDs)
		return len(wantIDs) > 0 && slices.Equal(got, wantIDs), fmt.Sprintf("preview lists %v, the engines hold %v beyond keep 2", got, wantIDs)
	})

	for _, b := range allCrash {
		checkSwarmLabels(t, b, "jn-crash", crashID)
	}
	for _, b := range wantKept {
		checkSwarmLabels(t, b, "jn-keep", keepID)
	}
	nodes := map[string]bool{}
	for _, b := range want {
		nodes[b.node.Hostname] = true
	}
	t.Logf("%d containers beyond keep-N on %d nodes", len(want), len(nodes))

	for _, it := range preview.Items {
		if it.Type != "container" {
			continue
		}
		var box swarmTaskBox
		for _, b := range want {
			if b.id == it.Ref {
				box = b
			}
		}
		if it.Class != "removable" || it.Reason.Code != "task_beyond_keep" || it.Reason.Service != "jn-crash" ||
			it.Reason.Slot != box.slotKey || it.Node != box.node.Hostname {
			t.Errorf("preview item %+v, want removable task_beyond_keep of jn-crash slot %s on %s", it, box.slotKey, box.node.Hostname)
		}
	}

	// The labelled service's containers are protected whatever their rank: every one is under notRemoved.
	for _, b := range wantKept {
		var found *janitorSwarmItem
		for i := range preview.NotRemoved {
			if preview.NotRemoved[i].Ref == b.id {
				found = &preview.NotRemoved[i]
			}
		}
		if found == nil || found.Class != "protected" || found.Reason.Code != "kept_label" {
			t.Errorf("jn-keep container %s = %+v under notRemoved %+v, want protected kept_label", b.id, found, preview.NotRemoved)
		}
	}
	for _, it := range preview.Items {
		if it.Type == "container" && it.Reason.Service == "jn-keep" {
			t.Errorf("a container of the labelled service is removable: %+v", it)
		}
	}

	// The orphan overlay is removable, and the only network that is.
	var networkItem *janitorSwarmItem
	for i, it := range preview.Items {
		if it.Type == "network" {
			if it.Ref != goneID {
				t.Errorf("unexpected removable network %+v, want only jn-gone (%s)", it, goneID)
				continue
			}
			networkItem = &preview.Items[i]
		}
	}
	if networkItem == nil || networkItem.Class != "removable" || networkItem.Reason.Code != "network_orphaned" {
		t.Fatalf("jn-gone = %+v in items %+v (networks withheld %v, withheld nodes %v), want removable network_orphaned",
			networkItem, preview.Items, preview.NetworksWithheld, preview.WithheldNodes)
	}

	// Clean now with every removable container and the network.
	type itemRef struct {
		Type   string `json:"type"`
		NodeID string `json:"nodeId"`
		Ref    string `json:"ref"`
	}
	var ticked []itemRef
	for _, it := range preview.Items {
		if it.Type == "container" || it.Type == "network" {
			ticked = append(ticked, itemRef{it.Type, it.NodeID, it.Ref})
		}
	}
	code, raw := f.srv.Do(http.MethodPost, base+"/runs", map[string]any{"items": ticked})
	var started struct {
		RunID string `json:"runId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(raw, &started) != nil || started.RunID == "" {
		t.Fatalf("POST runs = %d %s, want 202 and a run", code, raw)
	}
	type runItem struct {
		Type    string `json:"type"`
		Ref     string `json:"ref"`
		Outcome string `json:"outcome"`
		Code    string `json:"code"`
	}
	var run struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
		Counts struct {
			RemovedContainers int `json:"removedContainers"`
			RemovedNetworks   int `json:"removedNetworks"`
			RemovedImages     int `json:"removedImages"`
			Failed            int `json:"failed"`
			Kept              int `json:"kept"`
		} `json:"counts"`
		Items []runItem `json:"items"`
	}
	f.srv.WaitFor("the janitor run to end", 3*time.Minute, func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, base+"/runs/"+started.RunID, nil)
		run.Items = nil
		if code != http.StatusOK || json.Unmarshal(body, &run) != nil {
			return false, string(body)
		}
		return run.State != "running" && run.State != "waiting", string(body)
	})
	if run.State != "completed" || run.Counts.RemovedContainers != len(want) || run.Counts.RemovedNetworks != 1 || run.Counts.Failed != 0 || run.Counts.Kept != 0 {
		t.Fatalf("run = %+v, want completed with %d containers and 1 network removed, none failed or kept", run, len(want))
	}
	for _, it := range run.Items {
		if it.Outcome != "removed" {
			t.Errorf("run item %+v, want removed", it)
		}
	}

	// Gone from the node each was on; the newest two of each slot and every container of the labelled service stay.
	exists := func(b swarmTaskBox) bool {
		_, err := b.node.Sh(execCtx(t, time.Minute), "docker container inspect "+b.id)
		return err == nil
	}
	for _, b := range want {
		if exists(b) {
			t.Errorf("container %s (%s) is still on %s", b.taskName, b.id, b.node.Hostname)
		}
	}
	for _, b := range wantKept {
		if !exists(b) {
			t.Errorf("the labelled service's container %s (%s) was removed", b.taskName, b.id)
		}
	}
	left, err := taskBoxes(t, f, "jn-crash")
	if err != nil {
		t.Fatal(err)
	}
	for slot, s := range exitedBySlot(left) {
		if len(s) != 2 {
			t.Errorf("slot %s of jn-crash holds %d exited containers after the run, want the newest 2", slot, len(s))
		}
	}
	if len(networkIDs(t, mgr, "jn-gone")) != 0 {
		t.Errorf("jn-gone is still on the manager after the run")
	}
	order := auditOrder(t, f, "network.inspect_saved", "network.removed")
	if len(order) != 2 || !strings.HasPrefix(order[0], "network.inspect_saved ") || !strings.HasPrefix(order[1], "network.removed ") {
		t.Fatalf("audit order = %v, want network.inspect_saved then network.removed", order)
	}
	if rows := f.srv.AuditRows("network.removed"); len(rows) != 1 || rows[0].Result != "success" || rows[0].Details["source"] != "janitor" {
		t.Errorf("network.removed rows = %+v, want one success from the janitor", rows)
	}

	// The task history limit, through a manager's agent: Docker's 5, then 7, then back.
	taskHistory := func() (limit int64, dockerDefault int) {
		t.Helper()
		code, body := f.srv.Do(http.MethodGet, base+"/task-history", nil)
		var got struct {
			Limit         int64 `json:"limit"`
			DockerDefault int   `json:"dockerDefault"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &got) != nil {
			t.Fatalf("GET task-history = %d %s", code, body)
		}
		return got.Limit, got.DockerDefault
	}
	start, dockerDefault := taskHistory()
	if start != 5 || dockerDefault != 5 {
		t.Fatalf("task history limit = %d (docker default %d) on a new swarm, want Docker's 5", start, dockerDefault)
	}
	// The shared swarm's reset checks services, secrets, configs and nodes, not the cluster spec, so the case puts
	// the limit back itself: by the route, and by the engine's own command if the route cannot.
	t.Cleanup(func() {
		if code, body := f.srv.Do(http.MethodPut, base+"/task-history", map[string]int64{"limit": start}); code != http.StatusOK {
			t.Logf("restore the task history limit by the route: %d %s", code, body)
			if out, err := mgr.Sh(execCtx(t, time.Minute), "docker swarm update --task-history-limit "+strconv.FormatInt(start, 10)); err != nil {
				t.Errorf("restore the task history limit to %d: %v\n%s", start, err, out)
			}
		}
	})
	code, raw = f.srv.Do(http.MethodPut, base+"/task-history", map[string]int64{"limit": 7})
	if code != http.StatusOK || !strings.Contains(string(raw), `"limit":7`) || !strings.Contains(string(raw), `"changed":true`) {
		t.Fatalf("PUT task-history 7 = %d %s, want 200, limit 7, changed", code, raw)
	}
	if got, _ := taskHistory(); got != 7 {
		t.Errorf("task history limit read back = %d, want 7", got)
	}
	// The manager's own engine agrees. That nothing else about the swarm changes is the agent's
	// guarantee, proved by its proxy and fuzz tests, not by this test.
	if out := strings.TrimSpace(sh(t, mgr, "docker info --format '{{.Swarm.Cluster.Spec.Orchestration.TaskHistoryRetentionLimit}}'")); out != "7" {
		t.Errorf("the manager's engine reports a task history limit of %q, want 7", out)
	}
	code, raw = f.srv.Do(http.MethodPut, base+"/task-history", map[string]int64{"limit": start})
	if code != http.StatusOK || !strings.Contains(string(raw), `"limit":`+strconv.FormatInt(start, 10)) {
		t.Fatalf("PUT task-history %d = %d %s, want 200", start, code, raw)
	}
	if got, _ := taskHistory(); got != start {
		t.Errorf("task history limit after the restore = %d, want %d", got, start)
	}
}

// countBySlot is the number of exited containers in each slot, for a failure message.
func countBySlot(slots map[string][]swarmTaskBox) map[string]int {
	out := map[string]int{}
	for k, v := range slots {
		out[k] = len(v)
	}
	return out
}

// boxIDs is the sorted container IDs of a list joined, to tell whether two reads saw the same containers.
func boxIDs(boxes []swarmTaskBox) string {
	var out []string
	for _, b := range boxes {
		out = append(out, b.id+":"+b.state)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// checkSwarmLabels asserts a task container carries the labels the janitor groups swarm task containers by
// (RESEARCH A2): the service's ID and name, the task's ID and the task name {service}.{slot}.{task ID}.
func checkSwarmLabels(t *testing.T, b swarmTaskBox, service, serviceID string) {
	t.Helper()
	if b.serviceID != serviceID || b.serviceName != service || b.taskID == "" ||
		!strings.HasPrefix(b.taskName, service+".") || !strings.HasSuffix(b.taskName, "."+b.taskID) || b.slotKey == "" {
		t.Errorf("container %s has swarm labels service.id=%q service.name=%q task.id=%q task.name=%q, want %s %s and a task name %s.{slot}.{task id}",
			b.id, b.serviceID, b.serviceName, b.taskID, b.taskName, serviceID, service, service)
	}
}
