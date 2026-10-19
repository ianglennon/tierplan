// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
// matches the rule the agent holds object names to. The hash is never cut. Stack names start with
// a letter or digit (NameValid), so the cut part is never empty.
func ObjectName(stack, name string, content []byte) (full, hash string) {
	hash = ObjectHash(name, content)
	suffix := "_" + hash
	prefix := stack + "_" + name
	if room := maxObjectName - len(suffix); len(prefix) > room {
		prefix = strings.TrimRight(prefix[:room], "_.-")
	}
	return prefix + suffix, hash
}
