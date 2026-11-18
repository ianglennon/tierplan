// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backup_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/backup"
	"github.com/ianglennon/tierplan/internal/vault"
)

// TestIngressCertTablesTravelAsDecided pins the Phase 20 backup decisions (D-35, D-38): the certificates,
// the account, the state and the issuance ledger travel; an in-flight order never does.
func TestIngressCertTablesTravelAsDecided(t *testing.T) {
	if got, ok := backup.Tables["ingress_cert_orders"]; !ok || got != backup.Excluded {
		t.Errorf("ingress_cert_orders = %v (present %v), want Excluded", got, ok)
	}
	for _, name := range []string{"ingress_certs", "ingress_acme_account", "ingress_cert_state", "ingress_cert_issuances"} {
		if got, ok := backup.Tables[name]; !ok || got != backup.Included {
			t.Errorf("%s = %v (present %v), want Included", name, got, ok)
		}
	}
}

// TestExportKeyOpensTheIngressCertificateSealedValues seals one value in each new sealed column under the live
// key. After an export the certificate key and the account key open under the export key and not under the live
// key; the order key's row is excluded, so neither the row nor the live-key ciphertext of its key is in the
// backup.
func TestExportKeyOpensTheIngressCertificateSealedValues(t *testing.T) {
	f := newExportFixture(t)
	const directory = "https://acme.test/directory"
	certSealed := f.sealedFor(t, vault.ContextIngressCertKey+"cert-1", "certificate private key")
	orderSealed := f.sealedFor(t, vault.ContextIngressOrderKey+"order-1", "order private key")
	accountSealed := f.sealedFor(t, vault.ContextIngressACMEAccountKey+directory, "account private key")
	f.exec(t, `INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, key_sealed, created_at, updated_at)
	           VALUES ('cert-1', 'app.example.com', 'env-1', 'dom-1', 'acme', 'staging', ?, 1, 1)`, certSealed)
	f.exec(t, `INSERT INTO ingress_cert_orders (id, cert_id, directory_url, order_url, kind, names, cert_key_sealed, stage, created_at, updated_at)
	           VALUES ('order-1', 'cert-1', ?, 'https://acme.test/order/1', 'issue', '["app.example.com"]', ?, 'created', 1, 1)`, directory, orderSealed)
	f.exec(t, `INSERT INTO ingress_acme_account (directory_url, account_url, key_sealed, created_at) VALUES (?, 'https://acme.test/acct/1', ?, 1)`, directory, accountSealed)
	f.exec(t, `INSERT INTO ingress_cert_issuances (id, issuer, registered_domain, hostname, kind, issued_at) VALUES ('iss-1', 'staging', 'example.com', 'app.example.com', 'new', 1)`)
	f.exec(t, `UPDATE ingress_cert_state SET paused_reason = 'restore', terms_accepted_by = 'u1' WHERE id = 1`)

	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	o := openBackup(t, file, exportPass)
	raw, err := base64.StdEncoding.DecodeString(o.manifest.ExportKey)
	if err != nil || len(raw) != 32 {
		t.Fatalf("exportKey = %q, %v; want 32 base64 bytes", o.manifest.ExportKey, err)
	}
	export, err := vault.New(raw, "test")
	if err != nil {
		t.Fatal(err)
	}

	var cert, account string
	if err := o.db.QueryRow(`SELECT key_sealed FROM ingress_certs WHERE id = 'cert-1'`).Scan(&cert); err != nil {
		t.Fatalf("the certificate is missing from the backup: %v", err)
	}
	if err := o.db.QueryRow(`SELECT key_sealed FROM ingress_acme_account WHERE directory_url = ?`, directory).Scan(&account); err != nil {
		t.Fatalf("the ACME account is missing from the backup: %v", err)
	}
	if plain, err := export.Open(vault.ContextIngressCertKey+"cert-1", cert); err != nil || string(plain) != "certificate private key" {
		t.Fatalf("certificate key under the export key = %q, %v", plain, err)
	}
	if plain, err := export.Open(vault.ContextIngressACMEAccountKey+directory, account); err != nil || string(plain) != "account private key" {
		t.Fatalf("account key under the export key = %q, %v", plain, err)
	}
	if _, err := f.live.Open(vault.ContextIngressCertKey+"cert-1", cert); !errors.Is(err, vault.ErrUnknownKey) {
		t.Fatalf("the live key opens the backup's certificate key: %v", err)
	}
	if _, err := f.live.Open(vault.ContextIngressACMEAccountKey+directory, account); !errors.Is(err, vault.ErrUnknownKey) {
		t.Fatalf("the live key opens the backup's account key: %v", err)
	}

	// The order, its key and the live-key ciphertext of it do not travel.
	var orders int
	if err := o.db.QueryRow(`SELECT COUNT(*) FROM ingress_cert_orders`).Scan(&orders); err != nil || orders != 0 {
		t.Fatalf("ingress_cert_orders holds %d rows in the backup, %v; want none", orders, err)
	}
	if bytes.Contains(o.dbBytes, []byte(orderSealed)) {
		t.Error("the live-key ciphertext of an in-flight order's key is in the backup's database bytes")
	}

	// The ledger and the state travel.
	var issuances int
	var paused string
	if err := o.db.QueryRow(`SELECT COUNT(*) FROM ingress_cert_issuances`).Scan(&issuances); err != nil || issuances != 1 {
		t.Fatalf("ingress_cert_issuances holds %d rows in the backup, %v; want 1", issuances, err)
	}
	if err := o.db.QueryRow(`SELECT paused_reason FROM ingress_cert_state WHERE id = 1`).Scan(&paused); err != nil || paused != "restore" {
		t.Fatalf("ingress_cert_state paused_reason = %q, %v; want restore", paused, err)
	}
}
