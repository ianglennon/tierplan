// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

const testPass = "correct horse battery staple"

// testKDF returns the cheapest parameters the caps allow, so tests that open
// many variants stay fast. DefaultKDF is covered separately.
func testKDF(t testing.TB) KDFParams {
	t.Helper()
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	return KDFParams{Name: kdfName, T: 1, MKiB: minKDFMemoryKiB, P: 1, Salt: salt}
}

func testHeader(t testing.TB) Header {
	t.Helper()
	return Header{
		Format:     FormatVersion,
		KDF:        testKDF(t),
		Created:    time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC),
		AppVersion: "0.1.0-test",
		Schema:     12,
	}
}

type entry struct {
	name string
	data []byte
}

func randomBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// seal writes a valid backup with the manifest and a random database of
// dbSize bytes, and returns the file and the entries.
func seal(t testing.TB, h Header, pass string, dbSize int) ([]byte, []entry) {
	t.Helper()
	entries := []entry{
		{EntryManifest, []byte(`{"created":"2026-10-07T12:30:00Z","tables":{"users":3}}`)},
		{EntryDB, randomBytes(t, dbSize)},
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, h, []byte(pass))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, e := range entries {
		if err := w.Add(e.name, int64(len(e.data)), bytes.NewReader(e.data)); err != nil {
			t.Fatalf("Add %s: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes(), entries
}

// sealRaw writes a payload with arbitrary entry names, bypassing the writer's
// own name rule, to build hostile files.
func sealRaw(t testing.TB, h Header, pass string, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := newWriter(&buf, h, []byte(pass))
	if err != nil {
		t.Fatalf("newWriter: %v", err)
	}
	w.anyName = true
	for _, e := range entries {
		if err := w.Add(e.name, int64(len(e.data)), bytes.NewReader(e.data)); err != nil {
			t.Fatalf("Add %s: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// readAll opens file and reads every entry to the end.
func readAll(file []byte, pass string) ([]entry, error) {
	r, err := Open(bytes.NewReader(file), []byte(pass))
	if err != nil {
		return nil, err
	}
	var out []entry
	for {
		name, er, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		b, err := io.ReadAll(er)
		if err != nil {
			return out, err
		}
		out = append(out, entry{name, b})
	}
}

func TestRoundTrip(t *testing.T) {
	h := testHeader(t)
	file, want := seal(t, h, testPass, 1<<20)

	got, err := readAll(file, testPass)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].name != want[i].name || !bytes.Equal(got[i].data, want[i].data) {
			t.Errorf("entry %d (%s) differs from what was written", i, want[i].name)
		}
	}
}

func TestRoundTripDefaultKDF(t *testing.T) {
	kdf, err := DefaultKDF()
	if err != nil {
		t.Fatal(err)
	}
	if kdf.Name != "argon2id" || kdf.T != 3 || kdf.MKiB != 65536 || kdf.P != 4 || len(kdf.Salt) != 16 {
		t.Fatalf("DefaultKDF = %+v", kdf)
	}
	h := testHeader(t)
	h.KDF = kdf
	file, _ := seal(t, h, testPass, 1024)
	if _, err := readAll(file, testPass); err != nil {
		t.Fatalf("readAll with default KDF: %v", err)
	}
}

func TestReadHeaderWithoutPassphrase(t *testing.T) {
	h := testHeader(t)
	file, _ := seal(t, h, testPass, 1024)

	before := kdfRuns.Load()
	got, raw, err := ReadHeader(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if kdfRuns.Load() != before {
		t.Error("ReadHeader ran a key derivation")
	}
	if got.Format != FormatVersion || got.AppVersion != h.AppVersion || got.Schema != h.Schema || !got.Created.Equal(h.Created) {
		t.Errorf("header = %+v, want fields of %+v", got, h)
	}
	if !bytes.Equal(got.KDF.Salt, h.KDF.Salt) || got.KDF.T != h.KDF.T || got.KDF.MKiB != h.KDF.MKiB || got.KDF.P != h.KDF.P {
		t.Errorf("kdf = %+v, want %+v", got.KDF, h.KDF)
	}
	if !bytes.HasPrefix(file, raw) || !bytes.HasPrefix(raw, []byte(Magic)) {
		t.Error("raw header bytes are not the start of the file")
	}
	// The plaintext header must not carry an instance name, URL or counts.
	for _, banned := range []string{"http", "url", "name\":\"bay", "count", "instance"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("plaintext header contains %q", banned)
		}
	}
}

func TestWrongPassphrase(t *testing.T) {
	file, _ := seal(t, testHeader(t), testPass, 4096)
	got, err := readAll(file, "a completely different passphrase")
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable", err)
	}
	if len(got) != 0 {
		t.Errorf("a wrong passphrase returned %d entries", len(got))
	}
}

func TestPassphraseMinimum(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewWriter(&buf, testHeader(t), []byte(strings.Repeat("x", MinPassphraseLen-1))); !errors.Is(err, ErrPassphraseTooShort) {
		t.Errorf("15 characters: err = %v, want ErrPassphraseTooShort", err)
	}
	// Characters, not bytes: 15 two-byte characters are 30 bytes but too short.
	if _, err := NewWriter(&buf, testHeader(t), []byte(strings.Repeat("é", MinPassphraseLen-1))); !errors.Is(err, ErrPassphraseTooShort) {
		t.Errorf("15 multi-byte characters: err = %v, want ErrPassphraseTooShort", err)
	}
	buf.Reset()
	w, err := NewWriter(&buf, testHeader(t), []byte(strings.Repeat("é", MinPassphraseLen)))
	if err != nil {
		t.Fatalf("16 characters: %v", err)
	}
	_ = w.Close() //nolint:errcheck // incomplete on purpose
}

func TestTamperHeaderEveryByte(t *testing.T) {
	file, _ := seal(t, testHeader(t), testPass, 4096)
	_, raw, err := ReadHeader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}

	for _, mask := range []byte{0x01, 0xFF} {
		for i := range raw {
			bad := bytes.Clone(file)
			bad[i] ^= mask
			got, err := readAll(bad, testPass)
			if err == nil {
				t.Fatalf("byte %d ^ %#x: file opened", i, mask)
			}
			if len(got) != 0 {
				t.Fatalf("byte %d ^ %#x: returned %d entries", i, mask, len(got))
			}
			var ok bool
			switch {
			case i < len(Magic):
				ok = errors.Is(err, ErrNotBackup)
			case i < len(Magic)+2:
				ok = errors.Is(err, ErrNewerFormat) || errors.Is(err, ErrUnreadable)
			default:
				ok = errors.Is(err, ErrUnreadable) || errors.Is(err, ErrHeaderLimits)
			}
			if !ok {
				t.Errorf("byte %d ^ %#x: unexpected error %v", i, mask, err)
			}
		}
	}
}

// TestTamperBindsHeader changes a header field that appears nowhere else in the
// file, so only the binding of the header bytes into the wrap key can refuse it.
func TestTamperBindsHeader(t *testing.T) {
	file, _ := seal(t, testHeader(t), testPass, 4096)
	i := bytes.Index(file, []byte(`"app_version":"`))
	if i < 0 {
		t.Fatal("app_version not found in the header")
	}
	i += len(`"app_version":"`)
	bad := bytes.Clone(file)
	bad[i] ^= 0x01 // a valid, different string
	if _, _, err := ReadHeader(bytes.NewReader(bad)); err != nil {
		t.Fatalf("the tampered header must still be well formed: %v", err)
	}
	if _, err := readAll(bad, testPass); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable", err)
	}
}

func TestNewerFormat(t *testing.T) {
	file, _ := seal(t, testHeader(t), testPass, 1024)
	bad := bytes.Clone(file)
	binary.BigEndian.PutUint16(bad[len(Magic):], FormatVersion+1)
	before := kdfRuns.Load()
	if _, err := Open(bytes.NewReader(bad), []byte(testPass)); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("err = %v, want ErrNewerFormat", err)
	}
	if kdfRuns.Load() != before {
		t.Error("a newer format ran a key derivation")
	}
}

func TestNotBackup(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":   nil,
		"short":   []byte("BAYPLAN"),
		"random":  randomBytes(t, 512),
		"sqlite":  append([]byte("SQLite format 3\x00"), randomBytes(t, 512)...),
		"age":     []byte("age-encryption.org/v1\n-> scrypt abc 18\n"),
		"nomagic": []byte(strings.Repeat("A", 100)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Open(bytes.NewReader(in), []byte(testPass)); !errors.Is(err, ErrNotBackup) {
				t.Fatalf("err = %v, want ErrNotBackup", err)
			}
		})
	}
}

func TestTruncated(t *testing.T) {
	file, _ := seal(t, testHeader(t), testPass, 1<<20)
	for name, n := range map[string]int{
		"25%":       len(file) / 4,
		"50%":       len(file) / 2,
		"99%":       len(file) * 99 / 100,
		"last byte": len(file) - 1,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readAll(file[:n], testPass); !errors.Is(err, ErrUnreadable) {
				t.Fatalf("err = %v, want ErrUnreadable", err)
			}
		})
	}
}

func TestTruncatedEveryEarlyOffset(t *testing.T) {
	file, _ := seal(t, testHeader(t), testPass, 2048)
	_, raw, err := ReadHeader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	// Cut at every offset through the outer header and the age header, then at
	// a stride through the payload (each cut that reaches the stream pays for a
	// key derivation): none may open and read to the end.
	for n := len(Magic); n < len(file); n++ {
		if n > len(raw)+400 && n%61 != 0 {
			continue
		}
		if _, err := readAll(file[:n], testPass); err == nil {
			t.Fatalf("truncation at %d of %d (header %d bytes) opened", n, len(file), len(raw))
		}
	}
}

func TestAppended(t *testing.T) {
	file, _ := seal(t, testHeader(t), testPass, 1<<16)
	for name, extra := range map[string][]byte{
		"one zero byte":  {0},
		"one random":     randomBytes(t, 1),
		"a hundred":      randomBytes(t, 100),
		"a full chunk":   randomBytes(t, 64<<10+16),
		"another header": file[:64],
	} {
		t.Run(name, func(t *testing.T) {
			bad := append(bytes.Clone(file), extra...)
			if _, err := readAll(bad, testPass); !errors.Is(err, ErrUnreadable) {
				t.Fatalf("err = %v, want ErrUnreadable", err)
			}
		})
	}
}

func TestPayloadDamage(t *testing.T) {
	file, _ := seal(t, testHeader(t), testPass, 1<<16)
	_, raw, err := ReadHeader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	// Flip bytes through the encrypted stream: every one must be caught.
	for _, i := range []int{len(raw) + 10, len(raw) + 300, len(file) / 2, len(file) - 20, len(file) - 1} {
		bad := bytes.Clone(file)
		bad[i] ^= 0x01
		if _, err := readAll(bad, testPass); !errors.Is(err, ErrUnreadable) {
			t.Errorf("flip at %d: err = %v, want ErrUnreadable", i, err)
		}
	}
}

// outerHeader builds outer header bytes for h with a fixed length field and a
// body of junk after it, so it can be handed to Open without a stream.
func outerHeader(t testing.TB, h Header) []byte {
	t.Helper()
	raw, err := encodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, randomBytes(t, 64)...)
}

func TestLimits(t *testing.T) {
	cases := map[string]func(*Header){
		"memory above the cap": func(h *Header) { h.KDF.MKiB = 1048576 },
		"memory just above":    func(h *Header) { h.KDF.MKiB = maxKDFMemoryKiB + 1 },
		"memory below the min": func(h *Header) { h.KDF.MKiB = 1024 },
		"time above the cap":   func(h *Header) { h.KDF.T = 11 },
		"time zero":            func(h *Header) { h.KDF.T = 0 },
		"threads above":        func(h *Header) { h.KDF.P = 17 },
		"threads zero":         func(h *Header) { h.KDF.P = 0 },
		"short salt":           func(h *Header) { h.KDF.Salt = h.KDF.Salt[:15] },
		"long salt":            func(h *Header) { h.KDF.Salt = append(h.KDF.Salt, 0) },
		"no salt":              func(h *Header) { h.KDF.Salt = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := testHeader(t)
			mutate(&h)
			before := kdfRuns.Load()
			if _, err := Open(bytes.NewReader(outerHeader(t, h)), []byte(testPass)); !errors.Is(err, ErrHeaderLimits) {
				t.Fatalf("Open: err = %v, want ErrHeaderLimits", err)
			}
			if _, _, err := ReadHeader(bytes.NewReader(outerHeader(t, h))); !errors.Is(err, ErrHeaderLimits) {
				t.Fatalf("ReadHeader: err = %v, want ErrHeaderLimits", err)
			}
			if kdfRuns.Load() != before {
				t.Error("a refused header ran a key derivation")
			}
			var buf bytes.Buffer
			if _, err := NewWriter(&buf, h, []byte(testPass)); !errors.Is(err, ErrHeaderLimits) {
				t.Errorf("NewWriter: err = %v, want ErrHeaderLimits", err)
			}
			if buf.Len() != 0 {
				t.Error("NewWriter wrote bytes for a refused header")
			}
		})
	}

	t.Run("header length 4097", func(t *testing.T) {
		raw := []byte(Magic)
		raw = binary.BigEndian.AppendUint16(raw, FormatVersion)
		raw = binary.BigEndian.AppendUint32(raw, maxHeaderLen+1)
		raw = append(raw, bytes.Repeat([]byte(" "), maxHeaderLen+1)...)
		before := kdfRuns.Load()
		if _, err := Open(bytes.NewReader(raw), []byte(testPass)); !errors.Is(err, ErrHeaderLimits) {
			t.Fatalf("err = %v, want ErrHeaderLimits", err)
		}
		if kdfRuns.Load() != before {
			t.Error("a refused header ran a key derivation")
		}
	})

	t.Run("huge header length reads nothing", func(t *testing.T) {
		raw := []byte(Magic)
		raw = binary.BigEndian.AppendUint16(raw, FormatVersion)
		raw = binary.BigEndian.AppendUint32(raw, 0xFFFFFFFF)
		if _, err := Open(bytes.NewReader(raw), []byte(testPass)); !errors.Is(err, ErrHeaderLimits) {
			t.Fatalf("err = %v, want ErrHeaderLimits", err)
		}
	})

	t.Run("unknown kdf", func(t *testing.T) {
		h := testHeader(t)
		h.KDF.Name = "scrypt"
		before := kdfRuns.Load()
		if _, err := Open(bytes.NewReader(outerHeader(t, h)), []byte(testPass)); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("err = %v, want ErrUnreadable", err)
		}
		if kdfRuns.Load() != before {
			t.Error("an unknown kdf ran a key derivation")
		}
	})

	t.Run("caps themselves are accepted", func(t *testing.T) {
		h := testHeader(t)
		h.KDF.T, h.KDF.MKiB, h.KDF.P = maxKDFTime, maxKDFMemoryKiB, maxKDFThreads
		if _, _, err := ReadHeader(bytes.NewReader(outerHeader(t, h))); err != nil {
			t.Fatalf("caps exactly: %v", err)
		}
	})
}

func TestPayloadEntries(t *testing.T) {
	h := testHeader(t)
	manifest := entry{EntryManifest, []byte(`{}`)}
	db := entry{EntryDB, randomBytes(t, 256)}

	cases := map[string][]entry{
		"path traversal first": {{"../etc/passwd", []byte("root")}, db},
		"absolute path":        {{"/etc/passwd", []byte("root")}, db},
		"wrong order":          {db, manifest},
		"only the manifest":    {manifest},
		"no entries":           {},
		"third entry":          {manifest, db, {"extra", []byte("x")}},
		"duplicate manifest":   {manifest, manifest},
		"nested name":          {manifest, {"dir/bayplan.db", db.data}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			file := sealRaw(t, h, testPass, entries)
			got, err := readAll(file, testPass)
			if !errors.Is(err, ErrUnreadable) {
				t.Fatalf("err = %v, want ErrUnreadable (entries %v)", err, got)
			}
			for _, e := range got {
				if e.name != EntryManifest && e.name != EntryDB {
					t.Errorf("an entry named %q was returned", e.name)
				}
			}
		})
	}

	t.Run("a refused name is not read", func(t *testing.T) {
		file := sealRaw(t, h, testPass, []entry{{"../etc/passwd", []byte("root")}, db})
		r, err := Open(bytes.NewReader(file), []byte(testPass))
		if err != nil {
			t.Fatal(err)
		}
		if name, er, err := r.Next(); err == nil || er != nil || name != "" {
			t.Fatalf("Next = %q, %v, %v; want an error and no reader", name, er, err)
		}
		// The failure is sticky.
		if _, _, err := r.Next(); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("second Next: err = %v, want ErrUnreadable", err)
		}
	})
}

func TestEntrySizeCap(t *testing.T) {
	h := testHeader(t)
	file, _ := seal(t, h, testPass, 4096)

	t.Run("declared size above the cap is refused unread", func(t *testing.T) {
		r, err := Open(bytes.NewReader(file), []byte(testPass))
		if err != nil {
			t.Fatal(err)
		}
		r.maxEntry = 16 // the manifest is larger than this
		name, er, err := r.Next()
		if !errors.Is(err, ErrUnreadable) || er != nil || name != "" {
			t.Fatalf("Next = %q, %v, %v; want ErrUnreadable and no reader", name, er, err)
		}
	})

	t.Run("an entry at the cap is accepted", func(t *testing.T) {
		r, err := Open(bytes.NewReader(file), []byte(testPass))
		if err != nil {
			t.Fatal(err)
		}
		r.maxEntry = 4096
		for {
			_, er, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, er); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("writer refuses an oversize entry", func(t *testing.T) {
		var buf bytes.Buffer
		w, err := NewWriter(&buf, h, []byte(testPass))
		if err != nil {
			t.Fatal(err)
		}
		w.maxEntry = 8
		if err := w.Add(EntryManifest, 9, bytes.NewReader(make([]byte, 9))); err == nil {
			t.Error("Add accepted an entry above the cap")
		}
		if err := w.Add(EntryManifest, -1, bytes.NewReader(nil)); err == nil {
			t.Error("Add accepted a negative size")
		}
	})

	t.Run("the cap is the published constant", func(t *testing.T) {
		if MaxEntrySize != 16<<30 {
			t.Errorf("MaxEntrySize = %d", MaxEntrySize)
		}
	})
}

func TestWriterRules(t *testing.T) {
	h := testHeader(t)
	var buf bytes.Buffer
	w, err := NewWriter(&buf, h, []byte(testPass))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(EntryDB, 1, bytes.NewReader([]byte{1})); err == nil {
		t.Error("bayplan.db accepted before manifest.json")
	}
	if err := w.Add("other", 1, bytes.NewReader([]byte{1})); err == nil {
		t.Error("an unknown entry name was accepted")
	}
	if err := w.Add(EntryManifest, 4, bytes.NewReader([]byte{1})); err == nil {
		t.Error("a short entry was accepted")
	}
	if err := w.Close(); err == nil {
		t.Error("Close succeeded on an incomplete backup")
	}
}

func TestKDFSlotsBoundConcurrency(t *testing.T) {
	if cap(kdfSlots) != 2 {
		t.Fatalf("kdfSlots capacity = %d, want 2", cap(kdfSlots))
	}
}
