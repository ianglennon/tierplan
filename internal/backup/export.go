// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// metaKept is the one app_meta row a backup keeps: the HMAC of the newest audit
// entry retention deleted, which anchors a pruned chain. Every other row is
// about the instance that made the backup (app version, key rotation, backup
// status) and means nothing on another.
const metaKept = "audit_last_pruned_hmac"

// ErrChainBroken is returned when the audit chain does not verify and the
// export was not told to go on regardless.
var ErrChainBroken = errors.New("the audit chain does not verify")

// ExportOptions configures Export.
type ExportOptions struct {
	// AppVersion is the version recorded in the header and the manifest.
	AppVersion string
	// InstanceName and PublicURL are shown by a restore before it replaces
	// anything.
	InstanceName, PublicURL string
	// AllowBrokenChain exports even when the audit chain does not verify; the
	// manifest then says chainVerified is false.
	AllowBrokenChain bool
	// Now stamps the backup. Nil means time.Now.
	Now func() time.Time
}

// ExportResult describes a finished export.
type ExportResult struct {
	Manifest Manifest
	// Size is the number of bytes written to the destination.
	Size int64
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Export writes one backup of the live database to dst, locked with passphrase.
//
// It never touches the live data: it copies the database with VACUUM INTO into a
// private directory under workDir, strips the tables D-29 excludes, re-wraps
// every sealed value from the live master key to a fresh per-backup export key
// (kept only in the encrypted manifest), compacts the copy with secure_delete on
// so the stripped rows cannot be recovered from free pages, and writes the
// manifest and the copy as the two entries of the container. The private
// directory is removed on every path.
func Export(ctx context.Context, st *store.Store, live *vault.Vault, workDir string, dst io.Writer, passphrase []byte, o ExportOptions) (ExportResult, error) {
	if utf8.RuneCount(passphrase) < MinPassphraseLen {
		return ExportResult{}, ErrPassphraseTooShort
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	verified, err := verifyChain(ctx, st, live, o.AllowBrokenChain)
	if err != nil {
		return ExportResult{}, err
	}
	schema, err := st.SchemaVersion(ctx)
	if err != nil {
		return ExportResult{}, err
	}

	dir, err := os.MkdirTemp(workDir, ".bayplan-backup-*")
	if err != nil {
		return ExportResult{}, fmt.Errorf("create backup work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	copyPath := filepath.Join(dir, store.DatabaseFile)
	if err := vacuumInto(ctx, st, copyPath); err != nil {
		return ExportResult{}, err
	}

	exportKey, err := vault.GenerateKey()
	if err != nil {
		return ExportResult{}, fmt.Errorf("generate backup export key: %w", err)
	}
	exportVault, err := vault.New(exportKey, "backup-export")
	if err != nil {
		return ExportResult{}, fmt.Errorf("prepare backup export key: %w", err)
	}
	counts, err := scrub(ctx, copyPath, live, exportVault)
	if err != nil {
		return ExportResult{}, err
	}

	created := o.Now().UTC()
	m := Manifest{
		Format: ManifestFormat, Created: created, AppVersion: o.AppVersion, Schema: schema,
		InstanceName: o.InstanceName, PublicURL: o.PublicURL, Counts: counts, ChainVerified: verified,
		ExportKey: base64.StdEncoding.EncodeToString(exportKey),
	}
	body, err := json.Marshal(m)
	if err != nil {
		return ExportResult{}, fmt.Errorf("encode backup manifest: %w", err)
	}
	db, err := os.Open(copyPath) //nolint:gosec // a file in the private work directory
	if err != nil {
		return ExportResult{}, fmt.Errorf("open the database copy: %w", err)
	}
	defer func() { _ = db.Close() }()
	info, err := db.Stat()
	if err != nil {
		return ExportResult{}, fmt.Errorf("measure the database copy: %w", err)
	}

	kdf, err := DefaultKDF()
	if err != nil {
		return ExportResult{}, err
	}
	cw := &countingWriter{w: dst}
	w, err := NewWriter(cw, Header{KDF: kdf, Created: created, AppVersion: o.AppVersion, Schema: schema}, passphrase)
	if err != nil {
		return ExportResult{}, err
	}
	if err := w.Add(EntryManifest, int64(len(body)), bytes.NewReader(body)); err != nil {
		return ExportResult{}, err
	}
	if err := w.Add(EntryDB, info.Size(), db); err != nil {
		return ExportResult{}, err
	}
	if err := w.Close(); err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Manifest: m, Size: cw.n}, nil
}

// verifyChain reports whether the audit chain verifies. A broken chain is an
// error unless allow is set.
func verifyChain(ctx context.Context, st *store.Store, live *vault.Vault, allow bool) (bool, error) {
	res, err := audit.Verify(ctx, st, live)
	if err != nil {
		return false, fmt.Errorf("verify the audit chain: %w", err)
	}
	if res.Broken == nil {
		return true, nil
	}
	if !allow {
		return false, fmt.Errorf("%w: %s", ErrChainBroken, res.Broken.Reason)
	}
	return false, nil
}

// vacuumInto copies the live database to path. VACUUM cannot run inside a
// transaction, so it gets a dedicated connection from the write pool, which is
// handed back before anything else needs it (the pool has one connection).
func vacuumInto(ctx context.Context, st *store.Store, path string) error {
	conn, err := st.Write.Conn(ctx)
	if err != nil {
		return fmt.Errorf("copy the database: %w", err)
	}
	_, err = conn.ExecContext(ctx, "VACUUM INTO ?", path)
	closeErr := conn.Close()
	if err != nil {
		return fmt.Errorf("copy the database: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("copy the database: %w", closeErr)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict the database copy: %w", err)
	}
	return nil
}

// scrub turns the copy at path into what a backup carries and returns its
// headline counts. Everything runs on one connection with secure_delete on, in
// one transaction, and ends with VACUUM so no stripped row survives in a free
// page; the journal is returned to DELETE so the file is a single self-contained
// database.
func scrub(ctx context.Context, path string, live, export *vault.Vault) (Counts, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return Counts{}, fmt.Errorf("open the database copy: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return Counts{}, fmt.Errorf("open the database copy: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "PRAGMA secure_delete = ON"); err != nil {
		return Counts{}, fmt.Errorf("turn secure_delete on: %w", err)
	}
	// The copy of a WAL database may still be in WAL mode; the backup should be
	// one file, so leave the journal mode before the rows change.
	if _, err := conn.ExecContext(ctx, "PRAGMA journal_mode = DELETE"); err != nil {
		return Counts{}, fmt.Errorf("set the database copy's journal mode: %w", err)
	}
	if err := stripAndRewrap(ctx, conn, live, export); err != nil {
		return Counts{}, err
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return Counts{}, fmt.Errorf("compact the database copy: %w", err)
	}
	counts, err := count(ctx, conn)
	if err != nil {
		return Counts{}, err
	}
	if err := conn.Close(); err != nil {
		return Counts{}, fmt.Errorf("close the database copy: %w", err)
	}
	if err := db.Close(); err != nil {
		return Counts{}, fmt.Errorf("close the database copy: %w", err)
	}
	return counts, nil
}

func stripAndRewrap(ctx context.Context, conn *sql.Conn, live, export *vault.Vault) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("prepare the database copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	excluded := make([]string, 0, len(Tables))
	for name, class := range Tables {
		if class == Excluded {
			excluded = append(excluded, name)
		}
	}
	sort.Strings(excluded)
	for _, name := range excluded {
		// The names come from the static classification map.
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+name); err != nil { //nolint:gosec // identifiers come from the static table map
			return fmt.Errorf("remove %s from the database copy: %w", name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM app_meta WHERE key <> ?", metaKept); err != nil {
		return fmt.Errorf("clear instance bookkeeping from the database copy: %w", err)
	}
	if _, err := vault.RewrapAll(ctx, tx, live, export); err != nil {
		return fmt.Errorf("re-wrap secrets for the backup: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("prepare the database copy: %w", err)
	}
	return nil
}

func count(ctx context.Context, conn *sql.Conn) (Counts, error) {
	var c Counts
	for _, q := range []struct {
		dest *int64
		sql  string
	}{
		{&c.Users, "SELECT COUNT(*) FROM users WHERE deleted_at IS NULL"},
		{&c.Teams, "SELECT COUNT(*) FROM teams"},
		{&c.Environments, "SELECT COUNT(*) FROM environments"},
		{&c.AuditEntries, "SELECT COUNT(*) FROM audit_log"},
	} {
		if err := conn.QueryRowContext(ctx, q.sql).Scan(q.dest); err != nil {
			return Counts{}, fmt.Errorf("count the backup's contents: %w", err)
		}
	}
	return c, nil
}
