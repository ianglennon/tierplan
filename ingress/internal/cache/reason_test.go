// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

func TestReasonOf(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"too large", ingresswire.ErrBundleTooLarge, ingressbundle.ReasonTooLarge},
		{"too large, wrapped", fmt.Errorf("read: %w", ingresswire.ErrBundleTooLarge), ingressbundle.ReasonTooLarge},
		{"instance mismatch", ingresswire.ErrInstanceMismatch, ingressbundle.ReasonInstanceMismatch},
		{"malformed", fmt.Errorf("%w: unexpected end of JSON input", ingresswire.ErrMalformed), ingressbundle.ReasonMalformed},
		{"unknown message", ingresswire.ErrUnknownMessage, ingressbundle.ReasonMalformed},
		{"a wrapped verify error", fmt.Errorf("x: %w", &ingressbundle.VerifyError{Reason: "checksum"}), ingressbundle.ReasonChecksum},
		{"a verify error from Check", &ingressbundle.VerifyError{Path: "certs/a.crt", Reason: ingressbundle.ReasonCertMissing}, ingressbundle.ReasonCertMissing},
		{"a verify error with a code outside the closed set", &ingressbundle.VerifyError{Reason: "attacker chosen text"}, ingressbundle.ReasonMalformed},
		{"any other error", errors.New("boom"), ingressbundle.ReasonMalformed},
		{"nil", nil, ingressbundle.ReasonMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ReasonOf(c.err)
			if got != c.want {
				t.Fatalf("ReasonOf(%v) = %q, want %q", c.err, got, c.want)
			}
			if !slices.Contains(ingressbundle.RejectReasons(), got) {
				t.Fatalf("ReasonOf returned %q, which is not in RejectReasons", got)
			}
		})
	}
}

// TestDecodeOversizeIsTooLarge is the oversize case at the DecodeBundle seam: a frame one byte over
// the limit is refused before parsing, and ReasonOf reports too_large. Over a real WebSocket the
// library closes the connection first (research A1), so this is the only place the case can be tested.
func TestDecodeOversizeIsTooLarge(t *testing.T) {
	frame := make([]byte, ingresswire.MaxBundleMessageBytes+1)
	_, err := ingresswire.DecodeBundle(frame, "018f0000-0000-7000-8000-000000000003")
	if err == nil {
		t.Fatal("DecodeBundle accepted a frame over MaxBundleMessageBytes")
	}
	if got := ReasonOf(err); got != ingressbundle.ReasonTooLarge {
		t.Fatalf("ReasonOf = %q, want %q", got, ingressbundle.ReasonTooLarge)
	}
}
