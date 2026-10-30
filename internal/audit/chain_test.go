// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package audit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
)

// TestCanonicalV1IsFrozen pins the byte layout. Changing it invalidates every
// entry already written, so a failure here means the format was changed, not
// that the test needs updating (D-40).
func TestCanonicalV1IsFrozen(t *testing.T) {
	r := record{
		ID: "a", TS: 1, Action: "x", KeyID: "K",
		Details: map[string]string{"z": "2", "k": "v"},
	}
	want := []byte{
		0x01,      // version
		0x01, 'a', // id
		0, 0, 0, 0, 0, 0, 0, 1, // ts, 8 bytes big-endian
		0x00,      // actor type
		0x00,      // actor id
		0x00,      // actor name
		0x01, 'x', // action
		0x00,                 // target type
		0x00,                 // target id
		0x00,                 // env id
		0x00,                 // result
		0x00,                 // reason
		0x00,                 // ip
		0x00,                 // user agent
		0x02,                 // details: two pairs
		0x01, 'k', 0x01, 'v', // sorted by key: k first
		0x01, 'z', 0x01, '2',
		0x01, 'K', // key id
	}
	got := canonicalV1(r)
	if !bytes.Equal(got, want) {
		t.Fatalf("canonicalV1 = %x\nwant          %x", got, want)
	}
}

func TestCanonicalV1DistinguishesFieldBoundaries(t *testing.T) {
	a := canonicalV1(record{Action: "ab", TargetType: "c"})
	b := canonicalV1(record{Action: "a", TargetType: "bc"})
	if bytes.Equal(a, b) {
		t.Fatal("length prefixes must stop adjacent fields from running together")
	}
	c := canonicalV1(record{Details: map[string]string{"a": "bc"}})
	d := canonicalV1(record{Details: map[string]string{"ab": "c"}})
	if bytes.Equal(c, d) {
		t.Fatal("detail keys and values must be length prefixed")
	}
}

func TestComputeHMACIsHMACSHA256OverPrevThenCanonical(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	prev := bytes.Repeat([]byte{3}, 32)
	r := record{ID: "a", TS: 1, Action: "x", KeyID: "K"}

	mac := hmac.New(sha256.New, key)
	mac.Write(prev)
	mac.Write(canonicalV1(r))
	want := mac.Sum(nil)

	if got := computeHMAC(key, prev, r); !bytes.Equal(got, want) {
		t.Fatalf("computeHMAC = %x, want %x", got, want)
	}
}
