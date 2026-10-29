// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitsync"
)

// A webhook proves a request came from the git host, and only asks for a check (plan 09-22, GIT-05, D-15).

var hookSecret = []byte("0123456789abcdef0123456789abcdef-secret")

const hookBody = `{"ref":"refs/heads/other","after":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}`

// hmacHex is the HMAC-SHA256 of body under secret, in hex.
func hmacHex(secret []byte, body string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func header(kv ...string) http.Header {
	h := http.Header{}
	for pair := range slices.Chunk(kv, 2) {
		h.Add(pair[0], pair[1])
	}
	return h
}

func TestVerifyGitHub(t *testing.T) {
	t.Parallel()
	good := "sha256=" + hmacHex(hookSecret, hookBody)
	for name, tc := range map[string]struct {
		secret []byte
		h      http.Header
		body   string
		want   bool
	}{
		"a valid signature":            {hookSecret, header("X-Hub-Signature-256", good), hookBody, true},
		"the header name in any case":  {hookSecret, header("x-hub-signature-256", good), hookBody, true},
		"another secret":               {[]byte("another-secret-entirely"), header("X-Hub-Signature-256", good), hookBody, false},
		"a body changed in flight":     {hookSecret, header("X-Hub-Signature-256", good), hookBody + " ", false},
		"no header":                    {hookSecret, header(), hookBody, false},
		"an empty header":              {hookSecret, header("X-Hub-Signature-256", ""), hookBody, false},
		"only the prefix":              {hookSecret, header("X-Hub-Signature-256", "sha256="), hookBody, false},
		"no prefix":                    {hookSecret, header("X-Hub-Signature-256", hmacHex(hookSecret, hookBody)), hookBody, false},
		"the old sha1 prefix":          {hookSecret, header("X-Hub-Signature-256", "sha1="+hmacHex(hookSecret, hookBody)), hookBody, false},
		"malformed hex":                {hookSecret, header("X-Hub-Signature-256", "sha256=zz"+good[9:]), hookBody, false},
		"odd length hex":               {hookSecret, header("X-Hub-Signature-256", good[:len(good)-1]), hookBody, false},
		"a truncated digest":           {hookSecret, header("X-Hub-Signature-256", good[:20]), hookBody, false},
		"a gitea header is not github": {hookSecret, header("X-Gitea-Signature", hmacHex(hookSecret, hookBody)), hookBody, false},
		"an empty secret":              {nil, header("X-Hub-Signature-256", "sha256="+hmacHex(nil, hookBody)), hookBody, false},
		"an empty body, signed":        {hookSecret, header("X-Hub-Signature-256", "sha256="+hmacHex(hookSecret, "")), "", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := gitsync.VerifyDelivery(gitsync.FlavourGitHub, gitsync.PlacementHeader, tc.secret, tc.h, []byte(tc.body))
			if got != tc.want {
				t.Errorf("VerifyDelivery = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWebhookSecrets(t *testing.T) {
	t.Parallel()
	token, secret := gitsync.NewWebhookSecrets()
	// 32 bytes in unpadded URL-safe base64 are 43 characters: 256 bits each.
	for name, v := range map[string]string{"token": token, "secret": secret} {
		if len(v) != 43 || strings.ContainsAny(v, "+/= ") {
			t.Errorf("%s = %q, want 43 URL-safe characters", name, v)
		}
	}
	if token == secret {
		t.Error("the token and the secret are the same value")
	}
	if t2, s2 := gitsync.NewWebhookSecrets(); t2 == token || s2 == secret {
		t.Error("two calls gave the same value")
	}
	hash := gitsync.TokenHash(token)
	if len(hash) != 64 || hash != gitsync.TokenHash(token) || hash == gitsync.TokenHash(secret) || strings.Contains(hash, token) {
		t.Errorf("TokenHash(%q) = %q, want a stable 64 character hex digest that is not the token", token, hash)
	}
	// SHA-256 of "abc": a fixed vector, so the stored value is the plain hash and not a keyed one.
	if got := gitsync.TokenHash("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("TokenHash(abc) = %s", got)
	}
}
