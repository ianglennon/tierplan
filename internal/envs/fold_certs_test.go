// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// A fold moves the Local environment's certificates to the swarm environment instead of losing
// them to the ON DELETE CASCADE of ingress_certs.env_id (migration 00045): the rows keep their
// ids, chains and sealed keys, and only their environment changes.

// certFixture is one ingress_certs row to insert.
type certFixture struct {
	id, hostname, envID, domainID, source, issuer string
	retiredAt                                     int64
	retiredReason, phase                          string
	chainPEM, keySealed                           string
}

func (f *foldRig) insertCert(t *testing.T, c certFixture) {
	t.Helper()
	f.mustExec(t, `INSERT INTO ingress_certs
		(id, hostname, env_id, domain_id, source, issuer, retired_at, retired_reason, chain_pem, key_sealed, phase, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1)`,
		c.id, c.hostname, c.envID, c.domainID, c.source, c.issuer, c.retiredAt, c.retiredReason, c.chainPEM, c.keySealed, c.phase)
}

// certRow is what a fold may have changed about a certificate row.
type certRow struct {
	envID, hostname, domainID, source string
	retiredAt                         int64
	chainPEM, keySealed               []byte
	updatedAt                         int64
}

func (f *foldRig) readCert(t *testing.T, id string) certRow {
	t.Helper()
	var r certRow
	err := f.st.Read.QueryRow(`SELECT env_id, hostname, domain_id, source, retired_at, chain_pem, key_sealed, updated_at FROM ingress_certs WHERE id = ?`, id).
		Scan(&r.envID, &r.hostname, &r.domainID, &r.source, &r.retiredAt, &r.chainPEM, &r.keySealed, &r.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("certificate %s was deleted by the fold", id)
	}
	if err != nil {
		t.Fatalf("read certificate %s: %v", id, err)
	}
	return r
}

func TestTracerFoldLocalMovesTheLocalEnvironmentsCertificatesToTheSwarmEnvironment(t *testing.T) {
	f := newFoldRig(t, nil)
	certs := []certFixture{
		{id: "cert-acme", hostname: "app.example.org", envID: f.localEnv.ID, domainID: "dom-gone", source: "acme", issuer: "production",
			phase: "idle", chainPEM: "-----BEGIN CERTIFICATE-----\nacme-chain\n-----END CERTIFICATE-----\n", keySealed: "sealed-acme-key"},
		{id: "cert-upload", hostname: "upload.example.org", envID: f.localEnv.ID, domainID: "dom-gone", source: "upload",
			phase: "idle", chainPEM: "-----BEGIN CERTIFICATE-----\nupload-chain\n-----END CERTIFICATE-----\n", keySealed: "sealed-upload-key"},
		{id: "cert-retired", hostname: "app.example.org", envID: f.localEnv.ID, domainID: "dom-gone", source: "acme", issuer: "production",
			retiredAt: 5, retiredReason: "replaced", phase: "idle",
			chainPEM: "-----BEGIN CERTIFICATE-----\nretired-chain\n-----END CERTIFICATE-----\n", keySealed: "sealed-retired-key"},
	}
	for _, c := range certs {
		f.insertCert(t, c)
	}

	f.fold(t)

	if _, err := f.svc.Get(context.Background(), f.localEnv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the local environment after a fold: %v, want ErrNotFound", err)
	}
	for _, c := range certs {
		got := f.readCert(t, c.id)
		if got.envID != f.env.ID {
			t.Errorf("%s: env_id = %q, want the swarm environment %q", c.id, got.envID, f.env.ID)
		}
		if got.hostname != c.hostname || got.domainID != c.domainID || got.source != c.source || got.retiredAt != c.retiredAt {
			t.Errorf("%s: hostname/domain/source/retired_at = %q/%q/%q/%d, want %q/%q/%q/%d",
				c.id, got.hostname, got.domainID, got.source, got.retiredAt, c.hostname, c.domainID, c.source, c.retiredAt)
		}
		if !bytes.Equal(got.chainPEM, []byte(c.chainPEM)) {
			t.Errorf("%s: chain_pem changed to %q", c.id, got.chainPEM)
		}
		if !bytes.Equal(got.keySealed, []byte(c.keySealed)) {
			t.Errorf("%s: key_sealed changed to %q", c.id, got.keySealed)
		}
	}
	var left, total int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_certs WHERE env_id = ?`, f.localEnv.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_certs`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if left != 0 || total != 3 {
		t.Errorf("certificate rows: %d still on the retired Local environment, %d in total, want 0 and 3", left, total)
	}
	rows := f.audits(t, "environment.local_folded")
	if len(rows) != 1 || !strings.Contains(rows[0].Details, `"certificates_moved":"3"`) {
		t.Fatalf("audit rows = %+v, want one carrying certificates_moved 3", rows)
	}
}
