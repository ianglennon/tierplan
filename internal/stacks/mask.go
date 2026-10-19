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

// maxMaskedFileBytes and maxMaskedLines bound the work of hiding a secret file line by line. The
// file as a whole is always hidden, whatever its size.
const (
	maxMaskedFileBytes = 64 << 10
	maxMaskedLines     = 200
)

// NewMasker builds the masker of a working copy from its secrets: every secret variable value, and
// the content of every secret file, of MinSecretLen characters or more, is hidden wherever it
// appears, the longer values first so a secret that contains another is hidden whole.
//
// A secret file is one flagged secret, or one the compose file uses under secrets: even before a
// save has flagged it, so what a person has just typed is hidden too. Its content is hidden as a
// whole, without the line break an editor adds at the end, and, for a file of lines, each line
// and the value after the "=" of each (a secret file can be an env_file, whose values land in the
// rendered stack on their own).
func NewMasker(vars []Var, files []File) Masker {
	seen := map[string]struct{}{}
	var values []string
	add := func(v string) {
		if utf8.RuneCountInString(v) < MinSecretLen || v == Hidden {
			return
		}
		if _, dup := seen[v]; dup {
			return
		}
		seen[v] = struct{}{}
		values = append(values, v)
	}
	for _, v := range vars {
		if v.Secret {
			add(v.Value)
		}
	}
	for _, f := range MarkSecretFiles(files, nil) {
		if f.Secret {
			for _, v := range fileMaskValues(f.Content) {
				add(v)
			}
		}
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

// fileMaskValues are the texts that stand for a secret file's content: the whole of it, and for a
// small file of lines each line and each value after an "=".
func fileMaskValues(content []byte) []string {
	whole := strings.TrimSpace(string(content))
	if whole == "" {
		return nil
	}
	out := []string{whole}
	if len(content) > maxMaskedFileBytes {
		return out
	}
	lines := strings.Split(whole, "\n")
	if len(lines) > maxMaskedLines {
		lines = lines[:maxMaskedLines]
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == whole {
			continue
		}
		out = append(out, line)
	}
	for _, line := range lines {
		if _, value, ok := strings.Cut(line, "="); ok {
			out = append(out, unquote(strings.TrimSpace(value)))
		}
	}
	return out
}

// unquote drops one pair of matching quotes around a value, as an env file writes it.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
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
