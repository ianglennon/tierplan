// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backup_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/backup"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/vault"
)

// ingressPause is what the restored database says about the per-hostname certificate pause.
func ingressPause(t *testing.T, dataDir string) (reason string, updatedAt int64) {
	t.Helper()
	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Read.QueryRow(`SELECT paused_reason, updated_at FROM ingress_cert_state WHERE id = 1`).Scan(&reason, &updatedAt); err != nil {
		t.Fatal(err)
	}
	return reason, updatedAt
}

// addIngressCertificate puts one per-hostname certificate in the instance being backed up, its key sealed
// under the live key as the application does.
func (f *exportFixture) addIngressCertificate(t *testing.T) {
	t.Helper()
	sealed := f.sealedFor(t, vault.ContextIngressCertKey+"cert-1", "certificate private key")
	f.exec(t, `INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, key_sealed, created_at, updated_at)
	           VALUES ('cert-1', 'app.example.com', 'env-1', 'dom-1', 'acme', 'staging', ?, 1, 1)`, sealed)
}

// TestRestorePausesIngressCertificatesWhenTheBackupHasSome (D-32): a clone that holds a certificate could
// renew its names before anyone has looked, so the restore leaves the ingress certificates paused, stamped
// with the restore's own time.
func TestRestorePausesIngressCertificatesWhenTheBackupHasSome(t *testing.T) {
	f := newExportFixture(t)
	f.addIngressCertificate(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	staged, err := backup.Stage(ctx, bytes.NewReader(file), []byte(exportPass), dataDir, schemaOfBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	o := restoreOptions
	o.Now = func() time.Time { return at }
	if err := staged.Prepare(ctx, targetVault(t), o); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := staged.Swap(ctx, dataDir, at); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	reason, updatedAt := ingressPause(t, dataDir)
	if reason != "restore" || updatedAt != at.UnixNano() {
		t.Errorf("ingress_cert_state = paused %q, updated_at %d; want paused \"restore\" at %d", reason, updatedAt, at.UnixNano())
	}
}

// TestRestorePausesIngressForAnAcknowledgementOrAnAccountAlone: either could lead a clone to register and
// order, so neither needs a certificate beside it.
func TestRestorePausesIngressForAnAcknowledgementOrAnAccountAlone(t *testing.T) {
	cases := map[string]func(*exportFixture, *testing.T){
		"terms acknowledged": func(f *exportFixture, t *testing.T) {
			f.exec(t, `UPDATE ingress_cert_state SET terms_accepted_at = 5, terms_accepted_by = 'u1' WHERE id = 1`)
		},
		"ingress account": func(f *exportFixture, t *testing.T) {
			sealed := f.sealedFor(t, vault.ContextIngressACMEAccountKey+acmeDirectory, "account private key")
			f.exec(t, `INSERT INTO ingress_acme_account (directory_url, account_url, key_sealed, created_at) VALUES (?, 'https://acme.example/acct/2', ?, 1)`,
				acmeDirectory, sealed)
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			f := newExportFixture(t)
			seed(f, t)
			_, file, err := f.export(t, exportPass, testOptions)
			if err != nil {
				t.Fatal(err)
			}
			dataDir := t.TempDir()
			if _, err := restoreInto(t, file, dataDir, targetVault(t)); err != nil {
				t.Fatalf("restore: %v", err)
			}
			if reason, _ := ingressPause(t, dataDir); reason != "restore" {
				t.Errorf("paused_reason = %q, want \"restore\"", reason)
			}
		})
	}
}

// TestRestoreDoesNotPauseIngressWithNothingToProtect: no certificate, no account and no acknowledgement
// means nothing to spend, so the page does not show a pause nobody needs.
func TestRestoreDoesNotPauseIngressWithNothingToProtect(t *testing.T) {
	f := newExportFixture(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if _, err := restoreInto(t, file, dataDir, targetVault(t)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if reason, _ := ingressPause(t, dataDir); reason != "" {
		t.Errorf("paused_reason = %q on an instance with no ingress certificate, account or acknowledgement, want none", reason)
	}
}

// ingressTripwire makes any update of ingress_cert_state.paused_reason break the transaction at commit.
func ingressTripwire(f *exportFixture, t *testing.T) {
	t.Helper()
	f.exec(t, `CREATE TABLE tripwire_parent (id INTEGER PRIMARY KEY)`)
	f.exec(t, `CREATE TABLE tripwire_child (parent_id INTEGER REFERENCES tripwire_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	f.exec(t, `CREATE TRIGGER tripwire AFTER UPDATE OF paused_reason ON ingress_cert_state
		BEGIN INSERT INTO tripwire_child (parent_id) VALUES (999); END`)
}

// TestIngressPauseIsInTheRestoreTransaction gives the staged database a trigger that breaks the
// transaction at commit when the ingress pause is written. The restore then fails whole, with nothing
// staged and the live database untouched: the pause is written by the transaction that commits the
// restore, never after it. A backup with nothing to protect never writes the pause and restores fine.
func TestIngressPauseIsInTheRestoreTransaction(t *testing.T) {
	f := newExportFixture(t)
	f.addIngressCertificate(t)
	ingressTripwire(f, t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}

	dir, target, before := liveInstall(t)
	staged, err := backup.Stage(context.Background(), bytes.NewReader(file), []byte(exportPass), dir, schemaOfBinary(t))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := staged.Prepare(context.Background(), target, restoreOptions); err == nil {
		t.Fatal("Prepare succeeded although the transaction that writes the ingress pause cannot commit")
	}
	if _, err := os.Stat(staged.Dir); err == nil {
		t.Error("the staging directory survived a failed Prepare")
	}
	assertNoStaging(t, dir)
	if got := hashDatabase(t, dir); got != before {
		t.Error("the live database changed")
	}

	// With nothing to protect the pause is not written, the trigger never fires, and the backup restores.
	f2 := newExportFixture(t)
	ingressTripwire(f2, t)
	_, file2, err := f2.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoreInto(t, file2, t.TempDir(), targetVault(t)); err != nil {
		t.Errorf("a backup with nothing to protect failed to restore: %v", err)
	}
}

// ingressEnvPaused is the restored database's ingress_env row of an environment: whether it exists, the switch and the
// pause.
func ingressEnvPaused(t *testing.T, dataDir, envID string) (exists bool, enabled, paused int) {
	t.Helper()
	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	err = st.Read.QueryRow(`SELECT enabled, paused FROM ingress_env WHERE env_id = ?`, envID).Scan(&enabled, &paused)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, 0, 0
	case err != nil:
		t.Fatal(err)
	}
	return true, enabled, paused
}

// seedIngressEnvironments gives the instance being backed up three environments: one with ingress on, one that has a
// row with ingress off, and one that has no ingress_env row at all.
func (f *exportFixture) seedIngressEnvironments(t *testing.T) {
	t.Helper()
	f.exec(t, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES ('env-on', 'agent', 'On', '', 1, 1, 1, 'swarm')`)
	f.exec(t, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES ('env-off', 'agent', 'Off', '', 1, 1, 1, 'swarm')`)
	f.exec(t, `INSERT INTO ingress_env (env_id, enabled) VALUES ('env-on', 1)`)
	f.exec(t, `INSERT INTO ingress_env (env_id, enabled) VALUES ('env-off', 0)`)
}

// TestRestorePausesEveryIngressEnvironment (D-42): a clone restored from a backup observes every swarm and changes
// none until an Admin resumes it, whatever the environment's switch was. The environment that had no ingress state gets
// a paused row; the switches are not touched.
func TestRestorePausesEveryIngressEnvironment(t *testing.T) {
	f := newExportFixture(t)
	f.seedIngressEnvironments(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if _, err := restoreInto(t, file, dataDir, targetVault(t)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, c := range []struct {
		env         string
		wantEnabled int
	}{{"env-on", 1}, {"env-off", 0}, {"env-1", 0}} {
		exists, enabled, paused := ingressEnvPaused(t, dataDir, c.env)
		if !exists || paused != 1 || enabled != c.wantEnabled {
			t.Errorf("ingress_env of %s = exists %v enabled %d paused %d; want a row, enabled %d, paused 1", c.env, exists, enabled, paused, c.wantEnabled)
		}
	}
}

// TestIngressEnvPauseIsInTheRestoreTransaction: the environment pause is written by the transaction that commits the
// restore. A trigger on the paused column that breaks the transaction at commit makes the restore fail whole, with the
// live database untouched.
func TestIngressEnvPauseIsInTheRestoreTransaction(t *testing.T) {
	f := newExportFixture(t)
	f.seedIngressEnvironments(t)
	f.exec(t, `CREATE TABLE tripwire_parent (id INTEGER PRIMARY KEY)`)
	f.exec(t, `CREATE TABLE tripwire_child (parent_id INTEGER REFERENCES tripwire_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	f.exec(t, `CREATE TRIGGER tripwire_env AFTER UPDATE OF paused ON ingress_env
		BEGIN INSERT INTO tripwire_child (parent_id) VALUES (999); END`)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	dir, target, before := liveInstall(t)
	staged, err := backup.Stage(context.Background(), bytes.NewReader(file), []byte(exportPass), dir, schemaOfBinary(t))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := staged.Prepare(context.Background(), target, restoreOptions); err == nil {
		t.Fatal("Prepare succeeded although the transaction that pauses the ingress environments cannot commit")
	}
	assertNoStaging(t, dir)
	if got := hashDatabase(t, dir); got != before {
		t.Error("the live database changed")
	}
}
