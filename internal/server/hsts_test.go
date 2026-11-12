// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/tls"
	"net/http"
	"testing"
)

// hstsHost is the public host the HSTS tests serve and sign certificates for.
const hstsHost = "tierplan.example"

// hstsOverTLS asks the built server for /healthz over TLS with Host set to host.
func hstsOverTLS(t *testing.T, env map[string]string, host string, opts ...Option) string {
	t.Helper()
	return hstsFor(t, env, func(r *http.Request) {
		r.TLS = &tls.ConnectionState{}
		r.Host = host
	}, opts...)
}

// The toggle comes from TIERPLAN_HSTS: on over a certificate that verifies for the public host sends
// the first lifetime for that host and no other, off clears, and the self-signed certificate gets
// nothing whatever the variable says (D-10, D-14, T-16-29).
func TestHSTSFollowsTheEnvToggle(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &root, pkiFrom, pkiTo, hstsHost)
	crt, key := pkiWritePair(t, leaf)
	trusted := WithTrustRoots(pkiPool(root))
	files := func(extra map[string]string) map[string]string {
		env := map[string]string{
			"TIERPLAN_PUBLIC_URL":    "https://" + hstsHost,
			"TIERPLAN_TLS_CERT_FILE": crt,
			"TIERPLAN_TLS_KEY_FILE":  key,
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}

	t.Run("on, over TLS, for the public host", func(t *testing.T) {
		if got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), hstsHost, trusted); got != "max-age=300" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=300", got)
		}
	})
	t.Run("on, with a port and capitals in the Host header", func(t *testing.T) {
		if got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), "TierPlan.Example:8443", trusted); got != "max-age=300" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=300", got)
		}
	})
	t.Run("on, but plain HTTP", func(t *testing.T) {
		got := hstsFor(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), func(r *http.Request) { r.Host = hstsHost }, trusted)
		if got != "" {
			t.Errorf("Strict-Transport-Security = %q over plain HTTP, want none", got)
		}
	})
	t.Run("on, but another Host header", func(t *testing.T) {
		for _, host := range []string{"other.example", "evil." + hstsHost, hstsHost + ".evil.example", "example.com", ""} {
			if got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), host, trusted); got != "" {
				t.Errorf("Host %q: Strict-Transport-Security = %q, want none", host, got)
			}
		}
	})
	t.Run("unset clears", func(t *testing.T) {
		if got := hstsOverTLS(t, files(nil), hstsHost, trusted); got != "max-age=0" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=0", got)
		}
	})
	t.Run("off clears", func(t *testing.T) {
		if got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "off"}), hstsHost, trusted); got != "max-age=0" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=0", got)
		}
	})
	t.Run("self-signed sends nothing even when on", func(t *testing.T) {
		env := map[string]string{"TIERPLAN_PUBLIC_URL": "https://" + hstsHost, "TIERPLAN_HSTS": "on"}
		if got := hstsOverTLS(t, env, hstsHost); got != "" {
			t.Errorf("Strict-Transport-Security = %q, want none", got)
		}
	})
	t.Run("never a directive beyond max-age", func(t *testing.T) {
		got := hstsOverTLS(t, files(map[string]string{"TIERPLAN_HSTS": "on"}), hstsHost, trusted)
		if got != "max-age=300" {
			t.Errorf("Strict-Transport-Security = %q, want exactly max-age=300", got)
		}
	})
}
