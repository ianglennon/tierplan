// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// The D-33 rollup (plan 02-11, ENV-06, ENV-08, D-33, D-40, D-41, D-45). Rollup is
// a pure function of its input, so every case is a table row with an injected
// clock.

var (
	t0     = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	window = 30 * time.Second
	grace  = 60 * time.Second
)

// standalone is a one-agent environment input whose server started at start.
func standalone(now time.Time, agents ...AgentView) RollupInput {
	return RollupInput{
		EnvID: "env-1", Kind: KindAgent, EnvType: "standalone", Agents: agents,
		ServerStart: t0, Now: now, OfflineAfter: window, Grace: grace,
	}
}

// swarm is a multi-node environment input.
func swarm(now time.Time, agents ...AgentView) RollupInput {
	in := standalone(now, agents...)
	in.EnvType = "swarm"
	return in
}

// up is an agent that is connected and healthy.
func up(id, node string) AgentView {
	return AgentView{
		ID: id, Node: node, Role: "standalone", Connected: true, ConnectedAt: t0, LastSeen: t0,
		EngineReachable: true, Target: Status{State: StateOnline, Since: t0}, CertNotAfter: t0.AddDate(1, 0, 0),
	}
}

func TestRollupHasNoAgentsIsPendingNeverAnythingElse(t *testing.T) {
	for name, in := range map[string]RollupInput{
		"standalone":                    standalone(t0.Add(time.Hour)),
		"swarm":                         swarm(t0.Add(time.Hour)),
		"just after the server started": standalone(t0.Add(time.Second)),
	} {
		got := Rollup(in)
		if got.State != StatePending || got.Reason != "" || len(got.Issues) != 0 {
			t.Errorf("%s: %+v, want pending with no reason and no issues", name, got)
		}
	}
}

func TestRollupEveryAgentRevokedIsRevokedWithTheLatestRevocationAsSince(t *testing.T) {
	a, b := up("a", "n1"), up("b", "n2")
	a.Revoked, a.RevokedAt, a.Connected = true, t0.Add(-2*time.Hour), false
	b.Revoked, b.RevokedAt, b.Connected = true, t0.Add(-time.Hour), false
	got := Rollup(standalone(t0.Add(time.Hour), a, b))
	if got.State != StateRevoked || got.Reason != ReasonRevoked || !got.Since.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("got %+v, want revoked / revoked since %v", got, t0.Add(-time.Hour))
	}
}

func TestRollupARevokedAgentDoesNotCountTowardsPresenceOrTheExpectedNodes(t *testing.T) {
	gone := up("gone", "old")
	gone.Revoked, gone.RevokedAt = true, t0
	got := Rollup(swarm(t0.Add(time.Minute), gone, up("b", "n2"), up("c", "n3")))
	if got.State != StateOnline || len(got.Issues) != 0 {
		t.Fatalf("got %+v, want online: the revoked node is neither present nor missing", got)
	}
}

func TestRollupTheClusterChangedFlagWinsWhateverTheAgentsSay(t *testing.T) {
	changedAt := t0.Add(-time.Minute)
	for name, agents := range map[string][]AgentView{
		"no agents":      nil,
		"a healthy node": {up("a", "n1")},
		"all revoked":    {func() AgentView { a := up("a", "n1"); a.Revoked = true; return a }()},
	} {
		in := swarm(t0.Add(time.Hour), agents...)
		in.ClusterChanged, in.ClusterChangedAt = true, changedAt
		got := Rollup(in)
		if got.State != StateClusterChanged || got.Reason != ReasonClusterChanged || !got.Since.Equal(changedAt) {
			t.Errorf("%s: %+v, want cluster_changed since %v", name, got, changedAt)
		}
	}
}

func TestRollupBoundaries(t *testing.T) {
	// The server started at t0. The only agent is not connected and was last
	// heard from an hour before that.
	gone := up("a", "n1")
	gone.Connected, gone.LastSeen, gone.Ended, gone.LastCode = false, t0.Add(-time.Hour), t0.Add(-time.Hour), "closed"
	timedOut := gone
	timedOut.LastCode = ReasonTimeout

	t.Run("reconnecting at 59.999s", func(t *testing.T) {
		got := Rollup(standalone(t0.Add(60*time.Second-time.Millisecond), gone))
		if got.State != StateReconnecting || got.Reason != ReasonAgentOffline || !got.Since.Equal(t0) {
			t.Fatalf("got %+v, want reconnecting / agent_offline since the server start", got)
		}
	})
	t.Run("offline at 60s", func(t *testing.T) {
		got := Rollup(standalone(t0.Add(60*time.Second), gone))
		if got.State != StateOffline || got.Reason != ReasonAgentOffline {
			t.Fatalf("got %+v, want offline / agent_offline", got)
		}
		if !got.Since.Equal(gone.Ended) {
			t.Errorf("since = %v, want the time the session ended, %v", got.Since, gone.Ended)
		}
	})
	t.Run("offline at 60s after a timeout says timeout", func(t *testing.T) {
		got := Rollup(standalone(t0.Add(60*time.Second), timedOut))
		if got.State != StateOffline || got.Reason != ReasonTimeout {
			t.Fatalf("got %+v, want offline / timeout", got)
		}
	})
	t.Run("a disconnected agent heard 29.999s ago is still present", func(t *testing.T) {
		a := up("a", "n1")
		a.Connected, a.LastSeen, a.Ended, a.LastCode = false, t0.Add(time.Hour), t0.Add(time.Hour), ReasonTimeout
		got := Rollup(standalone(t0.Add(time.Hour+30*time.Second-time.Millisecond), a))
		if got.State != StateOnline {
			t.Fatalf("got %+v, want online: a blip shorter than the window changes nothing", got)
		}
	})
	t.Run("a disconnected agent heard 30s ago is absent", func(t *testing.T) {
		a := up("a", "n1")
		a.Connected, a.LastSeen, a.Ended, a.LastCode = false, t0.Add(time.Hour), t0.Add(time.Hour), ReasonTimeout
		got := Rollup(standalone(t0.Add(time.Hour+30*time.Second), a))
		if got.State != StateOffline || got.Reason != ReasonTimeout || !got.Since.Equal(a.Ended) {
			t.Fatalf("got %+v, want offline / timeout since %v", got, a.Ended)
		}
	})
	t.Run("a connected agent is present however old its last heartbeat", func(t *testing.T) {
		a := up("a", "n1")
		a.LastSeen = t0.Add(-24 * time.Hour)
		if got := Rollup(standalone(t0.Add(time.Hour), a)); got.State != StateOnline {
			t.Fatalf("got %+v, want online", got)
		}
	})
	t.Run("certificate with 6d23h left warns", func(t *testing.T) {
		a := up("a", "n1")
		now := t0.Add(time.Hour)
		a.CertNotAfter = now.Add(CertWarnBefore - time.Hour)
		got := Rollup(standalone(now, a))
		if got.State != StateDegraded || len(got.Issues) != 1 || got.Issues[0].Code != IssueCertExpiring || got.Issues[0].Node != "n1" {
			t.Fatalf("got %+v, want degraded with one cert_expiring issue on n1", got)
		}
		if got.Reason != IssueCertExpiring {
			t.Errorf("reason = %q, want the first issue's code", got.Reason)
		}
	})
	t.Run("certificate with exactly 7d left does not warn", func(t *testing.T) {
		a := up("a", "n1")
		now := t0.Add(time.Hour)
		a.CertNotAfter = now.Add(CertWarnBefore)
		if got := Rollup(standalone(now, a)); got.State != StateOnline || len(got.Issues) != 0 {
			t.Fatalf("got %+v, want online", got)
		}
	})
	t.Run("clock offset of 31s warns", func(t *testing.T) {
		for name, off := range map[string]time.Duration{"ahead": 31 * time.Second, "behind": -31 * time.Second} {
			a := up("a", "n1")
			a.ClockOffset = off
			got := Rollup(standalone(t0.Add(time.Hour), a))
			if got.State != StateDegraded || len(got.Issues) != 1 || got.Issues[0].Code != ReasonClockSkew {
				t.Errorf("%s: got %+v, want degraded with one clock_skew issue", name, got)
			}
		}
	})
	t.Run("clock offset of 30s does not warn", func(t *testing.T) {
		for _, off := range []time.Duration{30 * time.Second, -30 * time.Second} {
			a := up("a", "n1")
			a.ClockOffset = off
			if got := Rollup(standalone(t0.Add(time.Hour), a)); got.State != StateOnline {
				t.Errorf("offset %v: got %+v, want online", off, got)
			}
		}
	})
}

func TestRollupAStandaloneAgentThatCannotReachDockerIsOfflineWithEngineUnreachable(t *testing.T) {
	a := up("a", "n1")
	a.EngineReachable = false
	got := Rollup(standalone(t0.Add(time.Hour), a))
	if got.State != StateOffline || got.Reason != ReasonEngineUnreachable {
		t.Fatalf("got %+v, want offline / engine_unreachable", got)
	}
	// The monitor's own probe failing through the tunnel says the same, with its time.
	b := up("b", "n1")
	b.Target = Status{State: StateOffline, Since: t0.Add(5 * time.Minute), Reason: "unreachable"}
	got = Rollup(standalone(t0.Add(time.Hour), b))
	if got.State != StateOffline || got.Reason != ReasonEngineUnreachable || !got.Since.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("got %+v, want offline / engine_unreachable since the failed probe", got)
	}
}

func TestRollupAStandaloneAgentOnAnUnsupportedEngineIsUnsupported(t *testing.T) {
	a := up("a", "n1")
	a.Target = Status{State: StateUnsupported, Since: t0.Add(time.Minute), Reason: "api_too_old"}
	got := Rollup(standalone(t0.Add(time.Hour), a))
	if got.State != StateUnsupported || got.Reason != ReasonUnsupportedEngine || !got.Since.Equal(t0.Add(time.Minute)) {
		t.Fatalf("got %+v, want unsupported / unsupported_engine since the probe", got)
	}
}

func TestRollupAnAgentWhoseTargetHasNotBeenProbedYetIsNotHeldAgainstIt(t *testing.T) {
	a := up("a", "n1")
	a.Target = Status{State: StateUnknown}
	if got := Rollup(standalone(t0.Add(time.Hour), a)); got.State != StateOnline {
		t.Fatalf("got %+v, want online", got)
	}
}

func TestRollupOnAnOnlineStandaloneHostThereIsNoReasonAndNoIssue(t *testing.T) {
	got := Rollup(standalone(t0.Add(time.Hour), up("a", "n1")))
	if got.State != StateOnline || got.Reason != "" || len(got.Issues) != 0 {
		t.Fatalf("got %+v, want plain online", got)
	}
}

func TestRollupSomeButNotAllExpectedAgentsPresentIsDegradedNamingTheMissingNode(t *testing.T) {
	missing := up("b", "worker-2")
	missing.Connected, missing.LastSeen, missing.Ended = false, t0.Add(10*time.Minute), t0.Add(10*time.Minute)
	got := Rollup(swarm(t0.Add(time.Hour), up("a", "worker-1"), missing, up("c", "worker-3")))
	want := []Issue{{Code: ReasonAgentOffline, Node: "worker-2", Since: missing.Ended}}
	if got.State != StateDegraded || got.Reason != ReasonAgentOffline || !reflect.DeepEqual(got.Issues, want) {
		t.Fatalf("got %+v, want degraded / agent_offline with issues %+v", got, want)
	}
	if !got.Since.Equal(missing.Ended) {
		t.Errorf("since = %v, want when the node went missing, %v", got.Since, missing.Ended)
	}
}

func TestRollupExpectedMoreAgentsThanHaveEnrolledIsDegradedWithAnIssueForEachMissing(t *testing.T) {
	in := swarm(t0.Add(time.Hour), up("a", "n1"), up("b", "n2"))
	in.Expected = 4
	got := Rollup(in)
	if got.State != StateDegraded || len(got.Issues) != 2 {
		t.Fatalf("got %+v, want degraded with two issues for the two agents that never enrolled", got)
	}
	for _, is := range got.Issues {
		if is.Code != ReasonAgentOffline || is.Node != "" {
			t.Errorf("issue %+v, want agent_offline with no node name", is)
		}
	}
}

func TestRollupOnASwarmANodeProblemIsAnIssueNotTheStateOfTheWholeEnvironment(t *testing.T) {
	unsupported := up("b", "old-node")
	unsupported.Target = Status{State: StateUnsupported, Since: t0.Add(time.Minute), Reason: "api_too_old"}
	cannotReach := up("c", "no-docker")
	cannotReach.EngineReachable = false
	got := Rollup(swarm(t0.Add(time.Hour), up("a", "fine"), unsupported, cannotReach))
	if got.State != StateDegraded {
		t.Fatalf("got %+v, want degraded", got)
	}
	var codes []string
	for _, is := range got.Issues {
		codes = append(codes, is.Code+"@"+is.Node)
	}
	if want := "engine_unreachable@no-docker,unsupported_engine@old-node"; strings.Join(codes, ",") != want {
		t.Fatalf("issues = %v, want %s (worst first)", codes, want)
	}
}

func TestRollupIssuesAreOrderedBySeverityThenNodeAndTheSameWhateverOrderTheAgentsComeIn(t *testing.T) {
	now := t0.Add(time.Hour)
	missingB := up("b", "bravo")
	missingB.Connected, missingB.LastSeen, missingB.Ended = false, t0, t0
	missingA := up("a", "alpha")
	missingA.Connected, missingA.LastSeen, missingA.Ended = false, t0, t0
	clock := up("c", "charlie")
	clock.ClockOffset = time.Minute
	cert := up("d", "delta")
	cert.CertNotAfter = now.Add(time.Hour)
	unreachable := up("e", "echo")
	unreachable.EngineReachable = false
	unsupported := up("f", "foxtrot")
	unsupported.Target = Status{State: StateUnsupported}
	agents := []AgentView{up("g", "golf"), missingB, clock, cert, unreachable, unsupported, missingA}

	want := Rollup(swarm(now, agents...))
	var order []string
	for _, is := range want.Issues {
		order = append(order, is.Code+"@"+is.Node)
	}
	const wantOrder = "agent_offline@alpha,agent_offline@bravo,engine_unreachable@echo,unsupported_engine@foxtrot,cert_expiring@delta,clock_skew@charlie"
	if strings.Join(order, ",") != wantOrder {
		t.Fatalf("issue order = %s, want %s", strings.Join(order, ","), wantOrder)
	}
	// Reverse and rotate the agents: same Health, and the caller's slice untouched.
	for i := range agents {
		rotated := append(append([]AgentView(nil), agents[i:]...), agents[:i]...)
		before := append([]AgentView(nil), rotated...)
		if got := Rollup(swarm(now, rotated...)); !reflect.DeepEqual(got, want) {
			t.Fatalf("rotation %d: %+v, want %+v", i, got, want)
		}
		if !reflect.DeepEqual(rotated, before) {
			t.Fatalf("rotation %d: Rollup reordered the caller's agents", i)
		}
	}
}

func TestRollupIsDeterministicForTheSameInput(t *testing.T) {
	in := swarm(t0.Add(time.Hour), up("a", "n1"), up("b", "n2"))
	first := Rollup(in)
	for range 50 {
		if got := Rollup(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("a second call gave %+v, want %+v", got, first)
		}
	}
}

func TestRollupAnEnvironmentWithNoAgentPresentIsNeverOnlineWithZeroNodes(t *testing.T) {
	a := up("a", "n1")
	a.Connected = false
	a.LastSeen, a.Ended = t0.Add(-time.Hour), t0.Add(-time.Hour)
	b := a
	b.ID, b.Node = "b", "n2"
	for _, now := range []time.Time{t0.Add(time.Second), t0.Add(time.Hour)} {
		got := Rollup(swarm(now, a, b))
		if got.State != StateReconnecting && got.State != StateOffline {
			t.Errorf("at %v: %+v, want reconnecting or offline, never online", now, got)
		}
	}
}

func TestRollupNodeNamesAreCappedAndFreeOfControlCharacters(t *testing.T) {
	a := up("a", "bad\x1b[31m"+strings.Repeat("x", 200)+"\nname")
	a.Target = Status{State: StateUnsupported}
	b := up("b", "   ")
	b.EngineReachable = false
	got := Rollup(swarm(t0.Add(time.Hour), a, b, up("c", "ok")))
	if len(got.Issues) != 2 {
		t.Fatalf("issues = %+v", got.Issues)
	}
	for _, is := range got.Issues {
		if n := len([]rune(is.Node)); n < 1 || n > 64 || strings.ContainsAny(is.Node, "\x1b\n") {
			t.Errorf("node %q: want 1 to 64 characters and no control characters", is.Node)
		}
	}
}

func TestRollupTheLocalSocketKeepsItsProbeStateReasonAndSince(t *testing.T) {
	for _, st := range []Status{
		{State: StateOnline, Since: t0},
		{State: StateOffline, Since: t0, Reason: "unreachable"},
		{State: StateUnsupported, Since: t0, Reason: "podman"},
		{State: StateUnknown},
	} {
		got := Rollup(RollupInput{EnvID: "local", Kind: KindLocal, Local: st, Now: t0.Add(time.Hour), ServerStart: t0})
		if got.State != st.State || got.Reason != st.Reason || !got.Since.Equal(st.Since) || len(got.Issues) != 0 {
			t.Errorf("status %+v became %+v", st, got)
		}
	}
}

func TestRollupMeasuresClocksInWholeSecondsTowardZeroAndCertificatesInWholeDaysRoundedDown(t *testing.T) {
	for in, want := range map[time.Duration]int{
		0: 0, 999 * time.Millisecond: 0, time.Second: 1, 31*time.Second + 900*time.Millisecond: 31,
		-999 * time.Millisecond: 0, -31*time.Second - 900*time.Millisecond: -31, -time.Minute: -60,
	} {
		if got := ClockOffsetSeconds(in); got != want {
			t.Errorf("ClockOffsetSeconds(%v) = %d, want %d", in, got, want)
		}
	}
	day := 24 * time.Hour
	for in, want := range map[time.Duration]int{
		7 * day: 7, 7*day - time.Nanosecond: 6, 6*day + 23*time.Hour: 6, day: 1, day - time.Second: 0, 0: 0,
		-time.Second: -1, -day: -1, -day - time.Second: -2,
	} {
		if got := CertDaysRemaining(t0.Add(in), t0); got != want {
			t.Errorf("CertDaysRemaining(now%+v) = %d, want %d", in, got, want)
		}
	}
}

// The cluster view in the rollup (plan 02-20, ENV-08, ENV-09, D-43, D-44, D-47).

func mgr(id, node string) AgentView {
	a := up(id, node)
	a.Role = "manager"
	return a
}

func wrk(id, node string) AgentView {
	a := up(id, node)
	a.Role = "worker"
	return a
}

func issueList(h Health) string {
	var out []string
	for _, is := range h.Issues {
		out = append(out, is.Code+"@"+is.Node)
	}
	return strings.Join(out, ",")
}

func TestRollupNoQuorumIsDegradedWithReasonNoQuorumWhileTheNodesStayPresent(t *testing.T) {
	since := t0.Add(30 * time.Minute)
	in := swarm(t0.Add(time.Hour), mgr("a", "m1"), mgr("b", "m2"), wrk("c", "w1"))
	in.Cluster = &ClusterView{NoQuorum: true, NoQuorumSince: since}
	got := Rollup(in)
	if got.State != StateDegraded || got.Reason != ReasonNoQuorum || issueList(got) != "no_quorum@" {
		t.Fatalf("got %+v (%s), want degraded / no_quorum with that one issue", got, issueList(got))
	}
	if !got.Since.Equal(since) {
		t.Errorf("since = %v, want when the quorum was lost, %v", got.Since, since)
	}
}

func TestRollupOnlyWorkersPresentIsDegradedWithNotAManagerRankedAboveAMissingNode(t *testing.T) {
	gone := wrk("d", "w2")
	gone.Connected, gone.LastSeen, gone.Ended = false, t0, t0
	in := swarm(t0.Add(time.Hour), wrk("a", "w1"), wrk("b", "w3"), gone)
	in.Cluster = &ClusterView{NotAManager: true}
	got := Rollup(in)
	if got.State != StateDegraded || got.Reason != ReasonNotAManager || issueList(got) != "not_a_manager@,agent_offline@w2" {
		t.Fatalf("got %+v (%s), want degraded / not_a_manager first", got, issueList(got))
	}
}

func TestRollupAManagerThatFailedOverIsAnIssueUntilItIsBack(t *testing.T) {
	away := t0.Add(10 * time.Minute)
	in := swarm(t0.Add(time.Hour), mgr("a", "m1"), mgr("b", "m2"), mgr("c", "m3"))
	in.Cluster = &ClusterView{ManagersUnreachable: []NodeSince{{Node: "m2", Since: away}}}
	got := Rollup(in)
	if got.State != StateDegraded || got.Reason != ReasonAgentOffline || issueList(got) != "agent_offline@m2" || !got.Since.Equal(away) {
		t.Fatalf("got %+v (%s), want degraded / agent_offline@m2 since %v", got, issueList(got), away)
	}
	// Back: the view no longer lists it.
	in.Cluster = &ClusterView{}
	if got := Rollup(in); got.State != StateOnline {
		t.Fatalf("got %+v, want online once the manager is back", got)
	}
}

func TestRollupDoesNotListANodeTwiceWhenItsAgentAndTheClusterViewBothSayItIsGone(t *testing.T) {
	gone := mgr("b", "m2")
	gone.Connected, gone.LastSeen, gone.Ended = false, t0, t0
	in := swarm(t0.Add(time.Hour), mgr("a", "m1"), gone)
	in.Cluster = &ClusterView{ManagersUnreachable: []NodeSince{{Node: "m2", Since: t0}}}
	if got := Rollup(in); issueList(got) != "agent_offline@m2" {
		t.Fatalf("issues = %s, want one agent_offline@m2", issueList(got))
	}
}

func TestRollupANodeOnAnEngineBelow24IsAnIssueForThatNodeOnly(t *testing.T) {
	in := swarm(t0.Add(time.Hour), mgr("a", "m1"), wrk("b", "w1"))
	in.Cluster = &ClusterView{UnsupportedNodes: []NodeSince{{Node: "old-1", Since: t0}}}
	got := Rollup(in)
	if got.State != StateDegraded || got.Reason != ReasonUnsupportedEngine || issueList(got) != "unsupported_engine@old-1" {
		t.Fatalf("got %+v (%s), want degraded / unsupported_engine@old-1", got, issueList(got))
	}
	// An agent whose own probe already found it unsupported is not listed twice.
	unsupported := wrk("c", "old-1")
	unsupported.Target = Status{State: StateUnsupported, Since: t0}
	in = swarm(t0.Add(time.Hour), mgr("a", "m1"), unsupported)
	in.Cluster = &ClusterView{UnsupportedNodes: []NodeSince{{Node: "old-1", Since: t0}}}
	if got := Rollup(in); issueList(got) != "unsupported_engine@old-1" {
		t.Fatalf("issues = %s, want one unsupported_engine@old-1", issueList(got))
	}
}

func TestRollupIgnoresTheClusterViewOfAStandaloneHostAndWithoutOne(t *testing.T) {
	in := standalone(t0.Add(time.Hour), up("a", "host"))
	in.Cluster = &ClusterView{NoQuorum: true, NotAManager: true, UnsupportedNodes: []NodeSince{{Node: "x"}}}
	if got := Rollup(in); got.State != StateOnline {
		t.Fatalf("a standalone host got %+v from a cluster view", got)
	}
	sw := swarm(t0.Add(time.Hour), wrk("a", "w1"))
	if got := Rollup(sw); got.State != StateOnline {
		t.Fatalf("no cluster view must leave the agents' own rollup alone, got %+v", got)
	}
}

func TestOverviewTakesNodesAgentsAndFailingFromAKnownClusterView(t *testing.T) {
	in := swarm(t0.Add(time.Hour), mgr("a", "m1"), mgr("b", "m2"), wrk("c", "w1"))
	in.Cluster = &ClusterView{
		Known: true, NodesReady: 5, NodesTotal: 6, ExpectedAgents: 5,
		Failing: &FailingCounts{Failing: 2, Total: 9},
	}
	o := overview(in, Status{}, "")
	if o.Nodes == nil || *o.Nodes != (NodeCounts{Ready: 5, Total: 6}) {
		t.Errorf("nodes = %+v, want 5 of 6 from the cluster", o.Nodes)
	}
	if o.Agents == nil || *o.Agents != (AgentCounts{Connected: 3, Expected: 5}) {
		t.Errorf("agents = %+v, want 3 connected of 5 expected", o.Agents)
	}
	if o.Failing == nil || *o.Failing != (FailingCounts{Failing: 2, Total: 9}) {
		t.Errorf("failing = %+v, want 2 of 9", o.Failing)
	}

	// An unknown view (no quorum, or not read yet) leaves the counts the agents'.
	in.Cluster = &ClusterView{Failing: nil}
	o = overview(in, Status{}, "")
	if o.Nodes == nil || o.Nodes.Total != 3 || o.Agents == nil || o.Agents.Expected != 3 || o.Failing != nil {
		t.Errorf("without a node list nodes %+v agents %+v failing %+v, want the agents' own counts and no failing", o.Nodes, o.Agents, o.Failing)
	}

	// A standalone host carries only the failing containers.
	st := standalone(t0.Add(time.Hour), up("a", "host"))
	st.Cluster = &ClusterView{Failing: &FailingCounts{Failing: 1, Total: 4}}
	o = overview(st, Status{}, "")
	if o.Failing == nil || *o.Failing != (FailingCounts{Failing: 1, Total: 4}) || o.Agents != nil {
		t.Errorf("standalone overview = %+v, want failing 1 of 4 and no agent counts", o)
	}
}
