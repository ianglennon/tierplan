// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	defer f.mu.Unlock()
	k := ref.Repo() + ":" + ref.Tag
	f.calls[k]++
	a, ok := f.byEnv[envID+"|"+k]
	if !ok {
		a, ok = f.answers[k]
	}
	if !ok {
		return "", "", errors.New("connection refused")
	}
	return a.digest, a.credID, a.err
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
