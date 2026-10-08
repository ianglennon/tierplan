// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// rotateFixture is a data directory on a file-sourced key: a migrated database
// with an agent CA and a backup-schedule passphrase sealed under that key, and
// twenty audit entries.
type rotateFixture struct {
	dir string
	st  *store.Store
	old *vault.Vault
}

func newRotateFixture(t *testing.T) *rotateFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := store.Migrate(ctx, st, store.MigrateOptions{AppVersion: "test", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	old, err := vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f := &rotateFixture{dir: dir, st: st, old: old}

	ca, err := old.Seal(vault.ContextAgentCA+"env-1", []byte("ca private key"))
	if err != nil {
		t.Fatal(err)
	}
	pass, err := old.Seal(vault.ContextBackupSchedule, []byte("schedule passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'local', 'This host', '/var/run/docker.sock', 1, 1, 1)`)
	f.exec(t, `INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES ('env-1', 'pem', ?, 1)`, ca)
	f.exec(t, `INSERT INTO backup_schedule (id, passphrase_sealed) VALUES (1, ?)`, pass)
	w := audit.NewWriter(st, old, slog.New(slog.DiscardHandler), time.Now)
	for range 20 {
		if err := w.Record(ctx, audit.Entry{
			Actor:  audit.Actor{Type: audit.ActorUser, ID: "u", Name: "alice"},
			Action: audit.ActionAuthLogin, Result: audit.ResultSuccess,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *rotateFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.Write.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *rotateFixture) scalar(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s string
	if err := f.st.Read.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func TestRotateKeyMovesEverySecretAndTheWholeChainToTheNewKey(t *testing.T) {
	f := newRotateFixture(t)
	ctx := context.Background()
	oldCA := f.scalar(t, `SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`)

	res, err := backup.RotateKey(ctx, f.st, f.dir, f.old, audit.SystemCLI)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if res.OldKID != f.old.KeyID() || res.NewKID == "" || res.NewKID == res.OldKID {
		t.Fatalf("result = %+v, want the old ID and a different new one", res)
	}
	if res.Rewrapped != 2 || res.Entries != 20 {
		t.Fatalf("rewrapped %d values and %d entries, want 2 and 20", res.Rewrapped, res.Entries)
	}
	if res.KeyFileSwapped != true {
		t.Fatalf("a file-sourced key must be swapped into place: %+v", res)
	}

	// The key files: new key in bayplan.key, old one kept, no .new left.
	if _, err := os.Stat(filepath.Join(f.dir, vault.NewKeyFile)); err == nil {
		t.Errorf("%s was left behind", vault.NewKeyFile)
	}
	current, err := vault.ReadKeyFile(f.dir, vault.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	oldKeyBytes, err := vault.ReadKeyFile(f.dir, vault.OldKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := vault.New(current, "file")
	prev, _ := vault.New(oldKeyBytes, "file")
	if cur.KeyID() != res.NewKID || prev.KeyID() != res.OldKID {
		t.Fatalf("bayplan.key holds %s and bayplan.key.old holds %s, want %s and %s", cur.KeyID(), prev.KeyID(), res.NewKID, res.OldKID)
	}
	if got := f.scalar(t, `SELECT value FROM app_meta WHERE key = 'active_key_id'`); got != res.NewKID {
		t.Fatalf("active_key_id = %s, want %s", got, res.NewKID)
	}

	// Every sealed value opens under the new key alone.
	for _, c := range []struct{ q, ctx, want string }{
		{`SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`, vault.ContextAgentCA + "env-1", "ca private key"},
		{`SELECT passphrase_sealed FROM backup_schedule WHERE id = 1`, vault.ContextBackupSchedule, "schedule passphrase"},
	} {
		plain, err := cur.Open(c.ctx, f.scalar(t, c.q))
		if err != nil || string(plain) != c.want {
			t.Errorf("%s under the new key = %q, %v", c.q, plain, err)
		}
	}
	if _, err := prev.Open(vault.ContextAgentCA+"env-1", f.scalar(t, `SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`)); err == nil {
		t.Error("the CA key still opens under the old key alone")
	}
	if f.scalar(t, `SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`) == oldCA {
		t.Error("the CA key ciphertext did not change")
	}

	// The chain verifies under the new key alone (attestation unchecked) and
	// with both keys (attestation checked).
	only, err := audit.Verify(ctx, f.st, cur)
	if err != nil || only.Broken != nil || only.AttestationsUnchecked != 1 || only.Entries != 21 {
		t.Fatalf("verify under the new key = %+v, %v (broken %+v)", only, err, only.Broken)
	}
	both, err := vault.Load("", f.dir)
	if err != nil {
		t.Fatal(err)
	}
	full, err := audit.Verify(ctx, f.st, both)
	if err != nil || full.Broken != nil || full.AttestationsUnchecked != 0 {
		t.Fatalf("verify with both keys = %+v, %v (broken %+v)", full, err, full.Broken)
	}

	// The last entry is key.rotated by system:cli with the documented details.
	var action, actor, details string
	if err := f.st.Read.QueryRow(`SELECT action, actor_id, details FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&action, &actor, &details); err != nil {
		t.Fatal(err)
	}
	if action != audit.ActionKeyRotated || actor != audit.SystemCLI.ID {
		t.Fatalf("last entry = %s by %s", action, actor)
	}
	for _, k := range []string{`"old_kid"`, `"new_kid"`, `"old_head"`, `"entries"`, `"attestation"`} {
		if !bytes.Contains([]byte(details), []byte(k)) {
			t.Errorf("key.rotated details %s lack %s", details, k)
		}
	}
}

func TestRotateKeyRefusesWhenAnOldKeyFileIsAlreadyThereAndChangesNothing(t *testing.T) {
	f := newRotateFixture(t)
	if err := vault.WriteKeyFile(f.dir, vault.OldKeyFile, bytes.Repeat([]byte{4}, 32)); err != nil {
		t.Fatal(err)
	}
	before := f.scalar(t, `SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`)

	_, err := backup.RotateKey(context.Background(), f.st, f.dir, f.old, audit.SystemCLI)
	if !errors.Is(err, backup.ErrOldKeyPresent) {
		t.Fatalf("RotateKey = %v, want ErrOldKeyPresent", err)
	}
	if f.scalar(t, `SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`) != before {
		t.Error("a refused rotation changed a sealed value")
	}
	if _, err := os.Stat(filepath.Join(f.dir, vault.NewKeyFile)); err == nil {
		t.Errorf("a refused rotation left %s", vault.NewKeyFile)
	}
}

var errCrash = errors.New("simulated crash")

// crashAt makes RotateKey fail with errCrash when it reaches point.
func crashAt(t *testing.T, point string) {
	t.Helper()
	restore := backup.SetRotateHook(func(p string) error {
		if p == point {
			return errCrash
		}
		return nil
	})
	t.Cleanup(restore)
}

func TestRotateKeyCrashAtEveryPointLeavesAnInstanceThatRecoversAndVerifies(t *testing.T) {
	cases := []struct {
		point      string
		wantNewKey bool // whether the recovered instance runs on the new key
	}{
		{backup.CrashNewKeyWritten, false},
		{backup.CrashBeforeCommit, false},
		{backup.CrashAfterCommit, true},
		{backup.CrashFirstRename, true},
	}
	for _, c := range cases {
		t.Run(c.point, func(t *testing.T) {
			f := newRotateFixture(t)
			ctx := context.Background()
			crashAt(t, c.point)

			if _, err := backup.RotateKey(ctx, f.st, f.dir, f.old, audit.SystemCLI); !errors.Is(err, errCrash) {
				t.Fatalf("RotateKey = %v, want the simulated crash", err)
			}

			// The next start: recovery first, then the key is loaded. LoadOrCreate
			// must find a key and never mint a fresh one.
			if _, err := vault.RecoverRotation(ctx, f.dir, false, func(ctx context.Context) (string, error) {
				return backup.StoredActiveKeyID(ctx, f.dir)
			}); err != nil {
				t.Fatalf("RecoverRotation: %v", err)
			}
			v, err := vault.LoadOrCreate("", f.dir, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.KeyID() != f.old.KeyID(); got != c.wantNewKey {
				t.Fatalf("recovered instance on the new key = %v, want %v (active %s, old %s)", got, c.wantNewKey, v.KeyID(), f.old.KeyID())
			}
			for _, name := range []string{vault.NewKeyFile} {
				if _, err := os.Stat(filepath.Join(f.dir, name)); err == nil {
					t.Errorf("%s remains after recovery", name)
				}
			}
			if plain, err := v.Open(vault.ContextAgentCA+"env-1", f.scalar(t, `SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`)); err != nil || string(plain) != "ca private key" {
				t.Errorf("CA key = %q, %v", plain, err)
			}
			res, err := audit.Verify(ctx, f.st, v)
			if err != nil || res.Broken != nil {
				t.Fatalf("verify = %+v, %v (broken %+v)", res, err, res.Broken)
			}
			if c.wantNewKey && res.AttestationsUnchecked != 0 {
				t.Errorf("the old key is kept, so the attestation must be checked: %+v", res)
			}
		})
	}
}

func TestRotateKeyCanBeRunAgainAfterACrashBeforeTheCommit(t *testing.T) {
	f := newRotateFixture(t)
	ctx := context.Background()
	restore := backup.SetRotateHook(func(p string) error {
		if p == backup.CrashBeforeCommit {
			return errCrash
		}
		return nil
	})
	if _, err := backup.RotateKey(ctx, f.st, f.dir, f.old, audit.SystemCLI); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	restore()
	// A rolled-back transaction left the database as it was.
	if _, err := backup.RotateKey(ctx, f.st, f.dir, f.old, audit.SystemCLI); !errors.Is(err, backup.ErrNewKeyPresent) {
		t.Fatalf("a second run beside a stale bayplan.key.new = %v, want ErrNewKeyPresent until recovery has run", err)
	}
	if _, err := vault.RecoverRotation(ctx, f.dir, false, func(ctx context.Context) (string, error) {
		return backup.StoredActiveKeyID(ctx, f.dir)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.RotateKey(ctx, f.st, f.dir, f.old, audit.SystemCLI); err != nil {
		t.Fatalf("RotateKey after recovery: %v", err)
	}
}

func TestRotateKeyThatFailsBeforeTheCommitRemovesItsNewKeyFile(t *testing.T) {
	f := newRotateFixture(t)
	// A sealed value that does not open makes the re-wrap fail inside the transaction.
	f.exec(t, `UPDATE backup_schedule SET passphrase_sealed = 'v1.k1-0000000000000000.AAAA' WHERE id = 1`)

	if _, err := backup.RotateKey(context.Background(), f.st, f.dir, f.old, audit.SystemCLI); err == nil {
		t.Fatal("RotateKey succeeded over a value it cannot open")
	}
	if _, err := os.Stat(filepath.Join(f.dir, vault.NewKeyFile)); err == nil {
		t.Errorf("a failed rotation left %s", vault.NewKeyFile)
	}
	if got := f.scalar(t, `SELECT COUNT(*) FROM app_meta WHERE key = 'active_key_id'`); got != "0" {
		t.Errorf("a failed rotation recorded an active key")
	}
}
