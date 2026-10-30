// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxObjectName is the longest name a swarm secret or config may have. The agent refuses a longer
// one, so a name that is too long would fail on the engine's side.
const maxObjectName = 64

// objectHashLen is how many hex characters of the content hash end an object's name.
const objectHashLen = 8

// ObjectHash is the short content hash a stack secret or config carries in its name: the first
// eight lowercase hex digits of sha256 over the name the compose file uses, a NUL byte and the
// content. The NUL keeps ("ab", "c") apart from ("a", "bc"). The same name and content always
// give the same hash, so a redeploy of unchanged content finds the object it made before.
func ObjectHash(name string, content []byte) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))[:objectHashLen]
}

// ObjectName is the name a stack secret or config is created under on a swarm:
// <stack>_<name>_<hash>, where hash is ObjectHash(name, content). The <stack>_<name> part is cut
// so that the whole name fits the 64 characters a swarm allows, and any trailing "_", "." or "-"
// the cut leaves is dropped, so the name still ends in a letter or digit before "_<hash>" and
// matches the rule the agent holds object names to. The hash is never cut.
//
// The stack part is made safe first: a character outside letters, digits, "_", "." and "-" becomes "-",
// and leading characters that are not a letter or digit are dropped (a stack part left empty becomes
// "stack"). The stack-name rule (NameValid) already keeps such characters out and makes every name start
// with a letter or digit, so for a valid name this changes nothing; it is the defence in depth that keeps a
// hashed name inside the agent's rule for any input (D-48, T-08-38).
func ObjectName(stack, name string, content []byte) (full, hash string) {
	hash = ObjectHash(name, content)
	suffix := "_" + hash
	prefix := safeStackPart(stack) + "_" + name
	if room := maxObjectName - len(suffix); len(prefix) > room {
		prefix = strings.TrimRight(prefix[:room], "_.-")
	}
	return prefix + suffix, hash
}

// safeStackPart is the stack's name as the first part of an object name: see ObjectName.
func safeStackPart(stack string) string {
	var b strings.Builder
	for _, r := range stack {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeftFunc(b.String(), func(r rune) bool {
		return r == '_' || r == '.' || r == '-'
	})
	if out == "" {
		return "stack"
	}
	return out
}
