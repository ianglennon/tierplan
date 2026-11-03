// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/ianglennon/tierplan/internal/jobs"
)

// The forms a log line writes a secret in, besides a value it is told about.
var (
	// ansi matches the terminal escape sequences a program writes to colour or retitle its output:
	// CSI sequences (ESC [ ... final byte), OSC strings (ESC ] ... BEL or ESC \) and two-byte escapes.
	ansi = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[@-Z\\-_])`)
	// authorization matches an Authorization header or pair and keeps the scheme word, so
	// "Authorization: Bearer abc" becomes "Authorization: Bearer <hidden>".
	authorization = regexp.MustCompile(`(?i)(\b(?:proxy-)?authorization\b["']?\s*[:=]\s*["']?)((?:bearer|basic|token|digest)\s+)?[^\s"',;]+`)
	// bearer matches a bearer or basic credential written on its own. A short word after "bearer" is
	// prose, so at least eight characters of token are needed.
	bearer = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// quotedColon matches "name": "value" and name: 'value', a quoted value after a colon; a JSON log
	// line is written this way.
	quotedColon = regexp.MustCompile(`"?([A-Za-z_][A-Za-z0-9_.-]*)"?\s*:\s*("[^"]*"|'[^']*')`)
	// bareColon matches name: value with a space after the colon. A clock time (12:30:45) has none.
	bareColon = regexp.MustCompile(`(?i)\b([A-Za-z_][A-Za-z0-9_.-]*)\s*:\s+((?:(?:bearer|basic|token|digest)\s+)?[^\s"',;]+)`)
	// namedValue matches a name=value pair: a name of letters, digits, dots, dashes and underscores,
	// an equals sign, and a value that is a quoted text or a run without spaces.
	namedValue = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.-]*)=("[^"]*"|'[^']*'|[^\s"']+)`)
)

// MaskLogLine hides the secrets in one line of a container's log before it is stored (D-06). It
// replaces, in this order: every control character and terminal escape sequence is removed, so one
// cannot split a secret to get past the masks; every one of secrets of at least four characters,
// the longest first; the user-info of a URL (//user:password@host); the credential of an
// Authorization header or a Bearer or Basic token; and the value of each name=value or name: value
// pair whose name looks like a secret (password, token, key and the other words secretName knows),
// so "password=hunter2" becomes "password=<hidden>" and "PORT=8080" is left alone. Last, bytes that
// are not UTF-8 are dropped and the line is cut at jobs.MaxDetailRunes characters. The second result
// reports whether anything was replaced as a secret.
//
// Masking is best effort: a log can write a secret in any form, and the people who read what is
// stored are told so.
func MaskLogLine(line string, secrets []string) (string, bool) {
	base := StripControls(line)
	out := base
	if r := literalReplacer(secrets); r != nil {
		out = r.Replace(out)
	}
	out = maskUserInfo(out)
	out = authorization.ReplaceAllString(out, "${1}${2}"+Placeholder)
	out = bearer.ReplaceAllString(out, "${1}${2}"+Placeholder)
	out = quotedColon.ReplaceAllStringFunc(out, func(m string) string {
		sub := quotedColon.FindStringSubmatch(m)
		if !secretName(sub[1]) {
			return m
		}
		quote := sub[2][:1]
		return m[:len(m)-len(sub[2])] + quote + Placeholder + quote
	})
	out = bareColon.ReplaceAllStringFunc(out, func(m string) string {
		sub := bareColon.FindStringSubmatch(m)
		if !secretName(sub[1]) || strings.Contains(sub[2], Placeholder) {
			return m
		}
		return m[:len(m)-len(sub[2])] + Placeholder
	})
	out = namedValue.ReplaceAllStringFunc(out, func(m string) string {
		name, _, _ := strings.Cut(m, "=")
		if !secretName(name) || strings.HasSuffix(m, "="+Placeholder) {
			return m
		}
		return name + "=" + Placeholder
	})
	masked := out != base
	return jobs.Clean(out, nil), masked
}

// StripControls removes bytes that are not UTF-8, terminal escape sequences and every control
// character except a tab. MaskLogLine starts with it; a caller that masks a line with values of its
// own before MaskLogLine must strip the line first too, or an escape written inside a secret hides it
// from that mask and the stripping then puts it back together.
func StripControls(s string) string {
	s = ansi.ReplaceAllString(strings.ToValidUTF8(s, ""), "")
	return strings.Map(func(r rune) rune {
		if r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// literalReplacer replaces each of secrets by Placeholder, the longest first so a secret that holds
// another is hidden whole, or is nil when none is worth hiding.
func literalReplacer(secrets []string) *strings.Replacer {
	var values []string
	for _, s := range secrets {
		if len(s) >= minLogSecret && !slices.Contains(values, s) {
			values = append(values, s)
		}
	}
	if len(values) == 0 {
		return nil
	}
	slices.SortFunc(values, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	pairs := make([]string, 0, 2*len(values))
	for _, v := range values {
		pairs = append(pairs, v, Placeholder)
	}
	return strings.NewReplacer(pairs...)
}

// minLogSecret is the shortest value hidden wherever it appears in a log line. A shorter one would
// turn up inside ordinary words, so hiding it would only spoil the line.
const minLogSecret = 4

// SecretEnvValues are the values of the "NAME=value" entries of a container's environment whose
// name looks like a secret (see secretName), at least minLogSecret characters long. They are what a
// container's log is most likely to echo, so MaskLogLine hides them wherever they appear.
func SecretEnvValues(env []string) []string {
	var out []string
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if ok && len(value) >= minLogSecret && secretName(name) {
			out = append(out, value)
		}
	}
	return out
}
