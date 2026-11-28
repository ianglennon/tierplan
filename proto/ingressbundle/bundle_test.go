// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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
	b, err := Build(Schema2, Version{Config: 3, Certs: 2}, files, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b.Schema != Schema2 || b.Manifest.Schema != Schema2 {
		t.Fatalf("schema = %d / %d, want %d", b.Schema, b.Manifest.Schema, Schema2)
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
	if got := b.ID; got != ComputeID(Schema2, b.Manifest.Files) {
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
	base := mustBuild(t, Schema2, Version{Config: 1}, sampleFiles())

	t.Run("the same files twice give the same ID", func(t *testing.T) {
		again := mustBuild(t, Schema2, Version{Config: 1}, sampleFiles())
		if again.ID != base.ID {
			t.Fatalf("IDs differ: %s vs %s", base.ID, again.ID)
		}
	})

	t.Run("the input order does not matter", func(t *testing.T) {
		files := sampleFiles()
		slices.Reverse(files)
		if got := mustBuild(t, Schema2, Version{Config: 1}, files).ID; got != base.ID {
			t.Fatalf("reversed input gave %s, want %s", got, base.ID)
		}
	})

	for i, name := range []string{"the YAML", "the key", "the chain"} {
		t.Run("one byte changed in "+name, func(t *testing.T) {
			files := sampleFiles()
			// Replace, never edit in place: the fixtures share nothing, but the
			// copy keeps the intent plain.
			files[i].Data = append([]byte("x"), files[i].Data[1:]...)
			if got := mustBuild(t, Schema2, Version{Config: 1}, files).ID; got == base.ID {
				t.Fatalf("ID did not change when one byte of %s changed", name)
			}
		})
	}

	t.Run("the version alone does not change the ID", func(t *testing.T) {
		got := mustBuild(t, Schema2, Version{Config: 9, Certs: 4, Own: 2}, sampleFiles())
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
		if got := mustBuild(t, Schema2, Version{Config: 1}, files).ID; got == base.ID {
			t.Fatal("ID did not change when a file was renamed")
		}
	})
}

// idVector is the bundle ID of schema 1 (the old schema, kept as a historic vector: the ID encoding did
// not change when schema 2 was added) over certs/a.crt holding "a" and
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

// clone copies a bundle deeply enough that a test may change it freely.
func clone(b Bundle) Bundle {
	c := b
	c.Files = make([]File, len(b.Files))
	for i, f := range b.Files {
		c.Files[i] = File{Path: f.Path, Data: slices.Clone(f.Data)}
	}
	c.Manifest.Files = slices.Clone(b.Manifest.Files)
	c.Manifest.Routes = slices.Clone(b.Manifest.Routes)
	return c
}

// rename moves one file, in the manifest and in the file list, to another path.
func rename(b Bundle, from, to string) Bundle {
	for i := range b.Files {
		if b.Files[i].Path == from {
			b.Files[i].Path = to
		}
	}
	for i := range b.Manifest.Files {
		if b.Manifest.Files[i].Path == from {
			b.Manifest.Files[i].Path = to
		}
	}
	return b
}

func TestVerifyRefuses(t *testing.T) {
	valid := mustBuild(t, Schema2, Version{Config: 1}, sampleFiles())
	if err := valid.Verify(); err != nil {
		t.Fatalf("the base bundle must verify: %v", err)
	}

	rows := []struct {
		name   string
		mutate func(b Bundle) Bundle
		path   string
		reason string
	}{
		{"a file whose data was edited", func(b Bundle) Bundle {
			b.Files[0].Data[0] ^= 0xff
			return b
		}, chainPath, ReasonChecksum},
		{"a manifest entry with no file", func(b Bundle) Bundle {
			b.Files = slices.DeleteFunc(b.Files, func(f File) bool { return f.Path == keyPath })
			return b
		}, keyPath, ReasonMissingFile},
		{"a file with no manifest entry", func(b Bundle) Bundle {
			b.Manifest.Files = slices.DeleteFunc(b.Manifest.Files, func(e FileEntry) bool { return e.Path == keyPath })
			return b
		}, keyPath, ReasonMissingEntry},
		{"an ID that does not recompute", func(b Bundle) Bundle {
			b.ID = strings.Repeat("0", len(b.ID))
			return b
		}, "", ReasonID},
		{"an absolute path", func(b Bundle) Bundle { return rename(b, chainPath, "/etc/passwd") }, "/etc/passwd", ReasonPath},
		{"an absolute path under the node root", func(b Bundle) Bundle {
			return rename(b, chainPath, NodeRoot+"/"+chainPath)
		}, NodeRoot + "/" + chainPath, ReasonPath},
		{"a parent segment", func(b Bundle) Bundle { return rename(b, chainPath, "certs/../x") }, "certs/../x", ReasonPath},
		{"a parent segment that climbs out", func(b Bundle) Bundle { return rename(b, chainPath, "../etc/x") }, "../etc/x", ReasonPath},
		{"a dot segment", func(b Bundle) Bundle { return rename(b, chainPath, "certs/./a.crt") }, "certs/./a.crt", ReasonPath},
		{"an unclean path with a double slash", func(b Bundle) Bundle { return rename(b, chainPath, "certs//a.crt") }, "certs//a.crt", ReasonPath},
		{"an unclean path with a trailing slash", func(b Bundle) Bundle { return rename(b, chainPath, "certs/a.crt/") }, "certs/a.crt/", ReasonPath},
		{"a path outside dynamic and certs", func(b Bundle) Bundle { return rename(b, chainPath, "other/a") }, "other/a", ReasonPath},
		{"a bare file name", func(b Bundle) Bundle { return rename(b, chainPath, "a.crt") }, "a.crt", ReasonPath},
		{"a dynamic file other than tierplan.yml", func(b Bundle) Bundle {
			return rename(b, DynamicFile, "dynamic/extra.yml")
		}, "dynamic/extra.yml", ReasonPath},
		{"a nested path under certs", func(b Bundle) Bundle { return rename(b, chainPath, "certs/sub/a.crt") }, "certs/sub/a.crt", ReasonPath},
		{"a line break in a name", func(b Bundle) Bundle { return rename(b, chainPath, "certs/a\nb.crt") }, "certs/a\nb.crt", ReasonPath},
		{"a space in a name", func(b Bundle) Bundle { return rename(b, chainPath, "certs/a b.crt") }, "certs/a b.crt", ReasonPath},
		{"a duplicate manifest entry", func(b Bundle) Bundle {
			b.Manifest.Files = append(b.Manifest.Files, b.Manifest.Files[0])
			return b
		}, chainPath, ReasonDuplicate},
		{"a duplicate file", func(b Bundle) Bundle {
			b.Files = append(b.Files, File{Path: keyPath, Data: []byte("other")})
			return b
		}, keyPath, ReasonDuplicate},
		{"a schema of zero", func(b Bundle) Bundle {
			b.Schema, b.Manifest.Schema = 0, 0
			return b
		}, "", ReasonSchema},
		{"a negative schema", func(b Bundle) Bundle {
			b.Schema, b.Manifest.Schema = -1, -1
			return b
		}, "", ReasonSchema},
		{"a manifest that disagrees about the schema", func(b Bundle) Bundle {
			b.Manifest.Schema = Schema1 // the old schema: the manifest disagrees with the bundle's schema 2
			return b
		}, "", ReasonSchema},
		{"a key with the mode of a certificate", func(b Bundle) Bundle {
			for i := range b.Manifest.Files {
				if b.Manifest.Files[i].Path == keyPath {
					b.Manifest.Files[i].Mode = ModeConfig
				}
			}
			return b
		}, keyPath, ReasonMode},
		{"a certificate written in the dynamic step", func(b Bundle) Bundle {
			for i := range b.Manifest.Files {
				if b.Manifest.Files[i].Path == chainPath {
					b.Manifest.Files[i].Step = StepDynamic
				}
			}
			return b
		}, chainPath, ReasonStep},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := clone(valid).Verify()
			if err != nil {
				t.Fatalf("clone must verify: %v", err)
			}
			err = r.mutate(clone(valid)).Verify()
			var ve *VerifyError
			if !errors.As(err, &ve) {
				t.Fatalf("Verify = %v, want a *VerifyError", err)
			}
			if ve.Reason != r.reason || ve.Path != r.path {
				t.Fatalf("refused as %q at %q, want %q at %q", ve.Reason, ve.Path, r.reason, r.path)
			}
		})
	}

	t.Run("Build refuses what Verify refuses", func(t *testing.T) {
		for name, tc := range map[string]struct {
			schema int
			files  []File
			reason string
		}{
			"a schema below 1": {0, sampleFiles(), ReasonSchema},
			"a path outside":   {Schema2, []File{{Path: "other/a", Data: []byte("x")}}, ReasonPath},
			"an absolute path": {Schema2, []File{{Path: "/etc/passwd", Data: []byte("x")}}, ReasonPath},
			"a duplicate path": {Schema2, []File{{Path: chainPath}, {Path: chainPath}}, ReasonDuplicate},
		} {
			_, err := Build(tc.schema, Version{}, tc.files, nil)
			var ve *VerifyError
			if !errors.As(err, &ve) || ve.Reason != tc.reason {
				t.Errorf("%s: Build = %v, want a refusal for %q", name, err, tc.reason)
			}
		}
	})
}

const pem = "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE\n-----END PRIVATE KEY-----\n"

func TestBundleRedactsFileContents(t *testing.T) {
	files := []File{
		{Path: chainPath, Data: []byte("-----BEGIN CERTIFICATE-----\nFAKECHAIN\n-----END CERTIFICATE-----\n")},
		{Path: keyPath, Data: []byte(pem)},
		{Path: DynamicFile, Data: []byte("http: {}\n")},
	}
	b := mustBuild(t, Schema2, Version{Config: 7, Certs: 5, Own: 3}, files)
	// Guard the guard: the data really is in the bundle.
	if !strings.Contains(string(b.Files[1].Data), "PRIVATE KEY") {
		t.Fatal("the fixture has no key data")
	}

	verbs := []string{"%v", "%+v", "%#v", "%s"}
	subjects := map[string]any{"Bundle": b, "*Bundle": &b, "File": b.Files[1], "*File": &b.Files[1]}
	for name, subject := range subjects {
		for _, verb := range verbs {
			out := fmt.Sprintf(verb, subject)
			if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, "FAKEKEY") || strings.Contains(out, "FAKECHAIN") {
				t.Errorf("%s %s printed file data: %s", verb, name, out)
			}
		}
	}

	for _, verb := range verbs[:3] {
		out := fmt.Sprintf(verb, b)
		if !strings.Contains(out, b.ID[:12]) {
			t.Errorf("%s of a Bundle lacks the ID prefix: %s", verb, out)
		}
		for _, f := range b.Files {
			if !strings.Contains(out, f.Path) {
				t.Errorf("%s of a Bundle lacks the path %s: %s", verb, f.Path, out)
			}
		}
		if !strings.Contains(out, "7/5/3") || !strings.Contains(out, fmt.Sprintf("schema %d", Schema2)) {
			t.Errorf("%s of a Bundle lacks the schema or version: %s", verb, out)
		}
	}
	if got := fmt.Sprintf("%v", b.Files[1]); !strings.Contains(got, keyPath) || !strings.Contains(got, fmt.Sprint(len(pem))) {
		t.Errorf("a File prints the path and the length, got %q", got)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("applying", "bundle", b)
	log.Info("applying file", "file", b.Files[1])
	out := buf.String()
	if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, "FAKECHAIN") {
		t.Fatalf("a slog record carried file data: %s", out)
	}
	// Base64 of the data would also be a leak.
	for _, f := range b.Files {
		if strings.Contains(out, base64.StdEncoding.EncodeToString(f.Data)) {
			t.Fatalf("a slog record carried %s as base64: %s", f.Path, out)
		}
	}
	for _, want := range []string{b.ID, chainPath, keyPath, DynamicFile} {
		if !strings.Contains(out, want) {
			t.Errorf("the slog record lacks %q: %s", want, out)
		}
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &first); err != nil {
		t.Fatalf("the slog record is not JSON: %v", err)
	}
	group, ok := first["bundle"].(map[string]any)
	if !ok || group["id"] != b.ID || group["schema"] != float64(Schema2) {
		t.Fatalf("the bundle attribute is not a group of id and schema: %v", first["bundle"])
	}
}
