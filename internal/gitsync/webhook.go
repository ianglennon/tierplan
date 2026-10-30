// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

// Flavour is the git host a webhook is for: it decides which header carries the proof that the host sent the request.
type Flavour string

// The flavours of a webhook (D-15).
const (
	FlavourGitHub  Flavour = "github"
	FlavourGitLab  Flavour = "gitlab"
	FlavourGitea   Flavour = "gitea" // Gitea, Forgejo and Gogs
	FlavourGeneric Flavour = "generic"
)

// Valid says whether f is one of the four flavours.
func (f Flavour) Valid() bool {
	switch f {
	case FlavourGitHub, FlavourGitLab, FlavourGitea, FlavourGeneric:
		return true
	}
	return false
}

// Placement is where a generic webhook carries its secret. The other flavours always sign with a header.
type Placement string

// The placements of a webhook's secret.
const (
	// PlacementPath: the routing token in the address is the secret, and the request carries nothing else.
	PlacementPath Placement = "path"
	// PlacementHeader: the request carries the secret as "Authorization: Bearer <secret>" (generic) or signs its body
	// with it (the host flavours).
	PlacementHeader Placement = "header"
)

// Valid says whether p is one of the two placements.
func (p Placement) Valid() bool { return p == PlacementPath || p == PlacementHeader }

// secretBytes is the size of a token and of a secret: 256 bits.
const secretBytes = 32

// NewWebhookSecrets returns a fresh routing token and a fresh signing secret: 32 random bytes each from crypto/rand,
// in URL-safe base64 without padding. The token goes in the webhook's address and is stored only as TokenHash; the
// secret is sealed.
func NewWebhookSecrets() (token, secret string) {
	return randomString(), randomString()
}

func randomString() string {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on a supported platform; a webhook secret must never be guessable.
		panic("gitsync: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenHash is what is stored of a routing token: the hex SHA-256 of it. A token is 256 bits of randomness, so the hash
// needs no salt, and a lookup by it is a lookup by an unguessable value.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// The headers a delivery is signed in.
const (
	headerHubSignature256 = "X-Hub-Signature-256"
	headerGitLabToken     = "X-Gitlab-Token" //nolint:gosec // G101: a header name, not a credential
	headerAuthorization   = "Authorization"
)

// giteaSignatureHeaders are the headers of Gitea, Forgejo and Gogs. The first carries "sha256=<hex>", the others the
// bare hex digest.
var giteaSignatureHeaders = []string{headerHubSignature256, "X-Forgejo-Signature", "X-Gitea-Signature", "X-Gogs-Signature"}

// VerifyDelivery says whether a request proves that the git host sent it, judged from the raw body and the headers
// alone. Nothing in the body is read: it is only the input of a MAC.
//
//   - github: X-Hub-Signature-256 is "sha256=" and the HMAC-SHA256 of the body under the secret.
//   - gitlab: X-Gitlab-Token is the secret.
//   - gitea: any of X-Hub-Signature-256, X-Forgejo-Signature, X-Gitea-Signature and X-Gogs-Signature that is present
//     must verify, and at least one must be present.
//   - generic, placement path: the routing token already matched, so there is nothing more to check.
//   - generic, placement header: "Authorization: Bearer <secret>".
//
// Every comparison is constant time, and an empty digest or secret never verifies.
func VerifyDelivery(f Flavour, p Placement, secret []byte, h http.Header, body []byte) bool {
	switch f {
	case FlavourGitHub:
		return verifyHeaders(secret, h, body, []string{headerHubSignature256})
	case FlavourGitea:
		return verifyHeaders(secret, h, body, giteaSignatureHeaders)
	case FlavourGitLab:
		return verifyTokenHeader(secret, h.Values(headerGitLabToken))
	case FlavourGeneric:
		switch p {
		case PlacementPath:
			return true
		case PlacementHeader:
			return verifyBearer(secret, h.Values(headerAuthorization))
		}
	}
	return false
}

// verifyHeaders checks the HMAC headers named: each one present, with every value it has, must verify, and at least one
// must be present.
func verifyHeaders(secret []byte, h http.Header, body []byte, names []string) bool {
	if len(secret) == 0 {
		return false
	}
	want := computeHMAC(secret, body)
	seen := false
	ok := true
	for _, name := range names {
		for _, v := range h.Values(name) {
			seen = true
			if !equalDigest(want, v, name == headerHubSignature256) {
				ok = false // keep going: every header is judged, so the time does not say which one failed
			}
		}
	}
	return seen && ok
}

func computeHMAC(secret, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return m.Sum(nil)
}

// equalDigest compares the hex digest a header carries to want. X-Hub-Signature-256 always starts "sha256=" (needPrefix);
// the other headers carry the bare digest, and a prefix there is tolerated.
func equalDigest(want []byte, header string, needPrefix bool) bool {
	value := strings.TrimSpace(header)
	hexed, hadPrefix := strings.CutPrefix(value, "sha256=")
	if needPrefix && !hadPrefix {
		return false
	}
	got, err := hex.DecodeString(hexed)
	if err != nil || len(got) == 0 {
		return false
	}
	return hmac.Equal(want, got)
}

// verifyTokenHeader compares a shared-token header (GitLab's) to the secret, in constant time.
func verifyTokenHeader(secret []byte, values []string) bool {
	if len(secret) == 0 || len(values) == 0 {
		return false
	}
	ok := true
	for _, v := range values {
		if !equalSecret(secret, []byte(v)) {
			ok = false
		}
	}
	return ok
}

// verifyBearer checks an "Authorization: Bearer <secret>" header.
func verifyBearer(secret []byte, values []string) bool {
	if len(secret) == 0 || len(values) == 0 {
		return false
	}
	ok := true
	for _, v := range values {
		scheme, token, found := strings.Cut(strings.TrimSpace(v), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || !equalSecret(secret, []byte(strings.TrimSpace(token))) {
			ok = false
		}
	}
	return ok
}

// equalSecret compares two secrets without leaking where they differ or how long the stored one is: both are hashed to
// a fixed length first.
func equalSecret(secret, got []byte) bool {
	a, b := sha256.Sum256(secret), sha256.Sum256(got)
	return hmac.Equal(a[:], b[:]) && len(got) > 0
}
