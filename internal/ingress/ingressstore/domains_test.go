// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
)

func TestCreateDomainSameSuffixTwoEnvironmentsOneEach(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")

	first := registerDomain(t, s, "env-a", "apps.example.com")
	registerDomain(t, s, "env-b", "apps.example.com")

	_, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "Apps.Example.com.", Issuer: ingress.IssuerProduction, Actor: "other"}, nil)
	if !errors.Is(err, ErrDomainExists) {
		t.Fatalf("second registration in one environment = %v, want ErrDomainExists", err)
	}
	var issuer, by string
	if err := st.Read.QueryRow(`SELECT issuer, created_by FROM ingress_domains WHERE id = ?`, first.ID).Scan(&issuer, &by); err != nil {
		t.Fatal(err)
	}
	if issuer != "staging" || by != "admin" {
		t.Errorf("first row = issuer %q by %q, want it untouched (staging, admin)", issuer, by)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_domains WHERE suffix = 'apps.example.com'`); n != 2 {
		t.Errorf("%d rows for the suffix, want 2", n)
	}
}

func TestCreateDomainRefusesAPublicSuffix(t *testing.T) {
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")

	_, err := s.CreateDomain(context.Background(), CreateDomainInput{EnvID: "env-a", Suffix: "co.uk", Issuer: ingress.IssuerStaging, Actor: "admin"}, nil)
	var ne *ingress.NameError
	if !errors.As(err, &ne) || ne.Code != ingress.CodeDomainPublicSuffix {
		t.Fatalf("co.uk = %v, want a NameError %s", err, ingress.CodeDomainPublicSuffix)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_domains`); n != 0 {
		t.Errorf("%d domain rows after a refusal, want 0", n)
	}
	if v := configVersion(t, st, "env-a"); v != 0 {
		t.Errorf("config_version = %d after a refusal, want 0", v)
	}
}

func TestDomainTokenRoundTripAndNoToken(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")

	bare := registerDomain(t, s, "env-a", "bare.example.com")
	if _, err := s.DomainToken(ctx, "env-a", bare.ID); !errors.Is(err, ErrNoToken) {
		t.Fatalf("a domain with no token = %v, want ErrNoToken", err)
	}
	withToken, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "tok.example.com", Issuer: ingress.IssuerStaging, Token: []byte("cf-token-1"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DomainToken(ctx, "env-a", withToken.ID)
	if err != nil || string(got) != "cf-token-1" {
		t.Fatalf("DomainToken = %q, %v; want the token", got, err)
	}
}

func TestDomainTokenIsBoundToItsRow(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	d1, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "one.example.com", Issuer: ingress.IssuerStaging, Token: []byte("secret-one"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d2 := registerDomain(t, s, "env-a", "two.example.com")

	// An attacker with write access to the database copies one sealed value into another row.
	if _, err := st.Write.Exec(`UPDATE ingress_domains SET token_sealed = (SELECT token_sealed FROM ingress_domains WHERE id = ?) WHERE id = ?`, d1.ID, d2.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.DomainToken(ctx, "env-a", d2.ID)
	if err == nil {
		t.Fatal("a sealed token copied into another domain's row opened")
	}
	if errors.Is(err, ErrNoToken) {
		t.Errorf("the copied value reads as ErrNoToken; it must be a failure to open: %v", err)
	}
	if strings.Contains(err.Error(), "secret-one") {
		t.Errorf("the error carries the token: %v", err)
	}
}

func TestDomainValueNeverCarriesTheToken(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	d, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "one.example.com", Issuer: ingress.IssuerStaging, Token: []byte("plain-token-9"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasToken {
		t.Error("HasToken = false after a token was saved")
	}
	views, err := s.Domains(ctx, "env-a")
	if err != nil || len(views) != 1 {
		t.Fatalf("Domains = %v, %v; want one", views, err)
	}
	if !views[0].HasToken || views[0].ID != d.ID {
		t.Errorf("view = %+v, want the domain with a token", views[0])
	}
}

func TestDomainSetTokenReplacesTheTokenAndKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	dom, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "apps.example.com", Issuer: ingress.IssuerProduction, Token: []byte("old-token"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim(t, s, "env-a", "stack-a", "app.apps.example.com")
	var oldSealed string
	if err := st.Read.QueryRow(`SELECT token_sealed FROM ingress_domains WHERE id = ?`, dom.ID).Scan(&oldSealed); err != nil {
		t.Fatal(err)
	}
	versionBefore := configVersion(t, st, "env-a")

	later := fixedNow.Add(time.Hour)
	s.now = func() time.Time { return later }
	calls := 0
	var inTx bool
	updated, err := s.SetDomainToken(ctx, "env-a", dom.ID, []byte("new-token"), "bob", func(_ context.Context, tx *sql.Tx, d ingress.Domain) error {
		calls++
		inTx = tx != nil && d.ID == dom.ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !inTx {
		t.Errorf("hook ran %d times (in transaction with the domain: %v), want once", calls, inTx)
	}
	if !updated.HasToken || !updated.TokenUpdatedAt.Equal(later) {
		t.Errorf("returned domain = %+v, want a token stamped %v", updated, later)
	}
	if updated.Issuer != ingress.IssuerProduction || !updated.CreatedAt.Equal(dom.CreatedAt) {
		t.Errorf("issuer %q / created %v changed by a token replacement", updated.Issuer, updated.CreatedAt)
	}
	var newSealed string
	var tokenAt int64
	if err := st.Read.QueryRow(`SELECT token_sealed, token_updated_at FROM ingress_domains WHERE id = ?`, dom.ID).Scan(&newSealed, &tokenAt); err != nil {
		t.Fatal(err)
	}
	if newSealed == oldSealed || tokenAt != later.UnixNano() {
		t.Errorf("sealed value changed: %v, token_updated_at = %d, want %d", newSealed != oldSealed, tokenAt, later.UnixNano())
	}
	got, err := s.DomainToken(ctx, "env-a", dom.ID)
	if err != nil || string(got) != "new-token" {
		t.Fatalf("DomainToken = %q, %v; want the new token", got, err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE domain_id = ?`, dom.ID); n != 1 {
		t.Errorf("%d claims under the domain, want its 1", n)
	}
	if after := configVersion(t, st, "env-a"); after != versionBefore+1 {
		t.Errorf("config_version = %d, want %d", after, versionBefore+1)
	}
}

func TestDomainSetTokenRefusesAnEmptyToken(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	dom, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "apps.example.com", Issuer: ingress.IssuerStaging, Token: []byte("keep-me"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDomainToken(ctx, "env-a", dom.ID, nil, "bob", nil); err == nil {
		t.Fatal("an empty token replaced the saved one")
	}
	if got, err := s.DomainToken(ctx, "env-a", dom.ID); err != nil || string(got) != "keep-me" {
		t.Errorf("token after a refused replacement = %q, %v; want keep-me", got, err)
	}
}

func TestDomainSetTokenHookErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	dom, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "apps.example.com", Issuer: ingress.IssuerStaging, Token: []byte("keep-me"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("audit failed")
	_, err = s.SetDomainToken(ctx, "env-a", dom.ID, []byte("new"), "bob", func(context.Context, *sql.Tx, ingress.Domain) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the hook's error", err)
	}
	if got, _ := s.DomainToken(ctx, "env-a", dom.ID); string(got) != "keep-me" {
		t.Errorf("token = %q after a rolled-back replacement, want keep-me", got)
	}
}

func TestDeleteDomainInUseListsHolders(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-z", "env-a", "zeta")
	seedStack(t, st, "stack-b", "env-a", "beta")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	claim(t, s, "env-a", "stack-z", "z.apps.example.com")
	claim(t, s, "env-a", "stack-b", "b.apps.example.com")
	before := configVersion(t, st, "env-a")

	hooked := false
	err := s.DeleteDomain(ctx, "env-a", dom.ID, func(context.Context, *sql.Tx, ingress.Domain) error { hooked = true; return nil })
	var inUse *DomainInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("DeleteDomain = %v, want *DomainInUseError", err)
	}
	want := []Holder{
		{Hostname: "b.apps.example.com", StackID: "stack-b", StackName: "beta", EnvID: "env-a", EnvName: "Alpha"},
		{Hostname: "z.apps.example.com", StackID: "stack-z", StackName: "zeta", EnvID: "env-a", EnvName: "Alpha"},
	}
	if len(inUse.Holders) != len(want) {
		t.Fatalf("holders = %+v, want %+v", inUse.Holders, want)
	}
	for i := range want {
		if inUse.Holders[i] != want[i] {
			t.Errorf("holder %d = %+v, want %+v", i, inUse.Holders[i], want[i])
		}
	}
	if hooked {
		t.Error("the hook ran for a refused delete")
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_domains WHERE id = ?`, dom.ID); n != 1 {
		t.Error("the domain is gone after a refused delete")
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Errorf("config_version moved %d -> %d on a refused delete", before, after)
	}
}

func TestDeleteDomainWithOnlyReservedNamesSucceeds(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	if _, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: dom.ID, Hostname: "admin.apps.example.com", Actor: "admin"}, nil); err != nil {
		t.Fatal(err)
	}
	before := configVersion(t, st, "env-a")

	calls := 0
	if err := s.DeleteDomain(ctx, "env-a", dom.ID, func(_ context.Context, _ *sql.Tx, d ingress.Domain) error {
		calls++
		if d.Suffix != "apps.example.com" {
			t.Errorf("hook domain = %+v, want the deleted one", d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("hook ran %d times, want once", calls)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_domains`); n != 0 {
		t.Errorf("%d domains remain", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_reserved`); n != 0 {
		t.Errorf("%d reserved names remain, want them gone with the domain", n)
	}
	if after := configVersion(t, st, "env-a"); after != before+1 {
		t.Errorf("config_version = %d, want %d", after, before+1)
	}
}

func TestDeleteDomainParentIsNotBlockedByAClaimUnderTheChild(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	parent := registerDomain(t, s, "env-a", "example.com")
	child := registerDomain(t, s, "env-a", "apps.example.com")
	claim(t, s, "env-a", "stack-a", "app.apps.example.com")

	if err := s.DeleteDomain(ctx, "env-a", parent.ID, nil); err != nil {
		t.Fatalf("delete example.com: %v", err)
	}
	var inUse *DomainInUseError
	if err := s.DeleteDomain(ctx, "env-a", child.ID, nil); !errors.As(err, &inUse) {
		t.Fatalf("delete apps.example.com = %v, want it blocked by its own claim", err)
	}
}

func TestDeleteDomainThenClaimIsRefusedAndClaimThenDeleteIsRefused(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	dom := registerDomain(t, s, "env-a", "apps.example.com")
	decl := ingress.Declaration{Service: "web", Host: "app.apps.example.com", Label: "tierplan.ingress.host", Port: 80}

	// The write transactions run one after the other on the single write connection, so whichever
	// commits second sees the first.
	if err := s.DeleteDomain(ctx, "env-a", dom.ID, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "a", Decls: []ingress.Declaration{decl}})
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Problems[0].Code != ingress.CodeDomainNotAllowed {
		t.Fatalf("claim after the domain's delete = %v, want %s", err, ingress.CodeDomainNotAllowed)
	}

	dom = registerDomain(t, s, "env-a", "apps.example.com")
	claim(t, s, "env-a", "stack-a", "app.apps.example.com")
	var inUse *DomainInUseError
	if err := s.DeleteDomain(ctx, "env-a", dom.ID, nil); !errors.As(err, &inUse) {
		t.Fatalf("delete after a claim = %v, want *DomainInUseError", err)
	}
}

func TestDomainMethodsRefuseAForeignEnvironmentsDomain(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	dom, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "apps.example.com", Issuer: ingress.IssuerStaging, Token: []byte("secret-a"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := configVersion(t, st, "env-a")

	if _, err := s.DomainToken(ctx, "env-b", dom.ID); !errors.Is(err, ErrDomainNotFound) {
		t.Errorf("DomainToken from another environment = %v, want ErrDomainNotFound", err)
	}
	if _, err := s.SetDomainToken(ctx, "env-b", dom.ID, []byte("hijack"), "mallory", nil); !errors.Is(err, ErrDomainNotFound) {
		t.Errorf("SetDomainToken from another environment = %v, want ErrDomainNotFound", err)
	}
	if err := s.DeleteDomain(ctx, "env-b", dom.ID, nil); !errors.Is(err, ErrDomainNotFound) {
		t.Errorf("DeleteDomain from another environment = %v, want ErrDomainNotFound", err)
	}
	in := ReserveInput{EnvID: "env-b", DomainID: dom.ID, Hostname: "x.apps.example.com", Actor: "mallory"}
	if _, _, err := s.Reserve(ctx, in, nil); !errors.Is(err, ErrDomainNotFound) {
		t.Errorf("Reserve from another environment = %v, want ErrDomainNotFound", err)
	}
	if _, err := s.Unreserve(ctx, "env-b", dom.ID, "x.apps.example.com", nil); !errors.Is(err, ErrDomainNotFound) {
		t.Errorf("Unreserve from another environment = %v, want ErrDomainNotFound", err)
	}
	if _, err := s.DomainToken(ctx, "env-b", "no-such-id"); !errors.Is(err, ErrDomainNotFound) {
		t.Errorf("DomainToken of a missing id = %v, want ErrDomainNotFound", err)
	}

	if got, err := s.DomainToken(ctx, "env-a", dom.ID); err != nil || string(got) != "secret-a" {
		t.Errorf("the owner's token = %q, %v; want it untouched", got, err)
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Errorf("config_version moved %d -> %d on foreign calls", before, after)
	}
	if views, _ := s.Domains(ctx, "env-b"); len(views) != 0 {
		t.Errorf("env B lists %d domains of env A", len(views))
	}
}

func TestDomainsListWithReservedNamesSorted(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	b := registerDomain(t, s, "env-a", "b.example.com")
	a := registerDomain(t, s, "env-a", "a.example.com")
	for _, h := range []string{"z.a.example.com", "m.a.example.com"} {
		if _, _, err := s.Reserve(ctx, ReserveInput{EnvID: "env-a", DomainID: a.ID, Hostname: h, Actor: "admin"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	views, err := s.Domains(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].ID != a.ID || views[1].ID != b.ID {
		t.Fatalf("views = %+v, want a.example.com then b.example.com", views)
	}
	if got := strings.Join(views[0].Reserved, ","); got != "m.a.example.com,z.a.example.com" {
		t.Errorf("reserved = %q, want sorted", got)
	}
	if len(views[1].Reserved) != 0 {
		t.Errorf("b.example.com reserved = %v, want none", views[1].Reserved)
	}
}
