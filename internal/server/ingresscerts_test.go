// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Phase 20, CRT-03: the server builds the per-hostname certificate manager, starts it before it listens (a certificate
// that was ordering when the last process stopped is queued again), runs it in the background, and serves its Admin
// routes: an Admin's list answers 200, not 503 ingress_unavailable.
func TestServerMountsIngressCertificateRoutesAndStartsTheManager(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx := testsupport.Context(t)
	clock := acmetest.NewClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	prov := dnstest.NewFakeProvider()
	opts := []Option{
		WithACMEClock(clock),
		WithACMEDNS(acme.DNSDeps{Providers: map[string]dns.Factory{"cloudflare": prov.Factory("token")}}),
	}
	cfg := func() map[string]string { return map[string]string{"TIERPLAN_DATA_DIR": dir} }

	// The first process leaves a certificate mid-order, as a crash would.
	first, err := Build(ctx, testConfig(t, cfg()), slog.New(slog.DiscardHandler), opts...)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-c', 'agent', 'Certs env', '', 1, 1, 1)`,
		`INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, phase, created_at, updated_at)
		 VALUES ('cert-1', 'web.apps.example.com', 'env-c', 'dom-1', 'acme', 'production', 'ordering', 1, 1)`,
	} {
		if _, err := first.Store().Write.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// The second process starts the manager before it serves, and the routes are mounted.
	ts := httptest.NewUnstartedServer(nil)
	srv, err := Build(ctx, testConfig(t, cfg()), slog.New(slog.DiscardHandler), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()
	ts.StartTLS()
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close()
	})
	if srv.ingressCerts == nil || srv.ingressCertStore == nil {
		t.Fatal("the server built no certificate manager")
	}
	var ordering int
	if err := srv.Store().Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingress_certs WHERE phase = 'ordering'`).Scan(&ordering); err != nil || ordering != 0 {
		t.Errorf("%d certificates are still ordering after start, %v, want none: Start queues them again", ordering, err)
	}
	var state int
	if err := srv.Store().Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingress_cert_state WHERE id = 1`).Scan(&state); err != nil || state != 1 {
		t.Errorf("ingress_cert_state rows = %d, %v, want the singleton", state, err)
	}

	cookie, _ := signInAdmin(t, srv, ts)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/env/env-c/ingress/certificates", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the certificates: %d %s, want 200 (not 503 ingress_unavailable)", resp.StatusCode, body)
	}
	// The first round may already have judged the seeded row (nothing claims it), so only the shape is asserted here;
	// the rows themselves are the API tests' business.
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Items == nil {
		t.Errorf("list = %s (%v), want an items array", body, err)
	}
	if strings.Contains(string(body), "PRIVATE KEY") {
		t.Error("the list holds key material")
	}
}
