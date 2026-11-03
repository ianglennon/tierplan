// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/regclient/regclient/types/errs"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The check of an environment's images and the verdict each service gets from what was stored (plan
// 12-02, IMG-04, D-03, D-04, D-19).

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// dg is a sha256 digest made of one repeated hex digit, so a test reads which one it means.
func dg(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// answer is what the fake registry says for one tag.
type answer struct {
	digest string
	credID string
	err    error
}

// fakeHeads answers for the registry and counts the requests it was asked for. A tag nobody scripted
// is an unreachable registry.
type fakeHeads struct {
	mu      sync.Mutex
	answers map[string]answer
	calls   map[string]int
}

func newHeads() *fakeHeads {
	return &fakeHeads{answers: map[string]answer{}, calls: map[string]int{}}
}

// set scripts the answer for a tag: host/path:tag.
func (f *fakeHeads) set(tag string, a answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[tag] = a
}

func (f *fakeHeads) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakeHeads) Digest(_ context.Context, _ string, ref registry.Ref) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := ref.Repo() + ":" + ref.Tag
	f.calls[k]++
	a, ok := f.answers[k]
	if !ok {
		return "", "", errors.New("connection refused")
	}
	return a.digest, a.credID, a.err
}

func (f *fakeHeads) Index(context.Context, string, registry.Ref, string) (bool, map[string]string, error) {
	return false, nil, nil
}

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// rig is a real store, a fake registry, a checker and a service.
type rig struct {
	st    *store.Store
	reg   *registry.Store
	sql   updates.SQLStore
	heads *fakeHeads
	clk   *clock
	chk   *updates.Checker
	svc   *updates.Service
	logs  *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: t0}
	rs := registry.NewStore(st, v, clk.Now)
	sql := updates.SQLStore{Q: st.Q, W: st.Write}
	heads := newHeads()
	logs := &bytes.Buffer{}
	chk := &updates.Checker{Heads: heads, Store: sql, Now: clk.Now, Log: slog.New(slog.NewTextHandler(logs, nil))}
	svc := &updates.Service{Store: sql, Resolver: registry.NewResolver(rs), Checker: chk, Now: clk.Now}
	return &rig{st: st, reg: rs, sql: sql, heads: heads, clk: clk, chk: chk, svc: svc, logs: logs}
}

func (r *rig) addEnvironment(t *testing.T, id string) {
	t.Helper()
	if _, err := r.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES (?, 'agent', ?, '', 1, 1, 1)`, id, id); err != nil {
		t.Fatal(err)
	}
}

// row is a swarm service as the cluster read lists it, running image written as repo:tag@digest.
func row(id, image string) dockerx.ServiceRow {
	return dockerx.ServiceRow{ID: id, Name: "svc-" + id, Mode: "replicated", Image: image, Desired: 1, Running: 1}
}

func workloads(t *testing.T, rows ...dockerx.ServiceRow) []updates.Workload {
	t.Helper()
	ws := updates.ServiceWorkloads(rows)
	if len(ws) != len(rows) {
		t.Fatalf("ServiceWorkloads kept %d of %d rows", len(ws), len(rows))
	}
	for i := range ws {
		ws[i].EnvID = "env-1"
	}
	return ws
}

func stored(t *testing.T, r *rig, host, repo, tag, cred string) updates.Check {
	t.Helper()
	k := updates.Key{Host: host, Repo: repo, Tag: tag, CredID: cred}
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

func unauthorized(text string) error {
	return fmt.Errorf("%s: %w", text, errs.ErrHTTPUnauthorized)
}

func TestCheckEnvStoresSuccessAndFailure(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	rows := []dockerx.ServiceRow{
		row("a", "docker.io/library/nginx:1.27@"+dg('a')),
		row("b", "ghcr.io/acme/app:2@"+dg('b')),
		row("c", "example.com/private/svc:1@"+dg('c')),
	}
	ws := workloads(t, rows...)
	r.heads.set("docker.io/library/nginx:1.27", answer{digest: dg('a')})
	r.heads.set("ghcr.io/acme/app:2", answer{digest: dg('9')})
	r.heads.set("example.com/private/svc:1", answer{digest: dg('c')})

	res, err := r.chk.CheckEnv(ctx, "env-1", ws)
	if err != nil {
		t.Fatalf("CheckEnv: %v", err)
	}
	if res != (updates.CheckResult{Checked: 3, Updates: 1, Failed: 0}) {
		t.Errorf("first result = %+v, want 3 checked, 1 update, 0 failed", res)
	}
	same := stored(t, r, "docker.io", "library/nginx", "1.27", "")
	if same.Digest != dg('a') || !same.SuccessAt.Equal(t0) || !same.CheckedAt.Equal(t0) || same.Error != "" {
		t.Errorf("unchanged tag stored as %+v", same)
	}
	moved := stored(t, r, "ghcr.io", "acme/app", "2", "")
	if moved.Digest != dg('9') {
		t.Errorf("moved tag stored digest %q, want the registry's current %q", moved.Digest, dg('9'))
	}

	// An hour later the private registry refuses the sign-in: the row keeps the last good answer.
	r.clk.advance(time.Hour)
	r.heads.set("example.com/private/svc:1", answer{err: unauthorized("denied: token=abc")})
	res, err = r.chk.CheckEnv(ctx, "env-1", ws)
	if err != nil {
		t.Fatalf("CheckEnv: %v", err)
	}
	if res.Failed != 1 || res.Checked != 3 {
		t.Errorf("second result = %+v, want 1 failed of 3", res)
	}
	failed := stored(t, r, "example.com", "private/svc", "1", "")
	if failed.Error != updates.ReasonAuth {
		t.Errorf("failed check reason = %q, want auth", failed.Error)
	}
	if failed.Digest != dg('c') || !failed.SuccessAt.Equal(t0) {
		t.Errorf("failed check = %+v, want the digest and success time of the earlier good answer kept", failed)
	}
	if !failed.CheckedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("failed check CheckedAt = %v, want the time of the failed attempt", failed.CheckedAt)
	}
}

func TestCheckEnvThenServiceVerdicts(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	rows := []dockerx.ServiceRow{
		row("same", "docker.io/library/nginx:1.27@"+dg('a')),
		row("moved", "ghcr.io/acme/app:2@"+dg('b')),
		row("failing", "example.com/private/svc:1@"+dg('c')),
		row("never", "registry.example/never/asked:1@"+dg('d')),
	}
	r.heads.set("docker.io/library/nginx:1.27", answer{digest: dg('a')})
	r.heads.set("ghcr.io/acme/app:2", answer{digest: dg('9')})
	r.heads.set("example.com/private/svc:1", answer{err: unauthorized("denied")})

	// The check covers the first three services; the fourth is never asked about.
	if _, err := r.chk.CheckEnv(ctx, "env-1", workloads(t, rows[:3]...)); err != nil {
		t.Fatalf("CheckEnv: %v", err)
	}
	r.clk.advance(time.Minute)
	got, err := r.svc.ServiceVerdicts(ctx, "env-1", rows)
	if err != nil {
		t.Fatalf("ServiceVerdicts: %v", err)
	}
	if v := got["same"]; v.State != updates.StateUpToDate || v.RemoteDigest != dg('a') || v.RunningDigest != dg('a') {
		t.Errorf("same = %+v, want up_to_date", v)
	}
	if v := got["moved"]; v.State != updates.StateUpdateAvailable || v.RemoteDigest != dg('9') || v.RunningDigest != dg('b') {
		t.Errorf("moved = %+v, want update_available with the new digest", v)
	}
	if v := got["failing"]; v.State != updates.StateCheckFailed || v.Reason != updates.ReasonAuth {
		t.Errorf("failing = %+v, want check_failed with reason auth", v)
	}
	if v := got["never"]; v.State != updates.StateNotChecked {
		t.Errorf("never = %+v, want not_checked", v)
	}
}
