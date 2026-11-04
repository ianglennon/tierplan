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
