// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// asPool, asOverlay and asOrphan are the parts of the address-space answer these cases read.
type asPool struct {
	CIDR        string
	Mask        int
	Used        int64
	Capacity    int64
	Source      string
	Estimated   bool
	LargestFree string
}

type asOverlay struct {
	ID, Name    string
	Used        int64
	Capacity    int64
	Measured    bool
	Ingress     bool
	OutsidePool bool
}

type asOrphan struct {
	NetworkID   string
	Name        string
	NodeID      string
	Class       string
	FormerOwner string
}

type asBody struct {
	Measured bool
	Pools    []asPool
	Overlays []asOverlay
	Ingress  *asOverlay
	Orphans  []asOrphan
}

// addressSpace reads GET /api/env/{id}/address-space. The route is not cached by the
// server beyond its short read, so a poll sees the swarm change.
func addressSpace(t *testing.T, f *swarmFix) (asBody, string) {
	t.Helper()
	code, raw := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/address-space", nil)
	if code != http.StatusOK {
		t.Fatalf("GET address-space: %d %s", code, raw)
	}
	var b asBody
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("parse the address-space answer: %v\n%s", err, raw)
	}
	return b, string(raw)
}

func (b asBody) overlay(name string) (asOverlay, bool) {
	for _, o := range b.Overlays {
		if o.Name == name {
			return o, true
		}
	}
	return asOverlay{}, false
}

func (b asBody) orphan(name string) (asOrphan, bool) {
	for _, o := range b.Orphans {
		if o.Name == name {
			return o, true
		}
	}
	return asOrphan{}, false
}

// engineMajor is the major version of the engine an environment reports ("29.1.3" is 29).
func engineMajor(t *testing.T, version string) int {
	t.Helper()
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		t.Fatalf("engine version %q has no major number: %v", version, err)
	}
	return n
}

// cleanupSh runs a shell line on a node when the test ends and ignores its failure: the
// line removes what the case made, and a thing already gone is the goal, not an error. The
// shared swarm is reset after every case anyway; this keeps the case itself tidy.
func cleanupSh(t *testing.T, n *dockertest.Node, line string) {
	t.Helper()
	t.Cleanup(func() {
		// wall-clock: a deadline on a cleanup command in a dind node, not a wait
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if out, err := n.Sh(ctx, line); err != nil {
			t.Logf("cleanup on %s: %q: %v\n%s", n.Hostname, line, err, out)
		}
	})
}

// cleanupNetworks removes the named networks on a node when the test ends. The services and
// containers using them are removed by cleanups registered after this one, which run first,
// but their tasks stop a moment later, so the remove is retried until the networks are gone.
func cleanupNetworks(t *testing.T, n *dockertest.Node, names ...string) {
	t.Helper()
	list := strings.Join(names, " ")
	cleanupSh(t, n, `for i in $(seq 1 30); do left=""; for n in `+list+`; do `+
		`if docker network inspect "$n" >/dev/null 2>&1; then docker network rm "$n" >/dev/null 2>&1 || left=1; fi; done; `+
		`[ -z "$left" ] && exit 0; sleep 2; done; exit 1`)
}

// createOverlay makes a network through the API and answers its ID.
func createOverlay(t *testing.T, f *swarmFix, body map[string]any) string {
	t.Helper()
	code, raw := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/networks", body)
	if code != http.StatusCreated {
		t.Fatalf("create %v: %d %s", body, code, raw)
	}
	var out struct{ ID string }
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		t.Fatalf("create answer %s (%v)", raw, err)
	}
	return out.ID
}

// sharedAddressSpacePools reads the pool and overlay numbers of a real swarm of three
// managers and a worker through the page route (plan 10-15, NET-02, NET-03, D-05, RESEARCH
// F2, F3). The swarm uses its default pool, 10.0.0.0/8 at /24, and the ingress network is
// inside it, so the pool counts ingress once and each overlay the case adds one block more.
// On Engine 29 an overlay with a running task is measured, a /24 holding 256 addresses; on an
// older engine the overlays are not measured (no address status in the inspect) while the pool
// row, which only needs the subnets, still computes.
func sharedAddressSpacePools(t *testing.T, f *swarmFix) {
	dockertest.LoadImage(t, "busybox", f.sw.Nodes()...)
	f.install(t)
	e := f.waitSwarmOnline(t, 4)
	major := engineMajor(t, e.Engine.Version)
	mgr := f.sw.Managers[0]

	cleanupNetworks(t, mgr, "as-a", "as-b", "as-c")
	cleanupSh(t, mgr, "docker service rm as-svc")
	for _, name := range []string{"as-a", "as-b", "as-c"} {
		createOverlay(t, f, map[string]any{"name": name, "driver": "overlay"})
	}
	sh(t, mgr, "docker service create --detach --name as-svc --replicas 1 --network as-a busybox sleep 600")
	f.srv.WaitFor("a running task of the service on as-a", 120*time.Second, func() (bool, string) {
		out := sh(t, mgr, "docker service ps as-svc --filter desired-state=running --format '{{.CurrentState}}'")
		return strings.HasPrefix(strings.TrimSpace(out), "Running"), out
	})

	// ingress plus the three overlays are four blocks of the pool.
	const wantUsed = 1 + 3
	var got asBody
	f.srv.WaitFor("the pool to read ingress and the three overlays", 90*time.Second, func() (bool, string) {
		b, raw := addressSpace(t, f)
		got = b
		if len(b.Pools) != 1 {
			return false, raw
		}
		_, ok := b.overlay("as-c")
		if p := b.Pools[0]; !ok || p.Used != wantUsed {
			return false, raw
		}
		if major >= 29 {
			if o, _ := b.overlay("as-a"); !o.Measured {
				return false, raw
			}
		}
		return true, ""
	})

	p := got.Pools[0]
	if p.CIDR != "10.0.0.0/8" || p.Mask != 24 || p.Capacity != 65536 || p.Used != wantUsed || p.Source != "swarm" || p.Estimated {
		t.Errorf("pool = %+v, want 10.0.0.0/8 at /24, capacity 65536, used %d, source swarm, not estimated", p, wantUsed)
	}
	if got.Ingress == nil || !got.Ingress.Ingress || got.Ingress.OutsidePool {
		t.Errorf("ingress = %+v, want present, inside the pool (outsidePool false)", got.Ingress)
	}
	for _, o := range got.Overlays {
		if !o.Ingress && o.OutsidePool {
			t.Errorf("overlay %s reads outsidePool true; only ingress can", o.Name)
		}
	}

	for _, name := range []string{"as-a", "as-b", "as-c"} {
		if _, ok := got.overlay(name); !ok {
			t.Errorf("overlay %s is not in the answer: %+v", name, got.Overlays)
		}
	}
	a, _ := got.overlay("as-a")
	if major >= 29 {
		if !a.Measured || a.Capacity != 256 || a.Used < 1 || a.Used > 256 {
			t.Errorf("as-a = %+v on Engine %d, want measured, capacity 256 and some addresses used", a, major)
		}
	} else if a.Measured || a.Used != 0 || a.Capacity != 0 {
		t.Errorf("as-a = %+v on Engine %d, want not measured with no numbers", a, major)
	}
}

// auditOrder lists the actions among the given ones in the order they were written, each as
// "action target", so a test can say that one came before another.
func auditOrder(t *testing.T, f *swarmFix, actions ...string) []string {
	t.Helper()
	rows, err := f.srv.Core().Store().Read.Query(
		`SELECT action, target_id FROM audit_log ORDER BY seq`)
	if err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var action, target string
		if err := rows.Scan(&action, &target); err != nil {
			t.Fatalf("read the audit log: %v", err)
		}
		if slices.Contains(actions, action) {
			out = append(out, action+" "+target)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	return out
}

// sharedOrphanMatrix judges real networks on a swarm of three managers and a worker with the
// reader's clock a day and an hour on, so none is held back by the grace period (plan 10-15,
// D-10, D-12, Pitfall 7). The orphan list must hold a labelled overlay whose stack is gone
// and an unlabelled unused one, and must not hold the ones that look idle and are not: an
// overlay a service scaled to zero names, an overlay only a service's previous spec names,
// and an attachable overlay a standalone container on the worker uses. Deleting the orphan
// saves its inspect first; deleting the previous-spec overlay is refused as in use again
// and nothing reaches the engine.
func sharedOrphanMatrix(t *testing.T, f *swarmFix) {
	dockertest.LoadImage(t, "busybox", f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	base := "/api/env/" + f.env.ID + "/address-space"

	cleanupNetworks(t, mgr, "om-zero", "om-prev-a", "om-prev-b", "om-attach", "om-gone", "om-free")
	cleanupSh(t, mgr, "docker service rm om-zero-svc om-prev-svc")
	cleanupSh(t, worker, "docker rm -f om-wk")

	createOverlay(t, f, map[string]any{"name": "om-zero", "driver": "overlay"})
	prevA := createOverlay(t, f, map[string]any{"name": "om-prev-a", "driver": "overlay"})
	createOverlay(t, f, map[string]any{"name": "om-prev-b", "driver": "overlay"})
	createOverlay(t, f, map[string]any{"name": "om-attach", "driver": "overlay", "attachable": true})
	goneID := createOverlay(t, f, map[string]any{
		"name": "om-gone", "driver": "overlay",
		"labels": map[string]string{"com.docker.stack.namespace": "om_gone_stack"},
	})
	createOverlay(t, f, map[string]any{"name": "om-free", "driver": "overlay"})

	// A service scaled to zero names om-zero; its tasks are none, its spec still holds the network.
	sh(t, mgr, "docker service create --detach --name om-zero-svc --replicas 0 --network om-zero busybox sleep 600")
	// A service moved from om-prev-a to om-prev-b: a is now only in the previous spec.
	sh(t, mgr, "docker service create --detach --name om-prev-svc --replicas 1 --network om-prev-a busybox sleep 600")
	sh(t, mgr, "docker service update --detach --network-rm om-prev-a --network-add om-prev-b om-prev-svc")
	f.srv.WaitFor("om-prev-a to be only in the previous spec", 60*time.Second, func() (bool, string) {
		spec := sh(t, mgr, "docker service inspect --format '{{json .Spec.TaskTemplate.Networks}}|{{json .PreviousSpec.TaskTemplate.Networks}}' om-prev-svc")
		cur, prev, _ := strings.Cut(strings.TrimSpace(spec), "|")
		// The specs name networks by ID.
		return strings.Contains(prev, prevA) && !strings.Contains(cur, prevA), spec
	})
	// An attachable overlay a standalone container on the worker uses.
	sh(t, worker, "docker run -d --name om-wk --network om-attach busybox sleep 600")

	var got asBody
	f.srv.WaitFor("the orphan list to hold om-gone and om-free", 120*time.Second, func() (bool, string) {
		b, _ := addressSpace(t, f)
		got = b
		_, gone := b.orphan("om-gone")
		_, free := b.orphan("om-free")
		return gone && free, orphanNames(b)
	})

	if o, _ := got.orphan("om-gone"); o.Class != "orphaned" || o.FormerOwner != "om_gone_stack" {
		t.Errorf("om-gone = %+v, want orphaned with former owner om_gone_stack", o)
	}
	if o, _ := got.orphan("om-free"); o.Class != "unused" {
		t.Errorf("om-free = %+v, want unused", o)
	}
	for _, name := range []string{"om-zero", "om-prev-a", "om-prev-b", "om-attach"} {
		if o, ok := got.orphan(name); ok {
			t.Errorf("%s is on the orphan list as %s, but something still uses it", name, o.Class)
		}
	}
	for _, o := range got.Orphans {
		if o.Class == "cant_tell" {
			t.Errorf("%s reads cant_tell although every node and the manager answered: %s", o.Name, orphanNames(got))
		}
	}

	// Deleting the orphan saves its inspect first, then removes it.
	code, raw := f.srv.Do(http.MethodDelete, base+"/orphans/"+goneID, nil)
	var del struct{ Outcome, Reason, Name string }
	if err := json.Unmarshal(raw, &del); code != http.StatusOK || err != nil || del.Outcome != "deleted" {
		t.Fatalf("delete om-gone: %d %s (%v), want 200 deleted", code, raw, err)
	}
	order := auditOrder(t, f, "network.inspect_saved", "network.removed")
	if len(order) != 2 || !strings.HasPrefix(order[0], "network.inspect_saved ") || !strings.HasPrefix(order[1], "network.removed ") {
		t.Fatalf("audit order = %v, want network.inspect_saved then network.removed", order)
	}
	if len(networkIDs(t, mgr, "om-gone")) != 0 {
		t.Errorf("om-gone is still on the manager after the delete")
	}

	// The previous-spec overlay is kept: something could still roll back onto it.
	code, raw = f.srv.Do(http.MethodDelete, base+"/orphans/"+prevA, nil)
	if err := json.Unmarshal(raw, &del); code != http.StatusOK || err != nil || del.Outcome != "kept" || del.Reason != "in_use_again" {
		t.Fatalf("delete om-prev-a: %d %s (%v), want 200 kept in_use_again", code, raw, err)
	}
	if ids := networkIDs(t, mgr, "om-prev-a"); len(ids) != 1 || ids[0] != prevA {
		t.Errorf("om-prev-a on the manager = %v, want it to still be %s", ids, prevA)
	}
	// The refusal is audited as a failed removal that names why, and no inspect was saved for it:
	// the check stopped before the point where a remove is prepared.
	var removed []string
	for _, r := range f.srv.AuditRows("network.removed") {
		removed = append(removed, r.Result+"/"+r.Reason+"/"+r.Details["name"])
	}
	if want := []string{"success//om-gone", "failure/in_use_again/om-prev-a"}; !slices.Equal(removed, want) {
		t.Errorf("network.removed rows = %v, want %v", removed, want)
	}
	if saved := auditOrder(t, f, "network.inspect_saved"); len(saved) != 1 {
		t.Errorf("network.inspect_saved rows = %v, want only om-gone's", saved)
	}
}

// orphanNames is a one-line view of an orphan list for a failure message.
func orphanNames(b asBody) string {
	var parts []string
	for _, o := range b.Orphans {
		parts = append(parts, o.Name+"="+o.Class)
	}
	return strings.Join(parts, " ")
}
