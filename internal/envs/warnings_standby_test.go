// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"testing"
	"time"
)

// The agent on the folded TierPlan host's node still stands by, and the bell says so (D-19,
// D-22): its items are raised as for any agent and carry the flag, never suppressed.

func TestWarningsCarryTheStandbyFlagOnCertAndClockItems(t *testing.T) {
	e := warnEnv("e1", "Swarm", StateDegraded)
	agents := map[string][]AgentState{"e1": {
		{ID: "a1", Node: "folded", Standby: true, CertNotAfter: warnNow.Add(6 * 24 * time.Hour), ClockOffset: 2 * time.Minute},
		{ID: "a2", Node: "other", CertNotAfter: warnNow.Add(6 * 24 * time.Hour), ClockOffset: 2 * time.Minute},
	}}
	ws := Warnings([]Environment{e}, agents, warnNow)
	for _, id := range []string{WarnCertNotRenewing, WarnClockSkew} {
		got := byID(ws, id)
		if len(got) != 2 {
			t.Fatalf("%s items = %+v, want one per agent: the standby agent's is not suppressed", id, got)
		}
		for _, w := range got {
			if want := w.AgentID == "a1"; w.Standby != want {
				t.Errorf("%s of %s has Standby %v, want %v", id, w.AgentID, w.Standby, want)
			}
		}
	}
}

func TestTheManualUpdateItemIsMarkedWhenItCountsTheStandbyAgent(t *testing.T) {
	e := warnEnv("e1", "Swarm", StateOnline)
	cases := map[string]struct {
		agents  []AgentState
		standby bool
	}{
		"one of two is the standby": {[]AgentState{
			{ID: "a1", Node: "folded", Standby: true, ManualUpdate: true},
			{ID: "a2", Node: "other", ManualUpdate: true},
		}, true},
		"neither is the standby": {[]AgentState{
			{ID: "a1", Node: "n1", ManualUpdate: true},
			{ID: "a2", Node: "n2", UpdateFailed: true},
		}, false},
		"the standby needs nothing": {[]AgentState{
			{ID: "a1", Node: "folded", Standby: true},
			{ID: "a2", Node: "other", ManualUpdate: true},
			{ID: "a3", Node: "third", ManualUpdate: true},
		}, false},
	}
	for name, c := range cases {
		got := byID(Warnings([]Environment{e}, map[string][]AgentState{"e1": c.agents}, warnNow), WarnManualUpdateNeeded)
		wantCount := 0
		for _, a := range c.agents {
			if a.ManualUpdate || a.UpdateFailed {
				wantCount++
			}
		}
		if len(got) != 1 || got[0].Count != wantCount || got[0].Standby != c.standby {
			t.Errorf("%s: items = %+v, want one with Count %d and Standby %v", name, got, wantCount, c.standby)
		}
	}
}
