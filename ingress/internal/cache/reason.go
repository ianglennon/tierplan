// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"errors"
	"slices"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// ReasonOf maps the error of ingresswire.DecodeBundle or of Check to the stable code the node reports in
// rejected{id, reason} (D-37). A reason is one of ingressbundle.RejectReasons and never a path or file
// content, so an untrusted frame cannot put its own bytes into what the node sends back:
//
//	ingresswire.ErrBundleTooLarge     too_large
//	ingresswire.ErrInstanceMismatch   instance_mismatch
//	a *ingressbundle.VerifyError      its own code (wrapped or not)
//	anything else, ErrMalformed and
//	ErrUnknownMessage included        malformed
//
// A frame beyond the library's read limit closes the connection in coder/websocket before DecodeBundle
// sees it, so too_large is reported only for a frame the library delivered (research A1); the one
// DecodeBundle check is tested at its own seam. ReasonOf decodes no frame itself: the store takes the ID of
// a frame it could not decode from ingresswire.PeekBundleID.
func ReasonOf(err error) string {
	var ve *ingressbundle.VerifyError
	switch {
	case errors.Is(err, ingresswire.ErrBundleTooLarge):
		return ingressbundle.ReasonTooLarge
	case errors.Is(err, ingresswire.ErrInstanceMismatch):
		return ingressbundle.ReasonInstanceMismatch
	case errors.As(err, &ve):
		// A code outside the closed set is never put on the wire.
		if slices.Contains(ingressbundle.RejectReasons(), ve.Reason) {
			return ve.Reason
		}
		return ingressbundle.ReasonMalformed
	default:
		// ErrMalformed, ErrUnknownMessage and every other error.
		return ingressbundle.ReasonMalformed
	}
}
