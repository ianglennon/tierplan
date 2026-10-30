// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package buildinfo

import "testing"

const repo = "https://github.com/ianglennon/coaming"

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
		wantVersion string
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
			name: "fork source url with trailing slash", version: "v0.1.0", commit: full, source: "https://example.org/fork/coaming/",
			wantDisplay: "v0.1.0 (abc1234)", wantTree: "https://example.org/fork/coaming/tree/" + full,
		},
		{
			name: "empty source url falls back to default", source: "",
			wantDisplay: "dev (unknown)", wantTree: repo,
		},
		{
			name: "release tooling that strips the v is shown with it", version: "0.1.0", commit: full, source: repo,
			wantDisplay: "v0.1.0 (abc1234)", wantTree: repo + "/tree/" + full, wantVersion: "v0.1.0",
		},
		{
			name: "prerelease without a v is shown with it", version: "1.2.3-rc.1", commit: full, source: repo,
			wantDisplay: "v1.2.3-rc.1 (abc1234)", wantTree: repo + "/tree/" + full, wantVersion: "v1.2.3-rc.1",
		},
		{
			name: "snapshot suffix keeps the v", version: "0.1.1-SNAPSHOT-abc1234", commit: full, source: repo,
			wantDisplay: "v0.1.1-SNAPSHOT-abc1234 (abc1234)", wantTree: repo + "/tree/" + full, wantVersion: "v0.1.1-SNAPSHOT-abc1234",
		},
		{
			name: "edge is not a number and is unchanged", version: "edge", commit: full, source: repo,
			wantDisplay: "edge (abc1234)", wantTree: repo + "/tree/" + full, wantVersion: "edge",
		},
		{
			name: "dev is unchanged", version: "dev", commit: full, source: repo,
			wantDisplay: "dev (abc1234)", wantTree: repo + "/tree/" + full, wantVersion: "dev",
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
			if tc.wantVersion != "" && got.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVersion)
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
