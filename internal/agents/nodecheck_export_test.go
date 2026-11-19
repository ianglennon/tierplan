// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"context"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/agents"
)

// agents.CheckSwarmNode is the rule the agent and the ingress enrolment share (D-22). These
// tests pin its verdicts on their own, with a fixed clock and a provider of its own, so the
// two-minute freshness boundary and the one bounded refresh hold whoever calls it.

// verdictList is a list the provider holds for one environment.
type verdictList struct {
	ids []string
	at  time.Time
	ok  bool
}

// verdictLists is a NodeLists with one list per environment, a count of the questions and
// refreshes, and a list a refresh swaps in when next is set.
type verdictLists struct {
	lists       map[string]verdictList
	next        map[string]verdictList
	asked       int
	refreshes   int
	minAge      time.Duration
	hadDeadline bool
}

func (v *verdictLists) SwarmNodeIDs(envID string) ([]string, time.Time, bool) {
	v.asked++
	l := v.lists[envID]
	return l.ids, l.at, l.ok
}

func (v *verdictLists) RefreshSwarmNodeIDs(ctx context.Context, envID string, minAge time.Duration) ([]string, time.Time, bool) {
	v.refreshes++
	v.minAge = minAge
	_, v.hadDeadline = ctx.Deadline()
	if l, ok := v.next[envID]; ok {
		v.lists[envID] = l
	}
	return v.SwarmNodeIDs(envID)
}

var verdictNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func verdictClock() time.Time { return verdictNow }

func TestCheckSwarmNodeVerdicts(t *testing.T) {
	const env, node = "env-1", "node-a"
	held := []string{"node-m", node}
	other := []string{"node-m", "node-b"}

	tests := []struct {
		name string
		// nilProvider passes a nil NodeLists.
		nilProvider  bool
		nodeID       string
		list         verdictList
		next         *verdictList
		want         agents.NodeVerdict
		wantRefresh  int
		wantNotAsked bool
	}{
		{name: "nil provider is not checked", nilProvider: true, nodeID: node, want: agents.NodeNotChecked},
		{name: "empty node ID is unverified without asking the provider", nodeID: "", list: verdictList{ids: held, at: verdictNow, ok: true}, want: agents.NodeUnverified, wantNotAsked: true},
		{name: "no list held is unverified", nodeID: node, want: agents.NodeUnverified},
		{name: "a list the provider reports not ok is unverified", nodeID: node, list: verdictList{ids: held, at: verdictNow, ok: false}, want: agents.NodeUnverified},
		{name: "a list older than two minutes is unverified", nodeID: node, list: verdictList{ids: held, at: verdictNow.Add(-10 * time.Minute), ok: true}, want: agents.NodeUnverified},
		{name: "a list exactly two minutes old holding the node is listed", nodeID: node, list: verdictList{ids: held, at: verdictNow.Add(-2 * time.Minute), ok: true}, want: agents.NodeListed},
		{name: "a list two minutes and one nanosecond old is unverified", nodeID: node, list: verdictList{ids: held, at: verdictNow.Add(-2*time.Minute - time.Nanosecond), ok: true}, want: agents.NodeUnverified},
		{name: "a fresh list holding the node is listed", nodeID: node, list: verdictList{ids: held, at: verdictNow, ok: true}, want: agents.NodeListed},
		{name: "a fresh list lacking the node, under five seconds old, is not listed with no refresh", nodeID: node, list: verdictList{ids: other, at: verdictNow.Add(-4 * time.Second), ok: true}, want: agents.NodeNotListed},
		{name: "a list one nanosecond under five seconds old is not refreshed", nodeID: node, list: verdictList{ids: other, at: verdictNow.Add(-5*time.Second + time.Nanosecond), ok: true}, want: agents.NodeNotListed},
		{name: "a list exactly five seconds old lacking the node is refreshed once and still not listed", nodeID: node, list: verdictList{ids: other, at: verdictNow.Add(-5 * time.Second), ok: true}, want: agents.NodeNotListed, wantRefresh: 1},
		{
			name: "a refresh that brings the node makes it listed", nodeID: node,
			list: verdictList{ids: other, at: verdictNow.Add(-30 * time.Second), ok: true},
			next: &verdictList{ids: held, at: verdictNow, ok: true},
			want: agents.NodeListed, wantRefresh: 1,
		},
		{
			name: "a refresh that leaves the node out is not listed", nodeID: node,
			list: verdictList{ids: other, at: verdictNow.Add(-30 * time.Second), ok: true},
			next: &verdictList{ids: other, at: verdictNow, ok: true},
			want: agents.NodeNotListed, wantRefresh: 1,
		},
		{
			name: "a refresh that loses the list is unverified", nodeID: node,
			list: verdictList{ids: other, at: verdictNow.Add(-30 * time.Second), ok: true},
			next: &verdictList{ok: false},
			want: agents.NodeUnverified, wantRefresh: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &verdictLists{lists: map[string]verdictList{}, next: map[string]verdictList{}}
			if tc.list.ids != nil || tc.list.ok || !tc.list.at.IsZero() {
				p.lists[env] = tc.list
			}
			if tc.next != nil {
				p.next[env] = *tc.next
			}
			var nodes agents.NodeLists
			if !tc.nilProvider {
				nodes = p
			}
			if got := agents.CheckSwarmNode(context.Background(), nodes, verdictClock, env, tc.nodeID); got != tc.want {
				t.Fatalf("CheckSwarmNode = %d, want %d", got, tc.want)
			}
			if p.refreshes != tc.wantRefresh {
				t.Fatalf("refreshes = %d, want %d", p.refreshes, tc.wantRefresh)
			}
			if tc.wantRefresh > 0 && !p.hadDeadline {
				t.Fatal("the refresh context had no deadline")
			}
			if tc.wantNotAsked && p.asked != 0 {
				t.Fatalf("the provider was asked %d times, want 0", p.asked)
			}
		})
	}
}

func TestCheckSwarmNodeRefreshesAtMostOnce(t *testing.T) {
	const env = "env-1"
	// The refreshed list is still stale-looking for the node, and still lacks it: the check
	// must not loop reading again.
	p := &verdictLists{
		lists: map[string]verdictList{env: {ids: []string{"node-m"}, at: verdictNow.Add(-time.Minute), ok: true}},
		next:  map[string]verdictList{env: {ids: []string{"node-m"}, at: verdictNow.Add(-time.Minute), ok: true}},
	}
	if got := agents.CheckSwarmNode(context.Background(), p, verdictClock, env, "node-a"); got != agents.NodeNotListed {
		t.Fatalf("CheckSwarmNode = %d, want NodeNotListed", got)
	}
	if p.refreshes != 1 {
		t.Fatalf("refreshes = %d, want exactly 1", p.refreshes)
	}
	if !p.hadDeadline {
		t.Fatal("the refresh context had no deadline")
	}
	if p.minAge <= 0 {
		t.Fatalf("the refresh was asked with minAge %v, want the refresh-after interval", p.minAge)
	}
}
