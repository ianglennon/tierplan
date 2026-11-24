// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// aliasHashLen is the number of hex characters of the pair's hash that end an alias: 64 bits. The alias is the name the
// proxy reaches a backend by, and a stack name is chosen by whoever creates the stack, so finding a second pair with the
// same alias must be out of reach. With 8 characters (32 bits) a collision with a given long name was found in about a
// minute on four cores (T-25-47); 64 bits puts it beyond any search.
const aliasHashLen = 16

// Alias is the only name a routed standalone service has on the host's shared ingress network (D-15):
// the proxy reaches the service at http://<Alias(project, service)>:<port>. Two stacks that both have a
// service called web must not share a name on that network, so the bare service name and the container
// name are never aliases there.
//
// The alias is the two names as DNS-label slugs (lower case; every byte outside a-z, 0-9 and '-' becomes
// '-'), cut so the whole fits one 63-byte label, then '-' and the first 16 hex characters of
// sha256(project NUL service). The hash is over the original strings, not the slugs, so names that
// slug or truncate alike (a_b+c against a+b_c, case variants, two long names with one prefix) still
// differ. It is a pure function of the exact pair, and the attach and the renderer both call it, so
// they cannot disagree.
//
// It is costly to change: every routed container carries its alias, and a new function recreates each
// of them on its next deploy. The golden table in alias_test.go pins it.
func Alias(project, service string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + service))
	h := hex.EncodeToString(sum[:])[:aliasHashLen]
	p, s := slug(project), slug(service)
	// A DNS label starts with a letter or digit. A project name that NameValid accepts always does; a
	// hyphen first byte (not reachable that way) becomes 'x', and the hash still tells the pair apart.
	if p[0] == '-' {
		p = "x" + p[1:]
	}
	// The two separating hyphens and the hash take their share of the label.
	room := maxLabelLen - 2 - aliasHashLen
	for len(p)+len(s) > room {
		if len(p) >= len(s) {
			p = p[:len(p)-1]
		} else {
			s = s[:len(s)-1]
		}
	}
	return p + "-" + s + "-" + h
}

// slug lowers s and turns every byte outside a-z, 0-9 and '-' into '-'. An empty result is "x", so
// the alias never has an empty part (a name that NameValid accepts is never empty).
func slug(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			b[i] = '-'
		}
	}
	return string(b)
}
