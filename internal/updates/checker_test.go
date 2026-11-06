// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/regclient/regclient/types/errs"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
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
	byEnv   map[string]answer
	calls   map[string]int
	// indexes are what a digest names: a multi-platform index with these platforms. A digest nobody
	// scripted names one image. indexCalls counts the reads by digest, which a registry may count as pulls.
	indexes    map[string]indexAnswer
	indexCalls map[string]int
	// gate, when set, holds every Digest until it is closed, after sending on entered; credCalls counts
	// the credential lookups, which come before the request.
	gate      chan struct{}
	entered   chan struct{}
	credCalls int
}

// indexAnswer is what the fake registry says a digest names.
type indexAnswer struct {
	platforms map[string]string
	err       error
}

func newHeads() *fakeHeads {
	return &fakeHeads{answers: map[string]answer{}, byEnv: map[string]answer{}, calls: map[string]int{},
		indexes: map[string]indexAnswer{}, indexCalls: map[string]int{}}
}

// setIndex scripts digest as a multi-platform index with the platforms given (linux/amd64 to digest).
func (f *fakeHeads) setIndex(digest string, platforms map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexes[digest] = indexAnswer{platforms: platforms}
}

// goneIndex scripts digest as an index the registry no longer has.
func (f *fakeHeads) goneIndex(digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexes[digest] = indexAnswer{err: fmt.Errorf("manifest unknown: %w", errs.ErrNotFound)}
}

// gets is how many reads by digest were made.
func (f *fakeHeads) gets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.indexCalls {
		n += c
	}
	return n
}

// set scripts the answer for a tag: host/path:tag.
func (f *fakeHeads) set(tag string, a answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[tag] = a
}

// setEnv scripts the answer for a tag as one environment asks it, which is how a credential of that
// environment's own makes the registry answer differently.
func (f *fakeHeads) setEnv(env, tag string, a answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byEnv[env+"|"+tag] = a
}

func (f *fakeHeads) credLookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credCalls
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

func (f *fakeHeads) Digest(_ context.Context, envID string, ref registry.Ref) (string, string, error) {
	f.mu.Lock()
	k := ref.Repo() + ":" + ref.Tag
	f.calls[k]++
	a, ok := f.byEnv[envID+"|"+k]
	if !ok {
		a, ok = f.answers[k]
	}
	gate, entered := f.gate, f.entered
	f.mu.Unlock()
	if gate != nil {
		// The registry is slow to answer: the test says when.
		entered <- struct{}{}
		<-gate
	}
	if !ok {
		return "", "", errors.New("connection refused")
	}
	return a.digest, a.credID, a.err
}

// CredentialID names the credential the scripted answer for the tag carries, without counting a request.
func (f *fakeHeads) CredentialID(_ context.Context, envID string, ref registry.Ref) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credCalls++
	k := ref.Repo() + ":" + ref.Tag
	a, ok := f.byEnv[envID+"|"+k]
	if !ok {
		a = f.answers[k]
	}
	return a.credID, nil
}

func (f *fakeHeads) Index(_ context.Context, _ string, _ registry.Ref, digest string) (bool, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexCalls[digest]++
	a, ok := f.indexes[digest]
	if !ok {
		return false, nil, nil
	}
	if a.err != nil {
		return false, nil, a.err
	}
	return true, a.platforms, nil
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
	runs := updates.NewRunIndex()
	chk := &updates.Checker{Heads: heads, Store: sql, Runs: runs, Now: clk.Now, Log: slog.New(slog.NewTextHandler(logs, nil))}
	svc := &updates.Service{Store: sql, Resolver: registry.NewResolver(rs), Checker: chk, Runs: runs, Now: clk.Now}
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

func TestChecksKeyedByCredential(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	r.addEnvironment(t, "env-b")
	credA, err := r.reg.Create(ctx, registry.CreateInput{
		EnvID: "env-a", Name: "in-a", Type: registry.TypeGHCR, Prefix: "acme", Username: "bot", Secret: "ghp_0123456789abcdefghijklmnopqrstuvwxyz",
	}, "admin-1", nil)
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	rows := []dockerx.ServiceRow{row("app", "ghcr.io/acme/private:1@"+dg('1'))}
	const tag = "ghcr.io/acme/private:1"
	// Environment A signs in and sees the private build; environment B has no credential and sees the
	// public one.
	r.heads.setEnv("env-a", tag, answer{digest: dg('1'), credID: credA.ID})
	r.heads.setEnv("env-b", tag, answer{digest: dg('2'), credID: ""})

	for _, env := range []string{"env-a", "env-b"} {
		if _, err := r.chk.CheckEnv(ctx, env, workloads(t, rows...)); err != nil {
			t.Fatalf("CheckEnv %s: %v", env, err)
		}
	}
	if got := stored(t, r, "ghcr.io", "acme/private", "1", credA.ID); got.Digest != dg('1') {
		t.Errorf("A's row digest = %q, want %q", got.Digest, dg('1'))
	}
	if got := stored(t, r, "ghcr.io", "acme/private", "1", ""); got.Digest != dg('2') {
		t.Errorf("the anonymous row digest = %q, want %q", got.Digest, dg('2'))
	}

	r.clk.advance(time.Minute)
	a, err := r.svc.ServiceVerdicts(ctx, "env-a", rows)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.svc.ServiceVerdicts(ctx, "env-b", rows)
	if err != nil {
		t.Fatal(err)
	}
	if v := a["app"]; v.State != updates.StateUpToDate || v.RemoteDigest != dg('1') {
		t.Errorf("environment A = %+v, want up_to_date from its own row", v)
	}
	if v := b["app"]; v.State != updates.StateUpdateAvailable || v.RemoteDigest != dg('2') {
		t.Errorf("environment B = %+v, want update_available from the anonymous row, never A's", v)
	}
}

func TestReasonOfTable(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]updates.Reason{
		"sign_in_refused":      updates.ReasonAuth,
		"no_pull_access":       updates.ReasonAuth,
		"rate_limited":         updates.ReasonRateLimited,
		"registry_unreachable": updates.ReasonUnreachable,
		"registry_tls":         updates.ReasonUnreachable,
		"registry_http":        updates.ReasonUnreachable,
		"repository_not_found": updates.ReasonNotFound,
		"tag_not_found":        updates.ReasonNotFound,
		"check_failed":         updates.ReasonOther,
		"no_login_asked":       updates.ReasonOther,
		"":                     updates.ReasonOther,
		"something_new":        updates.ReasonOther,
	} {
		if got := updates.ReasonOf(code); got != want {
			t.Errorf("ReasonOf(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestRegistryTextNeverStored(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	rows := []dockerx.ServiceRow{row("a", "example.com/private/svc:1@"+dg('a'))}
	r.heads.set("example.com/private/svc:1", answer{err: unauthorized("denied: token=abc")})

	if _, err := r.chk.CheckEnv(ctx, "env-1", workloads(t, rows...)); err != nil {
		t.Fatalf("CheckEnv: %v", err)
	}
	if got := stored(t, r, "example.com", "private/svc", "1", ""); got.Error != updates.ReasonAuth {
		t.Errorf("reason = %q, want auth", got.Error)
	}
	var all string
	if err := r.st.Read.QueryRow(`SELECT host || '|' || repo || '|' || tag || '|' || cred_id || '|' || digest || '|' || platforms || '|' ||
		is_index || '|' || checked_at || '|' || success_at || '|' || error_code FROM image_checks`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"token", "abc", "denied"} {
		if strings.Contains(all, text) {
			t.Errorf("the stored row %q holds the registry's text %q", all, text)
		}
	}
	if strings.Contains(r.logs.String(), "token=abc") {
		t.Errorf("the log holds the registry's text: %s", r.logs.String())
	}
}

func TestNoDigestIsUnsupportedNeverUpToDate(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	rows := []dockerx.ServiceRow{row("a", "example.com/odd/svc:1@"+dg('a'))}
	// A registry that answers a HEAD without a digest, once as the client's error and once as an empty answer.
	r.heads.set("example.com/odd/svc:1", answer{err: fmt.Errorf("registry: head: %w", registry.ErrNoDigest)})
	if _, err := r.chk.CheckEnv(ctx, "env-1", workloads(t, rows...)); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, r, "example.com", "odd/svc", "1", ""); got.Error != updates.ReasonUnsupported {
		t.Errorf("reason = %q, want unsupported", got.Error)
	}
	r.heads.set("example.com/odd/svc:1", answer{digest: ""})
	if _, err := r.chk.CheckEnv(ctx, "env-1", workloads(t, rows...)); err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.ServiceVerdicts(ctx, "env-1", rows)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["a"]; v.State != updates.StateCheckFailed || v.Reason != updates.ReasonUnsupported {
		t.Errorf("verdict = %+v, want check_failed with reason unsupported", v)
	}
}

func TestInvalidTagNeverRequested(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	bad := updates.Workload{
		EnvID: "env-1", Kind: "service", ID: "x", Name: "x",
		Ref: registry.Ref{Domain: "docker.io", Path: "library/nginx", Tag: "../../etc/passwd"},
	}
	res, err := r.chk.CheckEnv(ctx, "env-1", []updates.Workload{bad})
	if err != nil {
		t.Fatalf("CheckEnv: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("result = %+v, want the tag counted as failed", res)
	}
	if n := r.heads.requests(); n != 0 {
		t.Errorf("the registry was asked %d times for a name that is not a tag", n)
	}
	if got := stored(t, r, "docker.io", "library/nginx", "../../etc/passwd", ""); got.Error != updates.ReasonNotFound {
		t.Errorf("reason = %q, want not_found", got.Error)
	}
}

func TestServiceWorkloadsSkipsAgentAndJobs(t *testing.T) {
	t.Parallel()
	agent := row("agent", "ghcr.io/ianglennon/tierplan-agent:1@"+dg('a'))
	agent.Agent, agent.Mode = true, "global"
	job := row("job", "docker.io/library/busybox:1@"+dg('b'))
	job.Mode = "replicated-job"
	gjob := row("gjob", "docker.io/library/busybox:1@"+dg('b'))
	gjob.Mode = "global-job"
	zero := row("zero", "docker.io/library/nginx:1@"+dg('c'))
	zero.Desired, zero.Running = 0, 0
	glob := row("glob", "docker.io/library/redis:7@"+dg('d'))
	glob.Mode = "global"
	broken := row("broken", "NOT A REFERENCE")
	var ids []string
	for _, w := range updates.ServiceWorkloads([]dockerx.ServiceRow{agent, job, gjob, zero, glob, broken}) {
		ids = append(ids, w.ID)
	}
	if got := strings.Join(ids, ","); got != "zero,glob" {
		t.Errorf("workloads = %q, want zero,glob: the agent, the jobs and an unreadable image are not workloads, scale 0 is", got)
	}
}

func TestServiceWorkloadsWrittenReference(t *testing.T) {
	t.Parallel()
	r1 := row("labelled", "docker.io/library/nginx:1.27@"+dg('a'))
	r1.Labels = map[string]string{"com.docker.stack.image": "nginx:stable", "tierplan.stack.image": "nginx:1.27"}
	r2 := row("swarm-label", "docker.io/library/nginx:1.27@"+dg('a'))
	r2.Labels = map[string]string{"com.docker.stack.image": "nginx:stable"}
	r3 := row("plain", "docker.io/library/nginx:1.27@"+dg('a'))
	r4 := row("pinned", "docker.io/library/nginx@"+dg('a'))
	by := map[string]updates.Workload{}
	for _, w := range updates.ServiceWorkloads([]dockerx.ServiceRow{r1, r2, r3, r4}) {
		by[w.ID] = w
	}
	if w := by["labelled"]; w.Written != "nginx:1.27" || w.Ref.Tag != "1.27" || w.Ref.Digest != "" || w.RunningDigest != dg('a') {
		t.Errorf("labelled = %+v, want the tierplan label's reference, digest dropped, running digest kept", w)
	}
	if w := by["swarm-label"]; w.Written != "nginx:stable" || w.Ref.Tag != "stable" {
		t.Errorf("swarm-label = %+v, want the stack deploy label's reference", w)
	}
	if w := by["plain"]; w.Written != r3.Image || w.Ref.Tag != "1.27" || w.PinnedInFile() {
		t.Errorf("plain = %+v, want the spec image, a tag to follow and not pinned", w)
	}
	if w := by["pinned"]; !w.PinnedInFile() || w.Checkable() {
		t.Errorf("pinned = %+v, want pinned in the file and never checked", w)
	}
}

func TestPinnedServiceIsNeverAsked(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	rows := []dockerx.ServiceRow{row("pinned", "docker.io/library/nginx@"+dg('a'))}
	res, err := r.chk.CheckEnv(ctx, "env-1", updates.ServiceWorkloads(rows))
	if err != nil {
		t.Fatal(err)
	}
	if res != (updates.CheckResult{}) || r.heads.requests() != 0 {
		t.Errorf("result = %+v, requests = %d, want nothing asked for a digest-only reference", res, r.heads.requests())
	}
	got, err := r.svc.ServiceVerdicts(ctx, "env-1", rows)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["pinned"]; v.State != updates.StatePinned {
		t.Errorf("verdict = %+v, want pinned_in_file", v)
	}
}

// fakeSnap is the aggregate of an environment: the nodes with their architectures, and the task
// containers of each service on the node it runs on.
type fakeSnap struct{ agg envs.Aggregate }

func (f fakeSnap) Snapshot(context.Context, string) (envs.Aggregate, envs.Health, error) {
	return f.agg, envs.Health{}, nil
}

// swarmOf builds an aggregate of nodes (id -> architecture) with the tasks given as service ID -> node IDs.
func swarmOf(nodes map[string]string, tasks map[string][]string) fakeSnap {
	agg := envs.Aggregate{NotReporting: []string{}, Unreachable: []envs.NodeRef{}}
	for id, arch := range nodes {
		agg.Nodes = append(agg.Nodes, envs.NodeRef{ID: id, Name: id, Arch: arch})
	}
	for svc, on := range tasks {
		for _, n := range on {
			agg.Containers = append(agg.Containers, envs.ContainerRow{
				ContainerRow: dockerx.ContainerRow{State: "running", Labels: map[string]string{"com.docker.swarm.service.id": svc}},
				NodeID:       n,
			})
		}
	}
	return fakeSnap{agg: agg}
}

// plats is the platform map of an index.
func plats(amd64, arm64 string) map[string]string {
	return map[string]string{"linux/amd64": amd64, "linux/arm64": arm64}
}

func TestPlatformRebuildNobodyRuns(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.svc.Aggregator = swarmOf(map[string]string{"n1": "amd64", "n2": "amd64"}, map[string][]string{"web": {"n1", "n2"}})
	const tag = "ghcr.io/acme/app:2"
	idx1, idx2, idx3 := dg('1'), dg('2'), dg('3')
	rows := []dockerx.ServiceRow{row("web", "ghcr.io/acme/app:2@"+idx1)}
	r.heads.setIndex(idx1, plats(dg('a'), dg('b')))
	r.heads.set(tag, answer{digest: idx1})
	verdict := func() updates.Verdict {
		t.Helper()
		got, err := r.svc.ServiceVerdicts(ctx, "env-1", rows)
		if err != nil {
			t.Fatal(err)
		}
		return got["web"]
	}

	// The service runs the index the tag points at.
	if _, err := r.chk.CheckEnv(ctx, "env-1", workloads(t, rows...)); err != nil {
		t.Fatal(err)
	}
	if v := verdict(); v.State != updates.StateUpToDate {
		t.Fatalf("unchanged tag: %+v, want up_to_date", v)
	}

	// The tag is republished with a new arm64 image and the same amd64 one: nobody runs arm64.
	r.heads.setIndex(idx2, plats(dg('a'), dg('c')))
	r.heads.set(tag, answer{digest: idx2})
	r.clk.advance(time.Hour)
	if _, err := r.chk.CheckEnv(ctx, "env-1", workloads(t, rows...)); err != nil {
		t.Fatal(err)
	}
	if v := verdict(); v.State != updates.StateUpToDate || len(v.ChangedPlatforms) != 0 || v.RemoteDigest != idx2 {
		t.Fatalf("arm64 rebuilt, service on amd64: %+v, want up_to_date with the new index as the remote digest", v)
	}

	// The amd64 image is rebuilt: the service runs it.
	r.heads.setIndex(idx3, plats(dg('d'), dg('c')))
	r.heads.set(tag, answer{digest: idx3})
	r.clk.advance(time.Hour)
	if _, err := r.chk.CheckEnv(ctx, "env-1", workloads(t, rows...)); err != nil {
		t.Fatal(err)
	}
	if v := verdict(); v.State != updates.StateUpdateAvailable || !slices.Equal(v.ChangedPlatforms, []string{"linux/amd64"}) {
		t.Fatalf("amd64 rebuilt, service on amd64: %+v, want update_available naming linux/amd64", v)
	}
}

// The registry request budget (plan 12-03, IMG-04, T-12-06): a check is one HEAD, and a read of the
// index by digest, which Docker Hub counts as a pull, is made only for a digest not seen before.

func TestUnchangedTagCostsOneHeadAndNoGet(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	rows := []dockerx.ServiceRow{
		row("multi", "docker.io/acme/multi:1@"+dg('1')),
		row("solo", "docker.io/acme/solo:1@"+dg('2')),
	}
	r.heads.setIndex(dg('1'), plats(dg('a'), dg('b')))
	r.heads.set("docker.io/acme/multi:1", answer{digest: dg('1')})
	r.heads.set("docker.io/acme/solo:1", answer{digest: dg('2')})
	ws := workloads(t, rows...)

	// The first time a digest is seen its platforms are read, once.
	if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
		t.Fatal(err)
	}
	if got, heads := r.heads.gets(), r.heads.requests(); got != 2 || heads != 2 {
		t.Fatalf("first check: %d reads by digest and %d HEADs, want 2 and 2", got, heads)
	}
	// Then, however often the tag is checked and found unchanged, it costs a HEAD and nothing else.
	for range 3 {
		r.clk.advance(time.Hour)
		if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
			t.Fatal(err)
		}
	}
	if got, heads := r.heads.gets(), r.heads.requests(); got != 2 || heads != 8 {
		t.Errorf("after three more checks: %d reads by digest and %d HEADs, want still 2 reads and 8 HEADs", got, heads)
	}
}

func TestMovedTagCostsOneGet(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	rows := []dockerx.ServiceRow{row("web", "docker.io/acme/app:1@"+dg('1'))}
	ws := workloads(t, rows...)
	r.heads.setIndex(dg('1'), plats(dg('a'), dg('b')))
	r.heads.setIndex(dg('2'), plats(dg('a'), dg('c')))
	r.heads.set("docker.io/acme/app:1", answer{digest: dg('1')})
	if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
		t.Fatal(err)
	}
	gets := r.heads.gets()

	r.heads.set("docker.io/acme/app:1", answer{digest: dg('2')})
	r.clk.advance(time.Hour)
	if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
		t.Fatal(err)
	}
	if got := r.heads.gets() - gets; got != 1 {
		t.Errorf("a moved tag cost %d reads by digest, want 1", got)
	}
	c := stored(t, r, "docker.io", "acme/app", "1", "")
	if c.Digest != dg('2') || !c.Index || c.Platforms["linux/arm64"] != dg('c') {
		t.Errorf("stored = %+v, want the new index with its platforms", c)
	}

	gets = r.heads.gets()
	r.clk.advance(time.Hour)
	if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
		t.Fatal(err)
	}
	if got := r.heads.gets() - gets; got != 0 {
		t.Errorf("the check after it cost %d reads by digest, want 0", got)
	}
}

func TestRunIndexFetchedOnce(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	heads := newHeads()
	heads.setIndex(dg('1'), plats(dg('a'), dg('b')))
	runs := updates.NewRunIndex()
	ref, err := registry.ParseRef("docker.io/acme/app:1")
	if err != nil {
		t.Fatal(err)
	}

	// Two environments, and many goroutines in each, ask for the index their services run.
	var wg sync.WaitGroup
	for _, env := range []string{"env-1", "env-2"} {
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := runs.Platforms(ctx, heads, env, ref, dg('1'))
				if err != nil || p["linux/amd64"] != dg('a') {
					t.Errorf("Platforms = %v, %v, want the platforms of the index", p, err)
				}
			}()
		}
	}
	wg.Wait()
	if got := heads.gets(); got != 1 {
		t.Errorf("the running index was read %d times, want once", got)
	}

	// An index the checker is about to replace is remembered without any read.
	runs.Put(ref, dg('3'), plats(dg('c'), dg('d')))
	if p, err := runs.Platforms(ctx, heads, "env-1", ref, dg('3')); err != nil || p["linux/arm64"] != dg('d') || heads.gets() != 1 {
		t.Errorf("a put index: %v, %v after %d reads, want its platforms and no read", p, err, heads.gets())
	}

	// One that is gone is ErrIndexGone, and a failure is not remembered.
	heads.goneIndex(dg('4'))
	if _, err := runs.Platforms(ctx, heads, "env-1", ref, dg('4')); !errors.Is(err, updates.ErrIndexGone) {
		t.Errorf("a gone index: %v, want ErrIndexGone", err)
	}
	heads.setIndex(dg('4'), plats(dg('e'), dg('f')))
	if p, err := runs.Platforms(ctx, heads, "env-1", ref, dg('4')); err != nil || p["linux/amd64"] != dg('e') {
		t.Errorf("the index served again: %v, %v, want its platforms", p, err)
	}
}

func TestRunningIndexGoneAndServiceWithNoTask(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.svc.Aggregator = swarmOf(map[string]string{"n1": "amd64"}, map[string][]string{"gone": {"n1"}})
	rows := []dockerx.ServiceRow{
		row("gone", "docker.io/acme/app:1@"+dg('1')),
		row("notask", "docker.io/acme/app:1@"+dg('1')),
	}
	r.heads.setIndex(dg('2'), plats(dg('a'), dg('c')))
	r.heads.set("docker.io/acme/app:1", answer{digest: dg('2')})
	r.heads.goneIndex(dg('1'))
	if _, err := r.chk.CheckEnv(ctx, "env-1", workloads(t, rows...)); err != nil {
		t.Fatal(err)
	}
	gets := r.heads.gets()
	got, err := r.svc.ServiceVerdicts(ctx, "env-1", rows)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["gone"]; v.State != updates.StateUpdateAvailable || v.Note != "running_index_gone" {
		t.Errorf("gone = %+v, want update_available with the note running_index_gone", v)
	}
	if v := got["notask"]; v.State != updates.StateUpdateAvailable || v.Note != "" || len(v.ChangedPlatforms) != 0 {
		t.Errorf("notask = %+v, want a plain update_available: no task, no platforms to compare", v)
	}
	if n := r.heads.gets() - gets; n != 1 {
		t.Errorf("the verdicts cost %d reads by digest, want 1: the gone index once, none for the service with no task", n)
	}
}

// --- Fleet checks: credential isolation, repeats, races and the pace of asking (plan 12-05) ---

// fleet runs CheckFleet over the environments with one service each running the same image.
func fleet(t *testing.T, r *rig, force bool, image string, envs ...string) map[string]updates.CheckResult {
	t.Helper()
	ws := map[string][]updates.Workload{}
	for _, env := range envs {
		w := workloads(t, row("app", image))
		w[0].EnvID = env
		ws[env] = w
	}
	res, err := r.chk.CheckFleet(testsupport.Context(t), envs, ws, force)
	if err != nil {
		t.Fatalf("CheckFleet: %v", err)
	}
	return res
}

func TestEnvironmentCredentialsNeverShareARow(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	r.addEnvironment(t, "env-b")
	credA, err := r.reg.Create(ctx, registry.CreateInput{
		EnvID: "env-a", Name: "in-a", Type: registry.TypeGHCR, Prefix: "acme", Username: "bot", Secret: "ghp_0123456789abcdefghijklmnopqrstuvwxyz",
	}, "admin-1", nil)
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	const tag = "ghcr.io/acme/private:1"
	image := tag + "@" + dg('1')
	// A's credential reads the private build. B has none and the registry refuses it.
	r.heads.setEnv("env-a", tag, answer{digest: dg('1'), credID: credA.ID})
	r.heads.setEnv("env-b", tag, answer{err: unauthorized("denied"), credID: ""})

	fleet(t, r, false, image, "env-a", "env-b")
	if n := r.heads.requests(); n != 2 {
		t.Errorf("two credentials made %d requests, want one each", n)
	}
	a := stored(t, r, "ghcr.io", "acme/private", "1", credA.ID)
	b := stored(t, r, "ghcr.io", "acme/private", "1", "")
	if a.Digest != dg('1') || a.Error != "" {
		t.Errorf("A's row = %+v, want its own success", a)
	}
	if b.Digest != "" || b.Error != updates.ReasonAuth {
		t.Errorf("B's row = %+v, want an auth failure and no digest: A's digest must never be readable through B's key", b)
	}

	// B keeps failing: A's row does not change with it.
	r.clk.advance(time.Hour)
	fleet(t, r, false, image, "env-a", "env-b")
	if again := stored(t, r, "ghcr.io", "acme/private", "1", credA.ID); again.Digest != dg('1') || again.Error != "" {
		t.Errorf("A's row after B failed again = %+v, want a success", again)
	}

	// The verdicts agree: B never sees A's digest.
	rows := []dockerx.ServiceRow{row("app", image)}
	vb, err := r.svc.ServiceVerdicts(ctx, "env-b", rows)
	if err != nil {
		t.Fatal(err)
	}
	if v := vb["app"]; v.State != updates.StateCheckFailed || v.RemoteDigest != "" {
		t.Errorf("environment B = %+v, want check_failed and no remote digest", v)
	}
	va, err := r.svc.ServiceVerdicts(ctx, "env-a", rows)
	if err != nil {
		t.Fatal(err)
	}
	if v := va["app"]; v.State != updates.StateUpToDate || v.RemoteDigest != dg('1') {
		t.Errorf("environment A = %+v, want up_to_date from its own row", v)
	}
}

func TestCheckTwiceIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	const tag = "docker.io/library/nginx:1.27"
	platforms := map[string]string{"linux/amd64": dg('a'), "linux/arm64": dg('b')}
	r.heads.set(tag, answer{digest: dg('9')})
	r.heads.setIndex(dg('9'), platforms)
	rows := []dockerx.ServiceRow{row("app", tag+"@"+dg('9'))}

	fleet(t, r, true, tag+"@"+dg('9'), "env-1")
	first := stored(t, r, "docker.io", "library/nginx", "1.27", "")
	vFirst, err := r.svc.ServiceVerdicts(ctx, "env-1", rows)
	if err != nil {
		t.Fatal(err)
	}
	headsBefore, getsBefore := r.heads.requests(), r.heads.gets()
	if headsBefore != 1 || getsBefore != 1 {
		t.Fatalf("the first check made %d HEAD and %d GET, want 1 and 1", headsBefore, getsBefore)
	}

	r.clk.advance(time.Minute)
	fleet(t, r, true, tag+"@"+dg('9'), "env-1")
	second := stored(t, r, "docker.io", "library/nginx", "1.27", "")
	if second.Digest != first.Digest || second.Index != first.Index || !maps.Equal(second.Platforms, first.Platforms) || second.Error != "" {
		t.Errorf("the second check changed the row: %+v then %+v", first, second)
	}
	if !second.CheckedAt.After(first.CheckedAt) {
		t.Errorf("the second check did not run: checked at %v then %v", first.CheckedAt, second.CheckedAt)
	}
	if h, g := r.heads.requests()-headsBefore, r.heads.gets()-getsBefore; h != 1 || g != 0 {
		t.Errorf("the second check made %d HEAD and %d GET, want 1 and 0", h, g)
	}
	vSecond, err := r.svc.ServiceVerdicts(ctx, "env-1", rows)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := vFirst["app"], vSecond["app"]; a.State != b.State || a.RemoteDigest != b.RemoteDigest || !slices.Equal(a.ChangedPlatforms, b.ChangedPlatforms) {
		t.Errorf("the badge changed: %+v then %+v", a, b)
	}
}

// countingStore counts the checks written.
type countingStore struct {
	updates.Store
	mu    sync.Mutex
	saves int
}

func (s *countingStore) SaveSuccess(ctx context.Context, c updates.Check) error {
	s.mu.Lock()
	s.saves++
	s.mu.Unlock()
	return s.Store.SaveSuccess(ctx, c)
}

func TestConcurrentChecksShareOneRequest(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	store := &countingStore{Store: r.sql}
	r.chk.Store = store
	const tag = "docker.io/library/nginx:1.27"
	image := tag + "@" + dg('9')
	r.heads.set(tag, answer{digest: dg('9')})
	r.heads.gate, r.heads.entered = make(chan struct{}), make(chan struct{}, 8)

	// A scheduled check is on its way to the registry, which has not answered.
	var wg sync.WaitGroup
	results := make([]map[string]updates.CheckResult, 2)
	run := func(i int, force bool) {
		defer wg.Done()
		w := workloads(t, row("app", image))
		res, err := r.chk.CheckFleet(ctx, []string{"env-1"}, map[string][]updates.Workload{"env-1": w}, force)
		if err != nil {
			t.Errorf("CheckFleet %d: %v", i, err)
		}
		results[i] = res
	}
	wg.Add(1)
	go run(0, false)
	<-r.heads.entered

	// A person presses Check now. Once it has looked up its credential it is past the point of no return:
	// the registry is asked once, whichever of the two meets the other first.
	wg.Add(1)
	go run(1, true)
	for r.heads.credLookups() < 2 && ctx.Err() == nil {
		runtime.Gosched()
	}
	close(r.heads.gate)
	wg.Wait()

	if n := r.heads.requests(); n != 1 {
		t.Errorf("the two checks sent %d requests, want 1", n)
	}
	store.mu.Lock()
	saves := store.saves
	store.mu.Unlock()
	if saves != 1 {
		t.Errorf("the key was written %d times, want once", saves)
	}
	if got := stored(t, r, "docker.io", "library/nginx", "1.27", ""); got.Digest != dg('9') || got.Error != "" {
		t.Errorf("stored row = %+v, want one whole success", got)
	}
	if results[0]["env-1"] != results[1]["env-1"] || results[0]["env-1"].Checked != 1 {
		t.Errorf("results differ: %+v and %+v", results[0], results[1])
	}
}

func TestMissingDigestHeaderIsUnsupported(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	const tag = "example.com/odd/svc:1"
	image := tag + "@" + dg('a')
	r.heads.set(tag, answer{digest: dg('a')})
	fleet(t, r, true, image, "env-1")

	// The registry stops sending Docker-Content-Digest on HEAD: once as the client's error, once as an
	// answer with no digest in it. Neither is "up to date", and the last good digest is kept.
	for _, a := range []answer{
		{err: fmt.Errorf("registry: head: %w", registry.ErrNoDigest)},
		{digest: ""},
	} {
		r.clk.advance(time.Minute)
		r.heads.set(tag, a)
		fleet(t, r, true, image, "env-1")
		got := stored(t, r, "example.com", "odd/svc", "1", "")
		if got.Error != updates.ReasonUnsupported || got.Digest != dg('a') {
			t.Errorf("row = %+v, want unsupported with the last good digest kept", got)
		}
		v, err := r.svc.ServiceVerdicts(ctx, "env-1", []dockerx.ServiceRow{row("app", image)})
		if err != nil {
			t.Fatal(err)
		}
		if s := v["app"]; s.State != updates.StateCheckFailed || s.Reason != updates.ReasonUnsupported {
			t.Errorf("verdict = %+v, want check_failed with reason unsupported, never up_to_date", s)
		}
	}
}

// credLog records what the checker tells the Registries page.
type credLog struct {
	mu       sync.Mutex
	failures map[string][]string
	clears   map[string]int
}

func newCredLog() *credLog {
	return &credLog{failures: map[string][]string{}, clears: map[string]int{}}
}

func (c *credLog) RecordFailure(_ context.Context, id, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[id] = append(c.failures[id], code)
	return nil
}

func (c *credLog) ClearFailure(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clears[id]++
	return nil
}

func TestCredentialFailureRecordedOncePerRun(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	log := newCredLog()
	r.chk.Creds = log
	// Credential x is refused for two repositories of one registry; credential y works for one; one image
	// is read anonymously and fails, which blames no credential.
	for _, tag := range []string{"ghcr.io/acme/one:1", "ghcr.io/acme/two:1"} {
		r.heads.set(tag, answer{err: unauthorized("denied"), credID: "cred-x"})
	}
	r.heads.set("example.com/other/svc:1", answer{digest: dg('c'), credID: "cred-y"})
	r.heads.set("example.com/anon/svc:1", answer{err: unauthorized("denied")})
	ws := []updates.Workload{}
	for _, img := range []string{"ghcr.io/acme/one:1@" + dg('1'), "ghcr.io/acme/two:1@" + dg('2'), "example.com/other/svc:1@" + dg('c'), "example.com/anon/svc:1@" + dg('d')} {
		w := workloads(t, row(strings.NewReplacer("/", "-", ":", "-", "@", "-", ".", "-").Replace(img), img))
		ws = append(ws, w...)
	}
	if _, err := r.chk.CheckFleet(testsupport.Context(t), []string{"env-1"}, map[string][]updates.Workload{"env-1": ws}, true); err != nil {
		t.Fatalf("CheckFleet: %v", err)
	}
	if got := log.failures["cred-x"]; len(got) != 1 || got[0] != registry.CodeSignInRefused {
		t.Errorf("credential x recorded %v, want one %q however many images it failed for", got, registry.CodeSignInRefused)
	}
	if log.clears["cred-x"] != 0 {
		t.Errorf("credential x was cleared %d times in the run that refused it", log.clears["cred-x"])
	}
	if log.clears["cred-y"] != 1 || len(log.failures["cred-y"]) != 0 {
		t.Errorf("credential y: %d clears, %v failures; want one clear and no failure", log.clears["cred-y"], log.failures["cred-y"])
	}
	if len(log.failures[""]) != 0 || log.clears[""] != 0 || len(log.failures) != 1 {
		t.Errorf("the anonymous call was recorded: failures %v, clears %v", log.failures, log.clears)
	}

	// Next run credential x works: it is cleared once, and not recorded again.
	r.clk.advance(time.Hour)
	for _, tag := range []string{"ghcr.io/acme/one:1", "ghcr.io/acme/two:1"} {
		r.heads.set(tag, answer{digest: dg('1'), credID: "cred-x"})
	}
	if _, err := r.chk.CheckFleet(testsupport.Context(t), []string{"env-1"}, map[string][]updates.Workload{"env-1": ws}, true); err != nil {
		t.Fatalf("CheckFleet: %v", err)
	}
	if log.clears["cred-x"] != 1 || len(log.failures["cred-x"]) != 1 {
		t.Errorf("after it worked, credential x: %d clears and %d failures, want 1 and 1", log.clears["cred-x"], len(log.failures["cred-x"]))
	}
}

func TestMinRecheck(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	const tag = "docker.io/library/nginx:1.27"
	image := tag + "@" + dg('a')
	r.heads.set(tag, answer{digest: dg('a')})

	fleet(t, r, false, image, "env-1")
	if n := r.heads.requests(); n != 1 {
		t.Fatalf("first run made %d requests, want 1", n)
	}
	r.clk.advance(5 * time.Minute)
	res := fleet(t, r, false, image, "env-1")
	if n := r.heads.requests(); n != 1 {
		t.Errorf("a scheduled run 5 minutes after a good check made %d requests in all, want 1", n)
	}
	if res["env-1"].Checked != 1 || res["env-1"].Failed != 0 {
		t.Errorf("a skipped key still counts as checked, not failed: %+v", res["env-1"])
	}
	fleet(t, r, true, image, "env-1")
	if n := r.heads.requests(); n != 2 {
		t.Errorf("a forced check 5 minutes after a good check made %d requests in all, want 2", n)
	}
	r.clk.advance(updates.MinRecheck)
	fleet(t, r, false, image, "env-1")
	if n := r.heads.requests(); n != 3 {
		t.Errorf("a scheduled run %v after a good check made %d requests in all, want 3", updates.MinRecheck, n)
	}
}
