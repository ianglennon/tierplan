// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"bytes"
	"crypto/x509"
	"io"
	"log/slog"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/registry/registrytest"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The fleet check against real registries (fakes over TLS) and the real client, store and resolver
// (plan 12-05, D-01, D-19): what the registry is asked, and how often.

// fleetRig is a checker that really asks the registries it is given.
type fleetRig struct {
	sql  updates.SQLStore
	chk  *updates.Checker
	clk  *clock
	logs *bytes.Buffer
}

func newFleetRig(t *testing.T, regs ...*registrytest.Registry) (*fleetRig, *registry.Resolver, *registry.Store) {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: t0}
	rs := registry.NewStore(st, v, clk.Now)
	resolver := registry.NewResolver(rs)
	pool := x509.NewCertPool()
	for _, r := range regs {
		pool.AddCert(r.Certificate())
	}
	clients := registry.NewClientCache(registry.NewEgressPolicy(nil).WithRootCAs(pool), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	sql := updates.SQLStore{Q: st.Q, W: st.Write}
	logs := &bytes.Buffer{}
	chk := &updates.Checker{
		Heads: updates.NewHeads(resolver, clients), Store: sql, Runs: updates.NewRunIndex(), Creds: resolver,
		Now: clk.Now, Log: slog.New(slog.NewTextHandler(logs, nil)),
	}
	return &fleetRig{sql: sql, chk: chk, clk: clk, logs: logs}, resolver, rs
}

// serviceOn is a workload running image repo:1 of the registry at host, in environment env.
func serviceOn(t *testing.T, env, host, repo string) updates.Workload {
	t.Helper()
	ws := updates.ServiceWorkloads([]dockerx.ServiceRow{{
		ID: env + "-" + repo, Name: "svc", Mode: "replicated", Image: host + "/" + repo + ":1@" + dg('0'), Desired: 1, Running: 1,
	}})
	if len(ws) != 1 {
		t.Fatalf("no workload for %s/%s", host, repo)
	}
	ws[0].EnvID = env
	return ws[0]
}

func checkOf(t *testing.T, r *fleetRig, host, repo, cred string) updates.Check {
	t.Helper()
	k := updates.Key{Host: host, Repo: repo, Tag: "1", CredID: cred}
	got, err := r.sql.Checks(testsupport.Context(t), []updates.Key{k})
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	c, ok := got[k]
	if !ok {
		t.Fatalf("no check stored for %+v", k)
	}
	return c
}

func TestFleetDedupeOneRequest(t *testing.T) {
	t.Parallel()
	t.Run("anonymous", func(t *testing.T) {
		t.Parallel()
		ctx := testsupport.Context(t)
		reg := registrytest.New(t, registrytest.Options{})
		img := registrytest.Image("a")
		reg.SetTag("acme/app", "1", img)
		r, _, _ := newFleetRig(t, reg)

		res, err := r.chk.CheckFleet(ctx, []string{"env-a", "env-b"}, map[string][]updates.Workload{
			"env-a": {serviceOn(t, "env-a", reg.Host(), "acme/app")},
			"env-b": {serviceOn(t, "env-b", reg.Host(), "acme/app")},
		}, false)
		if err != nil {
			t.Fatalf("CheckFleet: %v", err)
		}
		// One HEAD for the tag, and the one GET by digest that reads the digest nobody had stored.
		if reg.Heads() != 1 || reg.Gets() != 1 {
			t.Errorf("two environments, one image: %d HEAD and %d GET, want 1 and 1", reg.Heads(), reg.Gets())
		}
		for _, env := range []string{"env-a", "env-b"} {
			if res[env].Checked != 1 || res[env].Failed != 0 || res[env].Updates != 1 {
				t.Errorf("%s result = %+v, want 1 checked, 0 failed, 1 update", env, res[env])
			}
		}
		if got := checkOf(t, r, reg.Host(), "acme/app", ""); got.Digest != img.Digest() {
			t.Errorf("stored digest %q, want %q", got.Digest, img.Digest())
		}
	})

	t.Run("one instance credential", func(t *testing.T) {
		t.Parallel()
		ctx := testsupport.Context(t)
		reg := registrytest.New(t, registrytest.Options{User: "bot", Password: "s3cret-token-value-0123"})
		reg.SetTag("acme/app", "1", registrytest.Image("a"))
		r, _, rs := newFleetRig(t, reg)
		cred, err := rs.Create(ctx, registry.CreateInput{
			Name: "shared", Type: registry.TypeGeneric, Host: reg.Host(), Prefix: "acme", Username: "bot", Secret: "s3cret-token-value-0123",
		}, "admin-1", nil)
		if err != nil {
			t.Fatalf("create credential: %v", err)
		}

		if _, err := r.chk.CheckFleet(ctx, []string{"env-a", "env-b", "env-c"}, map[string][]updates.Workload{
			"env-a": {serviceOn(t, "env-a", reg.Host(), "acme/app")},
			"env-b": {serviceOn(t, "env-b", reg.Host(), "acme/app")},
			"env-c": {serviceOn(t, "env-c", reg.Host(), "acme/app")},
		}, false); err != nil {
			t.Fatalf("CheckFleet: %v", err)
		}
		if reg.Heads() != 1 {
			t.Errorf("three environments sharing one credential sent %d HEADs, want 1", reg.Heads())
		}
		if got := checkOf(t, r, reg.Host(), "acme/app", cred.ID); got.Error != "" || got.Digest == "" {
			t.Errorf("the row under the shared credential = %+v, want a success", got)
		}
	})
}

func TestBreakerIsPerHost(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	a := registrytest.New(t, registrytest.Options{})
	b := registrytest.New(t, registrytest.Options{})
	repos := []string{"acme/one", "acme/two", "acme/three", "acme/four", "acme/five", "acme/six"}
	for _, repo := range repos {
		a.SetTag(repo, "1", registrytest.Image("a-"+repo))
		b.SetTag(repo, "1", registrytest.Image("b-"+repo))
	}
	a.SetRateLimitAfter(0) // registry A answers "too many requests" to everything
	r, _, _ := newFleetRig(t, a, b)

	var wsA, wsB []updates.Workload
	for _, repo := range repos {
		wsA = append(wsA, serviceOn(t, "env-1", a.Host(), repo))
	}
	for _, repo := range repos[:2] {
		wsB = append(wsB, serviceOn(t, "env-1", b.Host(), repo))
	}
	res, err := r.chk.CheckFleet(ctx, []string{"env-1"}, map[string][]updates.Workload{"env-1": append(wsA, wsB...)}, false)
	if err != nil {
		t.Fatalf("CheckFleet: %v", err)
	}

	// A has four places for requests; the first four to be refused open the breaker, and the keys that
	// waited for a place meet it and are not sent.
	asked := 0
	for _, repo := range repos {
		if a.Requested(repo, "1") > 0 {
			asked++
		}
	}
	if asked == 0 || asked > 4 {
		t.Errorf("%d of 6 keys reached registry A, want between 1 and 4 (its cap)", asked)
	}
	for _, repo := range repos {
		got := checkOf(t, r, a.Host(), repo, "")
		if got.Error != updates.ReasonRateLimited {
			t.Errorf("%s on A stored error %q, want rate_limited", repo, got.Error)
		}
	}
	// B is a different host: its breaker is closed and both keys were read.
	for _, repo := range repos[:2] {
		if got := checkOf(t, r, b.Host(), repo, ""); got.Error != "" || got.Digest == "" {
			t.Errorf("%s on B = %+v, want a success", repo, got)
		}
		if n := b.Requested(repo, "1"); n != 1 {
			t.Errorf("registry B was asked %d times for %s, want 1", n, repo)
		}
	}
	if res["env-1"].Checked != 8 || res["env-1"].Failed != 6 {
		t.Errorf("result = %+v, want 8 checked and the 6 keys of A failed", res["env-1"])
	}
}
