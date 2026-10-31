// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
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

	cleanupSh(t, mgr, "docker network rm as-a as-b as-c")
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
