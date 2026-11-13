// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"slices"
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

	t.Run("restore pause raises and Resume clears", func(t *testing.T) {
		r := newACMERig(t, acmetest.Options{}, nil)
		srv := r.startNew()
		client := r.client(r.mirrorPool())
		cookie := r.signIn(client)
		csrf := r.csrf(client, cookie)
		if got := httpsIDs(r.bellItems(client, cookie)); len(got) != 0 {
			t.Fatalf("before the restore: %v, want no https_ item", got)
		}

		// What a restore leaves behind: the pause, as acme_restore_test.go finds it after the swap.
		ctx := context.Background()
		store := acme.NewStore(srv.Store(), srv.Vault())
		st, err := store.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		st.PausedReason = acme.PausedRestore
		if err := store.SaveState(ctx, st); err != nil {
			t.Fatal(err)
		}
		it := r.bellItem(client, cookie, "https_acme_paused")
		if it == nil || it.HTTPS == nil || it.HTTPS.Reason != acme.PausedRestore {
			t.Fatalf("after the restore: %v, want https_acme_paused with reason restore", httpsIDs(r.bellItems(client, cookie)))
		}

		req, err := http.NewRequestWithContext(testsupport.Context(t), http.MethodPost, r.base+"/api/https/acme/resume", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(cookie)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST /api/https/acme/resume: %d, want 204", resp.StatusCode)
		}
		if got := httpsIDs(r.bellItems(client, cookie)); len(got) != 0 {
			t.Errorf("on the first read after Resume: %v, want no https_ item", got)
		}
	})

	t.Run("expiry fallback", func(t *testing.T) {
		r := newACMERig(t, acmetest.Options{}, nil)
		srv := r.startNew()
		cookie := r.signIn(r.client(r.mirrorPool()))
		r.issue()
		first := leafOf(t, srv.https.Config())
		if got := httpsIDs(r.bellItems(r.client(r.fake.IssuerPool()), cookie)); len(got) != 0 {
			t.Fatalf("with a fresh Let's Encrypt certificate: %v, want no https_ item", got)
		}

		// One renewal fails and the clock jumps past the end of the certificate: the self-signed one is served.
		r.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: 400, Type: "malformed", Detail: "scripted refusal"})
		r.step(first.NotAfter.Sub(r.clock.Now()) + time.Hour)
		r.awaitRuns()

		client := r.client(r.mirrorPool())
		items := r.bellItems(client, cookie)
		ids := httpsIDs(items)
		if len(ids) == 0 || ids[0] != "https_cert_fallback" {
			t.Fatalf("after the expiry: %v, want https_cert_fallback first", ids)
		}
		if slices.Contains(ids, "https_cert_expiring") {
			t.Errorf("after the expiry: %v, want no https_cert_expiring beside the fallback", ids)
		}
		fb := r.bellItem(client, cookie, "https_cert_fallback")
		if fb.HTTPS == nil || fb.HTTPS.NotAfter == nil || !fb.HTTPS.NotAfter.Equal(first.NotAfter) {
			t.Errorf("the fallback item = %+v, want notAfter %s", fb.HTTPS, first.NotAfter)
		}
	})
}
