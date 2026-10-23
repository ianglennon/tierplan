// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"sort"
	"strconv"
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

// NewMasker builds the masker of a working copy from its secrets (D-02): every secret variable
// value, and the content of every secret file, of MinSecretLen characters or more, is hidden
// wherever it appears, the longer values first so a secret that contains another is hidden whole.
//
// A value is hidden as stored and in the forms the texts that quote it write it in: with the
// line break an editor adds trimmed (the loader trims its text), in Go's quoting (%q, strconv's
// errors for an integer or float cast, and the converter), and in the quoting of the compose
// schema validator (single quotes around the Go-quoted value, with \" turned back into " and ' into
// \'). The ASCII-only and JSON forms are not added: no path in Bayplan or compose-go writes
// them, and an answer is JSON-encoded after it is masked.
//
// A secret variable that holds several lines, such as a PEM key, is also hidden line by line, each
// line of MinSecretLen characters or more, for the first maxMaskedLines lines of a value of at
// most maxMaskedFileBytes, the cap a secret file has. Above the cap the value is still hidden whole
// in every form above, which is how the messages probed quote it; only a line quoted on its own
// would show. A variable is interpolated whole, so it is not split at "=".
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
		for _, q := range quotedForms(v) {
			if utf8.RuneCountInString(q) < MinSecretLen || q == Hidden {
				continue
			}
			if _, dup := seen[q]; dup {
				continue
			}
			seen[q] = struct{}{}
			values = append(values, q)
		}
	}
	for _, v := range vars {
		if v.Secret {
			for _, m := range varMaskValues(v.Value) {
				add(m)
			}
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

// quotedForms are the two ways an error text writes v inside quotes: Go's (%q, strconv) and the
// compose schema validator's. The validator (jsonschema v6, kind.quote) takes strconv.Quote with
// its outer quotes, turns every \" back into ", then every ' into \', and only then drops the
// outer quotes; doing the replacements on the inner text gives another form for a value that ends
// in a backslash.
func quotedForms(v string) []string {
	q := strconv.Quote(v)
	goForm := q[1 : len(q)-1]
	schema := strings.ReplaceAll(q, `\"`, `"`)
	schema = strings.ReplaceAll(schema, `'`, `\'`)
	schema = schema[1 : len(schema)-1]
	return []string{goForm, schema}
}

// varMaskValues are the texts that stand for a secret variable's value: as stored, trimmed (the
// loader trims its text, so a value that ends in a line break appears without it) and, for a value
// of several lines up to maxMaskedFileBytes, each non-empty trimmed line, at most maxMaskedLines
// of them. Above that cap the value is still hidden whole by the other forms.
func varMaskValues(value string) []string {
	out := []string{value}
	trimmed := strings.TrimSpace(value)
	if trimmed != value {
		out = append(out, trimmed)
	}
	if len(value) > maxMaskedFileBytes || !strings.Contains(trimmed, "\n") {
		return out
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > maxMaskedLines {
		lines = lines[:maxMaskedLines]
	}
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
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
