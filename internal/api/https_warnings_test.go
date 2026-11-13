// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/api"
)

// The https_ items of GET /api/warnings (phase 17, OBS-04, D-01 to D-04).

// httpsWarnings reads GET /api/warnings as the admin and returns the body and its items.
func (f *acmeFix) httpsWarnings(t *testing.T) (*httptest.ResponseRecorder, api.WarningsBody) {
	t.Helper()
	rec := f.get(t, "/api/warnings")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/warnings: %d %s", rec.Code, rec.Body)
	}
	var w api.WarningsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &w); err != nil {
		t.Fatalf("decode the warnings: %v", err)
	}
	return rec, w
}

func TestWarningsHTTPS(t *testing.T) {
	t.Parallel()

	t.Run("a failed renewal reaches the admin with its class and never the CA's words", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		if _, w := f.httpsWarnings(t); len(w.Items) != 0 {
			t.Fatalf("with a good certificate: %+v, want none", w.Items)
		}

		f.fake.FailValidation("tierplan.example.com", acmetest.Problem{Type: "connection", Detail: "ca-private-detail-1717"})
		f.clock.Advance(time.Minute)
		f.awaitRun(t, startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", "")))

		rec, w := f.httpsWarnings(t)
		if len(w.Items) != 1 || w.Items[0].ID != "https_renewal_failed" || w.Items[0].HTTPS == nil {
			t.Fatalf("got %+v, want one https_renewal_failed", w.Items)
		}
		h := w.Items[0].HTTPS
		if h.Class == "" || h.Host != "tierplan.example.com" || h.Port != 7480 || h.NoCertificate || h.NextAttemptAt == nil {
			t.Errorf("the facts are %+v, want class, host, port 7480, a next attempt and a certificate", h)
		}
		if strings.Contains(rec.Body.String(), "ca-private-detail-1717") {
			t.Errorf("the feed relays the CA's problem text: %s", rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("a restore pause on the real manager is listed as https_acme_paused with its reason", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		ctx := context.Background()
		st, err := f.store.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		st.PausedReason = acme.PausedRestore
		if err := f.store.SaveState(ctx, st); err != nil {
			t.Fatal(err)
		}
		rec, w := f.httpsWarnings(t)
		if len(w.Items) != 1 || w.Items[0].ID != "https_acme_paused" || w.Items[0].HTTPS == nil || w.Items[0].HTTPS.Reason != "restore" {
			t.Fatalf("got %s, want one https_acme_paused with reason restore", rec.Body)
		}
	})

	t.Run("a challenge record that could not be deleted is listed by name and never by value", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, acme.ChallengeDNS01, acmeOpts{})
		f.register(t, "")
		decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))
		f.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable})

		f.awaitRun(t, startRun(t, f.issue(t)))

		left := f.prov.Records()
		if len(left) != 1 {
			t.Fatalf("the provider holds %d records, want the one that could not be deleted", len(left))
		}
		rec, w := f.httpsWarnings(t)
		var txt *api.WarningBody
		for i := range w.Items {
			if w.Items[i].ID == "https_txt_left" {
				txt = &w.Items[i]
			}
		}
		if txt == nil || txt.HTTPS == nil || txt.HTTPS.Record != "_acme-challenge.tierplan.example.com" || txt.HTTPS.More != 0 {
			t.Fatalf("got %s, want https_txt_left naming _acme-challenge.tierplan.example.com", rec.Body)
		}
		body := rec.Body.String()
		for what, secret := range map[string]string{"value": left[0].Value(), "provider ID": left[0].ID, "token": dnsAPIToken} {
			if secret != "" && strings.Contains(body, secret) {
				t.Errorf("the feed carries the record's %s: %s", what, body)
			}
		}
	})

	t.Run("a user without settings.read is refused and told nothing", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		f.fake.FailValidation("tierplan.example.com", acmetest.Problem{Type: "connection", Detail: "refused"})
		f.clock.Advance(time.Minute)
		f.awaitRun(t, startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", "")))

		f.addUser(t, "viewer", false)
		viewer, _ := f.signIn(t, "viewer", false)
		rec := f.call(t, authReq{method: http.MethodGet, path: "/api/warnings", cookie: viewer})
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "https_") {
			t.Fatalf("GET /api/warnings as a non-admin: %d %s, want 403 with no https_ item", rec.Code, rec.Body)
		}
	})

	t.Run("every field of every kind reaches the JSON under its documented name", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		at := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
		f.httpsAlerts = func(context.Context) ([]acme.Alert, error) {
			return []acme.Alert{
				{Kind: acme.AlertFallback, NotAfter: at},
				{
					Kind: acme.AlertRenewalFailed, Class: "validation_dns", Host: "h.example.com", Profile: "classic",
					TermsURL: "https://ca.example/tos", Zone: "example.com", Target: "t.example.net",
					Resolvers: []string{"1.1.1.1", "9.9.9.9"}, NoCertificate: true, NextAttemptAt: at,
				},
				{Kind: acme.AlertPaused, Reason: acme.BlockToldToWait, Until: at},
				{Kind: acme.AlertClockOff, Offset: -90 * time.Second, MeasuredAt: at},
				{Kind: acme.AlertExpiring, Source: api.SourceCertFiles, NotAfter: at},
				{Kind: acme.AlertTXTLeft, Record: "_acme-challenge.example.com", More: 2, RetryPaused: true},
			}, nil
		}
		f.h = f.build(t)

		rec, _ := f.httpsWarnings(t)
		var raw struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		wantIDs := []string{"https_cert_fallback", "https_renewal_failed", "https_acme_paused", "https_clock_off", "https_cert_expiring", "https_txt_left"}
		wantFields := [][]string{
			{"notAfter"},
			{"class", "host", "port", "profile", "termsUrl", "zone", "target", "resolvers", "noCertificate", "nextAttemptAt"},
			{"reason", "until"},
			{"offsetSeconds", "measuredAt"},
			{"source", "notAfter"},
			{"record", "more", "retryPaused"},
		}
		if len(raw.Items) != len(wantIDs) {
			t.Fatalf("got %d items, want %d: %s", len(raw.Items), len(wantIDs), rec.Body)
		}
		for i, item := range raw.Items {
			var id string
			if err := json.Unmarshal(item["id"], &id); err != nil || id != wantIDs[i] {
				t.Errorf("item %d id = %q (%v), want %q", i, id, err, wantIDs[i])
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(item["https"], &body); err != nil {
				t.Fatalf("item %d has no https body: %s", i, rec.Body)
			}
			for _, name := range wantFields[i] {
				if _, ok := body[name]; !ok {
					t.Errorf("%s has no %q field: %s", wantIDs[i], name, item["https"])
				}
			}
		}
		var clock api.WarningHTTPSBody
		if err := json.Unmarshal(raw.Items[3]["https"], &clock); err != nil || clock.OffsetSeconds != -90 {
			t.Errorf("clock_off offsetSeconds = %d (%v), want -90", clock.OffsetSeconds, err)
		}
	})
}
