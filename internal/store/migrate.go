// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

//go:embed migrations/*.sql
var migrationsEmbed embed.FS

const (
	// snapshotsKept is how many pre-migration snapshots survive (D-83).
	snapshotsKept = 3
	// freeSpaceFactor is the multiple of the database size that must be free
	// before a snapshot is attempted (D-83).
	freeSpaceFactor = 2

	backupsDir     = "backups"
	snapshotPrefix = "bayplan-"
	snapshotSuffix = ".db"
	// snapshotTimeLayout is the UTC timestamp in a snapshot's file name. It
	// sorts lexically in time order.
	snapshotTimeLayout = "20060102T150405Z"

	metaAppVersion = "app_version"
)

// ErrDowngrade is returned when the database was migrated by a newer Bayplan
// than the running binary (D-84). There are no down-migrations.
var ErrDowngrade = errors.New("the database schema is newer than this Bayplan version")

// MigrateOptions configures Migrate. The zero value of every function and FS
// field selects the production behaviour; tests inject the rest.
type MigrateOptions struct {
	// AppVersion is recorded in app_meta and names the next snapshot. Empty
	// means "dev".
	AppVersion string
	// DataDir holds bayplan.db and the backups directory.
	DataDir string
	// SkipBackup is BAYPLAN_SKIP_PRE_MIGRATE_BACKUP: no snapshot and no
	// free-space check (D-83).
	SkipBackup bool
	// Migrations overrides the embedded migrations. Files sit at the root.
	Migrations fs.FS
	// FreeBytes reports the bytes available to an unprivileged user at path.
	FreeBytes func(path string) (uint64, error)
	// Now supplies the snapshot timestamp.
	Now func() time.Time
}

// MigrateResult describes what Migrate did.
type MigrateResult struct {
	// From and To are the schema versions before and after.
	From, To int64
	// SnapshotPath is the pre-migration snapshot, empty when none was taken.
	SnapshotPath string
}

// embeddedFS returns the embedded migrations rooted at the migrations folder.
func embeddedFS() (fs.FS, error) {
	return fs.Sub(migrationsEmbed, "migrations")
}

// embeddedTarget is the highest schema version this binary embeds.
func embeddedTarget() (int64, error) {
	fsys, err := embeddedFS()
	if err != nil {
		return 0, err
	}
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return 0, err
	}
	var highest int64
	for _, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %q has no numeric version prefix", name)
		}
		highest = max(highest, v)
	}
	return highest, nil
}

// TargetSchema is the highest schema version this binary embeds. A restore
// compares a backup's schema with it before doing any work (D-30).
func TargetSchema() (int64, error) { return embeddedTarget() }

// Migrate brings the database to this binary's schema version (D-82 to D-84).
//
// Before applying pending migrations to an existing database it writes a
// VACUUM INTO snapshot under <DataDir>/backups and keeps the newest three.
// A database newer than the binary is refused with ErrDowngrade.
func Migrate(ctx context.Context, s *Store, o MigrateOptions) (MigrateResult, error) {
	if o.AppVersion == "" {
		o.AppVersion = "dev"
	}
	if o.Migrations == nil {
		fsys, err := embeddedFS()
		if err != nil {
			return MigrateResult{}, err
		}
		o.Migrations = fsys
	}
	if o.FreeBytes == nil {
		o.FreeBytes = freeBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}

	p, err := goose.NewProvider(goose.DialectSQLite3, s.Write, o.Migrations)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("prepare migrations: %w", err)
	}
	current, target, err := p.GetVersions(ctx)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("read schema version: %w", err)
	}
	res := MigrateResult{From: current, To: current}

	if current > target {
		newest, _ := newestSnapshot(o.DataDir)
		if newest == "" {
			newest = "none found in " + filepath.Join(o.DataDir, backupsDir)
		}
		return res, fmt.Errorf("%w: the database is at schema version %d but this binary only knows schema version %d; "+
			"run a newer Bayplan, or restore the newest backup: %s", ErrDowngrade, current, target, newest)
	}

	pending, err := p.HasPending(ctx)
	if err != nil {
		return res, fmt.Errorf("check pending migrations: %w", err)
	}
	if pending && current > 0 && !o.SkipBackup {
		if err := checkFreeSpace(s, o); err != nil {
			return res, err
		}
		res.SnapshotPath, err = snapshot(ctx, s, o)
		if err != nil {
			return res, err
		}
	}

	if pending {
		if _, err := p.Up(ctx); err != nil {
			return res, fmt.Errorf("apply migrations (database snapshot: %s): %w", describeSnapshot(res.SnapshotPath), err)
		}
	}
	res.To = target

	err = s.Q.UpsertMeta(ctx, sqlc.UpsertMetaParams{
		Key: metaAppVersion, Value: o.AppVersion, UpdatedAt: o.Now().UTC().UnixNano(),
	})
	if err != nil {
		return res, fmt.Errorf("record app version: %w", err)
	}
	s.schemaVersion.Store(target)
	return res, nil
}

func describeSnapshot(path string) string {
	if path == "" {
		return "none was taken"
	}
	return path
}

// checkFreeSpace refuses a snapshot when the data directory has less than
// twice the database (plus its WAL) free (D-83).
func checkFreeSpace(s *Store, o MigrateOptions) error {
	var used int64
	for _, path := range []string{s.Path, s.Path + "-wal"} {
		if st, err := os.Stat(path); err == nil {
			used += st.Size()
		}
	}
	need := uint64(used) * freeSpaceFactor //nolint:gosec // file sizes are never negative
	free, err := o.FreeBytes(o.DataDir)
	if err != nil {
		return fmt.Errorf("check free space in %s before migrating: %w", o.DataDir, err)
	}
	if free < need {
		return fmt.Errorf("not enough free space in %s to snapshot the database before migrating: "+
			"%d bytes free, %d needed (twice the database size); free some space, or set "+
			"BAYPLAN_SKIP_PRE_MIGRATE_BACKUP=true to migrate without a snapshot", o.DataDir, free, need)
	}
	return nil
}

// snapshot writes VACUUM INTO <DataDir>/backups/bayplan-<oldver>-<ts>.db and
// prunes older snapshots.
func snapshot(ctx context.Context, s *Store, o MigrateOptions) (string, error) {
	dir := filepath.Join(o.DataDir, backupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create backups directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, snapshotPrefix+oldVersion(ctx, s)+"-"+o.Now().UTC().Format(snapshotTimeLayout)+snapshotSuffix)

	// VACUUM cannot run inside a transaction, so it gets a dedicated
	// connection from the write pool. The path is a bound parameter.
	conn, err := s.Write.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("snapshot the database: %w", err)
	}
	_, err = conn.ExecContext(ctx, "VACUUM INTO ?", path)
	// The write pool has one connection: release it before anything else
	// (Up, Upsert) needs it.
	closeErr := conn.Close()
	if err != nil {
		return "", fmt.Errorf("snapshot the database to %s: %w", path, err)
	}
	if closeErr != nil {
		return "", fmt.Errorf("snapshot the database to %s: %w", path, closeErr)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("restrict snapshot permissions on %s: %w", path, err)
	}
	if err := pruneSnapshots(dir); err != nil {
		return path, fmt.Errorf("prune old snapshots in %s: %w", dir, err)
	}
	return path, nil
}

// oldVersion is the app version recorded by the previous run, made safe for a
// file name.
func oldVersion(ctx context.Context, s *Store) string {
	v, err := s.Q.GetMeta(ctx, metaAppVersion)
	if err != nil || v == "" {
		return "unknown"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '+', r == '-':
			return r
		}
		return '_'
	}, v)
}

// listSnapshots returns snapshot paths oldest first, ordered by the timestamp
// in the file name (the old version, which can contain "-", is ignored).
func listSnapshots(dataDir string) ([]string, error) {
	dir := filepath.Join(dataDir, backupsDir)
	return listSnapshotsIn(dir)
}

func listSnapshotsIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	type snap struct{ ts, name string }
	var snaps []snap
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
			continue
		}
		stem := strings.TrimSuffix(name, snapshotSuffix)
		if len(stem) < len(snapshotTimeLayout) {
			continue
		}
		ts := stem[len(stem)-len(snapshotTimeLayout):]
		if _, err := time.Parse(snapshotTimeLayout, ts); err != nil {
			continue
		}
		snaps = append(snaps, snap{ts, name})
	}
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].ts != snaps[j].ts {
			return snaps[i].ts < snaps[j].ts
		}
		return snaps[i].name < snaps[j].name
	})
	paths := make([]string, len(snaps))
	for i, sn := range snaps {
		paths[i] = filepath.Join(dir, sn.name)
	}
	return paths, nil
}

func newestSnapshot(dataDir string) (string, error) {
	paths, err := listSnapshots(dataDir)
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[len(paths)-1], nil
}

func pruneSnapshots(dir string) error {
	paths, err := listSnapshotsIn(dir)
	if err != nil {
		return err
	}
	var errs []error
	for len(paths) > snapshotsKept {
		errs = append(errs, os.Remove(paths[0]))
		paths = paths[1:]
	}
	return errors.Join(errs...)
}

// freeBytes is the space available to an unprivileged process at path.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec // block counts and sizes are never negative
}
