// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// passphraseFileEnv names the file the backup passphrase is read from. The
// passphrase is never a flag: a flag shows in the process list and in shell
// history (D-32).
const passphraseFileEnv = "BAYPLAN_BACKUP_PASSPHRASE_FILE" //nolint:gosec // the name of an environment variable, not a credential

// maxPassphraseLine bounds what is read for a passphrase.
const maxPassphraseLine = 4096

// errNoPassphrase is the answer when there is nowhere to read a passphrase from.
var errNoPassphrase = errors.New("no passphrase: set " + passphraseFileEnv + " or pipe the passphrase on stdin")

func newBackupCmd() *cobra.Command {
	var (
		out         string
		allowBroken bool
	)
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write an encrypted backup while the server runs, for example: docker exec <container> bayplan backup",
		Long: "Writes the same encrypted .bayplan file as Settings > Backup, from a consistent copy of the database, while Bayplan runs.\n" +
			"The passphrase is read from the file named by " + passphraseFileEnv + ", or else from the first line of standard input; " +
			"it is never a flag. It must be at least " + strconv.Itoa(backup.MinPassphraseLen) + " characters.\n" +
			"The file goes to --out: a path (an existing file there is replaced), or - for standard output. By default it goes to " +
			"BAYPLAN_BACKUP_DIR, or backups/ in the data directory, as bayplan-manual-<date>-<time>.bayplan; a default name is never " +
			"given to a file that exists. Scheduled-backup retention does not touch these files, and Settings > Backup does not list them.\n" +
			"A broken audit chain refuses the backup unless --allow-broken-chain is given; the backup then records that it did not verify.\n" +
			"The backup is audited as backup.exported by system:cli.",
		Args: cobra.NoArgs,
		// The one-line answers below are the output; cobra's "Error:" prefix would only repeat them.
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runBackup(cmd, out, allowBroken) },
	}
	cmd.Flags().StringVar(&out, "out", "", "where to write the backup: a file path, or - for standard output (default: bayplan-manual-<date>-<time>.bayplan in BAYPLAN_BACKUP_DIR, or in backups/ in the data directory)")
	cmd.Flags().BoolVar(&allowBroken, "allow-broken-chain", false, "back up even if the audit chain does not verify")
	return cmd
}

func runBackup(cmd *cobra.Command, out string, allowBroken bool) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	fail := func(err error) error {
		fmt.Fprintln(cmd.ErrOrStderr(), "backup:", err)
		return err
	}

	passphrase, err := readPassphrase(cmd)
	if err != nil {
		return fail(err)
	}
	if utf8.RuneCountInString(passphrase) < backup.MinPassphraseLen {
		return fail(fmt.Errorf("the passphrase must be at least %d characters", backup.MinPassphraseLen))
	}

	// Load never generates a key: a backup made with a fresh one would not match
	// the server's.
	v, err := vault.Load(cfg.EncryptionKey, cfg.DataDir)
	if errors.Is(err, vault.ErrNoKey) {
		return fail(fmt.Errorf("no encryption key: set BAYPLAN_ENCRYPTION_KEY (or _FILE), or point BAYPLAN_DATA_DIR at the "+
			"directory that holds %s (looked in %s)", vault.KeyFile, cfg.DataDir))
	}
	if err != nil {
		return fail(err)
	}
	dbPath := filepath.Join(cfg.DataDir, store.DatabaseFile)
	if _, err := os.Stat(dbPath); err != nil {
		return fail(fmt.Errorf("no database at %s: set BAYPLAN_DATA_DIR to the directory Bayplan stores its data in", dbPath))
	}
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = st.Close() }()
	inst, err := instance.Load(ctx, st, cfg)
	if err != nil {
		return fail(err)
	}

	now := time.Now().UTC()
	dest, err := newBackupDestination(cmd, cfg.DataDir, cfg.BackupDir, out, now)
	if err != nil {
		return fail(err)
	}
	defer dest.abort()

	res, err := backup.Export(ctx, st, v, cfg.DataDir, dest.w, []byte(passphrase), backup.ExportOptions{
		AppVersion: buildinfo.Get().Version, InstanceName: inst.Name(), PublicURL: inst.PublicURL(),
		AllowBrokenChain: allowBroken, Now: func() time.Time { return now },
	})
	if errors.Is(err, backup.ErrChainBroken) {
		return fail(fmt.Errorf("%w; fix it, or run again with --allow-broken-chain to back up anyway", err))
	}
	if err != nil {
		return fail(err)
	}
	if !res.Manifest.ChainVerified {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: the audit chain does not verify; the backup records that")
	}

	// The audit writer logs every entry it appends; the command's own output is
	// the answer, so that log goes nowhere.
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil)
	if err := w.Record(ctx, audit.Entry{
		Actor:      audit.SystemCLI,
		Action:     audit.ActionBackupExported,
		TargetType: "backup",
		TargetID:   dest.label,
		Result:     audit.ResultSuccess,
		Details:    map[string]string{"file": dest.label, "size": strconv.FormatInt(res.Size, 10), "source": "cli"},
	}); err != nil {
		return fail(err)
	}
	if err := dest.commit(); err != nil {
		return fail(err)
	}
	if dest.path != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "backup written to %s (%d bytes)\n", dest.path, res.Size)
	}
	return nil
}

// readPassphrase takes the passphrase from the file the environment names, else
// from the first line of standard input when that is not a terminal. Trailing
// line breaks are dropped and nothing else.
func readPassphrase(cmd *cobra.Command) (string, error) {
	if path := os.Getenv(passphraseFileEnv); path != "" {
		f, err := os.Open(path) //nolint:gosec // the path the operator named
		if err != nil {
			return "", fmt.Errorf("read %s: %w", passphraseFileEnv, err)
		}
		defer func() { _ = f.Close() }()
		return firstLine(f, passphraseFileEnv)
	}
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok {
		if info, err := f.Stat(); err != nil || info.Mode()&os.ModeCharDevice != 0 {
			return "", errNoPassphrase
		}
	}
	return firstLine(in, "standard input")
}

func firstLine(r io.Reader, what string) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, maxPassphraseLine)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the passphrase from %s: %w", what, err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errNoPassphrase
	}
	return line, nil
}

// destination is where the backup goes. A file is written under a temporary name
// and renamed into place once the whole backup and its audit entry are done, so
// a failure never leaves half a backup under the real name.
type destination struct {
	w     io.Writer
	label string // the file name for the audit entry; "stdout" for standard output
	path  string // the final path; empty for standard output
	tmp   *os.File
	// reserved is set when path is an empty file this command created to claim its
	// name; a backup that fails removes it.
	reserved bool
}

// maxNameAttempts bounds the search for a free default name. Two backups in the
// same second are the only reason to go past the first.
const maxNameAttempts = 100

func newBackupDestination(cmd *cobra.Command, dataDir, backupDir, out string, now time.Time) (*destination, error) {
	if out == "-" {
		return &destination{w: cmd.OutOrStdout(), label: "stdout"}, nil
	}
	path, reserved := out, false
	if path == "" {
		// The folder the server writes its scheduled backups to, but under a name
		// of its own: a scheduled name here would be counted and deleted by the
		// retention of the scheduled backups, and could replace one (T-03-113).
		dir := backupDir
		if dir == "" {
			dir = filepath.Join(dataDir, "backups")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create backups directory %s: %w", dir, err)
		}
		var err error
		if path, err = reserveName(dir, now); err != nil {
			return nil, err
		}
		reserved = true
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bayplan-backup-*.tmp")
	if err != nil {
		if reserved {
			_ = os.Remove(path)
		}
		return nil, fmt.Errorf("create the backup file: %w", err)
	}
	return &destination{w: tmp, label: filepath.Base(path), path: path, tmp: tmp, reserved: reserved}, nil
}

// reserveName claims a default file name by creating it, empty and exclusively,
// so a file that exists, a scheduled backup or another manual one, is never
// replaced. commit later renames the finished backup over the empty claim.
func reserveName(dir string, now time.Time) (string, error) {
	for n := 1; n <= maxNameAttempts; n++ {
		path := filepath.Join(dir, backup.ManualName(now, n))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // a name this command builds in the backups folder
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create the backup file: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("create the backup file: %w", err)
		}
		return path, nil
	}
	return "", fmt.Errorf("no free backup name in %s for %s: use --out", dir, now.Format("2006-01-02 15:04:05"))
}

// commit moves a finished file into place, mode 0600.
func (d *destination) commit() error {
	if d.tmp == nil {
		return nil
	}
	tmp := d.tmp
	d.tmp = nil
	fail := func() {
		_ = os.Remove(tmp.Name())
		if d.reserved {
			_ = os.Remove(d.path)
		}
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		fail()
		return fmt.Errorf("restrict the backup file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		fail()
		return fmt.Errorf("write the backup file: %w", err)
	}
	if err := os.Rename(tmp.Name(), d.path); err != nil {
		fail()
		return fmt.Errorf("move the backup into place: %w", err)
	}
	return nil
}

// abort removes an unfinished file; it does nothing after commit.
func (d *destination) abort() {
	if d.tmp == nil {
		return
	}
	_ = d.tmp.Close()
	_ = os.Remove(d.tmp.Name())
	if d.reserved {
		_ = os.Remove(d.path)
	}
}
