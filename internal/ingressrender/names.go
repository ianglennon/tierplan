// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// nameHashLen is how many hex digits of a SHA-256 a Traefik name carries (D-18). Twelve hex digits are
// 48 bits, so a collision among the few thousand routes one environment holds is not a concern, and a
// bundle still refuses to render if two names ever did collide. The manifest carries the full hash.
const nameHashLen = 12

// routeHash is the full lower-case hex SHA-256 of a route's hostname and path prefix, with a zero byte
// between them so no pair of values can alias another. The manifest's RouteEntry carries it whole.
func routeHash(host, prefix string) string {
	return hexHash(host, prefix)
}

// hexHash is the lower-case hex SHA-256 of the parts joined by a zero byte.
func hexHash(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RouterName is the Traefik router name of a route: "r-" and the first 12 hex digits of its route hash.
// It holds no user text, so nothing a person typed can reach a YAML key or a Traefik name.
func RouterName(host, prefix string) string {
	return "r-" + routeHash(host, prefix)[:nameHashLen]
}

// MiddlewareName is the name of a route's strip-prefix middleware, built the same way as its router's.
func MiddlewareName(host, prefix string) string {
	return "m-strip-" + routeHash(host, prefix)[:nameHashLen]
}

// ServiceName is the Traefik service name of a backend: "s-" and the first 12 hex digits of a SHA-256
// over the scheme and the backend URL. Two routes to the same backend share one service.
func ServiceName(scheme ingress.Scheme, url string) string {
	return "s-" + hexHash(string(scheme), url)[:nameHashLen]
}
