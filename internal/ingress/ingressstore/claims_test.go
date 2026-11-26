// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/vault"
)

const host = "app.apps.example.com"

func TestTracerOneOwnerPerHostnameAcrossEnvironments(t *testing.T) {
	ctx := context.Background()
	s, st, v := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	seedStack(t, st, "stack-b", "env-b", "web-b")

	// D-04: the same suffix registers in both environments.
	const token = "cf-token-plaintext-1234" //nolint:gosec // G101: a fixed test fixture, not a credential
	domA, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "Apps.Example.com.", Issuer: ingress.IssuerStaging, Token: []byte(token), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("register the domain in env A: %v", err)
	}
	if domA.Suffix != "apps.example.com" || !domA.HasToken {
		t.Fatalf("domain = %+v, want the normalised suffix and a token", domA)
	}
	if _, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-b", Suffix: "apps.example.com", Issuer: ingress.IssuerProduction, Actor: "admin"}, nil); err != nil {
		t.Fatalf("register the same suffix in env B: %v", err)
	}
	if _, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "apps.example.com", Issuer: ingress.IssuerStaging, Actor: "admin"}, nil); !errors.Is(err, ErrDomainExists) {
		t.Fatalf("a second registration in one environment = %v, want ErrDomainExists", err)
	}

	// D-08: the token is sealed under the domain's own id.
	var sealed string
	if err := st.Read.QueryRow(`SELECT token_sealed FROM ingress_domains WHERE id = ?`, domA.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if sealed == "" || sealed == token || strings.Contains(sealed, token) {
		t.Fatalf("token_sealed = %.20q, want an envelope that hides the token", sealed)
	}
	if got, err := v.Open(vault.ContextIngressDNSToken+domA.ID, sealed); err != nil || string(got) != token {
		t.Fatalf("Open under its own domain id = %q, %v; want the token", got, err)
	}
	if _, err := v.Open(vault.ContextIngressDNSToken+"another-domain-id", sealed); err == nil {
		t.Fatal("the sealed token opened under another domain id")
	}

	// A fresh stack with no declarations commits nothing (D-23).
	before := configVersion(t, st, "env-a")
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice"}); err != nil {
		t.Fatalf("empty replace: %v", err)
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Fatalf("config_version moved %d -> %d on an empty replace", before, after)
	}

	// Stack A claims the hostname with a strip route (D-02).
	decl := ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", PathPrefix: "/api", Port: 8080, Strip: true}
	plan, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice", Decls: []ingress.Declaration{decl}})
	if err != nil {
		t.Fatalf("stack A claims %s: %v", host, err)
	}
	if got := plan.Hostnames(); len(got) != 1 || got[0] != host {
		t.Fatalf("plan hostnames = %v, want [%s]", got, host)
	}
	if after := configVersion(t, st, "env-a"); after != before+1 {
		t.Fatalf("config_version = %d after a claim, want %d", after, before+1)
	}
	var strip int
	if err := st.Read.QueryRow(`SELECT strip FROM ingress_routes WHERE hostname = ? AND path_prefix = '/api'`, host).Scan(&strip); err != nil || strip != 1 {
		t.Fatalf("stored strip = %d, %v; want 1", strip, err)
	}

	// LBL-03 / D-19: stack B in another environment is refused, and nothing of it is written.
	versionB := configVersion(t, st, "env-b")
	_, err = s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-b", StackID: "stack-b", Actor: "bob", Decls: []ingress.Declaration{decl},
		CanView: func(env string) bool { return env == "env-a" }})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("stack B claim = %v, want *RefusedError", err)
	}
	// Env B registered the suffix too, so the domain rule passes and the owner rule is what refuses.
	if len(refused.Problems) != 1 || refused.Problems[0].Code != ingress.CodeHostTaken {
		t.Fatalf("problems = %+v, want only %s", refused.Problems, ingress.CodeHostTaken)
	}
	// D-20: the caller may see env A, so the holder is named.
	if m := refused.Problems[0].Message; !strings.Contains(m, "web-a") || !strings.Contains(m, "Alpha") {
		t.Errorf("message %q does not name the holder the caller can see", m)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE env_id = 'env-b' OR stack_id = 'stack-b'`); n != 0 {
		t.Errorf("%d claim rows of stack B exist after a refusal", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes WHERE env_id = 'env-b'`); n != 0 {
		t.Errorf("%d route rows of env B exist after a refusal", n)
	}
	if after := configVersion(t, st, "env-b"); after != versionB {
		t.Errorf("env B config_version moved %d -> %d on a refusal", versionB, after)
	}

	// D-20: a caller who cannot see env A is told neither the stack nor the environment.
	_, err = s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-b", StackID: "stack-b", Actor: "bob", Decls: []ingress.Declaration{decl}})
	if !errors.As(err, &refused) || len(refused.Problems) != 1 {
		t.Fatalf("stack B claim without visibility = %v, want one problem", err)
	}
	if m := refused.Problems[0].Message; strings.Contains(m, "web-a") || strings.Contains(m, "Alpha") {
		t.Errorf("message %q leaks the holder to a caller who cannot see it", m)
	}

	// D-01: the database refuses a second owner of the hostname on its own.
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at)
		 SELECT ?, 'env-b', id, 'stack', 'stack-b', 'raw', 1 FROM ingress_domains WHERE env_id = 'env-b'`, host); err == nil {
		t.Fatal("a raw INSERT of a second owner of the hostname succeeded")
	}

	// LBL-04: a hostname outside every domain of the environment is refused.
	outside := ingress.Declaration{Service: "web", Host: "app.other.example.org", Label: "tierplan.ingress.host", Port: 80}
	_, err = s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice", Decls: []ingress.Declaration{outside}})
	if !errors.As(err, &refused) || len(refused.Problems) != 1 || refused.Problems[0].Code != ingress.CodeDomainNotAllowed {
		t.Fatalf("outside every domain = %v, want %s", err, ingress.CodeDomainNotAllowed)
	}
	// ... and the refusal left stack A's earlier claim in place.
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE stack_id = 'stack-a'`); n != 1 {
		t.Errorf("stack A holds %d claims after a refused replacement, want its earlier 1", n)
	}

	// A stack that is not in the environment is not found.
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-b", StackID: "stack-a", Actor: "x"}); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("a stack of another environment = %v, want ErrStackNotFound", err)
	}
}

// hostIn is a hostname under apps.example.com.
func hostIn(label string) string { return label + ".apps.example.com" }

// declFor is a route declaration of host to the service web on port.
func declFor(host string, port int) ingress.Declaration {
	return ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: port}
}

// listen subscribes to a notifier wired into the store and returns the subscription.
func listen(s *Store) *ingress.Subscription {
	s.notifier = ingress.NewNotifier()
	return s.notifier.Subscribe()
}

// woken reports whether a wake-up is waiting. Notify sends before it returns, so this is exact.
func woken(sub *ingress.Subscription) bool {
	select {
	case <-sub.C():
		return true
	default:
		return false
	}
}

func TestReplaceStackRoutesIdenticalPlanWritesNothing(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "apps.example.com")
	decls := []ingress.Declaration{declFor(hostIn("app"), 8080), {Service: "api", Host: hostIn("app"), Label: "tierplan.ingress.host", PathPrefix: "/api", Port: 9000, Strip: true}}
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice", Decls: decls}); err != nil {
		t.Fatal(err)
	}
	before := configVersion(t, st, "env-a")
	sub := listen(s)
	defer sub.Close()

	hooked := 0
	later := fixedNow.Add(time.Hour)
	s.now = func() time.Time { return later }
	plan, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "bob", Decls: decls,
		Hook: func(context.Context, *sql.Tx, ingress.Plan, ClaimDelta) error { hooked++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Hostnames(); len(got) != 1 || got[0] != hostIn("app") {
		t.Errorf("plan hostnames = %v, want the stored plan returned", got)
	}
	if hooked != 0 {
		t.Errorf("hook ran %d times for an identical plan", hooked)
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Errorf("config_version moved %d -> %d for an identical plan", before, after)
	}
	if woken(sub) {
		t.Error("a no-op write woke the notifier")
	}
	var by string
	var at int64
	if err := st.Read.QueryRow(`SELECT claimed_by, claimed_at FROM ingress_hostnames WHERE hostname = ?`, hostIn("app")).Scan(&by, &at); err != nil {
		t.Fatal(err)
	}
	if by != "alice" || at != fixedNow.UnixNano() {
		t.Errorf("claim = %q at %d, want it untouched (alice, %d)", by, at, fixedNow.UnixNano())
	}
}

func TestReplaceStackRoutesChangedPlanKeepsTheClaimOfAKeptHostname(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "apps.example.com")
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice", Decls: []ingress.Declaration{declFor(hostIn("keep"), 8080), declFor(hostIn("drop"), 8080)}}); err != nil {
		t.Fatal(err)
	}
	before := configVersion(t, st, "env-a")
	sub := listen(s)
	defer sub.Close()

	later := fixedNow.Add(time.Hour)
	s.now = func() time.Time { return later }
	hooked := 0
	_, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "bob",
		Decls: []ingress.Declaration{declFor(hostIn("keep"), 9090), declFor(hostIn("new"), 8080)},
		Hook:  func(context.Context, *sql.Tx, ingress.Plan, ClaimDelta) error { hooked++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if hooked != 1 {
		t.Errorf("hook ran %d times, want once", hooked)
	}
	if after := configVersion(t, st, "env-a"); after != before+1 {
		t.Errorf("config_version = %d, want %d", after, before+1)
	}
	if !woken(sub) {
		t.Fatal("a committed change did not wake the notifier")
	}
	if got := sub.Pending(); len(got) != 1 || got[0] != "env-a" {
		t.Errorf("pending = %v, want [env-a]", got)
	}
	if woken(sub) {
		t.Error("one change woke the notifier twice")
	}

	claims := map[string][2]any{}
	rows, err := st.Read.Query(`SELECT hostname, claimed_by, claimed_at FROM ingress_hostnames WHERE stack_id = 'stack-a'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var h, by string
		var at int64
		if err := rows.Scan(&h, &by, &at); err != nil {
			t.Fatal(err)
		}
		claims[h] = [2]any{by, at}
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %v, want keep and new", claims)
	}
	if got := claims[hostIn("keep")]; got != [2]any{"alice", fixedNow.UnixNano()} {
		t.Errorf("kept hostname claim = %v, want its original claimed_by and claimed_at", got)
	}
	if got := claims[hostIn("new")]; got != [2]any{"bob", later.UnixNano()} {
		t.Errorf("new hostname claim = %v, want bob at the new time", got)
	}
	if _, gone := claims[hostIn("drop")]; gone {
		t.Error("the removed declaration's hostname is still claimed")
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes WHERE hostname = ?`, hostIn("drop")); n != 0 {
		t.Errorf("%d routes of the released hostname remain", n)
	}
	var port int
	if err := st.Read.QueryRow(`SELECT port FROM ingress_routes WHERE hostname = ?`, hostIn("keep")).Scan(&port); err != nil || port != 9090 {
		t.Errorf("kept hostname route port = %d, %v; want the changed 9090", port, err)
	}
}

func TestReplaceStackRoutesPathAndSchemeDefaultsCountAsTheSamePlan(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "apps.example.com")
	explicit := declFor(hostIn("app"), 80)
	explicit.PathPrefix, explicit.Scheme = "/", ingress.SchemeHTTP
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "a", Decls: []ingress.Declaration{declFor(hostIn("app"), 80)}}); err != nil {
		t.Fatal(err)
	}
	before := configVersion(t, st, "env-a")
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "a", Decls: []ingress.Declaration{explicit}}); err != nil {
		t.Fatal(err)
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Errorf("an explicit default moved config_version %d -> %d", before, after)
	}
}

func TestReplaceStackRoutesRefusalChangesNothing(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	seedStack(t, st, "stack-b", "env-b", "web-b")
	domA := registerDomain(t, s, "env-a", "apps.example.com")
	registerDomain(t, s, "env-b", "apps.example.com")
	claim(t, s, "env-a", "stack-a", hostIn("taken"))
	if _, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: domA.ID, Hostname: hostIn("held-back"), Actor: "admin"}, nil); err != nil {
		t.Fatal(err)
	}
	before := configVersion(t, st, "env-b")
	sub := listen(s)
	defer sub.Close()

	for want, host := range map[string]string{
		ingress.CodeHostTaken:        hostIn("taken"),
		ingress.CodeHostReserved:     hostIn("held-back"),
		ingress.CodeDomainNotAllowed: "app.other.example.org",
	} {
		_, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-b", StackID: "stack-b", Actor: "bob", Decls: []ingress.Declaration{declFor(host, 80)}})
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Problems[0].Code != want {
			t.Errorf("%s = %v, want a refusal %s", host, err, want)
		}
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE env_id = 'env-b'`); n != 0 {
		t.Errorf("%d claims of env B after refusals", n)
	}
	if after := configVersion(t, st, "env-b"); after != before {
		t.Errorf("env B config_version moved %d -> %d on refusals", before, after)
	}
	if woken(sub) {
		t.Error("a refused write woke the notifier")
	}
}

func TestReleaseStackReleasesEverythingAndNotifiesOnce(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "apps.example.com")
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice", Decls: []ingress.Declaration{declFor(hostIn("one"), 80), declFor(hostIn("two"), 80)}}); err != nil {
		t.Fatal(err)
	}
	before := configVersion(t, st, "env-a")
	sub := listen(s)
	defer sub.Close()

	hooked := 0
	hook := func(_ context.Context, _ *sql.Tx, p ingress.Plan, _ ClaimDelta) error {
		hooked++
		if len(p.Claims) != 0 {
			t.Errorf("release hook plan = %+v, want empty", p)
		}
		return nil
	}
	if err := s.ReleaseStack(ctx, "env-a", "stack-a", "carol", hook); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE stack_id = 'stack-a'`); n != 0 {
		t.Errorf("%d claims remain", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes WHERE env_id = 'env-a'`); n != 0 {
		t.Errorf("%d routes remain", n)
	}
	if hooked != 1 {
		t.Errorf("hook ran %d times, want once", hooked)
	}
	if after := configVersion(t, st, "env-a"); after != before+1 {
		t.Errorf("config_version = %d, want %d", after, before+1)
	}
	if !woken(sub) || len(sub.Pending()) != 1 || woken(sub) {
		t.Error("release did not wake the notifier exactly once for one environment")
	}

	// A stack with nothing left is a no-op.
	if err := s.ReleaseStack(ctx, "env-a", "stack-a", "carol", hook); err != nil {
		t.Fatal(err)
	}
	if hooked != 1 || configVersion(t, st, "env-a") != before+1 || woken(sub) {
		t.Error("releasing a stack that holds nothing ran the hook, moved the version or woke the notifier")
	}
	if err := s.ReleaseStack(ctx, "env-b", "stack-a", "carol", nil); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("ReleaseStack of another environment's stack = %v, want ErrStackNotFound", err)
	}
}

func TestCascadeDeleteStackBumpsConfigVersion(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "apps.example.com")
	claim(t, s, "env-a", "stack-a", hostIn("app"))
	before := configVersion(t, st, "env-a")
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes WHERE hostname = ?`, hostIn("app")); n != 1 {
		t.Fatalf("%d routes before the delete, want 1", n)
	}

	// The path every stack deletion takes: the sqlc statement, with no ingress code involved.
	if n, err := st.Q.DeleteStack(ctx, "stack-a"); err != nil || n != 1 {
		t.Fatalf("DeleteStack = %d, %v", n, err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE stack_id = 'stack-a'`); n != 0 {
		t.Errorf("%d claims remain after the stack row went", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes WHERE hostname = ?`, hostIn("app")); n != 0 {
		t.Errorf("%d routes remain after the stack row went", n)
	}
	if after := configVersion(t, st, "env-a"); after <= before {
		t.Errorf("config_version = %d after the cascade, want more than %d", after, before)
	}
}

func TestReplaceStackRoutesTwoWritersOneWinner(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	seedStack(t, st, "stack-b", "env-b", "web-b")
	registerDomain(t, s, "env-a", "apps.example.com")
	registerDomain(t, s, "env-b", "apps.example.com")
	contested := hostIn("contested")

	type writer struct{ env, stack string }
	writers := []writer{{"env-a", "stack-a"}, {"env-b", "stack-b"}}
	errs := make([]error, len(writers))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: w.env, StackID: w.stack, Actor: w.stack, Decls: []ingress.Declaration{declFor(contested, 80)}})
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		var refused *RefusedError
		if !errors.As(err, &refused) || len(refused.Problems) != 1 || refused.Problems[0].Code != ingress.CodeHostTaken {
			t.Errorf("writer %d lost with %v, want a refusal %s", i, err, ingress.CodeHostTaken)
		}
	}
	if winners != 1 {
		t.Fatalf("%d writers won, want exactly 1 (errors: %v)", winners, errs)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE hostname = ?`, contested); n != 1 {
		t.Errorf("%d claim rows for the contested hostname, want 1", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes WHERE hostname = ?`, contested); n != 1 {
		t.Errorf("%d routes for the contested hostname, want 1", n)
	}
	// The loser wrote nothing: exactly one environment has a claim and a version.
	loserClaims := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE hostname = ? AND env_id = 'env-a'`, contested) +
		count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE hostname = ? AND env_id = 'env-b'`, contested)
	if loserClaims != 1 {
		t.Errorf("the contested hostname is held %d times", loserClaims)
	}
}

func TestReplaceStackRoutesHookGetsTheDelta(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "apps.example.com")

	var got []ClaimDelta
	hook := func(_ context.Context, _ *sql.Tx, _ ingress.Plan, d ClaimDelta) error {
		got = append(got, d)
		return nil
	}
	write := func(decls ...ingress.Declaration) {
		t.Helper()
		if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice", Decls: decls, Hook: hook}); err != nil {
			t.Fatal(err)
		}
	}
	a, b := hostIn("a"), hostIn("b")

	steps := []struct {
		name  string
		decls []ingress.Declaration
		want  *ClaimDelta // nil: no hook call
	}{
		{"a first write adds", []ingress.Declaration{declFor(a, 8080)}, &ClaimDelta{Added: []string{a}}},
		{"an identical plan calls no hook", []ingress.Declaration{declFor(a, 8080)}, nil},
		{"an added hostname keeps the other", []ingress.Declaration{declFor(b, 8080), declFor(a, 8080)}, &ClaimDelta{Added: []string{b}, Kept: []string{a}}},
		{"a changed port changes the hostname", []ingress.Declaration{declFor(a, 9090), declFor(b, 8080)}, &ClaimDelta{Kept: []string{a, b}, Changed: []string{a}}},
		{"a dropped hostname is removed", []ingress.Declaration{declFor(b, 8080)}, &ClaimDelta{Kept: []string{b}, Removed: []string{a}}},
		{"no declaration removes the rest", nil, &ClaimDelta{Removed: []string{b}}},
	}
	for _, step := range steps {
		got = nil
		write(step.decls...)
		if step.want == nil {
			if len(got) != 0 {
				t.Errorf("%s: hook ran %d times, want none", step.name, len(got))
			}
			continue
		}
		if len(got) != 1 {
			t.Fatalf("%s: hook ran %d times, want once", step.name, len(got))
		}
		if !reflect.DeepEqual(got[0], *step.want) {
			t.Errorf("%s: delta = %+v, want %+v", step.name, got[0], *step.want)
		}
	}
}
