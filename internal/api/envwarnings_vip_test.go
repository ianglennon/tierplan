// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
)

// The virtual IP's bell items on GET /api/environments/warnings (Phase 26, A-07, D-23, UI-SPEC S4): derived from live facts,
// never stored, shown only to callers who manage the environment's ingress. The facts come from a fake reader whose answer a
// test changes, and the clock is the tunnel's, so nothing here sleeps.

// vipBellItem is one item of the route's answer with the facts a virtual IP item carries.
type vipBellItem struct {
	ID            string `json:"id"`
	Severity      string `json:"severity"`
	EnvID         string `json:"envId"`
	EnvName       string `json:"envName"`
	Node          string `json:"node"`
	Since         string `json:"since"`
	Count         int    `json:"count"`
	OffsetSeconds int    `json:"offsetSeconds"`
	VIP           *struct {
		Address       string `json:"address"`
		RouterID      int    `json:"routerId"`
		From          string `json:"from"`
		To            string `json:"to"`
		MoveReason    string `json:"moveReason"`
		Sender        string `json:"sender"`
		WindowSeconds int    `json:"windowSeconds"`
	} `json:"vip"`
}

// fakeVIPFacts answers the bell's read of each environment's virtual IP.
type fakeVIPFacts struct {
	mu    sync.Mutex
	facts map[string]envs.VIPFacts
	asked []string
}

func (f *fakeVIPFacts) set(envID string, v envs.VIPFacts) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.facts == nil {
		f.facts = map[string]envs.VIPFacts{}
	}
	f.facts[envID] = v
}

func (f *fakeVIPFacts) VIPFacts(_ context.Context, envID string) (envs.VIPFacts, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, envID)
	v, ok := f.facts[envID]
	return v, ok, nil
}

// vipBellFix is an agent fixture whose bell reads a fake for the virtual IP facts.
func vipBellFix(t *testing.T) (*agentFix, *fakeVIPFacts) {
	t.Helper()
	f := newAgentFix(t)
	fake := &fakeVIPFacts{}
	f.envWarningsVIP = fake
	f.h = f.build(t)
	return f, fake
}

// vipItems is the virtual IP items of the answer of the route to a caller.
func (f *agentFix) vipItems(t *testing.T, cookie, csrf string) []vipBellItem {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments/warnings", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/environments/warnings = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Items []vipBellItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	var out []vipBellItem
	for _, it := range body.Items {
		if len(it.ID) > 4 && it.ID[:4] == "vip_" {
			out = append(out, it)
		}
	}
	return out
}

// TestVIPNoHolderBell is the tracer of the virtual IP's bell (A-07, D-23): with the virtual IP on and nobody holding it for
// more than 10 seconds the bell carries one vip_no_holder error with the address and since, and the item is gone on the
// first read after a holder appears. Nothing is stored: the same route answers from the facts it is given each time.
func TestVIPNoHolderBell(t *testing.T) {
	t.Parallel()
	f, fake := vipBellFix(t)
	env := f.srv.NewEnvironment(t, "Home swarm")
	lost := f.tclk.Now()
	facts := envs.VIPFacts{Enabled: true, Address: "192.168.10.250/24", RouterID: 51, NoHolderSince: lost}
	fake.set(env.ID, facts)

	// Five seconds without a holder is a handover in progress, not a problem.
	f.tclk.Advance(5 * time.Second)
	if got := f.vipItems(t, f.cookie, f.csrf); len(got) != 0 {
		t.Fatalf("5 s without a holder: %+v, want nothing", got)
	}

	// More than ten seconds is.
	f.tclk.Advance(6 * time.Second)
	got := f.vipItems(t, f.cookie, f.csrf)
	if len(got) != 1 {
		t.Fatalf("11 s without a holder: %+v, want exactly one item (asked %v)", got, fake.asked)
	}
	w := got[0]
	if w.ID != "vip_no_holder" || w.Severity != "error" || w.EnvID != env.ID || w.EnvName != "Home swarm" || w.Since != utc(lost) {
		t.Fatalf("item = %+v, want vip_no_holder / error for Home swarm since %s", w, utc(lost))
	}
	if w.VIP == nil || w.VIP.Address != "192.168.10.250/24" {
		t.Fatalf("item.vip = %+v, want the address", w.VIP)
	}

	// A holder appears: the very next read has no item, with no dismissal in between.
	facts.Holders = []envs.VIPHolderFact{{NodeID: "n1", Host: "node-a"}}
	facts.NoHolderSince = time.Time{}
	fake.set(env.ID, facts)
	if got := f.vipItems(t, f.cookie, f.csrf); len(got) != 0 {
		t.Fatalf("after a holder appeared: %+v, want the item gone", got)
	}

	// It loses its holder again, now: the clock restarts at that instant.
	facts.Holders = nil
	facts.NoHolderSince = f.tclk.Now()
	fake.set(env.ID, facts)
	if got := f.vipItems(t, f.cookie, f.csrf); len(got) != 0 {
		t.Fatalf("holder lost just now: %+v, want nothing yet", got)
	}
}

// bellKeys is every JSON object key in a body, so a test can check that no fact is named like a secret.
func bellKeys(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			into[k] = true
			bellKeys(val, into)
		}
	case []any:
		for _, val := range x {
			bellKeys(val, into)
		}
	}
}

// bellRaw is the raw answer of the route to a caller, with the virtual IP items decoded and every key it holds.
func (f *agentFix) bellRaw(t *testing.T, cookie, csrf string) ([]vipBellItem, map[string]bool) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments/warnings", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/environments/warnings = %d: %s", rec.Code, rec.Body)
	}
	var any1 any
	if err := json.Unmarshal(rec.Body.Bytes(), &any1); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	keys := map[string]bool{}
	bellKeys(any1, keys)
	return f.vipItems(t, cookie, csrf), keys
}

// TestVIPBellWireShape pins how each virtual IP item serialises (UI-SPEC S4): its facts under the agreed names, no fact named
// like a key, a secret or a token, and nothing for a caller who does not manage the environment's ingress (T-26-48).
func TestVIPBellWireShape(t *testing.T) {
	t.Parallel()
	f, fake := vipBellFix(t)
	env := f.srv.NewEnvironment(t, "Home swarm")
	now := f.tclk.Now()
	movedAt := now.Add(-4 * time.Minute)
	facts := envs.VIPFacts{
		Enabled: true, Address: "192.168.10.250/24", RouterID: 51, AuthWindow: 30 * time.Second,
		Holders: []envs.VIPHolderFact{{NodeID: "n1", Host: "node-a"}, {NodeID: "n2", Host: "node-b"}},
		Moves: []envs.VIPMoveFact{
			{At: now.Add(-40 * time.Minute), From: "node-a", To: "node-b", Reason: "holder_failed"},
			{At: movedAt, From: "node-b", To: "node-a", Reason: "probe_failing"},
		},
		ForeignRouter: true, ForeignSender: "192.168.10.77",
		ClockNodes:     []envs.VIPClockFact{{NodeID: "n2", Host: "node-b"}},
		PrivilegedLost: []string{"node-a"},
	}
	fake.set(env.ID, facts)

	items, keys := f.bellRaw(t, f.cookie, f.csrf)
	by := map[string]vipBellItem{}
	for _, it := range items {
		by[it.ID] = it
		if it.EnvID != env.ID || it.EnvName != "Home swarm" || it.Severity != "warning" || it.VIP == nil || it.VIP.Address != "192.168.10.250/24" {
			t.Fatalf("item = %+v (vip %+v), want a warning for Home swarm that carries the address", it, it.VIP)
		}
	}
	if len(items) != 5 {
		t.Fatalf("items = %+v, want one each of five ids", items)
	}
	if it := by["vip_multiple_holders"]; it.Count != 2 {
		t.Fatalf("vip_multiple_holders = %+v, want count 2", it)
	}
	if it := by["vip_moved"]; it.Count != 2 || it.Since != utc(movedAt) || it.VIP.From != "node-b" || it.VIP.To != "node-a" || it.VIP.MoveReason != "probe_failing" {
		t.Fatalf("vip_moved = %+v (vip %+v), want count 2 and the latest move node-b to node-a, probe_failing, at %s", it, it.VIP, utc(movedAt))
	}
	if it := by["vip_foreign_router"]; it.VIP.RouterID != 51 || it.VIP.Sender != "192.168.10.77" {
		t.Fatalf("vip_foreign_router = %+v (vip %+v), want router ID 51 and the sender", it, it.VIP)
	}
	if it := by["vip_clock_skew"]; it.Node != "node-b" || it.VIP.WindowSeconds != 30 {
		t.Fatalf("vip_clock_skew = %+v (vip %+v), want node-b and a window of 30 s", it, it.VIP)
	}
	if it := by["vip_privileged_lost"]; it.Count != 1 || it.Node != "node-a" {
		t.Fatalf("vip_privileged_lost = %+v, want count 1 and node-a", it)
	}

	// The only holder has not been heard from for a while, and another node has.
	facts.Holders, facts.HeardSince = []envs.VIPHolderFact{{NodeID: "n1", Host: "node-a", LastHeard: now.Add(-3 * time.Minute)}}, now.Add(-time.Hour)
	fake.set(env.ID, facts)
	items, _ = f.bellRaw(t, f.cookie, f.csrf)
	var unheard bool
	for _, it := range items {
		unheard = unheard || (it.ID == "vip_holder_unheard" && it.Severity == "warning" && it.Node == "node-a" &&
			it.Since == utc(now.Add(-3*time.Minute)) && it.VIP != nil && it.VIP.Address == "192.168.10.250/24")
	}
	if !unheard {
		t.Fatalf("items = %+v, want vip_holder_unheard about node-a", items)
	}

	// Nobody holds the address.
	facts.Holders, facts.NoHolderSince = nil, now.Add(-time.Minute)
	fake.set(env.ID, facts)
	items, more := f.bellRaw(t, f.cookie, f.csrf)
	for k := range more {
		keys[k] = true
	}
	var seen bool
	for _, it := range items {
		seen = seen || (it.ID == "vip_no_holder" && it.Severity == "error" && it.Since == utc(facts.NoHolderSince))
	}
	if !seen {
		t.Fatalf("items = %+v, want vip_no_holder as an error", items)
	}

	for _, want := range []string{"vip", "address", "routerId", "from", "to", "moveReason", "sender", "windowSeconds", "count", "since", "node"} {
		if !keys[want] {
			t.Errorf("no key %q in any item: %v", want, keys)
		}
	}
	for k := range keys {
		low := strings.ToLower(k)
		if strings.Contains(low, "key") || strings.Contains(low, "secret") || strings.Contains(low, "token") {
			t.Errorf("a key named %q is on the bell: no virtual IP fact is a key, a secret or a token", k)
		}
	}

	// A member who reads the environment but does not manage its ingress is told none of it, and the facts are not even read.
	f.addUser(t, "viola", false)
	f.grantRead(t, "viola", env.ID)
	cookie, csrf := f.signIn(t, "viola", false)
	fake.mu.Lock()
	before := len(fake.asked)
	fake.mu.Unlock()
	if got := f.vipItems(t, cookie, csrf); len(got) != 0 {
		t.Fatalf("a viewer is shown %+v, want no virtual IP item", got)
	}
	fake.mu.Lock()
	after := len(fake.asked)
	fake.mu.Unlock()
	if after != before {
		t.Fatalf("the facts were read %d times for a caller who may not see them", after-before)
	}
}
