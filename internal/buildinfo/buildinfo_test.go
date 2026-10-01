// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package buildinfo

import "testing"

const repo = "https://github.com/ianglennon/bayplan"

func TestResolve(t *testing.T) {
	full := "abc1234def5678abc1234def5678abc1234def56"
	tests := []struct {
		name        string
		version     string
		commit      string
		source      string
		vcsRev      string
		vcsModified bool
		wantDisplay string
		wantTree    string
		wantDirty   bool
	}{
		{
			name: "release build from ldflags", version: "v0.1.0", commit: full, source: repo,
			wantDisplay: "v0.1.0 (abc1234)", wantTree: repo + "/tree/" + full,
		},
		{
			name: "dev build clean from vcs info", source: repo, vcsRev: full,
			wantDisplay: "dev (abc1234)", wantTree: repo + "/tree/" + full,
		},
		{
			name: "dev build dirty from vcs info", source: repo, vcsRev: full, vcsModified: true,
			wantDisplay: "dev (abc1234-dirty)", wantTree: repo + "/tree/" + full, wantDirty: true,
		},
		{
			name: "no info at all", source: repo,
			wantDisplay: "dev (unknown)", wantTree: repo,
		},
		{
			name: "fork source url with trailing slash", version: "v0.1.0", commit: full, source: "https://example.org/fork/bayplan/",
			wantDisplay: "v0.1.0 (abc1234)", wantTree: "https://example.org/fork/bayplan/tree/" + full,
		},
		{
			name: "empty source url falls back to default", source: "",
			wantDisplay: "dev (unknown)", wantTree: repo,
		},
		{
			name: "ldflags commit wins over vcs info", commit: full, source: repo, vcsRev: "ffffffffffff", vcsModified: true,
			wantDisplay: "dev (abc1234)", wantTree: repo + "/tree/" + full,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.version, tc.commit, tc.source, tc.vcsRev, tc.vcsModified)
			if got.Display != tc.wantDisplay {
				t.Errorf("Display = %q, want %q", got.Display, tc.wantDisplay)
			}
			if got.SourceTreeURL != tc.wantTree {
				t.Errorf("SourceTreeURL = %q, want %q", got.SourceTreeURL, tc.wantTree)
			}
			if got.Dirty != tc.wantDirty {
				t.Errorf("Dirty = %v, want %v", got.Dirty, tc.wantDirty)
			}
		})
	}
}

func TestGetNeverEmpty(t *testing.T) {
	got := Get()
	if got.Display == "" || got.SourceURL == "" || got.SourceTreeURL == "" {
		t.Fatalf("Get() returned empty fields: %+v", got)
	}
}
