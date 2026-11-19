// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"crypto/sha256"
	"encoding/hex"
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

func mustBuild(t *testing.T, schema int, v Version, files []File) Bundle {
	t.Helper()
	b, err := Build(schema, v, files, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return b
}

func TestIDChangesExactlyWithContent(t *testing.T) {
	base := mustBuild(t, Schema1, Version{Config: 1}, sampleFiles())

	t.Run("the same files twice give the same ID", func(t *testing.T) {
		again := mustBuild(t, Schema1, Version{Config: 1}, sampleFiles())
		if again.ID != base.ID {
			t.Fatalf("IDs differ: %s vs %s", base.ID, again.ID)
		}
	})

	t.Run("the input order does not matter", func(t *testing.T) {
		files := sampleFiles()
		slices.Reverse(files)
		if got := mustBuild(t, Schema1, Version{Config: 1}, files).ID; got != base.ID {
			t.Fatalf("reversed input gave %s, want %s", got, base.ID)
		}
	})

	for i, name := range []string{"the YAML", "the key", "the chain"} {
		t.Run("one byte changed in "+name, func(t *testing.T) {
			files := sampleFiles()
			// Replace, never edit in place: the fixtures share nothing, but the
			// copy keeps the intent plain.
			files[i].Data = append([]byte("x"), files[i].Data[1:]...)
			if got := mustBuild(t, Schema1, Version{Config: 1}, files).ID; got == base.ID {
				t.Fatalf("ID did not change when one byte of %s changed", name)
			}
		})
	}

	t.Run("the version alone does not change the ID", func(t *testing.T) {
		got := mustBuild(t, Schema1, Version{Config: 9, Certs: 4, Own: 2}, sampleFiles())
		if got.ID != base.ID {
			t.Fatalf("ID changed with the version: %s vs %s", got.ID, base.ID)
		}
		if got.Version == base.Version {
			t.Fatal("the version was not carried")
		}
	})

	t.Run("another schema gives another ID", func(t *testing.T) {
		v1 := ComputeID(1, base.Manifest.Files)
		v2 := ComputeID(2, base.Manifest.Files)
		if v1 == v2 {
			t.Fatalf("schema 1 and 2 share the ID %s", v1)
		}
		b2 := mustBuild(t, 2, Version{}, sampleFiles())
		if b2.Schema != 2 || b2.ID != v2 {
			t.Fatalf("a schema 2 bundle: schema %d ID %s, want 2 and %s", b2.Schema, b2.ID, v2)
		}
	})

	t.Run("a renamed file gives another ID", func(t *testing.T) {
		files := sampleFiles()
		files[2].Path = CertsDir + "/bbbb.crt"
		if got := mustBuild(t, Schema1, Version{Config: 1}, files).ID; got == base.ID {
			t.Fatal("ID did not change when a file was renamed")
		}
	})
}

// idVector is the bundle ID of schema 1 over certs/a.crt holding "a" and
// dynamic/tierplan.yml holding "b". It is the SHA-256 of the literal bytes in
// TestComputeIDVector, computed outside this program with sha256sum. If this
// constant has to change, the ID encoding changed and that needs schema 2 (D-05).
const idVector = "b546ed91e972e062dc3149ee102fcd14294271886f90211e161308c9a397f312"

func TestComputeIDVector(t *testing.T) {
	entries := []FileEntry{
		// Out of order on purpose: the ID sorts by path.
		{Path: DynamicFile, SHA256: Checksum([]byte("b"))},
		{Path: CertsDir + "/a.crt", SHA256: Checksum([]byte("a"))},
	}
	const aSum = "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"
	const bSum = "3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d"
	if Checksum([]byte("a")) != aSum || Checksum([]byte("b")) != bSum {
		t.Fatal("Checksum is not the lower-case hex SHA-256")
	}
	literal := "tierplan-ingress-bundle\n" +
		"schema 1\n" +
		"certs/a.crt " + aSum + "\n" +
		"dynamic/tierplan.yml " + bSum + "\n"
	sum := sha256.Sum256([]byte(literal))
	if indep := hex.EncodeToString(sum[:]); indep != idVector {
		t.Fatalf("the literal encoding hashes to %s, not the pinned %s", indep, idVector)
	}
	if got := ComputeID(1, entries); got != idVector {
		t.Fatalf("ComputeID = %s, want %s", got, idVector)
	}
}
