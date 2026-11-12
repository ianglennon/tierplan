// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The https_ items of GET /api/warnings through the real server (phase 17, OBS-04, D-02 to D-05): each
// is raised by its cause and is gone on the first read after the cause clears, on the injected ACME clock.

// bellItem is the https_ item with the given ID in the warnings the admin reads, or nil.
func (r *acmeRig) bellItem(c *http.Client, cookie *http.Cookie, id string) *api.WarningBody {
	r.t.Helper()
	for _, it := range r.bellItems(c, cookie) {
		if it.ID == id {
			return &it
		}
	}
	return nil
}

// bellItems is every item GET /api/warnings lists for the admin.
func (r *acmeRig) bellItems(c *http.Client, cookie *http.Cookie) []api.WarningBody {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodGet, r.base+"/api/warnings", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	var w api.WarningsBody
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &w) != nil {
		r.t.Fatalf("GET /api/warnings: %d %s", resp.StatusCode, body)
	}
	return w.Items
}

// httpsIDs is the IDs of the https_ items, in the order listed.
func httpsIDs(items []api.WarningBody) []string {
	var out []string
	for _, it := range items {
		if strings.HasPrefix(it.ID, "https_") {
			out = append(out, it.ID)
		}
	}
	return out
}

func TestACMEBell(t *testing.T) {
	t.Run("expiring certificate files", func(t *testing.T) {
		crt, key, leaf := suppliedPair(t, acmeHost)
		r := newACMERig(t, acmetest.Options{}, map[string]string{"TIERPLAN_TLS_CERT_FILE": crt, "TIERPLAN_TLS_KEY_FILE": key})
		r.startNew()
		// The supplied leaf is its own trust anchor: Go accepts a certificate that is in the pool itself.
		pool := x509.NewCertPool()
		pool.AddCert(leaf)
		client := r.client(pool)
		cookie := r.signIn(client)

		if got := httpsIDs(r.bellItems(client, cookie)); len(got) != 0 {
			t.Fatalf("with the files far from their end: %v, want no https_ item", got)
		}

		// Exactly the threshold before the end: still none. A minute inside it: listed.
		r.step(leaf.NotAfter.Add(-acme.ExpiringWarnWithin).Sub(r.clock.Now()))
		if got := httpsIDs(r.bellItems(client, cookie)); len(got) != 0 {
			t.Fatalf("at exactly 14 days left: %v, want no https_ item", got)
		}
		r.step(time.Minute)
		it := r.bellItem(client, cookie, "https_cert_expiring")
		if it == nil || it.HTTPS == nil {
			t.Fatalf("inside the threshold: %v, want https_cert_expiring", httpsIDs(r.bellItems(client, cookie)))
		}
		if it.HTTPS.Source != api.SourceCertFiles || it.HTTPS.NotAfter == nil || !it.HTTPS.NotAfter.Equal(leaf.NotAfter) {
			t.Errorf("https = %+v, want source cert_files and notAfter %s", it.HTTPS, leaf.NotAfter)
		}

		// Past the end the pair is still listed.
		r.step(acme.ExpiringWarnWithin)
		if r.bellItem(client, cookie, "https_cert_expiring") == nil {
			t.Error("a pair past its end is not listed")
		}
	})
}
