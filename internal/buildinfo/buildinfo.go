// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package buildinfo reports which Bayplan build is running and where its
// source lives (AGPL section 13, D-61, D-62).
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// These are set at link time:
//
//	-ldflags "-X github.com/ianglennon/bayplan/internal/buildinfo.Version=v0.1.0
//	          -X github.com/ianglennon/bayplan/internal/buildinfo.Commit=<full sha>
//	          -X github.com/ianglennon/bayplan/internal/buildinfo.SourceURL=<fork url>"
var (
	Version   = ""
	Commit    = ""
	SourceURL = "https://github.com/ianglennon/bayplan"
)

// Info is the resolved build identity served at /api/version.
type Info struct {
	Version       string
	Commit        string
	ShortCommit   string
	Dirty         bool
	Display       string
	SourceURL     string
	SourceTreeURL string
}

// Get resolves the build identity from the link-time variables, falling back
// to the VCS data Go embeds in the binary when no commit was injected.
func Get() Info {
	rev, modified := "", false
	if Commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					rev = s.Value
				case "vcs.modified":
					modified = s.Value == "true"
				}
			}
		}
	}
	return resolve(Version, Commit, SourceURL, rev, modified)
}

// resolve is the pure core of Get so every case is table-testable.
func resolve(version, commit, sourceURL, vcsRevision string, vcsModified bool) Info {
	src := strings.TrimRight(strings.TrimSpace(sourceURL), "/")
	if src == "" {
		src = "https://github.com/ianglennon/bayplan"
	}

	full := strings.TrimSpace(commit)
	dirty := false
	if full == "" {
		full = strings.TrimSpace(vcsRevision)
		dirty = vcsModified
	}

	short := full
	if len(short) > 7 {
		short = short[:7]
	}

	label := "unknown"
	if short != "" {
		label = short
		if dirty {
			label += "-dirty"
		}
	}

	name := strings.TrimSpace(version)
	if name == "" {
		name = "dev"
	}
	// Release tooling commonly strips the v from a tag. The footer,
	// /api/version and `bayplan version` must read "v0.1.0 (abc1234)" whatever
	// packaged the binary (WR-13, D-61).
	if name[0] >= '0' && name[0] <= '9' {
		name = "v" + name
	}

	tree := src
	if full != "" {
		tree = src + "/tree/" + full
	}

	return Info{
		Version:       name,
		Commit:        full,
		ShortCommit:   short,
		Dirty:         dirty,
		Display:       name + " (" + label + ")",
		SourceURL:     src,
		SourceTreeURL: tree,
	}
}
