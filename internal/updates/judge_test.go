// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/updates"
)

// What Judge says about an image, platform by platform (plan 12-03, IMG-04, D-02).

// multiCheck is the check of a tag that points at the index digest dg('9') with these platforms.
func multiCheck(p map[string]string) *updates.Check {
	return &updates.Check{Digest: dg('9'), Index: true, Platforms: p, CheckedAt: t0, SuccessAt: t0}
}

// serving is a workload that runs the index dg('1') on the platforms given.
func serving(platforms ...string) updates.Workload {
	return updates.Workload{
		ID: "web", Kind: "service", Ref: registry.Ref{Domain: "docker.io", Path: "acme/app", Tag: "2"},
		RunningDigest: dg('1'), Platforms: platforms,
	}
}

func TestJudgePlatformMatrix(t *testing.T) {
	t.Parallel()
	all := map[string]string{"linux/amd64": dg('a'), "linux/arm64": dg('b'), "linux/arm/v7": dg('c')}
	run := map[string]string{"linux/amd64": dg('a'), "linux/arm64": dg('b'), "linux/arm/v7": dg('c')}
	with := func(base map[string]string, k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	for _, tc := range []struct {
		name    string
		in      updates.JudgeInput
		state   updates.State
		changed []string
		note    string
	}{
		{
			name:  "single arch, the digest moved",
			in:    updates.JudgeInput{Workload: serving("linux/amd64"), Check: &updates.Check{Digest: dg('9'), CheckedAt: t0, SuccessAt: t0}},
			state: updates.StateUpdateAvailable,
		},
		{
			name:    "multi arch, only arm64 changed, the service runs amd64 and arm64",
			in:      updates.JudgeInput{Workload: serving("linux/amd64", "linux/arm64"), Check: multiCheck(with(all, "linux/arm64", dg('f'))), RunPlatforms: run},
			state:   updates.StateUpdateAvailable,
			changed: []string{"linux/arm64"},
		},
		{
			name:    "multi arch, both platforms it runs on changed, named in order",
			in:      updates.JudgeInput{Workload: serving("linux/amd64", "linux/arm64"), Check: multiCheck(with(with(all, "linux/arm64", dg('f')), "linux/amd64", dg('e'))), RunPlatforms: run},
			state:   updates.StateUpdateAvailable,
			changed: []string{"linux/amd64", "linux/arm64"},
		},
		{
			name:  "multi arch, only arm/v7 changed, the service runs amd64",
			in:    updates.JudgeInput{Workload: serving("linux/amd64"), Check: multiCheck(with(all, "linux/arm/v7", dg('f'))), RunPlatforms: run},
			state: updates.StateUpToDate,
		},
		{
			name:  "multi arch, the digest moved back to the running one",
			in:    updates.JudgeInput{Workload: serving("linux/amd64"), Check: &updates.Check{Digest: dg('1'), Index: true, Platforms: all, CheckedAt: t0, SuccessAt: t0}, RunPlatforms: run},
			state: updates.StateUpToDate,
		},
		{
			name:  "the running index is not served any more",
			in:    updates.JudgeInput{Workload: serving("linux/amd64"), Check: multiCheck(all), RunIndexGone: true},
			state: updates.StateUpdateAvailable,
			note:  "running_index_gone",
		},
		{
			name:  "no task yet, so no platforms, and the index digest moved",
			in:    updates.JudgeInput{Workload: serving(), Check: multiCheck(all), RunPlatforms: run},
			state: updates.StateUpdateAvailable,
		},
		{
			name:  "the running index could not be read, so the digests are compared",
			in:    updates.JudgeInput{Workload: serving("linux/amd64"), Check: multiCheck(all)},
			state: updates.StateUpdateAvailable,
		},
		{
			name:    "the tag no longer has the platform the service runs",
			in:      updates.JudgeInput{Workload: serving("linux/arm/v7"), Check: multiCheck(map[string]string{"linux/amd64": dg('a')}), RunPlatforms: run},
			state:   updates.StateUpdateAvailable,
			changed: []string{"linux/arm/v7"},
		},
		{
			name:  "the stored check lists no platforms, so the digests are compared",
			in:    updates.JudgeInput{Workload: serving("linux/amd64"), Check: multiCheck(nil), RunPlatforms: run},
			state: updates.StateUpdateAvailable,
		},
		{
			name: "nothing it runs changed, but the success is older than the bound",
			in: updates.JudgeInput{Workload: serving("linux/amd64"), Check: multiCheck(with(all, "linux/arm64", dg('f'))), RunPlatforms: run,
				Now: t0.Add(30 * time.Hour), StaleAfter: 24 * time.Hour},
			state: updates.StateCheckStale,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := tc.in
			if in.Now.IsZero() {
				in.Now, in.StaleAfter = t0.Add(time.Hour), 24*time.Hour
			}
			v := updates.Judge(in)
			if v.State != tc.state || v.Note != tc.note || !slices.Equal(v.ChangedPlatforms, tc.changed) {
				t.Errorf("verdict = %s changed %v note %q, want %s changed %v note %q", v.State, v.ChangedPlatforms, v.Note, tc.state, tc.changed, tc.note)
			}
		})
	}
}

// The rules that decide what a badge may say (plan 12-03, D-03, IMG-04, T-12-05).

func TestJudgeTable(t *testing.T) {
	t.Parallel()
	const stale = 18 * time.Hour
	run := dg('1')
	w := func(digest string) updates.Workload {
		return updates.Workload{ID: "web", Kind: "service", Ref: registry.Ref{Domain: "docker.io", Path: "library/nginx", Tag: "1.27"}, RunningDigest: digest}
	}
	// ok is a check that found digest d at t0.
	ok := func(d string) *updates.Check { return &updates.Check{Digest: d, CheckedAt: t0, SuccessAt: t0} }
	pinned := updates.Workload{ID: "web", Kind: "service", Ref: registry.Ref{Domain: "docker.io", Path: "library/nginx", Digest: run}, RunningDigest: run}

	for _, tc := range []struct {
		name    string
		in      updates.JudgeInput
		state   updates.State
		reason  updates.Reason
		success time.Time // the SuccessAt the verdict must carry
		remote  string
	}{
		{name: "no stored check", in: updates.JudgeInput{Workload: w(run), Now: t0}, state: updates.StateNotChecked},
		{
			name: "failed with auth after an earlier success",
			in: updates.JudgeInput{Workload: w(run), Now: t0.Add(2 * time.Hour),
				Check: &updates.Check{Digest: run, CheckedAt: t0.Add(time.Hour), SuccessAt: t0, Error: updates.ReasonAuth}},
			state: updates.StateCheckFailed, reason: updates.ReasonAuth, success: t0,
		},
		{
			name: "failed with no success ever",
			in: updates.JudgeInput{Workload: w(run), Now: t0.Add(2 * time.Hour),
				Check: &updates.Check{CheckedAt: t0.Add(time.Hour), Error: updates.ReasonUnreachable}},
			state: updates.StateCheckFailed, reason: updates.ReasonUnreachable,
		},
		{
			name:  "the remote digest is the running one, an hour old",
			in:    updates.JudgeInput{Workload: w(run), Check: ok(run), Now: t0.Add(time.Hour), StaleAfter: stale},
			state: updates.StateUpToDate, success: t0, remote: run,
		},
		{
			name:  "the same digest, found longer ago than the bound",
			in:    updates.JudgeInput{Workload: w(run), Check: ok(run), Now: t0.Add(30 * time.Hour), StaleAfter: 24 * time.Hour},
			state: updates.StateCheckStale, success: t0, remote: run,
		},
		{
			name:  "the remote digest differs",
			in:    updates.JudgeInput{Workload: w(run), Check: ok(dg('9')), Now: t0.Add(time.Hour), StaleAfter: stale},
			state: updates.StateUpdateAvailable, success: t0, remote: dg('9'),
		},
		{
			name: "a failure newer than a success that had found another digest",
			in: updates.JudgeInput{Workload: w(run), Now: t0.Add(2 * time.Hour), StaleAfter: stale,
				Check: &updates.Check{Digest: dg('9'), CheckedAt: t0.Add(time.Hour), SuccessAt: t0, Error: updates.ReasonRateLimited}},
			state: updates.StateCheckFailed, reason: updates.ReasonRateLimited, success: t0,
		},
		{
			name:  "written as a digest with no tag, whatever the check says",
			in:    updates.JudgeInput{Workload: pinned, Check: ok(dg('9')), Now: t0.Add(time.Hour), StaleAfter: stale},
			state: updates.StatePinned,
		},
		{
			name: "written as a digest with no tag, with a failed check",
			in: updates.JudgeInput{Workload: pinned, Now: t0.Add(2 * time.Hour), StaleAfter: stale,
				Check: &updates.Check{CheckedAt: t0.Add(time.Hour), Error: updates.ReasonAuth}},
			state: updates.StatePinned,
		},
		{
			name:  "a success that named no digest is not an answer",
			in:    updates.JudgeInput{Workload: w(run), Check: &updates.Check{CheckedAt: t0, SuccessAt: t0}, Now: t0, StaleAfter: stale},
			state: updates.StateNotChecked, success: t0,
		},
		{
			name:  "the service names no digest to compare with",
			in:    updates.JudgeInput{Workload: w(""), Check: ok(dg('9')), Now: t0, StaleAfter: stale},
			state: updates.StateNotFromRegistry, success: t0, remote: dg('9'),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := updates.Judge(tc.in)
			if v.State != tc.state || v.Reason != tc.reason {
				t.Fatalf("verdict = %s (reason %q), want %s (reason %q)", v.State, v.Reason, tc.state, tc.reason)
			}
			if !v.SuccessAt.Equal(tc.success) {
				t.Errorf("SuccessAt = %v, want %v", v.SuccessAt, tc.success)
			}
			if v.RemoteDigest != tc.remote {
				t.Errorf("RemoteDigest = %q, want %q", v.RemoteDigest, tc.remote)
			}
			if v.Note != "" && v.Note != updates.NoteRunningIndexGone {
				t.Errorf("Note = %q, want empty or a code of the note enum", v.Note)
			}
		})
	}
}

// TestJudgeWrittenReference is the rule of the written reference: a spec image that carries a digest is
// checkable when the user wrote a tag, and pinned when the user wrote a digest.
func TestJudgeWrittenReference(t *testing.T) {
	t.Parallel()
	image := "docker.io/library/nginx:1.27@" + dg('1')
	tagged, digested := row("tagged", image), row("digested", image)
	tagged.Labels = map[string]string{"tierplan.stack.image": "nginx:1.27"}
	digested.Labels = map[string]string{"tierplan.stack.image": "nginx@" + dg('1')}
	for _, tc := range []struct {
		row       dockerx.ServiceRow
		pinned    bool
		wantState updates.State
	}{
		{tagged, false, updates.StateUpToDate},
		{digested, true, updates.StatePinned},
	} {
		ws := updates.ServiceWorkloads([]dockerx.ServiceRow{tc.row})
		if len(ws) != 1 {
			t.Fatalf("%s: %d workloads", tc.row.ID, len(ws))
		}
		if ws[0].PinnedInFile() != tc.pinned || ws[0].Checkable() == tc.pinned {
			t.Errorf("%s: pinned = %v, checkable = %v, want pinned %v", tc.row.ID, ws[0].PinnedInFile(), ws[0].Checkable(), tc.pinned)
		}
		chk := &updates.Check{Digest: dg('1'), CheckedAt: t0, SuccessAt: t0}
		if v := updates.Judge(updates.JudgeInput{Workload: ws[0], Check: chk, Now: t0, StaleAfter: time.Hour}); v.State != tc.wantState {
			t.Errorf("%s: state = %s, want %s", tc.row.ID, v.State, tc.wantState)
		}
	}
}

// TestJudgeNeverUpToDateWithoutFreshSuccess runs Judge over every combination of a check that is
// missing, failed, stale or fresh, a digest that is equal or not, and platform data that is complete,
// partial or absent, and asserts the one rule that matters: up_to_date is only ever said for a fresh
// success whose running digest is the remote one, or whose every platform the workload runs on has the
// same manifest in both.
func TestJudgeNeverUpToDateWithoutFreshSuccess(t *testing.T) {
	t.Parallel()
	const stale = 24 * time.Hour
	now := t0.Add(48 * time.Hour)
	run := dg('1')
	same := map[string]string{"linux/amd64": dg('a'), "linux/arm64": dg('b')}
	moved := map[string]string{"linux/amd64": dg('a'), "linux/arm64": dg('f')}
	movedAmd := map[string]string{"linux/amd64": dg('e'), "linux/arm64": dg('b')}

	type chkCase struct {
		name string
		c    *updates.Check
	}
	fresh, old := now.Add(-time.Hour), now.Add(-30*time.Hour)
	var checks []chkCase
	checks = append(checks, chkCase{"missing", nil})
	for _, digest := range []string{"", run, dg('9')} {
		for _, index := range []bool{false, true} {
			for _, p := range []map[string]string{nil, same, moved, movedAmd} {
				for _, when := range []struct {
					name      string
					chk, succ time.Time
					errCode   updates.Reason
				}{
					{"fresh", fresh, fresh, ""},
					{"stale", old, old, ""},
					{"never", fresh, time.Time{}, updates.ReasonOther},
					{"failed-after-fresh", fresh, fresh.Add(-time.Minute), updates.ReasonAuth},
					{"failed-after-stale", fresh, old, updates.ReasonUnreachable},
					{"failed-before-success", old, fresh, updates.ReasonAuth},
				} {
					checks = append(checks, chkCase{
						name: fmt.Sprintf("digest=%q index=%v platforms=%d %s", digest, index, len(p), when.name),
						c:    &updates.Check{Digest: digest, Index: index, Platforms: p, CheckedAt: when.chk, SuccessAt: when.succ, Error: when.errCode},
					})
				}
			}
		}
	}
	workloads := []updates.Workload{
		{RunningDigest: run, Ref: registry.Ref{Tag: "1"}},
		{RunningDigest: run, Ref: registry.Ref{Tag: "1"}, Platforms: []string{"linux/amd64"}},
		{RunningDigest: run, Ref: registry.Ref{Tag: "1"}, Platforms: []string{"linux/amd64", "linux/arm64"}},
		{RunningDigest: "", Ref: registry.Ref{Tag: "1"}},
		{RunningDigest: run, Ref: registry.Ref{Digest: run}},
	}
	runs := []map[string]string{nil, same, moved, {"linux/amd64": dg('a')}}

	for _, w := range workloads {
		for _, cc := range checks {
			for _, rp := range runs {
				for _, gone := range []bool{false, true} {
					v := updates.Judge(updates.JudgeInput{Workload: w, Check: cc.c, Now: now, StaleAfter: stale, RunPlatforms: rp, RunIndexGone: gone})
					if v.State != updates.StateUpToDate {
						continue
					}
					c := cc.c
					if c == nil {
						t.Fatalf("up_to_date with no stored check: workload %+v", w)
					}
					failedNewer := c.Error != "" && c.CheckedAt.After(c.SuccessAt)
					freshSuccess := !c.SuccessAt.IsZero() && now.Sub(c.SuccessAt) <= stale
					if failedNewer || !freshSuccess || c.Digest == "" || w.RunningDigest == "" || w.PinnedInFile() {
						t.Fatalf("up_to_date without a fresh success of a digest: check %s, workload %+v", cc.name, w)
					}
					if c.Digest != w.RunningDigest {
						// Only a multi-platform tag, and only when everything the workload runs on is the same.
						if gone || !c.Index || len(c.Platforms) == 0 || len(w.Platforms) == 0 {
							t.Fatalf("up_to_date with another digest and no platform proof: check %s, workload %+v, gone %v", cc.name, w, gone)
						}
						for _, p := range w.Platforms {
							if c.Platforms[p] == "" || c.Platforms[p] != rp[p] {
								t.Fatalf("up_to_date though platform %s differs: check %s, run platforms %v", p, cc.name, rp)
							}
						}
					}
				}
			}
		}
	}
}

// ownImage is a container workload of the image repo/app:1 that the registry would have to answer for.
func ownImage(store string) updates.Workload {
	return updates.Workload{
		ID: "c1", Kind: "container", Ref: registry.Ref{Domain: "docker.io", Path: "acme/app", Tag: "1"},
		RunningDigest: dg('1'), LocalStore: store,
	}
}

func TestJudgeLocalBuildInference(t *testing.T) {
	t.Parallel()
	failed := func(r updates.Reason, cred string) *updates.Check {
		return &updates.Check{Key: updates.Key{CredID: cred}, Error: r, CheckedAt: t0}
	}
	for _, tc := range []struct {
		name   string
		in     updates.JudgeInput
		state  updates.State
		reason updates.Reason
	}{
		{
			name:  "containerd, pulled multi-arch image, the anonymous check found the same digest",
			in:    updates.JudgeInput{Workload: ownImage(updates.LocalContainerd), Check: &updates.Check{Digest: dg('1'), CheckedAt: t0, SuccessAt: t0}},
			state: updates.StateUpToDate,
		},
		{
			name:  "containerd, never pushed, the anonymous check was not found",
			in:    updates.JudgeInput{Workload: ownImage(updates.LocalContainerd), Check: failed(updates.ReasonNotFound, "")},
			state: updates.StateNotFromRegistry,
		},
		{
			name:  "containerd, never pushed, the anonymous check was refused",
			in:    updates.JudgeInput{Workload: ownImage(updates.LocalContainerd), Check: failed(updates.ReasonAuth, "")},
			state: updates.StateNotFromRegistry,
		},
		{
			name:   "containerd, a credential matched and the check was refused: no inference",
			in:     updates.JudgeInput{Workload: ownImage(updates.LocalContainerd), Check: failed(updates.ReasonAuth, "cred-1")},
			state:  updates.StateCheckFailed,
			reason: updates.ReasonAuth,
		},
		{
			name:   "containerd, a credential matched and the name was not found: no inference",
			in:     updates.JudgeInput{Workload: ownImage(updates.LocalContainerd), Check: failed(updates.ReasonNotFound, "cred-1")},
			state:  updates.StateCheckFailed,
			reason: updates.ReasonNotFound,
		},
		{
			name:   "containerd, rate limited: only not_found and auth infer",
			in:     updates.JudgeInput{Workload: ownImage(updates.LocalContainerd), Check: failed(updates.ReasonRateLimited, "")},
			state:  updates.StateCheckFailed,
			reason: updates.ReasonRateLimited,
		},
		{
			name:   "containerd, unreachable",
			in:     updates.JudgeInput{Workload: ownImage(updates.LocalContainerd), Check: failed(updates.ReasonUnreachable, "")},
			state:  updates.StateCheckFailed,
			reason: updates.ReasonUnreachable,
		},
		{
			name:   "a repo digest alone never decides: no store hint, not found",
			in:     updates.JudgeInput{Workload: ownImage(""), Check: failed(updates.ReasonNotFound, "")},
			state:  updates.StateCheckFailed,
			reason: updates.ReasonNotFound,
		},
		{
			name:  "classic store, no repo digest at all, nothing was asked",
			in:    updates.JudgeInput{Workload: ownImage(updates.LocalClassic)},
			state: updates.StateNotFromRegistry,
		},
		{
			name:  "classic store, no repo digest, even with a failed check on file",
			in:    updates.JudgeInput{Workload: ownImage(updates.LocalClassic), Check: failed(updates.ReasonRateLimited, "")},
			state: updates.StateNotFromRegistry,
		},
		{
			name:  "classic store, pulled: compared on the repo digest and the same",
			in:    updates.JudgeInput{Workload: ownImage(""), Check: &updates.Check{Digest: dg('1'), CheckedAt: t0, SuccessAt: t0}},
			state: updates.StateUpToDate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.in.Now, tc.in.StaleAfter = t0.Add(time.Minute), 24*time.Hour
			v := updates.Judge(tc.in)
			if v.State != tc.state || v.Reason != tc.reason {
				t.Errorf("Judge = %s (%s), want %s (%s)", v.State, v.Reason, tc.state, tc.reason)
			}
		})
	}
}

// An update that was found a long time ago may have been followed by another, or taken back: the verdict
// keeps its state and says it may be out of date (plan 12-09, D-01).
func TestJudgeMayBeStale(t *testing.T) {
	t.Parallel()
	moved := func(age time.Duration) updates.JudgeInput {
		at := t0.Add(-age)
		return updates.JudgeInput{
			Workload: serving("linux/amd64"), Now: t0, StaleAfter: 18 * time.Hour,
			Check: &updates.Check{Digest: dg('9'), CheckedAt: at, SuccessAt: at},
		}
	}
	if v := updates.Judge(moved(30 * time.Hour)); v.State != updates.StateUpdateAvailable || v.Note != updates.NoteMayBeStale {
		t.Errorf("an update found 30h ago = %+v, want update_available with the note may_be_stale", v)
	}
	if v := updates.Judge(moved(17 * time.Hour)); v.State != updates.StateUpdateAvailable || v.Note != "" {
		t.Errorf("an update found 17h ago = %+v, want update_available with no note", v)
	}
	if v := updates.Judge(moved(18 * time.Hour)); v.Note != "" {
		t.Errorf("an update found exactly at the bound = %+v, want no note", v)
	}
	// The platform route says the same.
	all := map[string]string{"linux/amd64": dg('a'), "linux/arm64": dg('b')}
	in := updates.JudgeInput{
		Workload: serving("linux/amd64"), Now: t0, StaleAfter: 18 * time.Hour,
		Check:        &updates.Check{Digest: dg('9'), Index: true, Platforms: map[string]string{"linux/amd64": dg('f'), "linux/arm64": dg('b')}, CheckedAt: t0.Add(-30 * time.Hour), SuccessAt: t0.Add(-30 * time.Hour)},
		RunPlatforms: all,
	}
	if v := updates.Judge(in); v.State != updates.StateUpdateAvailable || v.Note != updates.NoteMayBeStale || !slices.Equal(v.ChangedPlatforms, []string{"linux/amd64"}) {
		t.Errorf("a platform change found 30h ago = %+v, want update_available, may_be_stale and the platform", v)
	}
	// A running index the registry no longer serves keeps the note that says so.
	gone := moved(30 * time.Hour)
	gone.RunIndexGone = true
	if v := updates.Judge(gone); v.State != updates.StateUpdateAvailable || v.Note != updates.NoteRunningIndexGone {
		t.Errorf("a gone index = %+v, want its own note", v)
	}
	// A same-digest result that old is stale, and carries no update note.
	same := moved(30 * time.Hour)
	same.Check.Digest = dg('1')
	if v := updates.Judge(same); v.State != updates.StateCheckStale || v.Note != "" {
		t.Errorf("the same digest 30h ago = %+v, want check_stale", v)
	}
}

// A workload that was never checked says when the schedule will check it; one that was has no use for it.
func TestJudgeNotCheckedCarriesNextCheck(t *testing.T) {
	t.Parallel()
	next := t0.Add(5 * time.Hour)
	if v := updates.Judge(updates.JudgeInput{Workload: serving("linux/amd64"), Now: t0, NextCheckAt: next}); v.State != updates.StateNotChecked || !v.NextCheckAt.Equal(next) {
		t.Errorf("no check stored = %+v, want not_checked and %v", v, next)
	}
	empty := &updates.Check{CheckedAt: t0}
	if v := updates.Judge(updates.JudgeInput{Workload: serving("linux/amd64"), Check: empty, Now: t0, NextCheckAt: next}); v.State != updates.StateNotChecked || !v.NextCheckAt.Equal(next) {
		t.Errorf("a row with no answer = %+v, want not_checked and %v", v, next)
	}
	ok := &updates.Check{Digest: dg('9'), CheckedAt: t0, SuccessAt: t0}
	if v := updates.Judge(updates.JudgeInput{Workload: serving("linux/amd64"), Check: ok, Now: t0, StaleAfter: time.Hour, NextCheckAt: next}); !v.NextCheckAt.IsZero() {
		t.Errorf("a checked workload = %+v, want no next check", v)
	}
}

// Skips and ignores in the verdict (plan 12-13, IMG-08, D-15, D-16).

// held is a service on acme/app:2 running dg('1'), with the labels given.
func held(labels map[string]string) updates.Workload {
	w := serving("linux/amd64")
	w.EnvID, w.Name, w.Labels = "env-a", "web", labels
	return w
}

// judgedWith matches ignores to the workload as the service does and judges it against the check.
func judgedWith(w updates.Workload, igs updates.Ignores, chk *updates.Check) updates.Verdict {
	remote := ""
	if chk != nil {
		remote = chk.Digest
	}
	return updates.Judge(updates.JudgeInput{Workload: w, Check: chk, Now: t0, StaleAfter: time.Hour, Ignore: igs.Match(w, remote)})
}

func TestJudgeIgnorePrecedence(t *testing.T) {
	t.Parallel()
	moved := &updates.Check{Digest: dg('9'), CheckedAt: t0, SuccessAt: t0}
	failedAfter := &updates.Check{Digest: dg('9'), Error: updates.ReasonRateLimited, CheckedAt: t0, SuccessAt: t0.Add(-time.Minute)}
	skip := func(d string) updates.Ignore {
		return updates.Ignore{EnvID: "env-a", Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", Digest: d, SetBy: "ann", SetAt: t0}
	}
	image := updates.Ignore{EnvID: "env-a", Mode: updates.ModeIgnore, Scope: updates.ScopeImage, Host: "docker.io", Repo: "acme/app", Tag: "2", SetBy: "bob", SetAt: t0}
	workload := updates.Ignore{EnvID: "env-a", Mode: updates.ModeIgnore, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", SetBy: "bob", SetAt: t0}
	label := map[string]string{stacks.LabelUpdates: "ignore"}

	for _, tc := range []struct {
		name  string
		w     updates.Workload
		igs   updates.Ignores
		chk   *updates.Check
		state updates.State
		scope string
	}{
		{name: "nothing set", w: held(nil), chk: moved, state: updates.StateUpdateAvailable},
		{name: "a skip of the offered digest wins over update_available", w: held(nil), igs: updates.Ignores{skip(dg('9'))}, chk: moved, state: updates.StateSkipped, scope: updates.ScopeWorkload},
		{name: "a skip of another digest lifts at once", w: held(nil), igs: updates.Ignores{skip(dg('8'))}, chk: moved, state: updates.StateUpdateAvailable},
		{name: "a skip wins over check_failed while the stored digest is the skipped one", w: held(nil), igs: updates.Ignores{skip(dg('9'))}, chk: failedAfter, state: updates.StateSkipped, scope: updates.ScopeWorkload},
		{name: "a skip does not turn an up to date workload into skipped", w: held(nil), igs: updates.Ignores{skip(dg('1'))}, chk: &updates.Check{Digest: dg('1'), CheckedAt: t0, SuccessAt: t0}, state: updates.StateUpToDate},
		{name: "an ignore of the image", w: held(nil), igs: updates.Ignores{image}, chk: moved, state: updates.StateIgnored, scope: updates.ScopeImage},
		{name: "an ignore of the workload", w: held(nil), igs: updates.Ignores{workload}, chk: moved, state: updates.StateIgnored, scope: updates.ScopeWorkload},
		{name: "ignored wins over skipped", w: held(nil), igs: updates.Ignores{skip(dg('9')), image}, chk: moved, state: updates.StateIgnored, scope: updates.ScopeImage},
		{name: "an ignore reads ignored before any check", w: held(nil), igs: updates.Ignores{workload}, state: updates.StateIgnored, scope: updates.ScopeWorkload},
		{name: "the label wins over a stored skip", w: held(label), igs: updates.Ignores{skip(dg('9'))}, chk: moved, state: updates.StateIgnored, scope: updates.ScopeLabel},
		{name: "the label alone", w: held(label), chk: moved, state: updates.StateIgnored, scope: updates.ScopeLabel},
		{name: "any other value of the label hides nothing", w: held(map[string]string{stacks.LabelUpdates: "true"}), chk: moved, state: updates.StateUpdateAvailable},
	} {
		v := judgedWith(tc.w, tc.igs, tc.chk)
		if v.State != tc.state {
			t.Errorf("%s: state = %s, want %s", tc.name, v.State, tc.state)
			continue
		}
		if tc.scope == "" {
			if v.Ignore != nil {
				t.Errorf("%s: ignore = %+v, want none", tc.name, v.Ignore)
			}
			continue
		}
		if v.Ignore == nil || v.Ignore.Scope != tc.scope {
			t.Errorf("%s: ignore = %+v, want scope %s", tc.name, v.Ignore, tc.scope)
		}
		if v.Reason != "" || v.Note != "" {
			t.Errorf("%s: reason %q note %q, want neither on a held verdict", tc.name, v.Reason, v.Note)
		}
	}

	// The skip answers who and what it passed over.
	v := judgedWith(held(nil), updates.Ignores{skip(dg('9'))}, moved)
	if v.RemoteDigest != dg('9') || v.RunningDigest != dg('1') || v.Ignore.SetBy != "ann" || v.Ignore.Digest != dg('9') {
		t.Errorf("skipped verdict = %+v, want both digests and who skipped", v)
	}

	// A workload with nothing to follow stays what it is: there is nothing to hide.
	pinned := held(nil)
	pinned.Ref = registry.Ref{Domain: "docker.io", Path: "acme/app", Digest: dg('1')}
	if v := judgedWith(pinned, updates.Ignores{workload}, nil); v.State != updates.StatePinned {
		t.Errorf("pinned workload with an ignore = %s, want pinned_in_file", v.State)
	}
}

func TestIgnoredNeverCounted(t *testing.T) {
	t.Parallel()
	moved := &updates.Check{Digest: dg('9'), CheckedAt: t0, SuccessAt: t0}
	igs := updates.Ignores{
		{EnvID: "env-a", Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", Digest: dg('9')},
		{EnvID: "env-a", Mode: updates.ModeIgnore, Scope: updates.ScopeWorkload, Kind: "service", Name: "api"},
	}
	named := func(name string) updates.Workload { w := held(nil); w.Name, w.ID = name, name; return w }
	web, api, db := judgedWith(named("web"), igs, moved), judgedWith(named("api"), igs, moved), judgedWith(named("db"), igs, moved)
	if web.State != updates.StateSkipped || api.State != updates.StateIgnored || db.State != updates.StateUpdateAvailable {
		t.Fatalf("states = %s %s %s, want skipped ignored update_available", web.State, api.State, db.State)
	}
	if got := updates.Aggregate([]updates.Verdict{web, api}); got.Count != 0 || got.State != updates.StateIgnored {
		t.Errorf("stack of a skipped and an ignored workload = %+v, want ignored and no count", got)
	}
	if got := updates.Aggregate([]updates.Verdict{web}); got.Count != 0 || got.State != updates.StateSkipped {
		t.Errorf("stack of a skipped workload = %+v, want skipped and no count", got)
	}
	if got := updates.Aggregate([]updates.Verdict{web, api, db}); got.Count != 1 || got.State != updates.StateUpdateAvailable {
		t.Errorf("stack with one real update = %+v, want it counted alone", got)
	}
}

// Updating and Busy (plan 12-16, IMG-05, IMG-06, UI-SPEC): what is happening now overrides what the check found, but only where
// it is true, and informational states are never turned into Busy.
func TestJudgeUpdatingAndBusy(t *testing.T) {
	t.Parallel()
	moved := &updates.Check{Digest: dg('9'), CheckedAt: t0, SuccessAt: t0}
	same := &updates.Check{Digest: dg('1'), CheckedAt: t0, SuccessAt: t0}
	failed := &updates.Check{Digest: dg('9'), Error: updates.ReasonRateLimited, CheckedAt: t0, SuccessAt: t0.Add(-time.Minute)}
	janitor := &stackops.Holder{Kind: stackops.HolderKindJanitor, RunID: "run-1", Started: t0}
	updateOfWeb := &stackops.Holder{Kind: stackops.HolderKindImageUpdate, Workload: "web", By: "ann", Started: t0}
	ignoreWeb := updates.Ignores{{EnvID: "env-a", Mode: updates.ModeIgnore, Scope: updates.ScopeWorkload, Kind: "service", Name: "web"}}
	named := func(name string) updates.Workload {
		w := held(nil)
		w.Name = name
		return w
	}
	pinned := held(nil)
	pinned.Written, pinned.Ref = "acme/app@"+dg('1'), registry.Ref{Domain: "docker.io", Path: "acme/app", Digest: dg('1')}

	for _, tc := range []struct {
		name   string
		w      updates.Workload
		chk    *updates.Check
		igs    updates.Ignores
		holder *stackops.Holder
		job    string
		state  updates.State
		jobID  string
		kind   string
	}{
		{name: "an update running for it reads updating, with its job", w: named("web"), chk: moved, job: "job-7", state: updates.StateUpdating, jobID: "job-7"},
		{name: "updating wins over busy when its own update holds the lock", w: named("web"), chk: moved, holder: updateOfWeb, job: "job-7", state: updates.StateUpdating, jobID: "job-7"},
		{name: "updating wins even over a check that says it is up to date", w: named("web"), chk: same, job: "job-7", state: updates.StateUpdating, jobID: "job-7"},
		{name: "the janitor holds the lock: an update that would be offered is busy", w: named("api"), chk: moved, holder: janitor, state: updates.StateBusy, kind: updates.HolderJanitor},
		{name: "an update of web holds the lock: api is busy with that holder", w: named("api"), chk: moved, holder: updateOfWeb, state: updates.StateBusy, kind: updates.HolderUpdate},
		{name: "up to date stays up to date under a lock", w: named("api"), chk: same, holder: janitor, state: updates.StateUpToDate},
		{name: "a failed check stays failed under a lock", w: named("api"), chk: failed, holder: janitor, state: updates.StateCheckFailed},
		{name: "not checked stays not checked under a lock", w: named("api"), holder: janitor, state: updates.StateNotChecked},
		{name: "pinned stays pinned under a lock", w: pinned, chk: moved, holder: janitor, state: updates.StatePinned},
		{name: "ignored stays ignored under a lock", w: named("web"), chk: moved, igs: ignoreWeb, holder: janitor, state: updates.StateIgnored},
		{name: "no lock and no run changes nothing", w: named("web"), chk: moved, state: updates.StateUpdateAvailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			remote := ""
			if tc.chk != nil {
				remote = tc.chk.Digest
			}
			got := updates.Judge(updates.JudgeInput{
				Workload: tc.w, Check: tc.chk, Now: t0, StaleAfter: time.Hour, Ignore: tc.igs.Match(tc.w, remote),
				Holder: tc.holder, RunningJob: tc.job,
			})
			if got.State != tc.state || got.JobID != tc.jobID {
				t.Fatalf("verdict = %s job %q, want %s job %q", got.State, got.JobID, tc.state, tc.jobID)
			}
			switch {
			case tc.state == updates.StateBusy:
				if got.Holder == nil || updates.HolderKind(*got.Holder) != tc.kind || got.Holder.Workload != tc.holder.Workload {
					t.Errorf("holder = %+v, want kind %s of %+v", got.Holder, tc.kind, tc.holder)
				}
			case got.Holder != nil:
				t.Errorf("holder = %+v on a %s verdict, want none", got.Holder, got.State)
			}
			if tc.state == updates.StateUpdating && (got.Reason != "" || got.Note != "" || got.Ignore != nil) {
				t.Errorf("an updating verdict carries %q %q %+v, want none of the old story", got.Reason, got.Note, got.Ignore)
			}
			if tc.state == updates.StateBusy && got.RemoteDigest != dg('9') {
				t.Errorf("a busy verdict keeps the digest it was offered: remote = %q", got.RemoteDigest)
			}
		})
	}
}

// The kinds of operation a Busy badge names (plan 12-16, UI-SPEC).
func TestHolderKinds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		h    stackops.Holder
		want string
	}{
		{"the janitor", stackops.Holder{Kind: stackops.HolderKindJanitor, RunID: "r"}, updates.HolderJanitor},
		{"a roll back", stackops.Holder{Kind: "rollback", StackName: "shop", By: "ann"}, updates.HolderRollback},
		{"a restore is a roll back", stackops.Holder{Kind: "restore", StackName: "shop", By: "ann"}, updates.HolderRollback},
		{"an image update", stackops.Holder{Kind: stackops.HolderKindImageUpdate, Workload: "web"}, updates.HolderUpdate},
		{"a stack's Update images", stackops.Holder{Kind: "update", StackName: "shop", By: "ann"}, updates.HolderUpdate},
		{"a person's deploy", stackops.Holder{Kind: "deploy", StackName: "shop", By: "ann"}, updates.HolderDeploy},
		{"a git auto-deploy by poll", stackops.Holder{Kind: "deploy", StackName: "shop", By: "system:gitsync"}, updates.HolderGitDeploy},
		{"a git auto-deploy for an owner", stackops.Holder{Kind: "deploy", StackName: "shop", By: "system:gitsync:u-1"}, updates.HolderGitDeploy},
		{"a git auto-deploy by webhook", stackops.Holder{Kind: "deploy", StackName: "shop", By: "system:webhook"}, updates.HolderGitDeploy},
		{"a stop", stackops.Holder{Kind: "stop", StackName: "shop", By: "ann"}, updates.HolderDeploy},
		{"a removal", stackops.Holder{Kind: stackops.ModeRemove, StackName: "shop", By: "ann"}, updates.HolderDeploy},
		{"an adoption", stackops.Holder{Kind: "adopt", StackName: "shop", By: "ann"}, updates.HolderDeploy},
	} {
		if got := updates.HolderKind(tc.h); got != tc.want {
			t.Errorf("%s: kind = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A digest that failed and was rolled back is not offered again: the badge reads rolled_back (rollback_failed when the restore
// failed too) for exactly that digest, the counts leave it out, and the skip and the ignore a person set still win (plan 12-22,
// IMG-07, D-14).
func TestRolledBackDigestNotReoffered(t *testing.T) {
	t.Parallel()
	moved := &updates.Check{Digest: dg('9'), CheckedAt: t0, SuccessAt: t0}
	failedWith := func(digest string, rollbackFailed bool) *updates.Failure {
		return &updates.Failure{
			EnvID: "env-a", Kind: "service", Workload: "web", Digest: digest, Reason: "didnt_start", RunID: "run-1",
			RollbackFailed: rollbackFailed, FailedAt: t0,
		}
	}
	judge := func(chk *updates.Check, f *updates.Failure, igs updates.Ignores) updates.Verdict {
		w := held(nil)
		remote := ""
		if chk != nil {
			remote = chk.Digest
		}
		return updates.Judge(updates.JudgeInput{
			Workload: w, Check: chk, Now: t0, StaleAfter: time.Hour, Ignore: igs.Match(w, remote), Failure: f,
		})
	}

	rolled := judge(moved, failedWith(dg('9'), false), nil)
	if rolled.State != updates.StateRolledBack || rolled.Failure == nil || rolled.Failure.Digest != dg('9') || rolled.RemoteDigest != dg('9') {
		t.Fatalf("verdict = %s %+v remote %q, want rolled_back for the digest that failed", rolled.State, rolled.Failure, rolled.RemoteDigest)
	}
	notRestored := judge(moved, failedWith(dg('9'), true), nil)
	if notRestored.State != updates.StateRollbackFailed || notRestored.Failure == nil || !notRestored.Failure.RollbackFailed {
		t.Fatalf("verdict = %s %+v, want rollback_failed when the restore failed too", notRestored.State, notRestored.Failure)
	}

	// Nothing that offers an update counts it: the stack roll-up reads the failure, not an update with a count.
	other := judge(&updates.Check{Digest: dg('8'), CheckedAt: t0, SuccessAt: t0}, nil, nil)
	if got := updates.Aggregate([]updates.Verdict{rolled}); got.State != updates.StateRolledBack || got.Count != 0 {
		t.Errorf("stack of a rolled back workload = %+v, want rolled_back and no count", got)
	}
	if got := updates.Aggregate([]updates.Verdict{notRestored}); got.State != updates.StateRollbackFailed || got.Count != 0 {
		t.Errorf("stack of a workload whose rollback failed = %+v, want rollback_failed and no count", got)
	}
	if got := updates.Aggregate([]updates.Verdict{rolled, other}); got.State != updates.StateUpdateAvailable || got.Count != 1 {
		t.Errorf("stack with one rolled back and one real update = %+v, want only the real one counted", got)
	}

	// A skip or an ignore a person set takes precedence over the failure.
	skip := updates.Ignores{{EnvID: "env-a", Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: "service", Name: "web", Digest: dg('9')}}
	ignore := updates.Ignores{{EnvID: "env-a", Mode: updates.ModeIgnore, Scope: updates.ScopeWorkload, Kind: "service", Name: "web"}}
	if got := judge(moved, failedWith(dg('9'), false), skip); got.State != updates.StateSkipped || got.Failure != nil {
		t.Errorf("a skipped, failed version = %s %+v, want skipped", got.State, got.Failure)
	}
	if got := judge(moved, failedWith(dg('9'), false), ignore); got.State != updates.StateIgnored || got.Failure != nil {
		t.Errorf("an ignored, failed workload = %s %+v, want ignored", got.State, got.Failure)
	}

	// The failure only replaces an update: a workload that is up to date, or whose check failed, reads what it did.
	same := &updates.Check{Digest: dg('1'), CheckedAt: t0, SuccessAt: t0}
	if got := judge(same, failedWith(dg('1'), false), nil); got.State != updates.StateUpToDate {
		t.Errorf("an up to date workload with an old failure = %s, want up_to_date", got.State)
	}
	checkFailed := &updates.Check{Digest: dg('9'), Error: updates.ReasonRateLimited, CheckedAt: t0, SuccessAt: t0.Add(-time.Minute)}
	if got := judge(checkFailed, failedWith(dg('9'), false), nil); got.State != updates.StateCheckFailed {
		t.Errorf("a failed check with an old failure = %s, want check_failed", got.State)
	}
}

// A failure is about one digest: when the tag points at another, the update is offered again as usual (plan 12-22, D-14).
func TestFailureLiftsOnNewerDigest(t *testing.T) {
	t.Parallel()
	f := &updates.Failure{EnvID: "env-a", Kind: "service", Workload: "web", Digest: dg('9'), Reason: "unhealthy", RunID: "run-1", FailedAt: t0}
	for _, tc := range []struct {
		name   string
		remote string
		state  updates.State
	}{
		{"the digest that failed", dg('9'), updates.StateRolledBack},
		{"a newer digest", dg('8'), updates.StateUpdateAvailable},
		{"the digest it ran again", dg('1'), updates.StateUpToDate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chk := &updates.Check{Digest: tc.remote, CheckedAt: t0, SuccessAt: t0}
			got := updates.Judge(updates.JudgeInput{Workload: held(nil), Check: chk, Now: t0, StaleAfter: time.Hour, Failure: f})
			if got.State != tc.state {
				t.Fatalf("verdict = %s, want %s", got.State, tc.state)
			}
			if tc.state != updates.StateRolledBack && got.Failure != nil {
				t.Errorf("failure = %+v on a %s verdict, want none", got.Failure, got.State)
			}
		})
	}

	// Under another operation's lock the failed version waits like any offered update; the story is kept.
	janitor := &stackops.Holder{Kind: stackops.HolderKindJanitor, RunID: "run-1", Started: t0}
	busy := updates.Judge(updates.JudgeInput{
		Workload: held(nil), Check: &updates.Check{Digest: dg('9'), CheckedAt: t0, SuccessAt: t0}, Now: t0, StaleAfter: time.Hour,
		Failure: f, Holder: janitor,
	})
	if busy.State != updates.StateBusy {
		t.Errorf("a rolled back workload under a lock = %s, want busy", busy.State)
	}
}
