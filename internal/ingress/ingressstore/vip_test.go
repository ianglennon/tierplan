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
