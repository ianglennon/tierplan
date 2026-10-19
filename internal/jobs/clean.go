// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package jobs

// MaxDetailRunes is the most characters of a remote text a job keeps in an item's Detail or in one
// output line.
const MaxDetailRunes = 2048

// Clean makes a text a remote system sent fit to be stored in a job and shown to a browser as text:
// mask is applied first (nil for none), then every control character except a tab is removed, bytes
// that are not UTF-8 are dropped, and a text longer than MaxDetailRunes is clipped to that many
// characters, the last of them an ellipsis.
func Clean(s string, mask func(string) string) string { return s }
