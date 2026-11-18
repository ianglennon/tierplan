// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// fixedNow is the clock every test store runs on.
var fixedNow = time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

// newTestStore returns a Store over a migrated database, a vault in a temporary directory and a fixed clock,
// with the raw database for assertions the Store does not offer.
func newTestStore(t *testing.T) (*Store, *store.Store) {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	return NewStore(StoreDeps{St: st, Vault: v, Now: func() time.Time { return fixedNow }}), st
}

// seedEnv inserts a standalone environment.
func seedEnv(t *testing.T, st *store.Store, id, name string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES (?, 'agent', ?, '', 1, 1, 1, 'standalone')`,
		id, name); err != nil {
		t.Fatalf("seed environment %s: %v", id, err)
	}
}

// versions reads an environment's certs_version and config_version, 0 for both when it has no row.
func versions(t *testing.T, st *store.Store, envID string) (certs, config int64) {
	t.Helper()
	err := st.Read.QueryRow(
		`SELECT COALESCE((SELECT certs_version FROM ingress_env WHERE env_id = ?1), 0),
		        COALESCE((SELECT config_version FROM ingress_env WHERE env_id = ?1), 0)`, envID).Scan(&certs, &config)
	if err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	return certs, config
}

// selfSigned makes a P-256 key and a self-signed leaf for host, with the key as PKCS#8 PEM.
func selfSigned(t *testing.T, host string) (key *ecdsa.PrivateKey, chainPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(0xabc123), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: fixedNow.Add(-time.Hour), NotAfter: fixedNow.Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encode the key: %v", err)
	}
	return key,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

// TestStoreKeepsASealedCertificateUnderItsRowID is the tracer for CRT-01 and CRT-02: a certificate for a
// hostname is written, its key sealed under the row's id, and both read back; the key does not open in
// another row, and the write moved certs_version and not config_version.
func TestStoreKeepsASealedCertificateUnderItsRowID(t *testing.T) {
	s, st := newTestStore(t)
	ctx := context.Background()
	seedEnv(t, st, "env-1", "one")

	pending, created, err := s.CreatePending(ctx, PendingInput{
		Hostname: "app.example.com", EnvID: "env-1", DomainID: "dom-1", Issuer: IssuerStaging, CreatedBy: "tester",
	}, nil)
	if err != nil || !created {
		t.Fatalf("CreatePending = %v, created %v", err, created)
	}
	if pending.Phase != PhaseQueued || pending.Source != SourceACME || pending.Issuer != IssuerStaging || pending.Ready {
		t.Fatalf("a new row is queued, acme, staging and not ready; got %+v", pending)
	}
	certsBefore, configBefore := versions(t, st, "env-1")
	if certsBefore != 1 || configBefore != 0 {
		t.Fatalf("after CreatePending certs_version %d config_version %d, want 1 and 0", certsBefore, configBefore)
	}

	key, chainPEM, keyPEM := selfSigned(t, "app.example.com")
	if err := s.writeDurable(ctx, []string{"env-1", "env-1"}, "the test certificate", func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		return true, s.writeCertificate(ctx, q, pending.ID, chainPEM, keyPEM, "https://acme.test/acct/1", fixedNow)
	}); err != nil {
		t.Fatalf("write the certificate: %v", err)
	}

	got, err := s.Cert(ctx, pending.ID)
	if err != nil {
		t.Fatalf("Cert: %v", err)
	}
	if !bytes.Equal(got.ChainPEM, chainPEM) {
		t.Fatalf("the chain did not read back equal")
	}
	if strings.Contains(got.KeySealed, "PRIVATE KEY") || got.KeySealed == "" {
		t.Fatalf("the key is not stored as a vault envelope: %q", got.KeySealed)
	}
	if got.Phase != PhaseIdle || got.Serial != "abc123" || got.AccountURL != "https://acme.test/acct/1" ||
		got.NotAfter.Sub(fixedNow) < 89*24*time.Hour || got.NotAfter.Sub(fixedNow) > 90*24*time.Hour {
		t.Fatalf("an issued row is idle with the leaf's serial and validity; got %+v", got)
	}
	signer, err := s.OpenKey(got)
	if err != nil {
		t.Fatalf("OpenKey: %v", err)
	}
	opened, ok := signer.(*ecdsa.PrivateKey)
	if !ok || !opened.Equal(key) {
		t.Fatalf("the opened key is not the key that was written")
	}
	live, found, err := s.LiveCert(ctx, "APP.example.com.")
	if err != nil || !found || live.ID != pending.ID {
		t.Fatalf("LiveCert by a non-canonical name = %v, found %v, id %q", err, found, live.ID)
	}

	certsAfter, configAfter := versions(t, st, "env-1")
	if certsAfter != certsBefore+1 || configAfter != configBefore {
		t.Fatalf("a certificate write moves certs_version once (%d -> %d) and never config_version (%d -> %d)",
			certsBefore, certsAfter, configBefore, configAfter)
	}
	if v, err := s.CertsVersion(ctx, "env-1"); err != nil || v != certsAfter {
		t.Fatalf("CertsVersion = %d, %v; want %d", v, err, certsAfter)
	}

	// A key_sealed copied into another row does not open there: the envelope is bound to the row's id.
	other, _, err := s.CreatePending(ctx, PendingInput{
		Hostname: "other.example.com", EnvID: "env-1", DomainID: "dom-1", Issuer: IssuerStaging, CreatedBy: "tester",
	}, nil)
	if err != nil {
		t.Fatalf("CreatePending other: %v", err)
	}
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET key_sealed = ? WHERE id = ?`, got.KeySealed, other.ID); err != nil {
		t.Fatalf("copy the envelope: %v", err)
	}
	moved, err := s.Cert(ctx, other.ID)
	if err != nil {
		t.Fatalf("Cert other: %v", err)
	}
	if _, err := s.OpenKey(moved); err == nil {
		t.Fatalf("a key sealed for one certificate opened in another row")
	}
	if _, err := s.OpenKey(pending); err == nil {
		t.Fatalf("the pending row (loaded before the write) has no key, OpenKey must refuse it")
	}
}

// TestStoreRefusesASecondLiveRowForAHostname pins D-01: the database keeps one live row per hostname for
// the whole install, and a retired row may sit beside the live one.
func TestStoreRefusesASecondLiveRowForAHostname(t *testing.T) {
	s, st := newTestStore(t)
	ctx := context.Background()
	seedEnv(t, st, "env-1", "one")
	seedEnv(t, st, "env-2", "two")

	in := PendingInput{Hostname: "app.example.com", EnvID: "env-1", DomainID: "dom-1", Issuer: IssuerProduction, CreatedBy: "tester"}
	first, created, err := s.CreatePending(ctx, in, nil)
	if err != nil || !created {
		t.Fatalf("first CreatePending = %v, created %v", err, created)
	}
	versionAfterFirst, _ := versions(t, st, "env-1")

	again, created, err := s.CreatePending(ctx, in, nil)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second CreatePending = id %q created %v err %v; want the first row (%q) with created false", again.ID, created, err, first.ID)
	}
	// Another environment asking for the same hostname gets the same row: hostnames are install-wide.
	elsewhere := in
	elsewhere.EnvID = "env-2"
	if row, created, err := s.CreatePending(ctx, elsewhere, nil); err != nil || created || row.ID != first.ID {
		t.Fatalf("CreatePending from another environment = id %q created %v err %v; want the first row", row.ID, created, err)
	}
	if v, _ := versions(t, st, "env-1"); v != versionAfterFirst {
		t.Fatalf("a refused duplicate moved certs_version (%d -> %d)", versionAfterFirst, v)
	}
	if v, _ := versions(t, st, "env-2"); v != 0 {
		t.Fatalf("a refused duplicate raised env-2's certs_version to %d", v)
	}

	const raw = `INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, created_at, updated_at) VALUES (?, 'app.example.com', 'env-1', 'dom-1', 'acme', 1, 1)`
	if _, err := st.Write.Exec(raw, "raw-live"); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("a raw second live row = %v; want a UNIQUE refusal from ingress_certs_live", err)
	}

	// A retired row coexists with the live one, and a retired hostname can be asked for again.
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET retired_at = 5, retired_reason = 'replaced' WHERE id = ?`, first.ID); err != nil {
		t.Fatalf("retire: %v", err)
	}
	second, created, err := s.CreatePending(ctx, in, nil)
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("CreatePending after a retirement = id %q created %v err %v; want a new live row", second.ID, created, err)
	}
	if _, err := st.Write.Exec(raw, "raw-retired-then-live"); err == nil {
		t.Fatalf("a third live row was accepted")
	}
	rows, err := s.EnvCerts(ctx, "env-1")
	if err != nil || len(rows) != 2 {
		t.Fatalf("EnvCerts = %d rows, %v; want the retired and the live row", len(rows), err)
	}
	live, err := s.LiveCerts(ctx)
	if err != nil || len(live) != 1 || live[0].ID != second.ID {
		t.Fatalf("LiveCerts = %v, %v; want only the new row", live, err)
	}
}
