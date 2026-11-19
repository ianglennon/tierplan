// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// putCertPEM is putCert that also hands back the chain and key PEM it stored, so a test can compare bytes.
func putCertPEM(t *testing.T, s *Store, host, source, issuer string, nb, na time.Time) (c Cert, chainPEM, keyPEM []byte) {
	t.Helper()
	ctx := context.Background()
	chainPEM, keyPEM = certAt(t, host, nb, na)
	var id string
	if source == SourceUpload {
		id = uuid.NewString()
		ts := toNanos(fixedNow)
		if err := s.st.Q.InsertIngressCertUploaded(ctx, sqlc.InsertIngressCertUploadedParams{
			ID: id, Hostname: host, EnvID: "env-1", DomainID: "dom-1", CreatedBy: "tester", CreatedAt: ts, UpdatedAt: ts,
		}); err != nil {
			t.Fatalf("insert the upload of %s: %v", host, err)
		}
	} else {
		pending, _, err := s.CreatePending(ctx, PendingInput{Hostname: host, EnvID: "env-1", DomainID: "dom-1", Issuer: issuer, CreatedBy: "tester"}, nil)
		if err != nil {
			t.Fatalf("CreatePending(%s): %v", host, err)
		}
		id = pending.ID
	}
	if err := s.write(ctx, []string{"env-1"}, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		return true, s.writeCertificate(ctx, q, id, chainPEM, keyPEM, "https://acme.test/acct/1", fixedNow)
	}); err != nil {
		t.Fatalf("write the certificate of %s: %v", host, err)
	}
	c, err := s.Cert(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c, chainPEM, keyPEM
}

// D-31 (tracer): a ready production certificate comes back with its chain and the key PEM exactly as sealed.
func TestServingReturnsTheStoredKeyPEMOfAReadyCertificate(t *testing.T) {
	ctx := context.Background()
	s, st := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	_, chainPEM, keyPEM := putCertPEM(t, s, "web.example.com", SourceACME, IssuerProduction, fixedNow.Add(-time.Hour), fixedNow.Add(60*24*time.Hour))

	version, got, err := s.Serving(ctx, "env-1", []string{"web.example.com"}, fixedNow)
	if err != nil {
		t.Fatalf("Serving: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Serving returned %d entries, want 1", len(got))
	}
	c := got[0]
	if c.Hostname != "web.example.com" || c.Reason != "" {
		t.Fatalf("entry = %v, want a servable web.example.com", c)
	}
	if !bytes.Equal(c.KeyPEM, keyPEM) {
		t.Error("the key PEM is not byte-identical to the PKCS#8 PEM that was sealed")
	}
	if !bytes.Equal(c.ChainPEM, chainPEM) {
		t.Error("the chain PEM is not byte-identical to the stored chain")
	}
	if want, _ := versions(t, st, "env-1"); version != want || version == 0 {
		t.Errorf("certs version = %d, want %d (non-zero)", version, want)
	}
}

// servingByHost runs Serving and returns its entries keyed by hostname, failing when a hostname repeats.
func servingByHost(t *testing.T, s *Store, hosts []string, now time.Time) (int64, map[string]ServingCert) {
	t.Helper()
	version, got, err := s.Serving(context.Background(), "env-1", hosts, now)
	if err != nil {
		t.Fatalf("Serving: %v", err)
	}
	out := map[string]ServingCert{}
	for _, c := range got {
		if _, dup := out[c.Hostname]; dup {
			t.Fatalf("Serving returned %s twice", c.Hostname)
		}
		out[c.Hostname] = c
	}
	return version, out
}

// D-09, RTE-01, CRT-09, PROH-21-01: every reason, and the certificates that are never served.
func TestServingReasons(t *testing.T) {
	s, st := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	seedEnv(t, st, "env-2", "two")
	day := 24 * time.Hour
	nb := fixedNow.Add(-day)

	putCert(t, s, "staging.example.com", SourceACME, IssuerStaging, nb, fixedNow.Add(60*day))
	putCert(t, s, "left23h.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(23*time.Hour))
	putCert(t, s, "left25h.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(25*time.Hour))
	putCert(t, s, "future.example.com", SourceACME, IssuerProduction, fixedNow.Add(time.Hour), fixedNow.Add(60*day))
	putCert(t, s, "upload.example.com", SourceUpload, "", nb, fixedNow.Add(30*day))
	donor := putCert(t, s, "donor.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(60*day))
	moved := putCert(t, s, "moved.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(60*day))
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET key_sealed = (SELECT key_sealed FROM ingress_certs WHERE id = ?) WHERE id = ?`, donor.ID, moved.ID); err != nil {
		t.Fatal(err)
	}
	foreign := putCert(t, s, "foreign.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(60*day))
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET env_id = 'env-2' WHERE id = ?`, foreign.ID); err != nil {
		t.Fatal(err)
	}

	hosts := []string{
		"nothing.example.com", "staging.example.com", "left23h.example.com", "left25h.example.com",
		"future.example.com", "upload.example.com", "donor.example.com", "moved.example.com", "foreign.example.com",
	}
	_, got := servingByHost(t, s, hosts, fixedNow)

	for _, tc := range []struct {
		host   string
		reason string
		source string
	}{
		{"nothing.example.com", ReasonNoCertificate, ""},
		{"staging.example.com", ReasonNotReady, ""},
		{"left23h.example.com", ReasonNotReady, ""},
		{"left25h.example.com", "", SourceACME},
		{"future.example.com", ReasonNotReady, ""},
		{"upload.example.com", "", SourceUpload},
		{"donor.example.com", "", SourceACME},
		{"moved.example.com", ReasonNotReady, ""},
		{"foreign.example.com", ReasonNoCertificate, ""},
	} {
		c, ok := got[tc.host]
		if !ok {
			t.Errorf("%s: no entry", tc.host)
			continue
		}
		if c.Reason != tc.reason {
			t.Errorf("%s: reason = %q, want %q", tc.host, c.Reason, tc.reason)
		}
		if c.Source != tc.source {
			t.Errorf("%s: source = %q, want %q", tc.host, c.Source, tc.source)
		}
		servable := tc.reason == ""
		if servable != (len(c.ChainPEM) > 0 && len(c.KeyPEM) > 0) {
			t.Errorf("%s: chain %d bytes, key %d bytes, but reason %q", tc.host, len(c.ChainPEM), len(c.KeyPEM), c.Reason)
		}
	}

	// The same rule judged later: the 25 hour certificate is inside the margin an hour and a bit on.
	_, later := servingByHost(t, s, []string{"left25h.example.com"}, fixedNow.Add(90*time.Minute))
	if later["left25h.example.com"].Reason != ReasonNotReady {
		t.Errorf("at a later now the 25h certificate has reason %q, want %q: readiness is judged at the caller's now", later["left25h.example.com"].Reason, ReasonNotReady)
	}
	// And the future certificate becomes servable once its validity has begun.
	_, begun := servingByHost(t, s, []string{"future.example.com"}, fixedNow.Add(2*time.Hour))
	if begun["future.example.com"].Reason != "" {
		t.Errorf("once valid the certificate has reason %q, want none", begun["future.example.com"].Reason)
	}
}

// D-31: the candidates and their order are Servable's, and the entries are one per hostname, sorted.
func TestServingFollowsTheServableOrder(t *testing.T) {
	ctx := context.Background()
	s, st := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	day := 24 * time.Hour
	nb := fixedNow.Add(-day)

	// A live staging row over a retired ready production row: the retired one is served.
	_, retiredChain, retiredKey := putCertPEM(t, s, "swap.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(20*day))
	retire(t, st, mustCertOf(t, s, "swap.example.com"))
	putCert(t, s, "swap.example.com", SourceACME, IssuerStaging, nb, fixedNow.Add(60*day))

	// A live ready row and a retired ready row: the live one wins.
	both := putCert(t, s, "both.example.com", SourceUpload, "", nb, fixedNow.Add(30*day))
	retire(t, st, both)
	_, liveChain, liveKey := putCertPEM(t, s, "both.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(60*day))

	// Two retired ready rows and a live one that is not ready: the newer retirement wins.
	older, _, _ := putCertPEM(t, s, "history.example.com", SourceUpload, "", nb, fixedNow.Add(30*day))
	retire(t, st, older)
	newer, newerChain, newerKey := putCertPEM(t, s, "history.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(40*day))
	retire(t, st, newer)
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET retired_at = ? WHERE id = ?`, toNanos(fixedNow.Add(-time.Hour)), older.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreatePending(ctx, PendingInput{Hostname: "history.example.com", EnvID: "env-1", DomainID: "dom-1", Issuer: IssuerProduction, CreatedBy: "tester"}, nil); err != nil {
		t.Fatal(err)
	}

	// Out of order and with a duplicate (and a differently cased spelling of the same name).
	_, got, err := s.Serving(ctx, "env-1", []string{"swap.example.com", "both.example.com", "history.example.com", "BOTH.example.com", "swap.example.com"}, fixedNow)
	if err != nil {
		t.Fatalf("Serving: %v", err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Hostname)
	}
	if strings.Join(names, ",") != "both.example.com,history.example.com,swap.example.com" {
		t.Fatalf("entries = %v, want one per hostname, sorted", names)
	}
	by := map[string]ServingCert{}
	for _, c := range got {
		by[c.Hostname] = c
	}
	check := func(host string, chain, key []byte, source string) {
		t.Helper()
		c := by[host]
		if c.Reason != "" || !bytes.Equal(c.ChainPEM, chain) || !bytes.Equal(c.KeyPEM, key) || c.Source != source {
			t.Errorf("%s: got %v, want the %s row's chain and key", host, c, source)
		}
	}
	check("swap.example.com", retiredChain, retiredKey, SourceACME)
	check("both.example.com", liveChain, liveKey, SourceACME)
	check("history.example.com", newerChain, newerKey, SourceACME)

	// Servable agrees on which row serves each hostname.
	want, err := s.Servable(ctx, "env-1")
	if err != nil {
		t.Fatalf("Servable: %v", err)
	}
	for _, w := range want {
		if c := by[w.Hostname]; !bytes.Equal(c.ChainPEM, w.ChainPEM) || !c.NotAfter.Equal(w.NotAfter) || c.Source != w.Source {
			t.Errorf("%s: Serving and Servable chose different rows", w.Hostname)
		}
	}
}

func mustCertOf(t *testing.T, s *Store, host string) Cert {
	t.Helper()
	c, found, err := s.LiveCert(context.Background(), host)
	if err != nil || !found {
		t.Fatalf("LiveCert(%s) = %v, %v", host, found, err)
	}
	return c
}

// Pitfall 5: the version comes back with the rows read after it, and a certificate written afterwards raises it.
func TestServingReadsTheVersionFirst(t *testing.T) {
	s, st := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	day := 24 * time.Hour
	putCert(t, s, "a.example.com", SourceACME, IssuerProduction, fixedNow.Add(-day), fixedNow.Add(60*day))

	// Before the environment has any certificate its version is 0 and nothing is an error.
	if v, got, err := s.Serving(context.Background(), "env-2", []string{"a.example.com"}, fixedNow); err != nil || v != 0 || len(got) != 1 || got[0].Reason != ReasonNoCertificate {
		t.Fatalf("Serving for an environment with no row = %d, %v, %v", v, got, err)
	}

	v1, first := servingByHost(t, s, []string{"a.example.com", "b.example.com"}, fixedNow)
	if want, _ := versions(t, st, "env-1"); v1 != want {
		t.Fatalf("version = %d, want certs_version %d", v1, want)
	}
	if first["b.example.com"].Reason != ReasonNoCertificate {
		t.Fatalf("b before it exists: %q", first["b.example.com"].Reason)
	}

	// A certificate written after the read is rendered again under a newer version: both move together.
	putCert(t, s, "b.example.com", SourceACME, IssuerProduction, fixedNow.Add(-day), fixedNow.Add(60*day))
	v2, second := servingByHost(t, s, []string{"a.example.com", "b.example.com"}, fixedNow)
	if v2 <= v1 {
		t.Errorf("version %d -> %d, want a raise for the new certificate", v1, v2)
	}
	if second["b.example.com"].Reason != "" {
		t.Errorf("b after it exists: reason %q, want servable", second["b.example.com"].Reason)
	}
}

// T-21-06: a ServingCert never prints its key or its chain, however it is formatted or logged.
func TestServingCertNeverPrintsItsKey(t *testing.T) {
	s, st := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	putCert(t, s, "web.example.com", SourceACME, IssuerProduction, fixedNow.Add(-time.Hour), fixedNow.Add(60*24*time.Hour))
	_, got := servingByHost(t, s, []string{"web.example.com"}, fixedNow)
	c := got["web.example.com"]
	if len(c.KeyPEM) == 0 {
		t.Fatal("the certificate is not servable, so the test would prove nothing")
	}

	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("serving", "cert", c)
	for name, out := range map[string]string{
		"%v":              fmt.Sprintf("%v", c),
		"%+v":             fmt.Sprintf("%+v", c),
		"%#v":             fmt.Sprintf("%#v", c),
		"%v of a pointer": fmt.Sprintf("%v", &c),
		"slog":            logged.String(),
	} {
		if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, "CERTIFICATE") {
			t.Errorf("%s shows PEM: %s", name, out)
		}
		if !strings.Contains(out, "web.example.com") {
			t.Errorf("%s does not name the hostname: %s", name, out)
		}
	}
	if !strings.Contains(logged.String(), `"key_len":`+fmt.Sprint(len(c.KeyPEM))) {
		t.Errorf("slog record does not show the key length: %s", logged.String())
	}
}
