// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// The swarm node an enrolling ingress node reports is a claim of a join-token holder. Unlike an agent, an ingress
// node is never accepted without a fresh manager-read node list holding it (D-22): the payload it receives is
// every private key of the environment.

const swarmNodeN = "xyz1abc2def3ghi4jkl5mno6p"

// countingLists is a node list provider that counts the questions asked, and on a refresh swaps in the list the
// test prepared, if any. It wraps the fixture's fake, so the held lists are the same.
type countingLists struct {
	*fakeNodeLists
	mu        sync.Mutex
	next      *nodeList
	asked     int
	refreshes int
	minAge    time.Duration
}

func (c *countingLists) SwarmNodeIDs(envID string) ([]string, time.Time, bool) {
	c.mu.Lock()
	c.asked++
	c.mu.Unlock()
	return c.fakeNodeLists.SwarmNodeIDs(envID)
}

func (c *countingLists) RefreshSwarmNodeIDs(ctx context.Context, envID string, minAge time.Duration) ([]string, time.Time, bool) {
	c.mu.Lock()
	c.refreshes++
	c.minAge = minAge
	next := c.next
	c.mu.Unlock()
	if next != nil {
		c.fakeNodeLists.set(envID, *next)
	}
	return c.fakeNodeLists.SwarmNodeIDs(envID)
}

func (c *countingLists) counts() (asked, refreshes int, minAge time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked, c.refreshes, c.minAge
}

// nodeCheckEnv is a swarm environment with an issued token and a counting provider installed in the service.
type nodeCheckEnv struct {
	f     *fixture
	envID string
	token string
	lists *countingLists
}

func newNodeCheckEnv(t *testing.T) *nodeCheckEnv {
	t.Helper()
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	tok := f.issue(t, envID)
	lists := &countingLists{fakeNodeLists: f.nodes}
	f.svc.WithNodeLists(lists)
	return &nodeCheckEnv{f: f, envID: envID, token: tok.Token, lists: lists}
}

// enrol makes one signed enrolment claiming swarm node swarmNodeID.
func (e *nodeCheckEnv) enrol(t *testing.T, swarmNodeID string) (string, error) {
	t.Helper()
	_, key := newKey(t)
	res, err := e.f.svc.Enrol(context.Background(), enrolRequest(t, key, e.token, "edge-a", swarmNodeID))
	return res.NodeID, err
}

// listAged holds ids as read age ago on the fixture clock.
func (e *nodeCheckEnv) listAged(age time.Duration, ids ...string) {
	e.f.nodes.set(e.envID, nodeList{ids: ids, at: e.f.clock.Now().Add(-age), ok: true})
}

// requireRefusal asserts the error is the refusal code with a retry of 15 seconds and audit reason code, and that
// nothing was written.
func (e *nodeCheckEnv) requireRefusal(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Enrol succeeded, want %s", code)
	}
	re := refusal(t, err)
	if re.Code != code || re.AuditReason != code {
		t.Errorf("refusal = code %q, audit %q, want %q for both", re.Code, re.AuditReason, code)
	}
	if re.RetryAfter != 15*time.Second {
		t.Errorf("RetryAfter = %v, want 15s", re.RetryAfter)
	}
	e.f.requireNothingWritten(t, e.envID)
}

func TestNodeCheckFreshListWithTheNodeEnrols(t *testing.T) {
	e := newNodeCheckEnv(t)
	e.listAged(0, "other-node", swarmNodeN)

	id, err := e.enrol(t, swarmNodeN)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if id == "" {
		t.Error("no node ID")
	}
	if _, refreshes, _ := e.lists.counts(); refreshes != 0 {
		t.Errorf("%d refreshes for a list that held the node, want 0", refreshes)
	}
	if n := e.f.countNodes(t, e.envID); n != 1 {
		t.Errorf("%d node rows, want 1", n)
	}
}

func TestNodeCheckFreshListWithoutTheNodeRefusesNodeNotInSwarm(t *testing.T) {
	e := newNodeCheckEnv(t)
	e.listAged(0, "other-node")

	_, err := e.enrol(t, swarmNodeN)
	e.requireRefusal(t, err, ingresswire.RefuseNodeNotInSwarm)
	if _, refreshes, _ := e.lists.counts(); refreshes != 0 {
		t.Errorf("%d refreshes of a list read just now, want none", refreshes)
	}
}

func TestNodeCheckAMissOnAListSecondsOldReadsItAgainOnce(t *testing.T) {
	t.Run("just under five seconds old: no refresh", func(t *testing.T) {
		e := newNodeCheckEnv(t)
		e.listAged(5*time.Second-time.Nanosecond, "other-node")
		_, err := e.enrol(t, swarmNodeN)
		e.requireRefusal(t, err, ingresswire.RefuseNodeNotInSwarm)
		if _, refreshes, _ := e.lists.counts(); refreshes != 0 {
			t.Errorf("%d refreshes, want none", refreshes)
		}
	})
	t.Run("five seconds old and the refreshed list holds the node: enrols", func(t *testing.T) {
		e := newNodeCheckEnv(t)
		e.listAged(5*time.Second, "other-node")
		e.lists.next = &nodeList{ids: []string{"other-node", swarmNodeN}, at: e.f.clock.Now(), ok: true}
		if _, err := e.enrol(t, swarmNodeN); err != nil {
			t.Fatalf("Enrol: %v", err)
		}
		if _, refreshes, minAge := e.lists.counts(); refreshes != 1 || minAge != 5*time.Second {
			t.Errorf("refreshes = %d, minAge = %v, want one refresh asking for a list at least 5s old", refreshes, minAge)
		}
	})
	t.Run("five seconds old and the refreshed list lacks it: refused after one refresh", func(t *testing.T) {
		e := newNodeCheckEnv(t)
		e.listAged(5*time.Second, "other-node")
		e.lists.next = &nodeList{ids: []string{"other-node"}, at: e.f.clock.Now(), ok: true}
		_, err := e.enrol(t, swarmNodeN)
		e.requireRefusal(t, err, ingresswire.RefuseNodeNotInSwarm)
		if _, refreshes, _ := e.lists.counts(); refreshes != 1 {
			t.Errorf("%d refreshes, want exactly one", refreshes)
		}
	})
}

func TestNodeCheckSwarmWithoutNodeIDRefusesNodeUnverified(t *testing.T) {
	e := newNodeCheckEnv(t)
	e.listAged(0, swarmNodeN)

	_, err := e.enrol(t, "")
	e.requireRefusal(t, err, ingresswire.RefuseNodeUnverified)
	if asked, refreshes, _ := e.lists.counts(); asked != 0 || refreshes != 0 {
		t.Errorf("provider asked %d times and refreshed %d times for a node that reported no ID, want 0 and 0", asked, refreshes)
	}
}

func TestNodeCheckNoFreshListRefusesNodeUnverifiedRetryable(t *testing.T) {
	cases := []struct {
		name string
		set  func(e *nodeCheckEnv)
	}{
		{"no list held", func(*nodeCheckEnv) {}},
		{"list not ok", func(e *nodeCheckEnv) {
			e.f.nodes.set(e.envID, nodeList{ids: []string{swarmNodeN}, at: e.f.clock.Now(), ok: false})
		}},
		{"list two minutes and one nanosecond old", func(e *nodeCheckEnv) {
			e.listAged(2*time.Minute+time.Nanosecond, swarmNodeN)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newNodeCheckEnv(t)
			tc.set(e)
			_, err := e.enrol(t, swarmNodeN)
			e.requireRefusal(t, err, ingresswire.RefuseNodeUnverified)
		})
	}
}

func TestNodeCheckListOfExactlyTwoMinutesIsFresh(t *testing.T) {
	e := newNodeCheckEnv(t)
	e.listAged(2*time.Minute, swarmNodeN)

	if _, err := e.enrol(t, swarmNodeN); err != nil {
		t.Fatalf("Enrol on a list of exactly two minutes: %v", err)
	}
	if n := e.f.countNodes(t, e.envID); n != 1 {
		t.Errorf("%d node rows, want 1", n)
	}
}

func TestNodeCheckWithoutAProviderRefusesNodeUnverified(t *testing.T) {
	e := newNodeCheckEnv(t)
	e.f.svc.WithNodeLists(nil)

	_, err := e.enrol(t, swarmNodeN)
	e.requireRefusal(t, err, ingresswire.RefuseNodeUnverified)
}
