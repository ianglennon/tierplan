// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"path"
	"strings"
)

// The reasons a bundle is refused. They are stable: the supervisor reports them
// and tests match them.
const (
	// ReasonChecksum means a file's data does not match its manifest checksum.
	ReasonChecksum = "checksum"
	// ReasonMissingFile means a manifest entry has no file.
	ReasonMissingFile = "missing_file"
	// ReasonMissingEntry means a file has no manifest entry.
	ReasonMissingEntry = "missing_entry"
	// ReasonID means the ID does not recompute from the schema and the entries.
	ReasonID = "id"
	// ReasonPath means a path is not a clean relative path under certs/ or the
	// one dynamic file.
	ReasonPath = "path"
	// ReasonDuplicate means a path appears twice.
	ReasonDuplicate = "duplicate"
	// ReasonSchema means the schema is below 1 or the manifest and bundle
	// disagree about it.
	ReasonSchema = "schema"
	// ReasonMode means an entry's mode is not the one its path requires. The ID
	// does not cover the mode, so Verify checks it against the path.
	ReasonMode = "mode"
	// ReasonStep means an entry's write step is not the one its path requires.
	ReasonStep = "step"
)

// VerifyError is why a bundle was refused. Path is the offending path, or empty
// when the fault is not about one file.
type VerifyError struct {
	Path   string
	Reason string
}

func (e *VerifyError) Error() string {
	if e.Path == "" {
		return "ingress bundle refused: " + e.Reason
	}
	return "ingress bundle refused: " + e.Reason + ": " + e.Path
}

// validPath reports whether p is a place a bundle may write: the one dynamic
// file, or a flat file name directly under certs/. A path is relative, clean
// (path.Clean(p) == p), has no dot segment, and a certs/ name uses only letters,
// digits, dot, dash and underscore, so no path can climb out of NodeRoot or
// carry a separator, a space or a line break into the ID's encoding.
func validPath(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || seg == "" {
			return false
		}
	}
	first, rest, ok := strings.Cut(p, "/")
	if !ok {
		return false
	}
	switch first {
	case DynamicDir:
		return p == DynamicFile
	case CertsDir:
		if strings.Contains(rest, "/") {
			return false
		}
		for _, r := range rest {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			default:
				return false
			}
		}
		return true
	}
	return false
}

// Verify checks the bundle the way the supervisor does before it writes
// anything. The order is fixed: the schema; each manifest entry's path,
// uniqueness, step and mode; each file's path and uniqueness; the entries and
// files matching one to one; each file's checksum; and last the ID. The first
// failure is returned as a *VerifyError.
func (b Bundle) Verify() error {
	if b.Schema < Schema1 || b.Manifest.Schema != b.Schema {
		return &VerifyError{Reason: ReasonSchema}
	}
	entries := make(map[string]FileEntry, len(b.Manifest.Files))
	for _, e := range b.Manifest.Files {
		if !validPath(e.Path) {
			return &VerifyError{Path: e.Path, Reason: ReasonPath}
		}
		if _, dup := entries[e.Path]; dup {
			return &VerifyError{Path: e.Path, Reason: ReasonDuplicate}
		}
		if e.Step != stepFor(e.Path) {
			return &VerifyError{Path: e.Path, Reason: ReasonStep}
		}
		if e.Mode != modeFor(e.Path) {
			return &VerifyError{Path: e.Path, Reason: ReasonMode}
		}
		entries[e.Path] = e
	}
	files := make(map[string]File, len(b.Files))
	for _, f := range b.Files {
		if !validPath(f.Path) {
			return &VerifyError{Path: f.Path, Reason: ReasonPath}
		}
		if _, dup := files[f.Path]; dup {
			return &VerifyError{Path: f.Path, Reason: ReasonDuplicate}
		}
		files[f.Path] = f
	}
	for _, e := range b.Manifest.Files {
		if _, ok := files[e.Path]; !ok {
			return &VerifyError{Path: e.Path, Reason: ReasonMissingFile}
		}
	}
	for _, f := range b.Files {
		if _, ok := entries[f.Path]; !ok {
			return &VerifyError{Path: f.Path, Reason: ReasonMissingEntry}
		}
	}
	for _, f := range b.Files {
		if Checksum(f.Data) != entries[f.Path].SHA256 {
			return &VerifyError{Path: f.Path, Reason: ReasonChecksum}
		}
	}
	if ComputeID(b.Schema, b.Manifest.Files) != b.ID {
		return &VerifyError{Reason: ReasonID}
	}
	return nil
}
