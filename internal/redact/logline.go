// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package redact

import (
	"regexp"
	"strings"
)

// namedValue matches a name=value pair in a log line: a name of letters, digits, dots, dashes and
// underscores, an equals sign, and a value that is a quoted text or a run without spaces.
var namedValue = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.-]*)=("[^"]*"|'[^']*'|[^\s"']+)`)

// MaskLogLine hides the secrets in one line of a container's log before it is stored (D-06). The
// value of each name=value pair whose name looks like a secret (password, token, key and the other
// words secretName knows) is replaced by Placeholder, so "password=hunter2" becomes
// "password=<hidden>" and "PORT=8080" is left alone. The second result reports whether anything was
// replaced. secrets are values to hide wherever they appear in the line.
//
// Masking is best effort: a log can write a secret in any form.
func MaskLogLine(line string, secrets []string) (string, bool) {
	_ = secrets
	masked := false
	out := namedValue.ReplaceAllStringFunc(line, func(m string) string {
		name, _, _ := strings.Cut(m, "=")
		if !secretName(name) {
			return m
		}
		masked = true
		return name + "=" + Placeholder
	})
	return out, masked
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
