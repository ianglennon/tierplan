// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
)

const selfHost = "tp.apps.example.org"

// selfClaim is the claim the tests store for a domain of an environment.
func selfClaim(envID, hostname, domainID string) SystemClaimInput {
	return SystemClaimInput{EnvID: envID, Hostname: hostname, DomainID: domainID, Service: "tierplan", Port: 7480, Actor: "u1"}
}

func TestSetSystemClaimWritesOneClaimAndOneRoute(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "e1", "One")
	d1 := registerDomain(t, s, "e1", "apps.example.org")

	if _, ok, err := s.SystemClaim(ctx); err != nil || ok {
		t.Fatalf("SystemClaim before any = ok %v, err %v; want ok=false", ok, err)
	}
	before := configVersion(t, st, "e1")

	var hooked []SystemClaim
	err := s.SetSystemClaim(ctx, selfClaim("e1", selfHost, d1.ID), func(ctx context.Context, tx *sql.Tx, c SystemClaim) error {
		// The rows are visible to the hook's own transaction.
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingress_routes WHERE hostname = ?`, c.Hostname).Scan(&n); err != nil || n != 1 {
			t.Errorf("routes visible to the hook = %d, %v; want 1", n, err)
		}
		hooked = append(hooked, c)
		return nil
	})
	if err != nil {
		t.Fatalf("SetSystemClaim: %v", err)
	}
	if len(hooked) != 1 {
		t.Fatalf("the hook ran %d times, want once", len(hooked))
	}

	got, ok, err := s.SystemClaim(ctx)
	if err != nil || !ok {
		t.Fatalf("SystemClaim = ok %v, err %v; want the claim", ok, err)
	}
	want := SystemClaim{EnvID: "e1", Hostname: selfHost, DomainID: d1.ID, Service: "tierplan", Port: 7480, ClaimedBy: "u1", ClaimedAt: fixedNow}
	if got != want {
		t.Fatalf("SystemClaim = %+v, want %+v", got, want)
	}
	if !hooked[0].ClaimedAt.Equal(want.ClaimedAt) || hooked[0].Hostname != selfHost || hooked[0].ClaimedBy != "u1" {
		t.Fatalf("the hook saw %+v, want the stored claim", hooked[0])
	}

	// One hostname row, owner system, no stack; one route row at "/" over https.
	var ownerKind string
	var stackID sql.NullString
	if err := st.Read.QueryRow(`SELECT owner_kind, stack_id FROM ingress_hostnames WHERE hostname = ?`, selfHost).Scan(&ownerKind, &stackID); err != nil {
		t.Fatalf("read the claim row: %v", err)
	}
	if ownerKind != "system" || stackID.Valid {
		t.Fatalf("claim row owner_kind %q stack_id %v, want system and NULL", ownerKind, stackID)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames`); n != 1 {
		t.Fatalf("%d hostname rows, want 1", n)
	}
	var name, path, service, scheme string
	var port, strip int
	if err := st.Read.QueryRow(`SELECT name, path_prefix, service, port, scheme, strip FROM ingress_routes WHERE hostname = ?`, selfHost).Scan(&name, &path, &service, &port, &scheme, &strip); err != nil {
		t.Fatalf("read the route row: %v", err)
	}
	if name != "tierplan" || path != "/" || service != "tierplan" || port != 7480 || scheme != "https" || strip != 0 {
		t.Fatalf("route = %q %q %q %d %q %d, want tierplan / tierplan 7480 https 0", name, path, service, port, scheme, strip)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes`); n != 1 {
		t.Fatalf("%d route rows, want 1", n)
	}

	// config_version went up by one in the same transaction.
	if after := configVersion(t, st, "e1"); after != before+1 {
		t.Fatalf("config_version = %d, want %d", after, before+1)
	}

	// The policy's world sees the host as held by the system.
	w, err := s.World(ctx, "e1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, held := w.Claims[selfHost]; !held || c.OwnerKind != ingress.OwnerSystem {
		t.Fatalf("World claim = %+v held=%v, want held by OwnerSystem", c, held)
	}
}

func TestSetSystemClaimRefusals(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "e1", "One")
	seedEnv(t, st, "e2", "Two")
	seedStack(t, st, "stack-1", "e1", "web")
	d1 := registerDomain(t, s, "e1", "apps.example.org")
	d2 := registerDomain(t, s, "e2", "apps.example.org")
	claim(t, s, "e1", "stack-1", "web.apps.example.org")

	// A hostname a stack holds is a host-taken refusal, and nothing is written.
	before := configVersion(t, st, "e1")
	err := s.SetSystemClaim(ctx, selfClaim("e1", "web.apps.example.org", d1.ID), nil)
	var refused *RefusedError
	if !errors.As(err, &refused) || len(refused.Problems) != 1 || refused.Problems[0].Code != ingress.CodeHostTaken {
		t.Fatalf("a held hostname = %v, want a host-taken refusal", err)
	}
	// The domain of another environment, the apex and a name outside the domain are refused too.
	if err := s.SetSystemClaim(ctx, selfClaim("e1", selfHost, d2.ID), nil); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("another environment's domain = %v, want ErrDomainNotFound", err)
	}
	if err := s.SetSystemClaim(ctx, selfClaim("e1", "apps.example.org", d1.ID), nil); !errors.Is(err, ErrNotUnderDomain) {
		t.Fatalf("the apex = %v, want ErrNotUnderDomain", err)
	}
	if err := s.SetSystemClaim(ctx, selfClaim("e1", "tp.other.example.net", d1.ID), nil); !errors.Is(err, ErrNotUnderDomain) {
		t.Fatalf("a name outside the domain = %v, want ErrNotUnderDomain", err)
	}
	var nameErr *ingress.NameError
	if err := s.SetSystemClaim(ctx, selfClaim("e1", "*.apps.example.org", d1.ID), nil); !errors.As(err, &nameErr) {
		t.Fatalf("a wildcard = %v, want a *ingress.NameError", err)
	}
	if after := configVersion(t, st, "e1"); after != before {
		t.Fatalf("config_version moved %d -> %d across refusals", before, after)
	}
	if _, ok, _ := s.SystemClaim(ctx); ok {
		t.Fatal("a refused call left a system claim")
	}

	// A second claim after a first is ErrSystemClaimHeld, in the same environment and in another.
	if err := s.SetSystemClaim(ctx, selfClaim("e1", selfHost, d1.ID), nil); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.SetSystemClaim(ctx, selfClaim("e1", "other.apps.example.org", d1.ID), nil); !errors.Is(err, ErrSystemClaimHeld) {
		t.Fatalf("second claim, same environment = %v, want ErrSystemClaimHeld", err)
	}
	if err := s.SetSystemClaim(ctx, selfClaim("e2", "tp.apps.example.org", d2.ID), nil); !errors.Is(err, ErrSystemClaimHeld) {
		t.Fatalf("second claim, other environment = %v, want ErrSystemClaimHeld", err)
	}

	// The database refuses a second system row even when the store's check is skipped (Pitfall 11).
	_, err = st.Write.Exec(`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at) VALUES ('x.apps.example.org', 'e2', ?, 'system', NULL, 'raw', 1)`, d2.ID)
	if err == nil {
		t.Fatal("the database accepted a second system claim")
	}
}

func TestSetSystemClaimHookErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "e1", "One")
	d1 := registerDomain(t, s, "e1", "apps.example.org")
	before := configVersion(t, st, "e1")

	boom := errors.New("audit write failed")
	err := s.SetSystemClaim(ctx, selfClaim("e1", selfHost, d1.ID), func(context.Context, *sql.Tx, SystemClaim) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("SetSystemClaim = %v, want the hook's error", err)
	}
	if _, ok, _ := s.SystemClaim(ctx); ok {
		t.Fatal("a claim survived a failing hook")
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes`); n != 0 {
		t.Fatalf("%d route rows survived a failing hook", n)
	}
	if after := configVersion(t, st, "e1"); after != before {
		t.Fatalf("config_version moved %d -> %d on a rolled-back claim", before, after)
	}
}

func TestTwoSystemClaimsAtOnceLeaveOne(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "e1", "One")
	seedEnv(t, st, "e2", "Two")
	d1 := registerDomain(t, s, "e1", "apps.example.org")
	d2 := registerDomain(t, s, "e2", "apps.example.net")

	versions := map[string]int64{"e1": configVersion(t, st, "e1"), "e2": configVersion(t, st, "e2")}

	type attempt struct {
		in    SystemClaimInput
		err   error
		hooks int
	}
	attempts := []*attempt{
		{in: selfClaim("e1", "host-a.apps.example.org", d1.ID)},
		{in: selfClaim("e2", "host-b.apps.example.net", d2.ID)},
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, a := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			a.err = s.SetSystemClaim(ctx, a.in, func(context.Context, *sql.Tx, SystemClaim) error {
				a.hooks++
				return nil
			})
		}()
	}
	close(start)
	wg.Wait()

	var winner, loser *attempt
	for _, a := range attempts {
		if a.err == nil {
			if winner != nil {
				t.Fatal("both system claims succeeded")
			}
			winner = a
		} else {
			loser = a
		}
	}
	if winner == nil || loser == nil {
		t.Fatalf("results %v and %v, want exactly one success", attempts[0].err, attempts[1].err)
	}
	if !errors.Is(loser.err, ErrSystemClaimHeld) {
		t.Fatalf("the loser got %v, want ErrSystemClaimHeld", loser.err)
	}
	if winner.hooks != 1 || loser.hooks != 0 {
		t.Fatalf("hooks ran winner %d, loser %d; want 1 and 0", winner.hooks, loser.hooks)
	}
	got, ok, err := s.SystemClaim(ctx)
	if err != nil || !ok {
		t.Fatalf("SystemClaim = ok %v, err %v", ok, err)
	}
	if got.EnvID != winner.in.EnvID || got.Hostname != winner.in.Hostname {
		t.Fatalf("SystemClaim = %+v, want the winner's %s in %s", got, winner.in.Hostname, winner.in.EnvID)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE owner_kind = 'system'`); n != 1 {
		t.Fatalf("%d system claims, want 1", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes`); n != 1 {
		t.Fatalf("%d route rows, want 1 (the loser wrote nothing)", n)
	}
	if v := configVersion(t, st, loser.in.EnvID); v != versions[loser.in.EnvID] {
		t.Fatalf("the loser's environment config_version = %d, want %d unchanged", v, versions[loser.in.EnvID])
	}
}
