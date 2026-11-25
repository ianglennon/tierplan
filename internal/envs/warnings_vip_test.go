// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"strings"
	"testing"
	"time"
)

// The virtual IP's bell items (Phase 26, A-07, UI-SPEC S4): a pure function of the facts, the agents and the clock.

var vipNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// vipOn is the facts of an environment whose virtual IP is on and held by one node: nothing to say.
func vipOn() VIPFacts {
	return VIPFacts{
		Enabled: true, Address: "192.168.10.250/24", RouterID: 51, AuthWindow: 30 * time.Second,
		Holders: []VIPHolderFact{{NodeID: "n1", Host: "node-a"}},
	}
}

// vipByID is the items of one ID.
func vipByID(ws []EnvWarning, id string) []EnvWarning {
	var out []EnvWarning
	for _, w := range ws {
		if w.ID == id {
			out = append(out, w)
		}
	}
	return out
}

func vipWarn(f VIPFacts, agents ...AgentState) []EnvWarning {
	return VIPWarnings("env-1", "Home swarm", f, agents, vipNow)
}

// TestVIPBellItems pins each item, its severity and the facts it carries (A-07, UI-SPEC S4, U-06, U-07, U-10).
func TestVIPBellItems(t *testing.T) {
	t.Run("a healthy virtual IP says nothing", func(t *testing.T) {
		if got := vipWarn(vipOn()); len(got) != 0 {
			t.Fatalf("got %+v, want nothing", got)
		}
	})

	t.Run("two holders", func(t *testing.T) {
		f := vipOn()
		f.Holders = append(f.Holders, VIPHolderFact{NodeID: "n2", Host: "node-b"})
		got := vipWarn(f)
		if len(got) != 1 || got[0].ID != WarnVIPMultipleHolders || got[0].Severity != SeverityWarning || got[0].Count != 2 ||
			got[0].VIP == nil || got[0].VIP.Address != "192.168.10.250/24" || got[0].EnvID != "env-1" || got[0].EnvName != "Home swarm" {
			t.Fatalf("got %+v, want one vip_multiple_holders warning with count 2 and the address", got)
		}
	})

	t.Run("the only holder cannot be heard", func(t *testing.T) {
		ago := func(d time.Duration) time.Time { return vipNow.Add(-d) }
		cases := []struct {
			name          string
			holder, other time.Time
			holders       int
			want          bool
		}{
			{"unheard for longer than the limit, another node heard", ago(VIPUnheardAfter + time.Second), ago(2 * time.Second), 1, true},
			{"unheard for exactly the limit", ago(VIPUnheardAfter), ago(2 * time.Second), 1, false},
			{"heard", ago(2 * time.Second), ago(2 * time.Second), 1, false},
			{"nobody is heard, which the environment's own items say", ago(5 * time.Minute), ago(5 * time.Minute), 1, false},
			{"never heard", time.Time{}, ago(2 * time.Second), 1, false},
			{"two holders are another item", ago(5 * time.Minute), ago(2 * time.Second), 2, false},
		}
		for _, c := range cases {
			f := vipOn()
			f.Holders[0].LastHeard, f.LastHeard = c.holder, c.other
			if c.holders == 2 {
				f.Holders = append(f.Holders, VIPHolderFact{NodeID: "n2", Host: "node-b", LastHeard: c.other})
			}
			got := vipByID(vipWarn(f), WarnVIPHolderUnheard)
			if (len(got) == 1) != c.want {
				t.Errorf("%s: got %+v, want the item: %v", c.name, got, c.want)
				continue
			}
			if c.want && (got[0].Severity != SeverityWarning || got[0].Node != "node-a" || !got[0].Since.Equal(c.holder) ||
				got[0].VIP == nil || got[0].VIP.Address != "192.168.10.250/24") {
				t.Errorf("%s: item = %+v, want a warning about node-a since it was last heard, with the address", c.name, got[0])
			}
		}
	})

	t.Run("three moves collapse into one item about the newest", func(t *testing.T) {
		f := vipOn()
		newest := vipNow.Add(-5 * time.Minute)
		f.Moves = []VIPMoveFact{
			{At: vipNow.Add(-50 * time.Minute), From: "node-a", To: "node-b", Reason: "holder_failed"},
			{At: newest, From: "node-b", To: "node-c", Reason: "drain_handoff"},
			{At: vipNow.Add(-20 * time.Minute), From: "node-c", To: "node-b", Reason: "probe_failing"},
		}
		got := vipWarn(f)
		if len(got) != 1 {
			t.Fatalf("got %+v, want one vip_moved item", got)
		}
		w := got[0]
		if w.ID != WarnVIPMoved || w.Severity != SeverityWarning || w.Count != 3 || !w.Since.Equal(newest) ||
			w.VIP == nil || w.VIP.From != "node-b" || w.VIP.To != "node-c" || w.VIP.MoveReason != "drain_handoff" {
			t.Fatalf("item = %+v (vip %+v), want count 3 and the newest move node-b to node-c, drain_handoff", w, w.VIP)
		}
	})

	t.Run("a move with no node on one side keeps the other", func(t *testing.T) {
		f := vipOn()
		f.Moves = []VIPMoveFact{{At: vipNow.Add(-time.Minute), From: "", To: "node-a", Reason: "unknown"}}
		got := vipWarn(f)
		if len(got) != 1 || got[0].Count != 1 || got[0].VIP.From != "" || got[0].VIP.To != "node-a" {
			t.Fatalf("got %+v, want one move to node-a from no node", got)
		}
	})

	t.Run("a move whose reason is disabled raises none", func(t *testing.T) {
		f := vipOn()
		f.Moves = []VIPMoveFact{{At: vipNow.Add(-time.Minute), From: "node-a", To: "", Reason: "disabled"}}
		if got := vipWarn(f); len(got) != 0 {
			t.Fatalf("got %+v, want nothing (U-06)", got)
		}
	})

	t.Run("a disabled move beside a real one is not counted", func(t *testing.T) {
		f := vipOn()
		f.Moves = []VIPMoveFact{
			{At: vipNow.Add(-time.Minute), From: "node-a", To: "", Reason: "disabled"},
			{At: vipNow.Add(-2 * time.Minute), From: "node-b", To: "node-a", Reason: "holder_failed"},
		}
		got := vipWarn(f)
		if len(got) != 1 || got[0].Count != 1 || got[0].VIP.MoveReason != "holder_failed" {
			t.Fatalf("got %+v, want one move counted, the holder_failed one", got)
		}
	})

	t.Run("a reason outside the closed list reads as unknown", func(t *testing.T) {
		f := vipOn()
		f.Moves = []VIPMoveFact{{At: vipNow.Add(-time.Minute), From: "a", To: "b", Reason: "<script>alert(1)</script>"}}
		got := vipWarn(f)
		if len(got) != 1 || got[0].VIP.MoveReason != "unknown" {
			t.Fatalf("got %+v, want the reason unknown", got)
		}
	})

	t.Run("a foreign router with a sender and without", func(t *testing.T) {
		f := vipOn()
		f.ForeignRouter, f.ForeignSender = true, "192.168.10.77"
		got := vipWarn(f)
		if len(got) != 1 || got[0].ID != WarnVIPForeignRouter || got[0].Severity != SeverityWarning ||
			got[0].VIP == nil || got[0].VIP.RouterID != 51 || got[0].VIP.Sender != "192.168.10.77" {
			t.Fatalf("got %+v, want vip_foreign_router with router ID 51 and the sender", got)
		}
		f.ForeignSender = ""
		got = vipWarn(f)
		if len(got) != 1 || got[0].VIP.Sender != "" || got[0].VIP.RouterID != 51 {
			t.Fatalf("got %+v, want vip_foreign_router without a sender", got)
		}
	})

	t.Run("a sender that is not an address is dropped", func(t *testing.T) {
		f := vipOn()
		f.ForeignRouter, f.ForeignSender = true, "evil <b>host</b>"
		got := vipWarn(f)
		if len(got) != 1 || got[0].VIP.Sender != "" {
			t.Fatalf("got %+v, want the item without its sender", got)
		}
	})

	t.Run("clock skew from the helper and from an agent are one item each", func(t *testing.T) {
		f := vipOn()
		f.ClockNodes = []VIPClockFact{{NodeID: "n2", Host: "node-b"}}
		agents := []AgentState{
			{ID: "ag-a", Node: "node-a", ClockOffset: 15 * time.Second},
			{ID: "ag-c", Node: "node-c", ClockOffset: 14 * time.Second},
			{ID: "ag-d", Node: "node-d", ClockOffset: -20 * time.Second},
			{ID: "ag-x", Node: "node-x", ClockOffset: 60 * time.Second, Revoked: true},
		}
		got := vipByID(vipWarn(f, agents...), WarnVIPClockSkew)
		if len(got) != 3 {
			t.Fatalf("got %+v, want three items: node-a (offset 15 s), node-b (helper) and node-d (-20 s)", got)
		}
		by := map[string]EnvWarning{}
		for _, w := range got {
			by[w.Node] = w
			if w.Severity != SeverityWarning || w.VIP == nil || w.VIP.WindowSeconds != 30 {
				t.Fatalf("item = %+v (vip %+v), want a warning with windowSeconds 30", w, w.VIP)
			}
		}
		if by["node-a"].Offset != 15*time.Second || by["node-d"].Offset != -20*time.Second || by["node-b"].Offset != 0 {
			t.Fatalf("offsets = %+v, want 15 s, -20 s and none for the helper-only node", by)
		}
	})

	t.Run("a node the helper and its agent both name is one item, with the agent's offset", func(t *testing.T) {
		f := vipOn()
		f.ClockNodes = []VIPClockFact{{NodeID: "n1", Host: "node-a"}}
		got := vipByID(vipWarn(f, AgentState{ID: "ag-a", Node: "node-a", ClockOffset: 22 * time.Second}), WarnVIPClockSkew)
		if len(got) != 1 || got[0].Offset != 22*time.Second {
			t.Fatalf("got %+v, want one item with the offset 22 s", got)
		}
	})

	t.Run("one refusing manager with a connected agent", func(t *testing.T) {
		f := vipOn()
		f.PrivilegedLost = []string{"node-a"}
		got := vipWarn(f)
		if len(got) != 1 || got[0].ID != WarnVIPPrivilegedLost || got[0].Severity != SeverityWarning || got[0].Count != 1 || got[0].Node != "node-a" {
			t.Fatalf("got %+v, want vip_privileged_lost with count 1 and node-a", got)
		}
	})

	t.Run("two refusing managers are one item with a count and no node", func(t *testing.T) {
		f := vipOn()
		f.PrivilegedLost = []string{"node-a", "node-b"}
		got := vipWarn(f)
		if len(got) != 1 || got[0].Count != 2 || got[0].Node != "" {
			t.Fatalf("got %+v, want one item with count 2 and no node", got)
		}
	})

	t.Run("only no-holder is an error", func(t *testing.T) {
		f := vipOn()
		f.Holders = []VIPHolderFact{{NodeID: "n1", Host: "a"}, {NodeID: "n2", Host: "b"}}
		f.Moves = []VIPMoveFact{{At: vipNow.Add(-time.Minute), From: "a", To: "b", Reason: "unknown"}}
		f.ForeignRouter = true
		f.ClockNodes = []VIPClockFact{{NodeID: "n1", Host: "a"}}
		f.PrivilegedLost = []string{"a"}
		for _, w := range vipWarn(f) {
			if w.Severity != SeverityWarning {
				t.Fatalf("%s is %q, want a warning", w.ID, w.Severity)
			}
		}
		g := vipOn()
		g.Holders, g.NoHolderSince = nil, vipNow.Add(-time.Minute)
		if got := vipWarn(g); len(got) != 1 || got[0].ID != WarnVIPNoHolder || got[0].Severity != SeverityError {
			t.Fatalf("got %+v, want vip_no_holder as an error", got)
		}
	})

	t.Run("host names are cleaned to 64 characters", func(t *testing.T) {
		f := vipOn()
		long := strings.Repeat("h", 200) + "\x07"
		f.Moves = []VIPMoveFact{{At: vipNow.Add(-time.Minute), From: long, To: "b", Reason: "unknown"}}
		f.PrivilegedLost = []string{long}
		f.ClockNodes = []VIPClockFact{{NodeID: "n1", Host: long}}
		for _, w := range vipWarn(f) {
			for _, v := range []string{w.Node, w.EnvName} {
				if len([]rune(v)) > maxNodeLen || strings.ContainsRune(v, '\x07') {
					t.Fatalf("%s carries %q, want at most %d characters and no control character", w.ID, v, maxNodeLen)
				}
			}
			if w.VIP != nil && (len([]rune(w.VIP.From)) > maxNodeLen || strings.ContainsRune(w.VIP.From, '\x07')) {
				t.Fatalf("%s carries from %q, want it cleaned", w.ID, w.VIP.From)
			}
		}
	})

	t.Run("the order is errors first and stable", func(t *testing.T) {
		f := vipOn()
		f.Holders, f.NoHolderSince = nil, vipNow.Add(-time.Minute)
		f.ForeignRouter = true
		f.PrivilegedLost = []string{"a"}
		got := vipWarn(f)
		if len(got) != 3 || got[0].ID != WarnVIPNoHolder {
			t.Fatalf("got %+v, want the error first among three", got)
		}
	})
}

// TestVIPBellQuietCases pins when the bell says nothing about a virtual IP (A-07).
func TestVIPBellQuietCases(t *testing.T) {
	noHolder := func() VIPFacts {
		f := vipOn()
		f.Holders, f.NoHolderSince = nil, vipNow.Add(-time.Minute)
		return f
	}
	t.Run("a held address raises no vip_no_holder", func(t *testing.T) {
		f := noHolder()
		f.Holders = []VIPHolderFact{{NodeID: "n1", Host: "a"}}
		if got := vipByID(vipWarn(f), WarnVIPNoHolder); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("ten seconds exactly is not yet", func(t *testing.T) {
		f := noHolder()
		f.NoHolderSince = vipNow.Add(-VIPNoHolderAfter)
		if got := vipWarn(f); len(got) != 0 {
			t.Fatalf("got %+v, want nothing at exactly 10 s", got)
		}
		f.NoHolderSince = vipNow.Add(-VIPNoHolderAfter - time.Millisecond)
		if got := vipWarn(f); len(got) != 1 {
			t.Fatalf("got %+v, want the item just after 10 s", got)
		}
	})
	t.Run("an instant the reader does not know raises nothing", func(t *testing.T) {
		f := noHolder()
		f.NoHolderSince = time.Time{}
		if got := vipWarn(f); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a FAULT within 15 s of its task's start raises nothing", func(t *testing.T) {
		f := noHolder()
		f.Faulting = []VIPFaultFact{{Host: "node-a", TaskStarted: vipNow.Add(-14 * time.Second)}}
		if got := vipWarn(f); len(got) != 0 {
			t.Fatalf("got %+v, want nothing during the first start", got)
		}
		f.Faulting = []VIPFaultFact{{Host: "node-a", TaskStarted: vipNow.Add(-VIPFaultGrace)}}
		if got := vipWarn(f); len(got) != 1 {
			t.Fatalf("got %+v, want the item once the grace is over", got)
		}
	})
	t.Run("a FAULT whose start is not known does not hide the item", func(t *testing.T) {
		f := noHolder()
		f.Faulting = []VIPFaultFact{{Host: "node-a"}}
		if got := vipWarn(f); len(got) != 1 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a virtual IP that is off raises nothing", func(t *testing.T) {
		f := noHolder()
		f.ForeignRouter, f.PrivilegedLost = true, []string{"a"}
		f.Enabled = false
		if got := vipWarn(f); len(got) != 0 {
			t.Fatalf("got %+v, want nothing for an environment with no virtual IP", got)
		}
		if got := vipWarn(VIPFacts{}); len(got) != 0 {
			t.Fatalf("got %+v for empty facts", got)
		}
	})
	t.Run("a virtual IP being switched off raises nothing", func(t *testing.T) {
		f := noHolder()
		f.Moves = []VIPMoveFact{{At: vipNow.Add(-time.Second), From: "a", To: "", Reason: "unknown"}}
		f.Removing = true
		if got := vipWarn(f); len(got) != 0 {
			t.Fatalf("got %+v, want nothing while it is removed", got)
		}
	})
}
