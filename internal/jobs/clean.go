// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package jobs

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxDetailRunes is the most characters of a remote text a job keeps in an item's Detail or in one
// output line.
const MaxDetailRunes = 2048

// maxCleanBytes bounds the work on a text from a remote system: only its first MaxDetailRunes
// characters are kept, so a text longer than this cannot change what is kept, and it is cut before
// the mask reads it. The mask still sees every byte that can reach the result.
const maxCleanBytes = 256 << 10

// Clean makes a text a remote system sent fit to be stored in a job and shown to a browser as text:
// mask is applied first (nil for none), then every control character except a tab is removed, bytes
// that are not UTF-8 are dropped, and a text longer than MaxDetailRunes is clipped to that many
// characters, the last of them an ellipsis.
//
// The mask runs before the clip so that a secret across the edge of the clip is hidden whole, never
// cut in two with its first half left in the text; and the controls are removed after the mask so
// that nothing the mask put back, and no control character placed inside a secret to defeat it, gets
// through. The result is a string to be drawn as text: it holds no escape sequence and no line break.
func Clean(s string, mask func(string) string) string {
	if len(s) > maxCleanBytes {
		s = s[:maxCleanBytes]
	}
	if mask != nil {
		s = mask(s)
	}
	var b strings.Builder
	b.Grow(min(len(s), 4*MaxDetailRunes))
	kept := 0
	clipped := false
	for _, r := range strings.ToValidUTF8(s, "") {
		if r != '\t' && unicode.IsControl(r) {
			continue
		}
		if kept == MaxDetailRunes {
			clipped = true
			break
		}
		b.WriteRune(r)
		kept++
	}
	if !clipped {
		return b.String()
	}
	// One character too many was seen: the last kept one gives way to the ellipsis.
	out := b.String()
	_, size := utf8.DecodeLastRuneInString(out)
	return out[:len(out)-size] + "…"
}
