// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

const (
	// stagingDir is where a restore prepares a database, under the data
	// directory so the final rename never crosses a file system.
	stagingDir = "restore-staging"
	// stagingMaxAge is how long a staging directory may sit before the next
	// restore removes it (T-03-68).
	stagingMaxAge = 15 * time.Minute
	// maxManifestSize bounds manifest.json, which is a few hundred bytes.
	maxManifestSize = 1 << 20
	// sqliteMagic starts every SQLite database file.
	sqliteMagic = "SQLite format 3\x00"
)

// ErrNewerBackup is returned for a backup made by a Bayplan whose database
// schema is newer than this binary's. There are no down-migrations, so it is
// refused like a downgrade, naming the version that made it (D-30).
var ErrNewerBackup = errors.New("backup was made by a newer Bayplan")

func newerBackup(appVersion string, schema, binarySchema int64) error {
	return fmt.Errorf("%w: it was made by Bayplan %s (schema version %d) and this Bayplan knows schema version %d; "+
		"restore it with Bayplan %s or newer", ErrNewerBackup, appVersion, schema, binarySchema, appVersion)
}

// Inspection is what a restore shows before it replaces anything: the outer
// header and the manifest from inside the encrypted stream.
type Inspection struct {
	Header   Header
	Manifest Manifest
}

// openChecked reads the header, refuses a newer backup before any key
// derivation runs, opens the stream with the passphrase and reads manifest.json.
// The reader is left at the database entry.
func openChecked(src io.Reader, passphrase []byte, binarySchema int64) (*Reader, Inspection, string, error) {
	h, raw, err := ReadHeader(src)
	if err != nil {
		return nil, Inspection{}, "", err
	}
	if h.Schema > binarySchema {
		return nil, Inspection{}, "", newerBackup(h.AppVersion, h.Schema, binarySchema)
	}
	r, err := Open(io.MultiReader(bytes.NewReader(raw), src), passphrase)
	if err != nil {
		return nil, Inspection{}, "", err
	}
	name, body, err := r.Next()
	if err != nil {
		return nil, Inspection{}, "", err
	}
	if name != EntryManifest {
		return nil, Inspection{}, "", fmt.Errorf("%w: the first entry is %q", ErrUnreadable, name)
	}
	data, err := io.ReadAll(io.LimitReader(body, maxManifestSize+1))
	if err != nil {
		return nil, Inspection{}, "", err
	}
	if len(data) > maxManifestSize {
		return nil, Inspection{}, "", fmt.Errorf("%w: the manifest is too large", ErrUnreadable)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, Inspection{}, "", fmt.Errorf("%w: the manifest is not valid", ErrUnreadable)
	}
	switch {
	case m.Format > ManifestFormat:
		return nil, Inspection{}, "", ErrNewerFormat
	case m.Format < 1:
		return nil, Inspection{}, "", fmt.Errorf("%w: the manifest has no format", ErrUnreadable)
	case m.Schema > binarySchema:
		return nil, Inspection{}, "", newerBackup(m.AppVersion, m.Schema, binarySchema)
	}
	key, err := base64.StdEncoding.DecodeString(m.ExportKey)
	if err != nil || len(key) != 32 {
		return nil, Inspection{}, "", fmt.Errorf("%w: the manifest has no usable export key", ErrUnreadable)
	}
	return r, Inspection{Header: h, Manifest: m}, m.ExportKey, nil
}

// Inspect reads a backup's header and manifest with the passphrase and nothing
// else: the database entry is not read, so a damaged tail is not found until
// Stage. A backup whose schema is above binarySchema is refused with
// ErrNewerBackup before the key derivation runs.
func Inspect(src io.Reader, passphrase []byte, binarySchema int64) (Inspection, error) {
	_, in, _, err := openChecked(src, passphrase, binarySchema)
	return in, err
}

// Staged is a backup decrypted to a private directory, waiting to be prepared
// and swapped in. Its directory is removed by Swap, by Discard and by any
// failure of Stage or Prepare.
type Staged struct {
	// Dir holds the staged bayplan.db.
	Dir        string
	Inspection Inspection

	exportKey string // base64, kept in memory only
	prepared  bool
}

// Stage decrypts the backup into <dataDir>/restore-staging/<random>/ and returns
// it, touching nothing else. Every damaged-file, wrong-passphrase and
// newer-backup failure happens here, before the live database is looked at. It
// first removes staging directories older than 15 minutes.
func Stage(ctx context.Context, src io.Reader, passphrase []byte, dataDir string, binarySchema int64) (*Staged, error) {
	r, in, exportKey, err := openChecked(src, passphrase, binarySchema)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := filepath.Join(dataDir, stagingDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create the restore staging directory: %w", err)
	}
	sweepStaging(root, time.Now())
	dir, err := os.MkdirTemp(root, "r-")
	if err != nil {
		return nil, fmt.Errorf("create the restore staging directory: %w", err)
	}
	s := &Staged{Dir: dir, Inspection: in, exportKey: exportKey}
	if err := s.writeDatabase(r); err != nil {
		s.Discard()
		return nil, err
	}
	return s, nil
}

// writeDatabase copies the database entry into the staging directory and
// checks that the stream ends where it should.
func (s *Staged) writeDatabase(r *Reader) error {
	name, body, err := r.Next()
	if err != nil {
		return err
	}
	if name != EntryDB {
		return fmt.Errorf("%w: the second entry is %q", ErrUnreadable, name)
	}
	path := filepath.Join(s.Dir, store.DatabaseFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a file in the private staging directory
	if err != nil {
		return fmt.Errorf("create the staged database: %w", err)
	}
	_, copyErr := io.Copy(f, body)
	if copyErr == nil {
		copyErr = f.Sync()
	}
	if err := f.Close(); copyErr == nil && err != nil {
		copyErr = err
	}
	if copyErr != nil {
		if errors.Is(copyErr, ErrUnreadable) {
			return copyErr
		}
		return fmt.Errorf("write the staged database: %w", copyErr)
	}
	// The end of the stream is authenticated: a truncated or extended file
	// fails here, not at some later step.
	if _, _, err := r.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("payload has an extra entry")
		}
		return fmt.Errorf("%w: %s", ErrUnreadable, err)
	}
	head := make([]byte, len(sqliteMagic))
	g, err := os.Open(path) //nolint:gosec // the file just written
	if err != nil {
		return fmt.Errorf("read the staged database: %w", err)
	}
	_, readErr := io.ReadFull(g, head)
	_ = g.Close()
	if readErr != nil || string(head) != sqliteMagic {
		return fmt.Errorf("%w: the database entry is not a SQLite database", ErrUnreadable)
	}
	return nil
}

// sweepStaging removes staging directories whose last change is older than
// stagingMaxAge. A restore that was killed leaves one behind; the plaintext
// database in it must not linger (T-03-68).
func sweepStaging(root string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < stagingMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// PrepareOptions configures Prepare.
type PrepareOptions struct {
	// Source is how the restore was started ("cli"), recorded in backup.restored.
	Source string
	// AppVersion is this binary's version, recorded as the instance's version.
	AppVersion string
	// Now stamps the audit entries. Nil means time.Now.
	Now func() time.Time
}

// Prepare turns the staged database into the one the target instance will run
// (D-29): it migrates forward to this binary's schema, re-wraps every sealed
// value from the backup's export key to the target master key, re-signs the
// audit chain under the target key, appends audit.reanchored and
// backup.restored, clears the key-backup acknowledgement, and verifies the new
// chain. Nothing outside the staging directory changes; on any error the
// staging directory is removed.
func (s *Staged) Prepare(ctx context.Context, target *vault.Vault, o PrepareOptions) (err error) {
	defer func() {
		if err != nil {
			s.Discard()
		}
	}()
	if o.Now == nil {
		o.Now = time.Now
	}
	key, err := base64.StdEncoding.DecodeString(s.exportKey)
	if err != nil {
		return fmt.Errorf("%w: the manifest has no usable export key", ErrUnreadable)
	}
	exportVault, err := vault.New(key, "backup-export")
	if err != nil {
		return fmt.Errorf("prepare the backup's export key: %w", err)
	}

	st, err := store.Open(ctx, s.Dir)
	if err != nil {
		return fmt.Errorf("open the staged database: %w", err)
	}
	closed := false
	closeStore := func() error {
		if closed {
			return nil
		}
		closed = true
		return st.Close()
	}
	defer func() { _ = closeStore() }()

	// The backup is migrated here, on the copy: the live database is never
	// migrated in place by a restore, and the copy needs no snapshot of itself.
	if _, err := store.Migrate(ctx, st, store.MigrateOptions{AppVersion: o.AppVersion, DataDir: s.Dir, SkipBackup: true}); err != nil {
		if errors.Is(err, store.ErrDowngrade) {
			binary, _ := store.TargetSchema()
			return newerBackup(s.Inspection.Manifest.AppVersion, s.Inspection.Manifest.Schema, binary)
		}
		return fmt.Errorf("migrate the backup: %w", err)
	}

	if err := s.rewrite(ctx, st, exportVault, target, o); err != nil {
		return err
	}
	res, err := audit.Verify(ctx, st, target)
	if err != nil {
		return fmt.Errorf("verify the restored audit chain: %w", err)
	}
	if res.Broken != nil {
		return fmt.Errorf("the restored audit chain does not verify (entry %d: %s)", res.Broken.Seq, res.Broken.Reason)
	}
	if err := closeStore(); err != nil {
		return fmt.Errorf("close the staged database: %w", err)
	}
	s.prepared = true
	return nil
}

// rewrite is Prepare's one write transaction.
func (s *Staged) rewrite(ctx context.Context, st *store.Store, export, target *vault.Vault, o PrepareOptions) error {
	m := s.Inspection.Manifest
	w := audit.NewWriter(st, target, slog.New(slog.DiscardHandler), o.Now)

	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("prepare the staged database: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	if _, err := vault.RewrapAll(ctx, tx, export, target); err != nil {
		return fmt.Errorf("re-wrap the backup's secrets: %w", err)
	}
	rekeyed, err := audit.Rekey(ctx, tx, st.Q, target)
	if err != nil {
		return fmt.Errorf("re-sign the backup's audit log: %w", err)
	}
	created := m.Created.UTC().Format(time.RFC3339)
	if err := w.AppendReanchorTx(ctx, tx, rekeyed, m.InstanceName, created, m.ChainVerified); err != nil {
		return fmt.Errorf("record the audit re-anchor: %w", err)
	}
	if err := w.RecordTx(ctx, tx, audit.Entry{
		Actor:      audit.SystemRestore,
		Action:     audit.ActionBackupRestored,
		TargetType: "backup",
		Result:     audit.ResultSuccess,
		Details: map[string]string{
			"source_instance": m.InstanceName,
			"backup_created":  created,
			"app_version":     m.AppVersion,
			"schema":          strconv.FormatInt(m.Schema, 10),
			"source":          o.Source,
		},
	}); err != nil {
		return fmt.Errorf("record the restore: %w", err)
	}
	// The restored instance has a new master key the admin has not yet backed
	// up, so the acknowledgement of the old one does not carry over.
	if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", "key_backup_ack"); err != nil {
		return fmt.Errorf("clear the key backup acknowledgement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("prepare the staged database: %w", err)
	}
	return nil
}

// Swap replaces <dataDir>/bayplan.db with the prepared staged database and
// removes the staging directory. The replacement is one rename of a file that
// is complete and verified, so an interrupted restore leaves either the old
// database or the new one, never a mix. It returns the path of the pre-restore
// snapshot, empty when there was no database to replace.
func (s *Staged) Swap(ctx context.Context, dataDir string, now time.Time) (snapshotPath string, err error) {
	defer s.Discard()
	if !s.prepared {
		return "", errors.New("the staged database has not been prepared")
	}
	staged := filepath.Join(s.Dir, store.DatabaseFile)
	// A staged database that kept a journal would lose it in the rename.
	if info, err := os.Stat(staged + "-wal"); err == nil && info.Size() > 0 {
		return "", errors.New("the staged database still has an unfinished journal")
	}
	live := filepath.Join(dataDir, store.DatabaseFile)

	if err := syncFile(staged); err != nil {
		return "", fmt.Errorf("flush the staged database: %w", err)
	}
	// The database being replaced is saved first, and its journal is folded in
	// by closing it cleanly, so removing the journal files below loses nothing.
	if _, statErr := os.Stat(live); statErr == nil {
		snapshotPath, err = PreRestoreSnapshot(ctx, dataDir, now)
		if err != nil {
			return "", err
		}
		if err := prunePreRestore(filepath.Join(dataDir, backupsDir), preRestoreKept); err != nil {
			return "", fmt.Errorf("prune old pre-restore snapshots: %w", err)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", fmt.Errorf("look for %s: %w", live, statErr)
	}
	// The live database's journal files belong to the old file; left beside the
	// new one they would be replayed into it.
	for _, ext := range []string{"-wal", "-shm"} {
		if err := os.Remove(live + ext); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("remove %s: %w", live+ext, err)
		}
	}
	if err := os.Rename(staged, live); err != nil {
		return "", fmt.Errorf("move the restored database into place: %w", err)
	}
	if err := syncFile(dataDir); err != nil {
		return "", fmt.Errorf("flush the data directory: %w", err)
	}
	return snapshotPath, nil
}

// syncFile fsyncs a file or a directory.
func syncFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // a path inside the data directory
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Discard removes the staging directory. It is safe to call more than once.
func (s *Staged) Discard() {
	if s == nil || s.Dir == "" {
		return
	}
	_ = os.RemoveAll(s.Dir)
	// The parent goes too when nothing else is staged; Remove fails on a
	// directory that still holds another restore's files, which is what we want.
	_ = os.Remove(filepath.Dir(s.Dir))
}
