// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresswire

import "encoding/json"

// This file is additive node-side code beside the frozen wire shapes: it adds one function and changes
// no message type, field, constant or golden file (Phase 22 D-07 set the precedent of an additive
// function beside frozen code). It imports the standard library only.

// bundleIDLen is the length of a bundle ID: the lower-case hex of a SHA-256.
const bundleIDLen = 64

// peekHead is the two members of a bundle message PeekBundleID reads, under the JSON names that
// BundleEnvelope and ingressbundle.Bundle carry and frozen_test.go pins.
type peekHead struct {
	T      string `json:"t"`
	Bundle struct {
		ID string `json:"id"`
	} `json:"bundle"`
}

// PeekBundleID returns the bundle ID of a frame the node could not decode or verify, or "" when the
// frame does not name one safely.
//
// A node answers a bundle it refuses with rejected{id, reason}, and the server matches the ID to the
// bundle it sent. The ID must name the bundle without echoing untrusted bytes, so it is returned only
// when the frame is at most MaxBundleMessageBytes (the size is checked before any decoding, as
// DecodeBundle does), its type is bundle, and the ID is exactly 64 characters of 0-9 and a-f. Any
// other frame, including a decode failure, gives "". It checks no instance ID and no bundle content;
// that is DecodeBundle's job.
func PeekBundleID(frame []byte) string {
	if len(frame) > MaxBundleMessageBytes {
		return ""
	}
	var head peekHead
	if err := json.Unmarshal(frame, &head); err != nil {
		return ""
	}
	if head.T != TypeBundle || !isBundleID(head.Bundle.ID) {
		return ""
	}
	return head.Bundle.ID
}

// isBundleID reports whether s is 64 lower-case hex characters.
func isBundleID(s string) bool {
	if len(s) != bundleIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
