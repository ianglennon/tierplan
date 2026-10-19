// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// MinSecretLen is the shortest value a secret variable may have, in characters. A shorter value
// would turn up inside ordinary words, so masking it would hide common substrings, and a person
// who typed one is told so at save time (422 secret_too_short) rather than given a mask that
// cannot work.
const MinSecretLen = 4

// Masker replaces the secret values of one stack with Hidden. It is the one masker every sink
// uses (check answers, rendered views, diffs, the console, logs and audit details), so a secret
// that reaches Docker's error text or a lint message is withheld wherever it would be shown. It is
// a value: the zero Masker hides nothing, and a Masker is safe to share between goroutines.
type Masker struct {
	replacer *strings.Replacer
}

// NewMasker builds the masker of a working copy from its secret variables: every secret value of
// MinSecretLen characters or more is hidden wherever it appears, the longer values first so a
// secret that contains another is hidden whole. Plan 07-13 adds the content of secret files, which
// is why the files are passed.
func NewMasker(vars []Var, _ []File) Masker {
	seen := map[string]struct{}{}
	var values []string
	for _, v := range vars {
		if !v.Secret || utf8.RuneCountInString(v.Value) < MinSecretLen || v.Value == Hidden {
			continue
		}
		if _, dup := seen[v.Value]; dup {
			continue
		}
		seen[v.Value] = struct{}{}
		values = append(values, v.Value)
	}
	if len(values) == 0 {
		return Masker{}
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	pairs := make([]string, 0, 2*len(values))
	for _, v := range values {
		pairs = append(pairs, v, Hidden)
	}
	return Masker{replacer: strings.NewReplacer(pairs...)}
}

// Mask replaces every secret value in s with Hidden.
func (m Masker) Mask(s string) string {
	if m.replacer == nil || s == "" {
		return s
	}
	return m.replacer.Replace(s)
}

// MaskValue is the text to show for a value that is known to be a secret's: Hidden, or nothing
// for an empty value, which has nothing to hide.
func (m Masker) MaskValue(v string) string {
	if v == "" {
		return ""
	}
	return Hidden
}
