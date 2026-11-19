// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"context"
	"database/sql"
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
