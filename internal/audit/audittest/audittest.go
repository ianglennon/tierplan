// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package audittest is for tests only. It holds the one check every proof test of the ingress pages calls to show
// that the audit rows it provoked carry no secret (A-01, D-23): the Writer already refuses a secret-named detail key,
// and this also checks the values, the reason and the target against the secrets the test itself planted.
package audittest

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/store"
)

// secretWords are the fragments that make a detail key a secret key. They are a copy of the Writer's unexported list;
// TestSecretWordsMatchTheWriter pins the two equal.
var secretWords = []string{"password", "token", "secret", "key", "cookie", "session", "csrf"}

// pemMarker opens every PEM block, a private key or a certificate chain among them.
const pemMarker = "-----BEGIN"

// SecretWords returns the key-name fragments AssertNoSecrets refuses, for the test that pins them to the Writer's.
func SecretWords() []string { return append([]string(nil), secretWords...) }

// AssertNoSecrets fails t once per leak in rows. A row leaks when a detail key contains a secret word (an empty value
// under such a key is the same leak: the key must not exist), when redact.MaskLogLine masks any of secrets in a detail
// value, the reason or the target, or when any of those holds a PEM marker. The failure names the action, the row ID
// and the key or field, never the secret.
func AssertNoSecrets(t testing.TB, rows []audit.Row, secrets ...string) {
	t.Helper()
	for _, r := range rows {
		for key, value := range r.Details {
			if word, ok := secretKey(key); ok {
				if value == "" {
					t.Errorf("audit row %s (%s): detail key %q is secret-bearing (%q) and empty; the row must not carry the key",
						r.ID, r.Action, key, word)
				} else {
					t.Errorf("audit row %s (%s): detail key %q is secret-bearing (%q)", r.ID, r.Action, key, word)
				}
			}
			checkValue(t, r, "detail "+key, value, secrets)
		}
		checkValue(t, r, "reason", r.Reason, secrets)
		checkValue(t, r, "target", r.TargetID, secrets)
	}
}

// secretKey reports whether key contains a secret word, and which.
func secretKey(key string) (string, bool) {
	lower := strings.ToLower(key)
	for _, w := range secretWords {
		if strings.Contains(lower, w) {
			return w, true
		}
	}
	return "", false
}

// checkValue fails t when value leaks one of secrets or carries a PEM marker.
func checkValue(t testing.TB, r audit.Row, field, value string, secrets []string) {
	t.Helper()
	if value == "" {
		return
	}
	if strings.Contains(value, pemMarker) {
		t.Errorf("audit row %s (%s): %s holds a PEM block", r.ID, r.Action, field)
	}
	if _, masked := redact.MaskLogLine(value, secrets); masked {
		t.Errorf("audit row %s (%s): %s holds a secret (<secret>)", r.ID, r.Action, field)
	}
}

// Rows reads every row of one action, newest first, across all environments.
func Rows(t testing.TB, st *store.Store, action string) []audit.Row {
	t.Helper()
	var all []audit.Row
	var cursor int64
	for {
		rows, next, err := audit.Query(context.Background(), st, audit.Filter{AnyEnv: true, Actions: []string{action}}, cursor, 100)
		if err != nil {
			t.Fatalf("audittest: read the rows of %s: %v", action, err)
		}
		all = append(all, rows...)
		if next == 0 {
			return all
		}
		cursor = next
	}
}
