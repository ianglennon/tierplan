// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"slices"
	"testing"
)

// Fake material: none of these is a real certificate or key.
const (
	chainPath = CertsDir + "/aaaa.crt"
	keyPath   = CertsDir + "/aaaa.key"
)

func sampleFiles() []File {
	return []File{
		{Path: DynamicFile, Data: []byte("http:\n  routers: {}\n")},
		{Path: keyPath, Data: []byte("fake key bytes")},
		{Path: chainPath, Data: []byte("fake chain bytes")},
	}
}

func TestBuildVerifiesAndDerivesTheID(t *testing.T) {
	files := sampleFiles()
	b, err := Build(Schema1, Version{Config: 3, Certs: 2}, files, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b.Schema != Schema1 || b.Manifest.Schema != Schema1 {
		t.Fatalf("schema = %d / %d, want %d", b.Schema, b.Manifest.Schema, Schema1)
	}
	if b.Version != (Version{Config: 3, Certs: 2}) {
		t.Fatalf("version = %+v", b.Version)
	}
	want := []FileEntry{
		{Path: chainPath, SHA256: Checksum([]byte("fake chain bytes")), Mode: 0o644, Step: StepCerts},
		{Path: keyPath, SHA256: Checksum([]byte("fake key bytes")), Mode: 0o600, Step: StepCerts},
		{Path: DynamicFile, SHA256: Checksum([]byte("http:\n  routers: {}\n")), Mode: 0o644, Step: StepDynamic},
	}
	if !slices.Equal(b.Manifest.Files, want) {
		t.Fatalf("manifest files = %+v\nwant %+v", b.Manifest.Files, want)
	}
	if got := b.ID; got != ComputeID(Schema1, b.Manifest.Files) {
		t.Fatalf("ID = %s, want ComputeID over the entries", got)
	}
	if len(b.Files) != 3 {
		t.Fatalf("files = %d, want 3", len(b.Files))
	}
	if err := b.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Build does not reorder the caller's slice.
	if files[0].Path != DynamicFile {
		t.Fatalf("Build reordered the input: %s first", files[0].Path)
	}
}
