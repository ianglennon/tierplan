// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
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
