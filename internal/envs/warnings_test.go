// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"strings"
	"testing"
	"time"
)

// The environment warnings of the header bell (ENV-06, ENV-08, D-37, D-39): a pure
// function of the environments, their agents and the clock, so a table of cases covers
// it.

var warnNow = time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

// warnEnv is an agent environment in the given state.
func warnEnv(id, name string, state State) Environment {
	return Environment{ID: id, Kind: KindAgent, Name: name, Enabled: true, CreatedAt: warnNow.Add(-time.Hour), Health: Health{State: state}}
}

// byID picks the warnings with one ID, so a test reads only what it is about.
func byID(ws []EnvWarning, id string) []EnvWarning {
	var out []EnvWarning
	for _, w := range ws {
		if w.ID == id {
			out = append(out, w)
		}
	}
	return out
}

func TestWarningsAnOfflineEnvironmentIsAnErrorWithSinceAndReasonAndARevokedOneIsNothing(t *testing.T) {
	since := warnNow.Add(-40 * time.Minute)
	off := warnEnv("e1", "Edge", StateOffline)
	off.Health.Reason, off.Health.Since = ReasonAgentOffline, since
	revoked := warnEnv("e2", "Old", StateRevoked)
	revoked.Health.Reason = ReasonRevoked
	online := warnEnv("e3", "Fine", StateOnline)

	got := Warnings([]Environment{off, revoked, online}, nil, warnNow)
	if len(got) != 1 {
		t.Fatalf("warnings = %+v, want exactly the offline one", got)
	}
	w := got[0]
	if w.ID != WarnEnvOffline || w.Severity != SeverityError || w.EnvID != "e1" || w.EnvName != "Edge" ||
		w.Reason != ReasonAgentOffline || !w.Since.Equal(since) {
		t.Fatalf("offline warning = %+v", w)
	}
}

func TestWarningsACertificateWithSixDaysLeftIsAWarningAndAnExpiredOneAnError(t *testing.T) {
	e := warnEnv("e1", "Edge", StateDegraded)
	agents := map[string][]AgentState{"e1": {
		{ID: "a1", Node: "node-a", CertNotAfter: warnNow.Add(6 * 24 * time.Hour)},
		{ID: "a2", Node: "node-b", CertNotAfter: warnNow.Add(-time.Minute)},
		{ID: "a3", Node: "node-c", CertNotAfter: warnNow.Add(30 * 24 * time.Hour)},
		{ID: "a4", Node: "node-d", CertNotAfter: warnNow.Add(time.Hour), Revoked: true},
	}}
	got := byID(Warnings([]Environment{e}, agents, warnNow), WarnCertNotRenewing)
	if len(got) != 2 {
		t.Fatalf("cert warnings = %+v, want node-a and node-b only (a healthy and a revoked agent say nothing)", got)
	}
	bySeverity := map[string]EnvWarning{}
	for _, w := range got {
		bySeverity[w.Severity] = w
	}
	if w := bySeverity[SeverityWarning]; w.Node != "node-a" || w.EnvID != "e1" || w.EnvName != "Edge" || !w.ExpiresAt.Equal(warnNow.Add(6*24*time.Hour)) {
		t.Errorf("six days left = %+v, want a warning for node-a with its expiry", w)
	}
	if w := bySeverity[SeverityError]; w.Node != "node-b" {
		t.Errorf("expired = %+v, want an error for node-b", w)
	}
}

func TestWarningsTwoAgentsThatNeedAManualUpdateAreOneItemWithCountTwo(t *testing.T) {
	a, b := warnEnv("e1", "A", StateOnline), warnEnv("e2", "B", StateOnline)
	agents := map[string][]AgentState{
		"e1": {{ID: "a1", Node: "n1", UpdateFailed: true}, {ID: "a2", Node: "n2"}},
		"e2": {{ID: "a3", Node: "n3", ManualUpdate: true}, {ID: "a4", Node: "n4", ManualUpdate: true, Revoked: true}},
	}
	got := byID(Warnings([]Environment{a, b}, agents, warnNow), WarnManualUpdateNeeded)
	if len(got) != 1 || got[0].Count != 2 || got[0].Severity != SeverityWarning {
		t.Fatalf("manual update warnings = %+v, want one with count 2 (a revoked agent is not counted)", got)
	}
	if got[0].EnvID != "" {
		t.Errorf("an item that spans two environments names %q, want none", got[0].EnvID)
	}

	one := byID(Warnings([]Environment{a}, map[string][]AgentState{"e1": {{ID: "a1", Node: "n1", ManualUpdate: true}}}, warnNow), WarnManualUpdateNeeded)
	if len(one) != 1 || one[0].Count != 1 || one[0].EnvID != "e1" || one[0].EnvName != "A" {
		t.Errorf("one environment: %+v, want count 1 naming e1", one)
	}
	if none := Warnings([]Environment{a}, map[string][]AgentState{"e1": {{ID: "a1", Node: "n1"}}}, warnNow); len(none) != 0 {
		t.Errorf("an agent that needs nothing gave %+v", none)
	}
}

func TestWarningsAClockOffByMoreThanThirtySecondsNamesTheNodeAndTheOffset(t *testing.T) {
	e := warnEnv("e1", "Edge", StateDegraded)
	e.Health.Issues = []Issue{{Code: ReasonClockSkew, Node: "node-a", Since: warnNow.Add(-5 * time.Minute)}}
	agents := map[string][]AgentState{"e1": {
		{ID: "a1", Node: "node-a", ClockOffset: 45 * time.Second},
		{ID: "a2", Node: "node-b", ClockOffset: -29 * time.Second},
		{ID: "a3", Node: "node-c", ClockOffset: -2 * time.Minute},
	}}
	got := byID(Warnings([]Environment{e}, agents, warnNow), WarnClockSkew)
	if len(got) != 2 {
		t.Fatalf("clock warnings = %+v, want node-a and node-c", got)
	}
	for _, w := range got {
		switch w.Node {
		case "node-a":
			if w.Offset != 45*time.Second || w.Severity != SeverityWarning || !w.Since.Equal(warnNow.Add(-5*time.Minute)) {
				t.Errorf("node-a = %+v", w)
			}
		case "node-c":
			if w.Offset != -2*time.Minute {
				t.Errorf("node-c = %+v, want the signed offset", w)
			}
		default:
			t.Errorf("unexpected clock warning %+v", w)
		}
	}
}

func TestWarningsAPendingEnvironmentIsLongAtSevenDaysAndNotBefore(t *testing.T) {
	at := func(age time.Duration) []EnvWarning {
		e := warnEnv("e1", "Waiting", StatePending)
		e.CreatedAt = warnNow.Add(-age)
		return byID(Warnings([]Environment{e}, nil, warnNow), WarnPendingLong)
	}
	if got := at(6*24*time.Hour + 23*time.Hour); len(got) != 0 {
		t.Fatalf("pending for 6 days 23 hours: %+v, want nothing", got)
	}
	got := at(7*24*time.Hour + time.Minute)
	if len(got) != 1 || got[0].Days != 7 || got[0].EnvID != "e1" || got[0].EnvName != "Waiting" || got[0].Severity != SeverityWarning {
		t.Fatalf("pending for 7 days and a minute: %+v, want one with days 7", got)
	}
	if got := at(10*24*time.Hour + 5*time.Hour); len(got) != 1 || got[0].Days != 10 {
		t.Fatalf("pending for 10 days 5 hours: %+v, want days 10", got)
	}
	// Only a pending environment: one that has an agent is not waiting for one.
	online := warnEnv("e2", "Done", StateOnline)
	online.CreatedAt = warnNow.Add(-30 * 24 * time.Hour)
	if got := Warnings([]Environment{online}, nil, warnNow); len(got) != 0 {
		t.Fatalf("an old online environment gave %+v", got)
	}
}

func TestWarningsAReinitialisedClusterAndAFoldOfferHaveTheirItems(t *testing.T) {
	changed := warnEnv("e1", "Swarm", StateClusterChanged)
	changed.Health.Reason, changed.Health.Since = ReasonClusterChanged, warnNow.Add(-time.Hour)
	swarm := warnEnv("e2", "Prod swarm", StateOnline)
	swarm.FoldOffered = true
	local := Environment{ID: "e3", Kind: KindLocal, Name: "This host", Enabled: true, FoldOffered: true, Health: Health{State: StateOnline}}

	got := Warnings([]Environment{changed, swarm, local}, nil, warnNow)
	if c := byID(got, WarnClusterChanged); len(c) != 1 || c[0].EnvID != "e1" || c[0].EnvName != "Swarm" {
		t.Errorf("cluster_changed = %+v", c)
	}
	f := byID(got, WarnFoldOffered)
	if len(f) != 1 || f[0].EnvID != "e2" || f[0].EnvName != "Prod swarm" || f[0].Severity != SeverityWarning {
		t.Fatalf("fold_offered = %+v, want one item naming the swarm environment", f)
	}

	// A caller who may read the local host but not the swarm learns nothing of the swarm.
	if only := Warnings([]Environment{local}, nil, warnNow); len(only) != 0 {
		t.Errorf("the local environment alone gave %+v, want nothing (it would name a swarm the caller cannot read)", only)
	}
}

func TestWarningsAreOrderedErrorsFirstThenNewestAndNamesAreCapped(t *testing.T) {
	long := strings.Repeat("n", 200)
	older := warnEnv("e1", "Older", StateOffline)
	older.Health.Since = warnNow.Add(-2 * time.Hour)
	newer := warnEnv("e2", long, StateOffline)
	newer.Health.Since = warnNow.Add(-time.Minute)
	e3 := warnEnv("e3", "Certs", StateDegraded)
	agents := map[string][]AgentState{"e3": {{ID: "a1", Node: long, CertNotAfter: warnNow.Add(24 * time.Hour)}}}

	got := Warnings([]Environment{older, e3, newer}, agents, warnNow)
	if len(got) != 3 {
		t.Fatalf("warnings = %+v", got)
	}
	if got[0].EnvID != "e2" || got[1].EnvID != "e1" || got[2].ID != WarnCertNotRenewing {
		t.Fatalf("order = %s, %s, %s; want the newer error, the older error, then the warning", got[0].EnvID, got[1].EnvID, got[2].ID)
	}
	if n := len([]rune(got[0].EnvName)); n != 64 {
		t.Errorf("a 200 character environment name is %d characters, want it capped at 64", n)
	}
	if n := len([]rune(got[2].Node)); n != 64 {
		t.Errorf("a 200 character node name is %d characters, want it capped at 64", n)
	}
}

// Restart loops in the bell (plan 06-35, RES-13, D-15): one warning per flagged service that
// nobody muted, derived from the loops of the last read.

func TestRestartLoopWarningsAreOneErrorPerUnmutedFlaggedServiceWithWhatTheBellSays(t *testing.T) {
	e := warnEnv("e1", "Edge", StateOnline)
	since := warnNow.Add(-3 * time.Minute)
	loops := map[string]RestartLoop{
		"svc-web":   {ServiceID: "svc-web", Name: "web", Count: 4, Window: 10 * time.Minute, LatestError: "task: non-zero exit (1)", Since: since},
		"svc-quiet": {ServiceID: "svc-quiet", Name: "quiet", Count: 3, Window: 10 * time.Minute, Since: since, Muted: true},
	}
	got := RestartLoopWarnings(e, loops)
	if len(got) != 1 {
		t.Fatalf("warnings = %+v, want only the unmuted service", got)
	}
	w := got[0]
	if w.ID != WarnRestartLoop || w.Severity != SeverityError || w.EnvID != "e1" || w.EnvName != "Edge" ||
		w.Service != "web" || w.ServiceID != "svc-web" || w.Count != 4 || w.WindowMinutes != 10 ||
		w.LatestError != "task: non-zero exit (1)" || !w.Since.Equal(since) {
		t.Fatalf("warning = %+v", w)
	}
	if got := RestartLoopWarnings(e, nil); len(got) != 0 {
		t.Errorf("no loops gave %+v, want none", got)
	}
}

func TestRestartLoopWarningsCutLongNamesAndTextAndSortAfterTheirEnvironmentsOtherErrors(t *testing.T) {
	e := warnEnv("e1", "Edge", StateOnline)
	long := strings.Repeat("n", 200)
	loops := map[string]RestartLoop{
		"svc-1": {ServiceID: "svc-1", Name: long, Count: 3, Window: 5 * time.Minute, LatestError: strings.Repeat("e", 1000), Since: warnNow},
	}
	ws := RestartLoopWarnings(e, loops)
	if len(ws) != 1 {
		t.Fatalf("warnings = %+v, want one for the one loop", ws)
	}
	w := ws[0]
	if len(w.Service) > 64 || len(w.LatestError) > 300 {
		t.Errorf("service is %d characters and latestError %d, want them cut", len(w.Service), len(w.LatestError))
	}

	off := warnEnv("e0", "Down", StateOffline)
	off.Health.Since = warnNow.Add(-time.Hour)
	all := append(Warnings([]Environment{off}, nil, warnNow), RestartLoopWarnings(e, map[string]RestartLoop{
		"svc-b": {ServiceID: "svc-b", Name: "b", Count: 3, Window: time.Minute, Since: warnNow},
		"svc-a": {ServiceID: "svc-a", Name: "a", Count: 3, Window: time.Minute, Since: warnNow},
	})...)
	SortWarnings(all)
	var order []string
	for _, w := range all {
		order = append(order, w.ID+":"+w.ServiceID)
	}
	if want := "restart_loop:svc-a,restart_loop:svc-b,env_offline:"; strings.Join(order, ",") != want {
		t.Errorf("order = %v, want errors newest first, then by service ID", order)
	}
}
