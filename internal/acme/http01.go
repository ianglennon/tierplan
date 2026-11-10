// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// challengePrefix is where an HTTP-01 challenge is fetched. The server's plain listener routes
// exactly this prefix to Challenges.Handler.
const challengePrefix = "/.well-known/acme-challenge/"

// Solver answers one kind of ACME challenge while an order is validated. HTTP-01's token store
// is one variant; Phase 15 puts DNS-01 behind the same seam, so the order code never learns
// which kind it is driving. The signatures match the shape lego's providers use, with a context.
type Solver interface {
	// Type is the challenge type this solver answers, such as ChallengeHTTP01.
	Type() string
	// Present makes the challenge answerable for domain.
	Present(ctx context.Context, domain, token, keyAuth string) error
	// CleanUp withdraws what Present made answerable.
	CleanUp(ctx context.Context, domain, token, keyAuth string) error
}

var _ Solver = (*Challenges)(nil)

// Challenges is the HTTP-01 token store: the key authorization of every token an order has
// presented and not yet cleaned up. It lives in memory only and holds a token for the length of
// an order. A token is answered as often as it is asked until CleanUp, because Let's Encrypt
// validates from several vantage points.
type Challenges struct {
	mu     sync.RWMutex
	tokens map[string]string
}

// NewChallenges returns an empty token store.
func NewChallenges() *Challenges {
	return &Challenges{tokens: map[string]string{}}
}

// Type reports ChallengeHTTP01.
func (c *Challenges) Type() string { return ChallengeHTTP01 }

// Present stores keyAuth under token. The domain is not part of the key: the plain listener
// answers whatever host name the CA asks for, and the token is unguessable.
func (c *Challenges) Present(_ context.Context, _, token, keyAuth string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[token] = keyAuth
	return nil
}

// CleanUp forgets token. Forgetting an unknown token is not an error.
func (c *Challenges) CleanUp(_ context.Context, _, token, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, token)
	return nil
}

// Len is the number of tokens currently answerable.
func (c *Challenges) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.tokens)
}

func (c *Challenges) lookup(token string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keyAuth, ok := c.tokens[token]
	return keyAuth, ok
}

// validToken reports whether s is a base64url token: one or more of A-Z a-z 0-9 _ -.
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '_', b == '-':
		default:
			return false
		}
	}
	return true
}

// Handler answers a plain HTTP-01 request from the token map, never from the file system. A
// token outside the base64url alphabet is 404 without a lookup; any method but GET and HEAD is
// 405; an unknown token is 404. A known token is its key authorization as text/plain, not cached.
func (c *Challenges) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.URL.Path, challengePrefix)
		if !ok || !validToken(token) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		keyAuth, found := c.lookup(token)
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(keyAuth))
		}
	})
}
