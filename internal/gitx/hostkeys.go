// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import "strings"

// BuiltinHostKeys maps each built-in host (lower case, no port) to its pinned keys in
// authorized_keys form. A host listed here never needs a person to trust it, and a key saved on a
// repository of such a host is ignored (Phase 9 D-03).
var BuiltinHostKeys = map[string][]string{}

// builtinFor returns the pinned keys of host, or nil when the host has none. The host is compared
// case-insensitively and a port is ignored.
func builtinFor(host string) []HostKey {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	lines := BuiltinHostKeys[host]
	if len(lines) == 0 {
		return nil
	}
	keys := make([]HostKey, len(lines))
	for i, l := range lines {
		keys[i] = HostKey{Line: l}
	}
	return keys
}
