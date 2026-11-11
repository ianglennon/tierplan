// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dns_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme/dns"
)

// TestTXTValue checks the RFC 8555 section 8.4 derivation against SHA-256's published test vector
// for "abc", written out here so the test does not repeat the code it checks.
func TestTXTValue(t *testing.T) {
	const want = "ungWv48Bz-pBQUDeXa4iI7ADYaOWF3qctBD_YfIAFa0" // base64url(SHA-256("abc")), no padding
	if got := dns.TXTValue("abc"); got != want {
		t.Fatalf("TXTValue(abc) = %q, want %q", got, want)
	}
	got := dns.TXTValue("token.thumbprint")
	if len(got) != 43 || strings.ContainsAny(got, "=+/") {
		t.Fatalf("TXTValue is not 43 unpadded base64url characters: %q", got)
	}
}

func TestChallengeFQDN(t *testing.T) {
	for in, want := range map[string]string{
		"tp.example.com":   "_acme-challenge.tp.example.com",
		"TP.Example.COM.":  "_acme-challenge.tp.example.com",
		" tp.example.com ": "_acme-challenge.tp.example.com",
	} {
		if got := dns.ChallengeFQDN(in); got != want {
			t.Errorf("ChallengeFQDN(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRecordCommentFitsCloudflare checks the comment for the longest owner id a record carries: an
// order UUID after an install UUID. Cloudflare's Free plan allows 100 characters and no newline.
func TestRecordCommentFitsCloudflare(t *testing.T) {
	install, owner := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	got := dns.RecordComment(install, owner)
	if want := "tierplan:" + install + ":" + owner; got != want {
		t.Fatalf("RecordComment = %q, want %q", got, want)
	}
	if len(got) > dns.MaxCommentLen {
		t.Fatalf("comment is %d bytes, over the %d Cloudflare allows", len(got), dns.MaxCommentLen)
	}
	for i := 0; i < len(got); i++ {
		if c := got[i]; c < 0x20 || c >= 0x7f {
			t.Fatalf("comment has a non-printable-ASCII byte %#x at %d", c, i)
		}
	}
	if test := dns.RecordComment(install, "test-0a1b2c3d"); !strings.HasSuffix(test, ":test-0a1b2c3d") {
		t.Fatalf("test owner comment = %q", test)
	}
}

// TestRecordCommentIsAlwaysSafe checks that input that would break the limits cannot get through.
func TestRecordCommentIsAlwaysSafe(t *testing.T) {
	got := dns.RecordComment("in\nstallé", strings.Repeat("x", 300))
	if len(got) != dns.MaxCommentLen {
		t.Fatalf("a long comment is %d bytes, want it cut to %d", len(got), dns.MaxCommentLen)
	}
	if strings.ContainsAny(got, "\n\r") || strings.ContainsRune(got, 'é') {
		t.Fatalf("comment kept a newline or a non-ASCII character: %q", got)
	}
}
