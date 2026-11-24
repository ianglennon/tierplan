// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

// vipFixture is a virtual IP configuration for one environment.
func vipFixture(envID string) VIPConfig {
	return VIPConfig{
		EnvID: envID, Address: netip.MustParseAddr("192.168.10.250"), PrefixLen: 24, RouterID: 51,
		RouterDerived: true, KeySealed: "v1.sealed-opaque-text", KeyGeneration: 1, Enabled: true,
	}
}

// TestVIPConfigRoundTrip pins the tracer: a configuration put is the configuration got, an update of
// the address changes the address and nothing else, and an environment with none is ErrVIPNotFound.
func TestVIPConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")

	if _, err := s.GetVIP(ctx, "env-a"); !errors.Is(err, ErrVIPNotFound) {
		t.Fatalf("GetVIP before any put = %v, want ErrVIPNotFound", err)
	}

	want := vipFixture("env-a")
	if err := s.PutVIP(ctx, want); err != nil {
		t.Fatalf("PutVIP: %v", err)
	}
	want.CreatedAt, want.UpdatedAt = fixedNow, fixedNow
	got, err := s.GetVIP(ctx, "env-a")
	if err != nil {
		t.Fatalf("GetVIP: %v", err)
	}
	if got != want {
		t.Fatalf("GetVIP = %+v, want %+v", got, want)
	}
	if p := got.Prefix(); p != netip.MustParsePrefix("192.168.10.250/24") {
		t.Fatalf("Prefix = %v, want 192.168.10.250/24", p)
	}

	if err := s.UpdateVIPAddress(ctx, "env-a", netip.MustParseAddr("10.0.0.5"), 16, 77, false); err != nil {
		t.Fatalf("UpdateVIPAddress: %v", err)
	}
	got, err = s.GetVIP(ctx, "env-a")
	if err != nil {
		t.Fatalf("GetVIP after update: %v", err)
	}
	if got.Address != netip.MustParseAddr("10.0.0.5") || got.PrefixLen != 16 || got.RouterID != 77 || got.RouterDerived {
		t.Fatalf("after the update the row is %+v, want 10.0.0.5/16 router 77 chosen", got)
	}
	if got.KeySealed != want.KeySealed || got.KeyGeneration != 1 || !got.Enabled {
		t.Fatalf("the update touched the key or the switch: %+v", got)
	}

	if err := s.UpdateVIPAddress(ctx, "env-none", netip.MustParseAddr("10.0.0.5"), 16, 77, false); !errors.Is(err, ErrVIPNotFound) {
		t.Fatalf("UpdateVIPAddress of a missing row = %v, want ErrVIPNotFound", err)
	}
	if _, err := s.GetVIP(ctx, "env-none"); !errors.Is(err, ErrVIPNotFound) {
		t.Fatalf("GetVIP of a missing row = %v, want ErrVIPNotFound", err)
	}
}

// TestVIPEnsureAndSwitch pins the ensure record and the switch: neither moves the other's fields, a
// replace keeps the ensure state and the creation time, and a missing row is ErrVIPNotFound.
func TestVIPEnsureAndSwitch(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	if err := s.PutVIP(ctx, vipFixture("env-a")); err != nil {
		t.Fatalf("PutVIP: %v", err)
	}

	at := fixedNow.Add(time.Minute)
	if err := s.RecordVIPEnsure(ctx, "env-a", at, "unreachable"); err != nil {
		t.Fatalf("RecordVIPEnsure: %v", err)
	}
	if err := s.SetVIPEnabled(ctx, "env-a", false); err != nil {
		t.Fatalf("SetVIPEnabled: %v", err)
	}
	got, err := s.GetVIP(ctx, "env-a")
	if err != nil {
		t.Fatalf("GetVIP: %v", err)
	}
	if got.Enabled || !got.EnsureAt.Equal(at) || got.EnsureError != "unreachable" {
		t.Fatalf("row = %+v, want disabled with ensure %v / unreachable", got, at)
	}

	replace := vipFixture("env-a")
	replace.KeyGeneration = 2
	if err := s.PutVIP(ctx, replace); err != nil {
		t.Fatalf("PutVIP replace: %v", err)
	}
	got, _ = s.GetVIP(ctx, "env-a")
	if got.KeyGeneration != 2 || !got.EnsureAt.Equal(at) || got.EnsureError != "unreachable" {
		t.Fatalf("a replace lost the ensure state or the generation: %+v", got)
	}

	list, err := s.ListVIPs(ctx)
	if err != nil || len(list) != 1 || list[0].EnvID != "env-a" {
		t.Fatalf("ListVIPs = %+v, %v; want the one row", list, err)
	}

	if err := s.RecordVIPEnsure(ctx, "env-none", at, ""); !errors.Is(err, ErrVIPNotFound) {
		t.Fatalf("RecordVIPEnsure of a missing row = %v, want ErrVIPNotFound", err)
	}
	if err := s.SetVIPEnabled(ctx, "env-none", true); !errors.Is(err, ErrVIPNotFound) {
		t.Fatalf("SetVIPEnabled of a missing row = %v, want ErrVIPNotFound", err)
	}
	if ok, err := s.DeleteVIP(ctx, "env-a"); err != nil || !ok {
		t.Fatalf("DeleteVIP = %v, %v; want true", ok, err)
	}
	if ok, err := s.DeleteVIP(ctx, "env-a"); err != nil || ok {
		t.Fatalf("a second DeleteVIP = %v, %v; want false, nil", ok, err)
	}
}

// TestVIPMoveRoundTrip pins one move through LatestVIPMove and a first page.
func TestVIPMoveRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")

	if _, ok, err := s.LatestVIPMove(ctx, "env-a"); err != nil || ok {
		t.Fatalf("LatestVIPMove with no moves = %v, %v; want false, nil", ok, err)
	}
	m := VIPMove{EnvID: "env-a", At: fixedNow, FromNodeID: "n1", FromNodeName: "one", ToNodeID: "n2", ToNodeName: "two", Reason: "holder_failed"}
	if err := s.InsertVIPMove(ctx, m); err != nil {
		t.Fatalf("InsertVIPMove: %v", err)
	}
	got, ok, err := s.LatestVIPMove(ctx, "env-a")
	if err != nil || !ok {
		t.Fatalf("LatestVIPMove = %v, %v; want a move", ok, err)
	}
	m.ID = got.ID
	if got != m || got.ID == 0 {
		t.Fatalf("LatestVIPMove = %+v, want %+v", got, m)
	}
	page, cur, err := s.ListVIPMoves(ctx, "env-a", nil, 10)
	if err != nil || len(page) != 1 || cur != nil || page[0] != m {
		t.Fatalf("ListVIPMoves = %+v, %v, %v; want the one move and no cursor", page, cur, err)
	}
	if err := s.InsertVIPMove(ctx, VIPMove{EnvID: "env-a", At: fixedNow, Reason: "bogus"}); err == nil {
		t.Fatal("a move with a reason outside the five was stored")
	}
}

// TestVIPMovesPruneTo500 pins the bound: 505 moves leave the newest 500 of that environment, and
// another environment's rows are untouched.
func TestVIPMovesPruneTo500(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")

	for i := 0; i < 3; i++ {
		if err := s.InsertVIPMove(ctx, VIPMove{EnvID: "env-b", At: fixedNow.Add(time.Duration(i) * time.Second), Reason: "unknown"}); err != nil {
			t.Fatalf("insert for env-b: %v", err)
		}
	}
	for i := 0; i < VIPMovesKept+5; i++ {
		m := VIPMove{EnvID: "env-a", At: fixedNow.Add(time.Duration(i) * time.Second), ToNodeID: "n", Reason: "holder_failed"}
		if err := s.InsertVIPMove(ctx, m); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	if n := count(t, st, `SELECT COUNT(*) FROM ingress_vip_moves WHERE env_id = 'env-a'`); n != VIPMovesKept {
		t.Fatalf("env-a keeps %d moves, want %d", n, VIPMovesKept)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_vip_moves WHERE env_id = 'env-b'`); n != 3 {
		t.Fatalf("env-b keeps %d moves, want 3 (another environment's pruning touched it)", n)
	}
	// The five oldest are the ones that went.
	oldest := count(t, st, `SELECT COUNT(*) FROM ingress_vip_moves WHERE env_id = 'env-a' AND at < ?`, fixedNow.Add(5*time.Second).UnixNano())
	if oldest != 0 {
		t.Fatalf("%d of the five oldest moves survived the prune", oldest)
	}
	latest, ok, err := s.LatestVIPMove(ctx, "env-a")
	if err != nil || !ok || !latest.At.Equal(fixedNow.Add(time.Duration(VIPMovesKept+4)*time.Second)) {
		t.Fatalf("LatestVIPMove = %+v, %v, %v; want the 505th", latest, ok, err)
	}
}

// TestVIPMovesOrderAndPaging pins the order and the keyset cursor: newest first, equal instants by
// insertion id descending, two moves at one instant are two rows, and paging by two visits all five
// once with the boundary falling inside the run of equal instants.
func TestVIPMovesOrderAndPaging(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-empty", "Empty")

	// Inserted: three at the early instant, then two at the later one, with a name each to tell them apart.
	early, late := fixedNow, fixedNow.Add(time.Hour)
	for _, c := range []struct {
		name string
		at   time.Time
	}{{"e1", early}, {"e2", early}, {"e3", early}, {"l1", late}, {"l2", late}} {
		if err := s.InsertVIPMove(ctx, VIPMove{EnvID: "env-a", At: c.at, ToNodeName: c.name, Reason: "unknown"}); err != nil {
			t.Fatalf("insert %s: %v", c.name, err)
		}
	}
	want := []string{"l2", "l1", "e3", "e2", "e1"}

	all, cur, err := s.ListVIPMoves(ctx, "env-a", nil, 10)
	if err != nil || cur != nil {
		t.Fatalf("one big page = %v, cursor %v; want no cursor", err, cur)
	}
	if got := moveNames(all); !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	var paged []string
	var cursor *VIPMoveCursor
	pages := 0
	for {
		page, next, err := s.ListVIPMoves(ctx, "env-a", cursor, 2)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		paged = append(paged, moveNames(page)...)
		pages++
		if next == nil {
			break
		}
		if pages > 5 {
			t.Fatal("paging does not end")
		}
		cursor = next
	}
	if !equalStrings(paged, want) {
		t.Fatalf("paged by 2 = %v, want %v (a boundary inside equal instants repeated or skipped a row)", paged, want)
	}
	if pages != 3 {
		t.Fatalf("paging by 2 took %d pages, want 3", pages)
	}

	empty, cur, err := s.ListVIPMoves(ctx, "env-empty", nil, 10)
	if err != nil || empty == nil || len(empty) != 0 || cur != nil {
		t.Fatalf("an environment with no moves gave %#v, cursor %v, %v; want an empty non-nil slice and no cursor", empty, cur, err)
	}

	since, err := s.VIPMovesSince(ctx, "env-a", early)
	if err != nil || len(since) != 2 {
		t.Fatalf("VIPMovesSince(early) = %d moves, %v; want the two later ones", len(since), err)
	}
	none, err := s.VIPMovesSince(ctx, "env-empty", early)
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("VIPMovesSince of an empty environment = %#v, %v; want an empty non-nil slice", none, err)
	}
}

func moveNames(ms []VIPMove) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ToNodeName)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestVIPRowsCascade pins that deleting the environment removes the virtual IP and its history, and
// that a row for an environment that does not exist is refused.
func TestVIPRowsCascade(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	for _, id := range []string{"env-a", "env-b"} {
		if err := s.PutVIP(ctx, vipFixture(id)); err != nil {
			t.Fatalf("PutVIP %s: %v", id, err)
		}
		if err := s.InsertVIPMove(ctx, VIPMove{EnvID: id, At: fixedNow, Reason: "disabled"}); err != nil {
			t.Fatalf("InsertVIPMove %s: %v", id, err)
		}
	}

	if _, err := st.Write.Exec(`DELETE FROM environments WHERE id = 'env-a'`); err != nil {
		t.Fatalf("delete the environment: %v", err)
	}
	for _, table := range []string{"ingress_vip", "ingress_vip_moves"} {
		if n := count(t, st, `SELECT COUNT(*) FROM `+table+` WHERE env_id = 'env-a'`); n != 0 {
			t.Fatalf("%s kept %d rows of the deleted environment", table, n)
		}
		if n := count(t, st, `SELECT COUNT(*) FROM `+table+` WHERE env_id = 'env-b'`); n != 1 {
			t.Fatalf("%s has %d rows of the other environment, want 1", table, n)
		}
	}
	if err := s.PutVIP(ctx, vipFixture("env-gone")); err == nil {
		t.Fatal("a virtual IP for an environment that does not exist was stored")
	}
}
