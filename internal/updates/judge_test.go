// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"slices"
	"testing"
	"time"

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
