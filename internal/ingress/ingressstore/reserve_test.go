// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
)

func TestReserveNormalisesAndStores(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	before := configVersion(t, st, "env-a")

	calls := 0
	r, created, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: "API.apps.example.com.", Actor: "admin"},
		func(_ context.Context, tx *sql.Tx, r Reservation) error {
			calls++
			if tx == nil || r.Hostname != "api.apps.example.com" {
				t.Errorf("hook got %+v", r)
			}
			return nil
		})
	if err != nil || !created {
		t.Fatalf("Reserve = %+v, created %v, %v; want newly reserved", r, created, err)
	}
	if r.Hostname != "api.apps.example.com" || r.DomainID != dom.ID || r.Suffix != "apps.example.com" || r.CreatedBy != "admin" || !r.CreatedAt.Equal(fixedNow) {
		t.Errorf("reservation = %+v", r)
	}
	if calls != 1 {
		t.Errorf("hook ran %d times, want once", calls)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_reserved WHERE hostname = 'api.apps.example.com'`); n != 1 {
		t.Errorf("%d rows stored, want 1", n)
	}
	if after := configVersion(t, st, "env-a"); after != before+1 {
		t.Errorf("config_version = %d, want %d", after, before+1)
	}
}

func TestReserveRefusesNamesNotStrictlyBelowTheDomain(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	before := configVersion(t, st, "env-a")

	for _, h := range []string{"apps.example.com", "x.other.com", "badapps.example.com"} {
		_, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: h, Actor: "admin"}, nil)
		if !errors.Is(err, ErrNotUnderDomain) {
			t.Errorf("Reserve(%q) = %v, want ErrNotUnderDomain", h, err)
		}
	}
	for _, h := range []string{"*.apps.example.com", "", "under_score.apps.example.com"} {
		_, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: h, Actor: "admin"}, nil)
		var ne *ingress.NameError
		if !errors.As(err, &ne) {
			t.Errorf("Reserve(%q) = %v, want a *NameError", h, err)
		}
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_reserved`); n != 0 {
		t.Errorf("%d reserved rows after refusals, want 0", n)
	}
	if v := configVersion(t, st, "env-a"); v != before {
		t.Errorf("config_version moved %d -> %d on refusals", before, v)
	}
}

func TestReserveClaimedNameIsRefusedAndRedacted(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "secretstack")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	claim(t, s, "env-a", "stack-a", "app.apps.example.com")
	before := configVersion(t, st, "env-a")

	// A caller who may see the environment is told who holds the name.
	_, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: "app.apps.example.com", Actor: "admin",
		CanView: func(string) bool { return true }}, nil)
	var rc *ReservedClaimedError
	if !errors.As(err, &rc) {
		t.Fatalf("Reserve of a claimed name = %v, want *ReservedClaimedError", err)
	}
	if !strings.Contains(rc.Message, "secretstack") || !strings.Contains(rc.Message, "Alpha") {
		t.Errorf("message %q does not name the holder the caller can see", rc.Message)
	}

	// A caller who may not see it is told neither name.
	for name, canView := range map[string]func(string) bool{"nil": nil, "false": func(string) bool { return false }} {
		_, _, err = s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: "app.apps.example.com", Actor: "admin", CanView: canView}, nil)
		if !errors.As(err, &rc) {
			t.Fatalf("[%s] Reserve of a claimed name = %v, want *ReservedClaimedError", name, err)
		}
		if strings.Contains(rc.Message, "secretstack") || strings.Contains(rc.Message, "Alpha") {
			t.Errorf("[%s] message %q leaks the holder", name, rc.Message)
		}
		if strings.Contains(err.Error(), "secretstack") || strings.Contains(err.Error(), "Alpha") {
			t.Errorf("[%s] Error() %q leaks the holder", name, err.Error())
		}
		if rc.Holder.StackName != "" || rc.Holder.EnvName != "" || rc.Holder.StackID != "" {
			t.Errorf("[%s] Holder %+v carries names the caller may not see", name, rc.Holder)
		}
		if want := ingress.DescribeHolder(ingress.ClaimInfo{Claim: ingress.Claim{Hostname: "app.apps.example.com", OwnerKind: ingress.OwnerStack, EnvID: "env-a"}}, canView); rc.Message != want {
			t.Errorf("[%s] message %q is not DescribeHolder's %q", name, rc.Message, want)
		}
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_reserved`); n != 0 {
		t.Errorf("%d reserved rows after a refusal, want 0", n)
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Errorf("config_version moved %d -> %d on a refusal", before, after)
	}
}

func TestReserveTwiceIsAQuietNoOp(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	in := ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: "admin.apps.example.com", Actor: "first"}
	first, created, err := s.Reserve(ctx, in, nil)
	if err != nil || !created {
		t.Fatalf("first Reserve = %v, %v", created, err)
	}
	before := configVersion(t, st, "env-a")

	calls := 0
	in.Actor = "second"
	again, created, err := s.Reserve(ctx, in, func(context.Context, *sql.Tx, Reservation) error { calls++; return nil })
	if err != nil || created {
		t.Fatalf("second Reserve = created %v, %v; want (existing, false, nil)", created, err)
	}
	if again.CreatedBy != "first" || !again.CreatedAt.Equal(first.CreatedAt) || again.Hostname != first.Hostname {
		t.Errorf("second Reserve = %+v, want the first reservation", again)
	}
	if calls != 0 {
		t.Errorf("hook ran %d times for a no-op", calls)
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Errorf("config_version moved %d -> %d on a no-op", before, after)
	}
}

func TestUnreserveAbsentIsANoOpAndPresentRemoves(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	before := configVersion(t, st, "env-a")

	calls := 0
	hook := func(_ context.Context, _ *sql.Tx, r Reservation) error {
		calls++
		if r.Hostname != "admin.apps.example.com" || r.CreatedBy != "first" {
			t.Errorf("hook got %+v, want the reservation that was removed", r)
		}
		return nil
	}
	removed, err := s.Unreserve(ctx, "env-a", dom.ID, "admin.apps.example.com", hook)
	if err != nil || removed {
		t.Fatalf("Unreserve of an absent name = %v, %v; want (false, nil)", removed, err)
	}
	if calls != 0 || configVersion(t, st, "env-a") != before {
		t.Errorf("a no-op ran the hook (%d) or moved the version", calls)
	}

	if _, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: "admin.apps.example.com", Actor: "first"}, nil); err != nil {
		t.Fatal(err)
	}
	reserved := configVersion(t, st, "env-a")
	removed, err = s.Unreserve(ctx, "env-a", dom.ID, "Admin.apps.example.com.", hook)
	if err != nil || !removed {
		t.Fatalf("Unreserve of a present name = %v, %v; want (true, nil)", removed, err)
	}
	if calls != 1 {
		t.Errorf("hook ran %d times, want once", calls)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_reserved`); n != 0 {
		t.Errorf("%d reserved rows remain", n)
	}
	if after := configVersion(t, st, "env-a"); after != reserved+1 {
		t.Errorf("config_version = %d, want %d", after, reserved+1)
	}
}

func TestReserveHookErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	before := configVersion(t, st, "env-a")
	boom := errors.New("audit failed")
	_, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: "x.apps.example.com", Actor: "a"},
		func(context.Context, *sql.Tx, Reservation) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the hook's error", err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_reserved`); n != 0 {
		t.Errorf("%d reserved rows after a rolled-back reserve", n)
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Errorf("config_version moved %d -> %d on a rollback", before, after)
	}
}

func TestReserveInOneEnvironmentRefusesAStackOfAnotherEnvironment(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	seedStack(t, st, "stack-b", "env-b", "web-b")
	domA := registerDomain(t, s, "env-a", "apps.example.com")
	registerDomain(t, s, "env-b", "apps.example.com")
	if _, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: domA.ID, Hostname: "admin.apps.example.com", Actor: "admin"}, nil); err != nil {
		t.Fatal(err)
	}
	before := configVersion(t, st, "env-b")

	decl := ingress.Declaration{Service: "web", Host: "admin.apps.example.com", Label: "tierplan.ingress.host", Port: 80}
	_, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-b", StackID: "stack-b", Actor: "bob", Decls: []ingress.Declaration{decl}})
	var refused *RefusedError
	if !errors.As(err, &refused) || len(refused.Problems) != 1 || refused.Problems[0].Code != ingress.CodeHostReserved {
		t.Fatalf("claim of a name reserved under env A's domain = %v, want %s", err, ingress.CodeHostReserved)
	}
	if after := configVersion(t, st, "env-b"); after != before {
		t.Errorf("env B config_version moved %d -> %d on a refusal", before, after)
	}
}
