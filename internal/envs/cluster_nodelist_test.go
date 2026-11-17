// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The node list the fold proof reads (SWM-02, D-06, T-19-02): the list a manager agent read,
// believed under the one trust rule Clusters.SwarmNodeIDs applies, with each node's role.

// nodeReads is how many node-list requests the swarm's managers have served: one per cluster
// read, whichever manager answered it.
func (s *swarmRig) nodeReads() int {
	n := 0
	for _, a := range s.m {
		for _, p := range a.fake.Requests() {
			if strings.HasSuffix(p, "/nodes") {
				n++
			}
		}
	}
	return n
}

func TestClustersNodeListBelievesOnlyAFreshListThatNamesItsAnswerer(t *testing.T) {
	// Never read: no list, whether the environment is unknown or just not read yet.
	bare := newSwarmRigBare(t, managerNodes(), nil)
	if nodes, at, ok := bare.c.NodeList(bare.env.ID); ok || nodes != nil || !at.IsZero() {
		t.Fatalf("before any read: %v %v %v, want no list", nodes, at, ok)
	}
	if _, _, ok := bare.c.NodeList("no-such-environment"); ok {
		t.Error("an unknown environment has a node list")
	}

	extra := dockertest.FakeNode{ID: "node-w1", Hostname: "wrk-1", Role: "worker", EngineVersion: "29.1.0", OS: "linux"}
	nodes := managerNodes(extra)
	s := newSwarmRig(t, nodes, nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	rows, at, ok := s.c.NodeList(s.env.ID)
	st, _ := s.c.Cached(s.env.ID)
	roles := map[string]string{}
	for _, r := range rows {
		roles[r.ID] = r.Role
	}
	want := map[string]string{"node-m1": roleManager, "node-m2": roleManager, "node-m3": roleManager, "node-w1": roleWorker}
	if !ok || at.IsZero() || !at.Equal(st.FetchedAt) || len(rows) != len(want) {
		t.Fatalf("after a read: %d rows at %v (%v), want %d rows at %v", len(rows), at, ok, len(want), st.FetchedAt)
	}
	for id, role := range want {
		if roles[id] != role {
			t.Errorf("node %s has role %q, want %q", id, roles[id], role)
		}
	}

	// The slice is a copy: changing it does not change what is held.
	rows[0].ID = "tampered"
	slices.Reverse(rows)
	again, _, _ := s.c.NodeList(s.env.ID)
	for _, r := range again {
		if r.ID == "tampered" {
			t.Fatal("changing the returned list changed the held one")
		}
	}

	// A list that leaves out the manager that answered it is not believed (T-02-68).
	answerless := []dockertest.FakeNode{{ID: "node-w1", Hostname: "wrk-1", Role: "worker", EngineVersion: "29.1.0", OS: "linux"}}
	for i, a := range s.m {
		a.fake.Reconfigure(managerConfig(i+1, answerless, nil))
	}
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.c.Cached(s.env.ID); !st.Known() || st.AnsweredBy == "" {
		t.Fatalf("the read of a list without its answerer: %+v, want it held, with its answerer", st)
	}
	if rows, _, ok := s.c.NodeList(s.env.ID); ok || rows != nil {
		t.Errorf("a list that leaves out its answerer: %v %v, want no list", rows, ok)
	}

	// A lost quorum is no basis either.
	for i, a := range s.m {
		cfg := managerConfig(i+1, nodes, nil)
		cfg.QuorumLost = true
		a.fake.Reconfigure(cfg)
	}
	if err := s.c.Refresh(ctx, s.env.ID); err == nil || ReasonFor(err) != ReasonNoQuorum {
		t.Fatalf("Refresh = %v, want no_quorum", err)
	}
	if rows, _, ok := s.c.NodeList(s.env.ID); ok || rows != nil {
		t.Errorf("while the quorum is lost: %v %v, want no list", rows, ok)
	}
}

func TestClustersRefreshNodeListReadsAColdEnvironmentOnce(t *testing.T) {
	// minAge is short because a longer advance of the rig clock makes its agents look offline.
	const minAge = 5 * time.Second
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if st, found := s.c.Cached(s.env.ID); found && !st.FetchedAt.IsZero() {
		t.Fatal("the environment was read before the test asked")
	}
	base := s.nodeReads()

	// Cold: one read, and its list.
	rows, at, ok := s.c.RefreshNodeList(ctx, s.env.ID, minAge)
	if !ok || len(rows) != 3 || at.IsZero() {
		t.Fatalf("cold environment: %d rows at %v (%v), want the three nodes", len(rows), at, ok)
	}
	oneRead := s.nodeReads() - base
	if oneRead == 0 {
		t.Fatal("a cold environment was not read")
	}

	// Younger than minAge: the held list answers and no manager is asked.
	if _, again, ok := s.c.RefreshNodeList(ctx, s.env.ID, minAge); !ok || !again.Equal(at) {
		t.Fatalf("a list younger than minAge: read at %v (%v), want the held one at %v", again, ok, at)
	}
	if got := s.nodeReads() - base; got != oneRead {
		t.Fatalf("a fresh list was read again: %d node requests, want %d", got, oneRead)
	}

	// Older than minAge: read again.
	s.clk.Advance(2 * minAge)
	_, later, ok := s.c.RefreshNodeList(ctx, s.env.ID, minAge)
	if !ok || !later.After(at) {
		t.Fatalf("a list older than minAge: read at %v (%v), want a newer read than %v", later, ok, at)
	}
	if got := s.nodeReads() - base; got != 2*oneRead {
		t.Fatalf("an old list: %d node requests, want %d (one more read)", got, 2*oneRead)
	}

	// A read in flight is shared, not repeated: whether the caller meets the lock held or
	// the finished read's fresh list, the managers are asked once in all.
	s.clk.Advance(2 * minAge)
	e := s.c.entry(s.env.ID)
	before := s.nodeReads()
	e.refresh.Lock()
	type answer struct {
		rows []dockerx.NodeRow
		ok   bool
	}
	done := make(chan answer, 1)
	go func() {
		rows, _, ok := s.c.RefreshNodeList(ctx, s.env.ID, minAge)
		done <- answer{rows, ok}
	}()
	if err := s.c.refresh(ctx, s.env.ID, e); err != nil {
		e.refresh.Unlock()
		t.Fatal(err)
	}
	e.refresh.Unlock()
	if a := <-done; !a.ok || len(a.rows) != 3 {
		t.Fatalf("the shared read: %d rows (%v), want the three nodes", len(a.rows), a.ok)
	}
	if got := s.nodeReads() - before; got != oneRead {
		t.Fatalf("a read in flight was repeated: %d node requests, want %d", got, oneRead)
	}
}

func TestClustersRefreshNodeListStopsWaitingForARunningReadWhenItsContextEnds(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	e := s.c.entry(s.env.ID)
	// A read of the environment is running: its lock is held and stays held for the whole test.
	e.refresh.Lock()
	defer e.refresh.Unlock()
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	cancel()
	done := make(chan struct{})
	go func() {
		s.c.RefreshNodeList(ctx, s.env.ID, time.Second)
		close(done)
	}()
	// A wait that ignored the context would still be here when the test gives up.
	testsupport.Await(t, "a cancelled request to stop waiting for the running read", done)
}

func TestFoldNodeListIsNilWithoutClustersAWholeListOrAFreshOne(t *testing.T) {
	// A service with no Clusters has nothing to prove against.
	if got := newTunRig(t, nil).svc.foldNodeList("any"); got != nil {
		t.Fatalf("without Clusters: %v, want nil", got)
	}

	bare := newSwarmRigBare(t, managerNodes(), nil)
	if got := bare.svc.foldNodeList(bare.env.ID); got != nil {
		t.Fatalf("before any read: %v, want nil", got)
	}

	s := newSwarmRig(t, managerNodes(), nil)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.svc.foldNodeList(s.env.ID); len(got) != 3 {
		t.Fatalf("a fresh believed list: %d rows, want 3", len(got))
	}
	s.clk.Advance(foldNodeListMaxAge)
	if got := s.svc.foldNodeList(s.env.ID); len(got) != 3 {
		t.Fatalf("a list exactly foldNodeListMaxAge old: %d rows, want 3", len(got))
	}
	s.clk.Advance(time.Second)
	if got := s.svc.foldNodeList(s.env.ID); got != nil {
		t.Fatalf("a list older than foldNodeListMaxAge: %v, want nil", got)
	}
}
