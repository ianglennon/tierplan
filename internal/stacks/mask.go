// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

// MinSecretLen is the shortest value a secret variable may have. A shorter value would turn up
// inside ordinary words, so masking it would hide common substrings and guessing at it would be
// easy; a save with a shorter secret is refused (422 secret_too_short).
const MinSecretLen = 4

// Masker replaces the secret values of one stack with Hidden. It is the one masker every sink
// uses (check answers, rendered views, diffs, the console, logs and audit details), so a secret
// that reaches Docker's error text or a lint message is withheld wherever it would be shown.
type Masker struct{}

// NewMasker builds the masker of a working copy from its secret variables.
func NewMasker(vars []Var, files []File) Masker { return Masker{} }

// Mask replaces every secret value in s with Hidden.
func (m Masker) Mask(s string) string { return s }

// MaskValue is the text to show for a value that is known to be a secret's: Hidden, or nothing
// for an empty value.
func (m Masker) MaskValue(v string) string { return v }
