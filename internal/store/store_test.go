// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// plus returns the embedded baseline migration plus n extra migrations, each
// creating its own table, so a test can stage "a newer binary".
func plus(t *testing.T, n int) fstest.MapFS {
	t.Helper()
	base, err := os.ReadFile("migrations/00001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := fstest.MapFS{"00001_baseline.sql": {Data: base}}
	for i := 2; i < 2+n; i++ {
		m[fmt.Sprintf("%05d_extra%d.sql", i, i)] = &fstest.MapFile{Data: []byte(
			fmt.Sprintf("-- +goose Up\nCREATE TABLE extra%d (id INTEGER PRIMARY KEY);\n", i))}
	}
	return m
}

// embeddedTarget is the highest version in the real embedded migrations, so
// tests of the real set do not hard-code how many there are.
func embeddedTarget(t *testing.T) int64 {
	t.Helper()
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no embedded migrations found: %v", err)
	}
	var highest int64
	for _, f := range files {
		prefix, _, _ := strings.Cut(filepath.Base(f), "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %s has no numeric prefix", f)
		}
		highest = max(highest, v)
	}
	return highest
}

func open(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func snapshots(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "backups", "coaming-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func fixedClock(start time.Time) func() time.Time {
	t := start
	return func() time.Time { t = t.Add(time.Second); return t }
}

func TestMigrateFreshDatabase(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	res, err := store.Migrate(context.Background(), s, store.MigrateOptions{AppVersion: "1.0.0", DataDir: dir})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if want := embeddedTarget(t); res.From != 0 || res.To != want {
		t.Errorf("From, To = %d, %d; want 0, %d", res.From, res.To, want)
	}
	if res.SnapshotPath != "" || len(snapshots(t, dir)) != 0 {
		t.Errorf("a fresh database must not be snapshotted, got %q", res.SnapshotPath)
	}
	got, err := s.Q.GetMeta(context.Background(), "app_version")
	if err != nil || got != "1.0.0" {
		t.Errorf("app_version = %q, %v; want 1.0.0", got, err)
	}
}

func TestMigrateEmptyVersionIsDev(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(context.Background(), s, store.MigrateOptions{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Q.GetMeta(context.Background(), "app_version"); got != "dev" {
		t.Errorf("app_version = %q, want dev", got)
	}
}

func TestMigrateSnapshotsBeforePendingMigration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v1", DataDir: dir, Migrations: plus(t, 0)}); err != nil {
		t.Fatal(err)
	}
	res, err := store.Migrate(ctx, s, store.MigrateOptions{
		AppVersion: "v2", DataDir: dir, Migrations: plus(t, 1),
		Now: func() time.Time { return time.Date(2026, 10, 1, 12, 30, 5, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.From != 1 || res.To != 2 {
		t.Errorf("From, To = %d, %d; want 1, 2", res.From, res.To)
	}
	files := snapshots(t, dir)
	if len(files) != 1 {
		t.Fatalf("got %d snapshots, want 1: %v", len(files), files)
	}
	if !regexp.MustCompile(`coaming-v1-20261001T123005Z\.db$`).MatchString(files[0]) || files[0] != res.SnapshotPath {
		t.Errorf("snapshot %q (result %q) does not match backups/coaming-<oldver>-<ts>.db", files[0], res.SnapshotPath)
	}
	if st, err := os.Stat(files[0]); err != nil || st.Size() == 0 || st.Mode().Perm() != 0o600 {
		t.Errorf("snapshot stat = %v, %v; want a non-empty 0600 file", st, err)
	}
	if _, err := s.Write.ExecContext(ctx, "INSERT INTO extra2 (id) VALUES (1)"); err != nil {
		t.Errorf("the pending migration was not applied: %v", err)
	}
}

func TestMigrateKeepsNewestThreeSnapshots(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	clock := fixedClock(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v1", DataDir: dir, Now: clock, Migrations: plus(t, 0)}); err != nil {
		t.Fatal(err)
	}
	var newest string
	for i := 1; i <= 4; i++ {
		res, err := store.Migrate(ctx, s, store.MigrateOptions{
			AppVersion: fmt.Sprintf("v%d", i+1), DataDir: dir, Migrations: plus(t, i), Now: clock,
		})
		if err != nil {
			t.Fatalf("migration %d: %v", i, err)
		}
		newest = res.SnapshotPath
	}
	files := snapshots(t, dir)
	if len(files) != 3 {
		t.Fatalf("got %d snapshots, want 3: %v", len(files), files)
	}
	found := false
	for _, f := range files {
		if f == newest {
			found = true
		}
		if strings.Contains(f, "coaming-v1-") {
			t.Errorf("the oldest snapshot %s should have been pruned", f)
		}
	}
	if !found {
		t.Errorf("the newest snapshot %s was pruned", newest)
	}
}

func TestMigrateRefusesWhenFreeSpaceIsLow(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v1", DataDir: dir, Migrations: plus(t, 0)}); err != nil {
		t.Fatal(err)
	}
	low := func(string) (uint64, error) { return 1, nil }
	_, err := store.Migrate(ctx, s, store.MigrateOptions{
		AppVersion: "v2", DataDir: dir, Migrations: plus(t, 1), FreeBytes: low,
	})
	if err == nil || !strings.Contains(err.Error(), "COAMING_SKIP_PRE_MIGRATE_BACKUP") {
		t.Fatalf("error = %v, want one naming COAMING_SKIP_PRE_MIGRATE_BACKUP", err)
	}
	if len(snapshots(t, dir)) != 0 {
		t.Error("a refused migration must not leave a snapshot")
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{
		AppVersion: "v2", DataDir: dir, Migrations: plus(t, 1), FreeBytes: low, SkipBackup: true,
	})
	if err != nil {
		t.Fatalf("SkipBackup should proceed: %v", err)
	}
	if res.To != 2 || res.SnapshotPath != "" || len(snapshots(t, dir)) != 0 {
		t.Errorf("SkipBackup result = %+v, want To 2 and no snapshot", res)
	}
}

func TestMigrateRefusesDowngrade(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v1", DataDir: dir, Migrations: plus(t, 0)}); err != nil {
		t.Fatal(err)
	}
	up, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v2", DataDir: dir, Migrations: plus(t, 1)})
	if err != nil {
		t.Fatal(err)
	}

	// An older binary (migrations up to 1) meets the schema at version 2.
	_, err = store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v1", DataDir: dir, Migrations: plus(t, 0)})
	if !errors.Is(err, store.ErrDowngrade) {
		t.Fatalf("error = %v, want ErrDowngrade", err)
	}
	for _, want := range []string{"version 2", "version 1", up.SnapshotPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if got, _ := s.Q.GetMeta(ctx, "app_version"); got != "v2" {
		t.Errorf("a refused downgrade must not rewrite app_version, got %q", got)
	}
}

func TestPragmas(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	want := map[string]string{
		"journal_mode": "wal",
		"foreign_keys": "1",
		"busy_timeout": "5000",
		"synchronous":  "1", // NORMAL
	}
	for pragma, v := range want {
		var got string
		if err := s.Write.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("write PRAGMA %s: %v", pragma, err)
		}
		if strings.ToLower(got) != v {
			t.Errorf("write pool PRAGMA %s = %q, want %q", pragma, got, v)
		}
		if err := s.Read.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("read PRAGMA %s: %v", pragma, err)
		}
		if strings.ToLower(got) != v {
			t.Errorf("read pool PRAGMA %s = %q, want %q", pragma, got, v)
		}
	}
	if n := s.Write.Stats().MaxOpenConnections; n != 1 {
		t.Errorf("write pool MaxOpenConnections = %d, want 1", n)
	}
	if n := s.Read.Stats().MaxOpenConnections; n != 4 {
		t.Errorf("read pool MaxOpenConnections = %d, want 4", n)
	}
	if s.Path != filepath.Join(dir, "coaming.db") {
		t.Errorf("Path = %q", s.Path)
	}
}

func TestRestartKeepsStateAndRerunsNothing(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s1, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, s1, store.MigrateOptions{AppVersion: "v1", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Q.UpsertSetting(ctx, sqlc.UpsertSettingParams{Key: "greeting", Value: "hello", UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open(t, dir)
	res, err := store.Migrate(ctx, s2, store.MigrateOptions{AppVersion: "v1", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if want := embeddedTarget(t); res.From != want || res.To != want || res.SnapshotPath != "" {
		t.Errorf("restart result = %+v, want From %d To %d and no snapshot", res, want, want)
	}
	if got, err := s2.RQ.GetSetting(ctx, "greeting"); err != nil || got != "hello" {
		t.Errorf("setting after restart = %q, %v; want hello", got, err)
	}
}

func TestReadyReflectsMigrationState(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	if err := s.Ready(ctx); err == nil {
		t.Error("Ready before Migrate must fail")
	}
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v1", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(ctx); err != nil {
		t.Errorf("Ready after Migrate: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(ctx); err == nil {
		t.Error("Ready after Close must fail")
	}
}

func TestFailedMigrationNamesSnapshotAndRollsBack(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v1", DataDir: dir, Migrations: plus(t, 0)}); err != nil {
		t.Fatal(err)
	}
	bad := plus(t, 1)
	bad["00002_extra2.sql"] = &fstest.MapFile{Data: []byte(
		"-- +goose Up\nCREATE TABLE half (id INTEGER PRIMARY KEY);\nINSERT INTO no_such_table VALUES (1);\n")}

	_, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v2", DataDir: dir, Migrations: bad})
	if err == nil {
		t.Fatal("a broken migration must fail")
	}
	files := snapshots(t, dir)
	if len(files) != 1 || !strings.Contains(err.Error(), files[0]) {
		t.Errorf("error %q must name the snapshot %v", err, files)
	}
	if _, err := s.Write.ExecContext(ctx, "INSERT INTO half (id) VALUES (1)"); err == nil {
		t.Error("the failed migration must be rolled back, but table half exists")
	}
}

func TestOpenMakesTheDatabaseFilesPrivate(t *testing.T) {
	// Not parallel: the process umask is global.
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // a broad directory is the case under test
		t.Fatal(err)
	}
	s := open(t, dir)
	if _, err := s.Write.ExecContext(context.Background(), "CREATE TABLE probe (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{store.DatabaseFile, store.DatabaseFile + "-wal", store.DatabaseFile + "-shm"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, perm)
		}
	}
}

func TestOpenTightensADatabaseLeftReadableByAnOlderBuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, store.DatabaseFile)
	if err := os.WriteFile(path, nil, 0o644); err != nil { //nolint:gosec // the legacy mode is the case under test
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // defeat the umask
		t.Fatal(err)
	}
	_ = open(t, dir)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestSchemaVersionIsTheHighestAppliedMigration(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(context.Background(), s, store.MigrateOptions{AppVersion: "1.0.0", DataDir: dir}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got, err := s.SchemaVersion(context.Background())
	if err != nil || got != embeddedTarget(t) {
		t.Fatalf("SchemaVersion = %d, %v; want %d", got, err, embeddedTarget(t))
	}
}

func TestSchemaVersionOfAnUnmigratedDatabaseIsAnError(t *testing.T) {
	s := open(t, t.TempDir())
	if v, err := s.SchemaVersion(context.Background()); err == nil {
		t.Fatalf("SchemaVersion of an empty database = %d, nil; want an error", v)
	}
}

func TestFileURIMakesARelativePathAbsolute(t *testing.T) {
	wd := t.TempDir()
	t.Chdir(wd)
	wdReal, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a relative path gets no host and an absolute path", func(t *testing.T) {
		got, err := store.FileURI(".devdata/coaming.db", "_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse %q: %v", got, err)
		}
		if u.Host != "" {
			t.Errorf("host = %q, want empty (%s)", u.Host, got)
		}
		if want := filepath.Join(wdReal, ".devdata", "coaming.db"); u.Path != want {
			t.Errorf("path = %q, want %q", u.Path, want)
		}
		if u.RawQuery != "_pragma=foreign_keys(1)" {
			t.Errorf("query = %q", u.RawQuery)
		}
	})

	t.Run("?, # and % in a directory name are escaped", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "a?b#c%d", "coaming.db")
		got, err := store.FileURI(p, "")
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse %q: %v", got, err)
		}
		if u.Path != p {
			t.Errorf("path = %q, want %q", u.Path, p)
		}
	})

	t.Run("SQLite opens the URI of a relative path", func(t *testing.T) {
		if err := os.Mkdir("dd", 0o700); err != nil {
			t.Fatal(err)
		}
		dsn, err := store.FileURI("./dd/x.db", "")
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
			t.Fatalf("create table through %s: %v", dsn, err)
		}
		if _, err := os.Stat(filepath.Join(wdReal, "dd", "x.db")); err != nil {
			t.Errorf("the database is not at the absolute path: %v", err)
		}
	})
}
