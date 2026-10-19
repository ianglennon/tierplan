// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// archiveModTime is the modification time of every test tar: a fixed instant, so a test body is
// the same bytes on every run.
var archiveModTime = time.Unix(1_700_000_000, 0)

// sdkArchive builds the tar the Compose SDK's createTar builds for one injected secret or config:
// one header with the container path as its name, the content's length, the mode and an owner, then
// the content, then the end-of-archive blocks.
func sdkArchive(t testing.TB, name, content string, mode int64) []byte {
	t.Helper()
	return tarOf(t, tarEntry{hdr: tar.Header{Name: name, Size: int64(len(content)), Mode: mode, ModTime: archiveModTime}, content: content})
}

type tarEntry struct {
	hdr     tar.Header
	content string
}

// tarOf writes the entries as a tar, with the end-of-archive blocks.
func tarOf(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, e := range entries {
		h := e.hdr
		if err := w.WriteHeader(&h); err != nil {
			t.Fatalf("write the header of %q: %v", h.Name, err)
		}
		if _, err := w.Write([]byte(e.content)); err != nil {
			t.Fatalf("write the content of %q: %v", h.Name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// resealed sets the link name of the first header of a tar and writes the header's checksum again,
// for the one malformed header archive/tar's writer will not produce.
func resealed(raw []byte, linkname string) []byte {
	out := bytes.Clone(raw)
	copy(out[157:257], make([]byte, 100))
	copy(out[157:], linkname)
	copy(out[148:156], "        ")
	sum := 0
	for _, c := range out[:512] {
		sum += int(c)
	}
	copy(out[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return out
}

func TestValidateArchive(t *testing.T) {
	big := func(n int) string { return strings.Repeat("x", n) }
	twelve := "s3cret-value"
	if len(twelve) != 12 {
		t.Fatal("the secret in the SDK case is 12 bytes")
	}
	zeros := func(n int) []byte { return make([]byte, n) }
	withTrailer := func(raw []byte, extra ...byte) []byte { return append(bytes.Clone(raw), extra...) }

	for _, tc := range []struct {
		name string
		raw  []byte
		want string // "" means accepted; otherwise a word the refusal names
	}{
		{"the tar the SDK builds for a secret, as the SDK names the file", sdkArchive(t, "/run/secrets/tok", twelve, 0o444), ""},
		{"the same file named without the leading slash", sdkArchive(t, "run/secrets/tok", twelve, 0o444), ""},
		{"a config in the root of the container", sdkArchive(t, "/app.conf", "a=1\n", 0o644), ""},
		{"an executable that is not setuid", sdkArchive(t, "/usr/local/bin/x", "#!/bin/sh\n", 0o755), ""},
		{"an empty file", sdkArchive(t, "/run/secrets/empty", "", 0o444), ""},
		{"a file of exactly 1,000 KB", sdkArchive(t, "/big", big(maxArchiveContent), 0o444), ""},
		{"zero padding after the end of the archive", withTrailer(sdkArchive(t, "/a", "x", 0o444), zeros(10240)...), ""},

		{"a file of 1,000,001 bytes", sdkArchive(t, "/big", big(maxArchiveContent+1), 0o444), "over"},
		{"garbage after the two zero blocks", withTrailer(sdkArchive(t, "/a", "x", 0o444), []byte("junk")...), "after its end"},
		{"a second archive after the end blocks", withTrailer(sdkArchive(t, "/a", "x", 0o444), sdkArchive(t, "/b", "y", 0o444)...), "after its end"},
		{"a nonzero byte hidden in the padding", withTrailer(sdkArchive(t, "/a", "x", 0o444), append(zeros(100), 1)...), "after its end"},
		{"a second entry", tarOf(t,
			tarEntry{hdr: tar.Header{Name: "/a", Size: 1, Mode: 0o444, ModTime: archiveModTime}, content: "x"},
			tarEntry{hdr: tar.Header{Name: "/b", Size: 1, Mode: 0o444, ModTime: archiveModTime}, content: "y"},
		), "second entry"},
		{"no entry", tarOf(t), "no entry"},
		{"no bytes", nil, "no entry"},
		{"not a tar", []byte("hello, this is not a tar archive at all"), "not a tar"},
		{"a header cut short", sdkArchive(t, "/a", "x", 0o444)[:300], "not a tar"},
		{"content cut short", sdkArchive(t, "/a", big(2000), 0o444)[:512+100], "cannot be read"},

		{"a symlink", tarOf(t, tarEntry{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "/run/secrets/tok", Linkname: "/etc/shadow", Mode: 0o777, ModTime: archiveModTime}}), "not a regular file"},
		{"a hard link", tarOf(t, tarEntry{hdr: tar.Header{Typeflag: tar.TypeLink, Name: "/run/secrets/tok", Linkname: "/etc/shadow", Mode: 0o644, ModTime: archiveModTime}}), "not a regular file"},
		{"a character device", tarOf(t, tarEntry{hdr: tar.Header{Typeflag: tar.TypeChar, Name: "/dev/x", Devmajor: 1, Devminor: 3, Mode: 0o666, ModTime: archiveModTime}}), "not a regular file"},
		{"a block device", tarOf(t, tarEntry{hdr: tar.Header{Typeflag: tar.TypeBlock, Name: "/dev/y", Devmajor: 8, Devminor: 0, Mode: 0o660, ModTime: archiveModTime}}), "not a regular file"},
		{"a FIFO", tarOf(t, tarEntry{hdr: tar.Header{Typeflag: tar.TypeFifo, Name: "/run/f", Mode: 0o644, ModTime: archiveModTime}}), "not a regular file"},
		{"a directory entry", tarOf(t, tarEntry{hdr: tar.Header{Typeflag: tar.TypeDir, Name: "/run/secrets/", Mode: 0o755, ModTime: archiveModTime}}), "not a regular file"},
		{"a regular file that names a link target", resealed(sdkArchive(t, "/run/secrets/tok", twelve, 0o444), "/etc/shadow"), "link target"},

		{"a dot-dot first", sdkArchive(t, "../etc/passwd", "x", 0o444), ".. segment"},
		{"a dot-dot after the slash", sdkArchive(t, "/../etc/passwd", "x", 0o444), "clean"},
		{"a dot-dot in the middle", sdkArchive(t, "/run/../etc/passwd", "x", 0o444), "clean"},
		{"a dot-dot that stays inside", sdkArchive(t, "run/secrets/../tok", "x", 0o444), "clean"},
		{"a name that is only dot-dot", sdkArchive(t, "..", "x", 0o444), ".. segment"},
		{"two leading slashes", sdkArchive(t, "//etc/passwd", "x", 0o444), "clean"},
		{"a dot segment", sdkArchive(t, "./run/tok", "x", 0o444), "clean"},
		{"a double slash inside", sdkArchive(t, "/run//tok", "x", 0o444), "clean"},
		{"a trailing slash (the writer makes it a directory)", sdkArchive(t, "/run/tok/", "", 0o444), "not a regular file"},
		{"the root (the writer makes it a directory)", sdkArchive(t, "/", "", 0o444), "not a regular file"},
		{"a dot", sdkArchive(t, ".", "x", 0o444), "clean"},
		{"an empty name", sdkArchive(t, "", "x", 0o444), "empty"},

		{"a setuid mode", sdkArchive(t, "/bin/x", "x", 0o4755), "setuid"},
		{"a setgid mode", sdkArchive(t, "/bin/x", "x", 0o2755), "setuid"},
		{"a sticky mode", sdkArchive(t, "/tmp/x", "x", 0o1777), "setuid"},
		{"the file type bits in the mode", sdkArchive(t, "/bin/x", "x", 0o100644), "setuid"},

		{"an extended attribute", tarOf(t, tarEntry{hdr: tar.Header{
			Name: "/bin/x", Size: 1, Mode: 0o755, ModTime: archiveModTime, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "\x01"},
		}, content: "x"}), "extended"},
		// A name no ustar header can hold travels in an extended record, which is refused whole.
		{"a name that needs an extended path record", sdkArchive(t, "/"+strings.Repeat("a", 150), "x", 0o444), "extended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArchive(tc.raw)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateArchive refused it: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateArchive accepted it, want a refusal naming %q", tc.want)
			}
			if !errors.Is(err, errBodyRefused) {
				t.Errorf("the refusal %v does not wrap errBodyRefused", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %q, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestArchiveNameRefusesANULAndAnOverlongName(t *testing.T) {
	for name, in := range map[string]string{
		"a NUL":            "/run/tok\x00/../etc",
		"a long name":      "/" + strings.Repeat("a", maxArchiveName),
		"a NUL at the end": "/run/tok\x00",
	} {
		if err := archiveNameOK(in); err == nil {
			t.Errorf("%s: archiveNameOK accepted %.40q", name, in)
		}
	}
	for _, in := range []string{"/", ".", "..", "", "/.", "/..", "a//b", "a/", "/a/./b"} {
		if err := archiveNameOK(in); err == nil {
			t.Errorf("archiveNameOK accepted %q", in)
		}
	}
	if err := archiveNameOK("/" + strings.Repeat("a", maxArchiveName-1)); err != nil {
		t.Errorf("a name of exactly %d bytes was refused: %v", maxArchiveName, err)
	}
}

func TestValidateBodyForwardsAValidArchiveByteForByteAndRefusesAnInvalidOne(t *testing.T) {
	raw := sdkArchive(t, "/run/secrets/tok", "s3cret-value", 0o444)
	got, err := validateBody(BodyArchive, bytes.NewReader(raw), bodyEnv{})
	if err != nil {
		t.Fatalf("validateBody refused the SDK's tar: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("the forwarded archive is not the received one")
	}
	if _, err := validateBody(BodyArchive, bytes.NewReader(sdkArchive(t, "../x", "y", 0o444)), bodyEnv{}); !errors.Is(err, errBodyRefused) {
		t.Errorf("an archive with a dot-dot name = %v, want a refusal", err)
	}
	// The cap is the archive's own, not the 64 KiB of a JSON body.
	if bodyCap(BodyArchive) != maxArchiveBody || maxArchiveBody <= maxArchiveContent {
		t.Errorf("bodyCap(BodyArchive) = %d, maxArchiveBody = %d", bodyCap(BodyArchive), maxArchiveBody)
	}
	over := bytes.NewReader(make([]byte, maxArchiveBody+1))
	if _, err := validateBody(BodyArchive, over, bodyEnv{}); !errors.Is(err, errBodyRefused) {
		t.Errorf("a body over the archive cap = %v, want a refusal", err)
	}
}

func TestArchiveTargetOK(t *testing.T) {
	compose := map[string]string{composeProjectLabel: "shop"}
	lookup := func(labels map[string]string, running, found bool, err error) containerLookup {
		return func(context.Context, string) (map[string]string, bool, bool, error) {
			return labels, running, found, err
		}
	}
	for _, tc := range []struct {
		name    string
		inspect containerLookup
		want    string // "" accepted, "refused" a policy refusal, "fault" an engine fault
	}{
		{"a compose container that is not running", lookup(compose, false, true, nil), ""},
		{"a compose container that is running", lookup(compose, true, true, nil), "refused"},
		{"a container without the compose project label", lookup(map[string]string{"app": "x"}, false, true, nil), "refused"},
		{"a container without any label", lookup(nil, false, true, nil), "refused"},
		{"an empty compose project label", lookup(map[string]string{composeProjectLabel: ""}, false, true, nil), "refused"},
		{"a container the engine does not have", lookup(nil, false, false, nil), "refused"},
		{"an engine that does not answer", lookup(nil, false, false, errors.New("boom")), "fault"},
		{"no lookup at all", nil, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := archiveTargetOK(context.Background(), "web-1", tc.inspect)
			switch tc.want {
			case "":
				if err != nil {
					t.Fatalf("archiveTargetOK refused it: %v", err)
				}
			case "refused":
				if !errors.Is(err, errBodyRefused) || errors.Is(err, errContainerLookup) {
					t.Fatalf("archiveTargetOK = %v, want a policy refusal", err)
				}
			case "fault":
				if !errors.Is(err, errContainerLookup) || errors.Is(err, errBodyRefused) {
					t.Fatalf("archiveTargetOK = %v, want an engine fault", err)
				}
			}
		})
	}
}

func TestArchiveContainerIDReadsTheContainerOutOfThePath(t *testing.T) {
	for in, want := range map[string]string{
		"/containers/web-1/archive":        "web-1",
		"/v1.52/containers/abc123/archive": "abc123",
		"/containers/a.b_c-d/archive":      "a.b_c-d",
		"/containers/web-1/json":           "",
		"/containers/archive":              "",
		"/images/x/archive":                "",
	} {
		if got := archiveContainerID(in); got != want {
			t.Errorf("archiveContainerID(%q) = %q, want %q", in, got, want)
		}
	}
}
