// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import "strings"

// Rule is the Traefik router rule for a hostname and a normalised path prefix (D-15). The root prefix
// matches the whole host. Any other prefix matches the path itself and everything below it, but not a
// sibling that merely starts with the same letters: /api matches /api and /api/x, never /apiary.
//
// host and prefix are values the model already validated (a hostname has no backtick and a prefix has
// none either), and the name of a router carries neither of them, so nothing here can reach a YAML key.
func Rule(host, prefix string) string {
	if prefix == "/" {
		return "Host(`" + host + "`)"
	}
	return "Host(`" + host + "`) && (Path(`" + prefix + "`) || PathPrefix(`" + prefix + "/`))"
}

// Priority is the explicit router priority for a normalised path prefix (D-16). The root is the
// fallback at 1. Any other prefix is 1000 per segment plus its length, so a deeper prefix always beats
// a shallower one and, at equal depth, a longer one beats a shorter one: /api is 1004, /api/v2 is 2007,
// /apiary is 1007 and /api/v2x is 2008. The segment count is the number of slashes because the parser
// guarantees a leading slash, no trailing slash and no empty segment, and caps the length at 255, so
// the length term never reaches the next thousand.
func Priority(prefix string) int {
	if prefix == "/" {
		return 1
	}
	return 1000*strings.Count(prefix, "/") + len(prefix)
}
