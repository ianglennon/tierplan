// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/ingressrender"
)

// corruptedPrefix is a stored path that the parser would never let in: it holds braces.
const corruptedPrefix = "/a{{b}}"

// corruptRoute rewrites the stored path of host's route by SQL, bypassing the parser as corrupted data would.
func corruptRoute(t *testing.T, rg *rig, host, prefix string) {
	t.Helper()
	res, err := rg.st.Write.Exec(`UPDATE ingress_routes SET path_prefix = ? WHERE hostname = ?`, prefix, host)
	if err != nil {
		t.Fatalf("corrupt the route of %s: %v", host, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("corrupted %d routes of %s, want 1", n, host)
	}
}

// D-20 / D-08 / PROH-21-06 tracer: a stored route the renderer refuses leaves the last good bundle in place, tells
// Changes nothing and is recorded as a Failure that names the route and the code.
func TestFailedRenderKeepsTheLastGoodBundle(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	rg.certs.setServable("app.example.com")
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", decl("app.example.com", "/"))

	r := New(rg.deps(nil))
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("the first render: %v", err)
	}
	good, ok := r.Current(rigEnv)
	if !ok {
		t.Fatal("Current has no bundle after the first render")
	}
	if got := r.Failures(); len(got) != 0 {
		t.Fatalf("Failures = %+v after a good render, want none", got)
	}

	corruptRoute(t, rg, "app.example.com", corruptedPrefix)
	sub := r.Changes().Subscribe()
	t.Cleanup(sub.Close)

	err := r.Reconcile(ctx, rigEnv)
	var refused *ingressrender.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Reconcile = %v, want a *ingressrender.RefusedError", err)
	}
	kept, ok := r.Current(rigEnv)
	if !ok || kept.ID != good.ID {
		t.Fatalf("Current = %q (have %v), want the last good bundle %q", kept.ID, ok, good.ID)
	}
	select {
	case <-sub.C():
		t.Fatalf("Changes was notified for %v, want nobody told of a failed render", sub.Pending())
	default:
	}
	got := r.Failures()
	if len(got) != 1 {
		t.Fatalf("Failures = %+v, want one", got)
	}
	f := got[0]
	if f.EnvID != rigEnv || f.Hostname != "app.example.com" || f.PathPrefix != corruptedPrefix || f.Code != ingressrender.CodeUnsafeValue {
		t.Errorf("Failure = %+v, want %s, app.example.com, %q and %s", f, rigEnv, corruptedPrefix, ingressrender.CodeUnsafeValue)
	}
}

// flakyModel is a Model whose Snapshot fails while err is set.
type flakyModel struct {
	Model
	mu  sync.Mutex
	err error
}

func (m *flakyModel) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *flakyModel) Snapshot(ctx context.Context, envID string) (ingress.Snapshot, error) {
	m.mu.Lock()
	err := m.err
	m.mu.Unlock()
	if err != nil {
		return ingress.Snapshot{}, err
	}
	return m.Model.Snapshot(ctx, envID)
}

// flakyCerts is a Certs whose Serving fails while err is set.
type flakyCerts struct {
	Certs
	mu  sync.Mutex
	err error
}

func (c *flakyCerts) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *flakyCerts) Serving(ctx context.Context, envID string, hostnames []string, now time.Time) (int64, []ingresscert.ServingCert, error) {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return 0, nil, err
	}
	return c.Certs.Serving(ctx, envID, hostnames, now)
}

// flakyRender is a Deps.Render that returns err for the environments in failFor and renders the rest for real.
type flakyRender struct {
	mu      sync.Mutex
	failFor map[string]error
}

func (f *flakyRender) fail(envID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFor == nil {
		f.failFor = map[string]error{}
	}
	if err == nil {
		delete(f.failFor, envID)
		return
	}
	f.failFor[envID] = err
}

func (f *flakyRender) render(in ingressrender.Input) (ingressrender.Result, error) {
	f.mu.Lock()
	err := f.failFor[in.Snapshot.Settings.EnvID]
	f.mu.Unlock()
	if err != nil {
		return ingressrender.Result{}, err
	}
	return ingressrender.Render(in)
}

// D-20: a read failure (the snapshot or the certificates) or a render error the renderer does not name keeps the last
// good bundle and is recorded with the code that says which it was, and no route.
func TestReadFailuresKeepTheLastGoodBundle(t *testing.T) {
	cases := []struct {
		name string
		trip func(m *flakyModel, c *flakyCerts, rd *flakyRender, err error)
		code string
	}{
		{"the snapshot cannot be read", func(m *flakyModel, _ *flakyCerts, _ *flakyRender, err error) { m.fail(err) }, CodeReadFailed},
		{"the certificates cannot be read", func(_ *flakyModel, c *flakyCerts, _ *flakyRender, err error) { c.fail(err) }, CodeReadFailed},
		{"the render fails without a refusal", func(_ *flakyModel, _ *flakyCerts, rd *flakyRender, err error) { rd.fail(rigEnv, err) }, CodeRenderFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rg := newRig(t)
			rg.certs.setServable("app.example.com")
			rg.seedSwarmStack("stack-shop", "shop")
			rg.storeRoutes("stack-shop", decl("app.example.com", "/"))

			model, certs, rd := &flakyModel{Model: rg.model}, &flakyCerts{Certs: rg.certs}, &flakyRender{}
			d := rg.deps(nil)
			d.Model, d.Certs, d.Render = model, certs, rd.render
			r := New(d)
			if err := r.Reconcile(ctx, rigEnv); err != nil {
				t.Fatalf("the first render: %v", err)
			}
			good, _ := r.Current(rigEnv)
			skipped := r.Skipped(rigEnv)
			sub := r.Changes().Subscribe()
			t.Cleanup(sub.Close)

			cause := errors.New("the cause")
			tc.trip(model, certs, rd, cause)
			if err := r.Reconcile(ctx, rigEnv); !errors.Is(err, cause) {
				t.Fatalf("Reconcile = %v, want the cause", err)
			}
			if kept, ok := r.Current(rigEnv); !ok || kept.ID != good.ID {
				t.Fatalf("Current = %q (have %v), want the last good bundle %q", kept.ID, ok, good.ID)
			}
			if got := r.Skipped(rigEnv); !slices.Equal(got, skipped) {
				t.Errorf("Skipped = %+v, want the last good list %+v", got, skipped)
			}
			select {
			case <-sub.C():
				t.Fatalf("Changes was notified for %v, want nobody told", sub.Pending())
			default:
			}
			got := r.Failures()
			if len(got) != 1 || got[0].EnvID != rigEnv || got[0].Code != tc.code || got[0].Hostname != "" || got[0].PathPrefix != "" {
				t.Fatalf("Failures = %+v, want one for %s with code %s and no route", got, rigEnv, tc.code)
			}
		})
	}
}

// warnCounter is a slog.Handler that keeps every Warn record, with its attributes, as one line of text.
type warnCounter struct {
	mu    *sync.Mutex
	lines *[]string
}

func (h warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h warnCounter) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h warnCounter) WithGroup(string) slog.Handler            { return h }
func (h warnCounter) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level != slog.LevelWarn {
		return nil
	}
	line := rec.Message
	rec.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	h.mu.Lock()
	*h.lines = append(*h.lines, line)
	h.mu.Unlock()
	return nil
}

// D-20 / T-21-40: the same failure is logged once however often it repeats, and its Since stays at the first sighting
// on the injected clock; a different failure is logged and starts its own Since; a good render clears the failure and
// what was logged, so the same failure is said again when it comes back.
func TestFailureLoggedOncePerDistinctError(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	rg.certs.setServable("app.example.com")
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", decl("app.example.com", "/"))

	var mu sync.Mutex
	var warns []string
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(warns)
	}
	d := rg.deps(nil)
	d.Log = slog.New(warnCounter{mu: &mu, lines: &warns})
	r := New(d)
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("the first render: %v", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("a good render logged %d warnings, want none", n)
	}

	// The same refusal three times, the clock moving between the attempts.
	corruptRoute(t, rg, "app.example.com", "/a{{one}}")
	first := rg.clock.Now()
	for range 3 {
		if err := r.Reconcile(ctx, rigEnv); err == nil {
			t.Fatal("Reconcile of a refused route returned no error")
		}
		rg.clock.Advance(time.Minute)
	}
	if n := count(); n != 1 {
		t.Fatalf("the same failure three times logged %d warnings, want 1: %q", n, warns)
	}
	f := r.Failures()
	if len(f) != 1 || !f[0].Since.Equal(first) {
		t.Fatalf("Failures = %+v, want Since %v, the first attempt, though the clock moved on", f, first)
	}

	// A different refusal.
	corruptRoute(t, rg, "app.example.com", "/a{{two}}")
	second := rg.clock.Now()
	if err := r.Reconcile(ctx, rigEnv); err == nil {
		t.Fatal("Reconcile of a second refused route returned no error")
	}
	if n := count(); n != 2 {
		t.Fatalf("a different failure left %d warnings in all, want 2", n)
	}
	if f := r.Failures(); len(f) != 1 || !f[0].Since.Equal(second) || f[0].PathPrefix != "/a{{two}}" {
		t.Fatalf("Failures = %+v, want the second failure with Since %v", f, second)
	}

	// A good render clears it, and the first refusal again is said again.
	corruptRoute(t, rg, "app.example.com", "/")
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("the render after the repair: %v", err)
	}
	if f := r.Failures(); len(f) != 0 {
		t.Fatalf("Failures = %+v after a good render, want none", f)
	}
	corruptRoute(t, rg, "app.example.com", "/a{{one}}")
	if err := r.Reconcile(ctx, rigEnv); err == nil {
		t.Fatal("Reconcile of the first refused route returned no error the second time")
	}
	if n := count(); n != 3 {
		t.Fatalf("the failure that came back left %d warnings in all, want 3", n)
	}
	for _, w := range warns {
		if !strings.Contains(w, "env_id=env-1") || !strings.Contains(w, "hostname=app.example.com") || !strings.Contains(w, "code="+ingressrender.CodeUnsafeValue) {
			t.Errorf("warning %q does not name the environment, the route and the code", w)
		}
	}
}

// D-20 / T-21-41: Failures is sorted by environment id whatever order the failures arrived in, and what it holds
// is names and codes, never certificate or key material, though the renders that failed were handed some.
func TestFailuresAreSortedAndHoldNamesOnly(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	seedSecondEnv(t, rg)
	rg.certs.setServable("app.example.com")
	rg.certs.setServable("blog.example.org")
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", decl("app.example.com", "/"))

	rd := &flakyRender{}
	d := rg.deps(nil)
	d.Render = rd.render
	r := New(d)
	for _, env := range []string{rigEnv, "env-2"} {
		if err := r.Reconcile(ctx, env); err != nil {
			t.Fatalf("the first render of %s: %v", env, err)
		}
	}

	// env-2 fails first, then env-1.
	rd.fail("env-2", errors.New("the render of env-2 failed"))
	if err := r.Reconcile(ctx, "env-2"); err == nil {
		t.Fatal("Reconcile of env-2 returned no error")
	}
	corruptRoute(t, rg, "app.example.com", corruptedPrefix)
	if err := r.Reconcile(ctx, rigEnv); err == nil {
		t.Fatal("Reconcile of env-1 returned no error")
	}

	got := r.Failures()
	if len(got) != 2 || got[0].EnvID != rigEnv || got[1].EnvID != "env-2" {
		t.Fatalf("Failures = %+v, want env-1 then env-2", got)
	}
	if got[0].Code != ingressrender.CodeUnsafeValue || got[1].Code != CodeRenderFailed {
		t.Errorf("codes = %s, %s; want %s, %s", got[0].Code, got[1].Code, ingressrender.CodeUnsafeValue, CodeRenderFailed)
	}
	for _, s := range []string{fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got)} {
		for _, secret := range []string{"PRIVATE KEY", "BEGIN CERTIFICATE", "key of app.example.com"} {
			if strings.Contains(s, secret) {
				t.Errorf("Failures print %q, which is certificate or key material", secret)
			}
		}
	}

	// The slice is a copy: changing it does not change the book.
	got[0].Code = "changed"
	if r.Failures()[0].Code == "changed" {
		t.Error("Failures returned the book itself, want a copy")
	}
}
