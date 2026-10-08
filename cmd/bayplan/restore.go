// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// exitCodeError makes main exit with a code other than 1.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

// exitCode is the process exit code for err.
func exitCode(err error) int {
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	return 1
}

func newRestoreCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Restore an encrypted backup into the data directory, with Bayplan stopped",
		Long: "Restores a .bayplan file made by Settings > Backup or `bayplan backup`. Bayplan must not be running: stop it, then run this as a " +
			"one-off container on the same volume, for example:\n" +
			"  docker run --rm -i -v bayplan:/data ghcr.io/ianglennon/bayplan restore --yes /data/backups/<file>\n" +
			"The passphrase is read from the file named by " + passphraseFileEnv + ", or else from the first line of standard input; it is never a flag.\n" +
			"The backup is checked, migrated to this version and re-keyed to this instance's master key on a private copy first; the data " +
			"directory changes only when all of that has worked. A database that is replaced is first saved as backups/prerestore-<time>.db.\n" +
			"Replacing an existing database needs --yes; without it the command shows what the backup holds and exits with status 2.\n" +
			"A restored instance has no sessions, invite links, reset links or agent join tokens, so everyone signs in again.",
		Args: cobra.ExactArgs(1),
		// The one-line answers below are the output; cobra's "Error:" prefix would only repeat them.
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, args []string) error { return runRestore(cmd, args[0], yes) },
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "replace the current data without asking")
	return cmd
}

func runRestore(cmd *cobra.Command, file string, yes bool) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	fail := func(err error) error {
		fmt.Fprintln(cmd.ErrOrStderr(), "restore:", err)
		return err
	}

	passphrase, err := readPassphrase(cmd)
	if err != nil {
		return fail(err)
	}
	binarySchema, err := store.TargetSchema()
	if err != nil {
		return fail(err)
	}

	in, err := inspectRestoreFile(file, []byte(passphrase), binarySchema)
	if err != nil {
		return fail(describeRestoreError(file, err))
	}
	printBackupSummary(cmd, file, in)

	live := filepath.Join(cfg.DataDir, store.DatabaseFile)
	_, statErr := os.Stat(live)
	replacing := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fail(fmt.Errorf("look for %s: %w", live, statErr))
	}
	if replacing && !yes {
		fmt.Fprintln(cmd.ErrOrStderr(), "restore: this would replace the current data in "+cfg.DataDir+"; re-run with --yes to replace the current data")
		return &exitCodeError{code: 2, msg: "restore: not confirmed"}
	}

	// Nothing may write the database while it is replaced: the running server
	// holds this lock for as long as it runs, so a restore waits for it to stop.
	// It is taken before the key is loaded, so a refused restore creates nothing.
	release, err := datadir.Lock(cfg.DataDir)
	if errors.Is(err, datadir.ErrLocked) {
		fmt.Fprintln(cmd.ErrOrStderr(), "restore: Bayplan is running on this data directory ("+err.Error()+")")
		fmt.Fprintln(cmd.ErrOrStderr(), "stop it, then run: docker run --rm -i -v <volume>:/data ghcr.io/ianglennon/bayplan restore --yes <file>")
		return &exitCodeError{code: 1, msg: "restore: " + err.Error()}
	}
	if err != nil {
		return fail(err)
	}
	defer func() { _ = release() }()

	// The target key is this data directory's: an existing install keeps its
	// own, a fresh volume gets a new one. Nothing from the backup's instance
	// needs to be carried across.
	target, err := vault.LoadOrCreate(cfg.EncryptionKey, cfg.DataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		return fail(fmt.Errorf("load encryption key: %w", err))
	}

	src, err := os.Open(file) //nolint:gosec // the path the operator named
	if err != nil {
		return fail(err)
	}
	defer func() { _ = src.Close() }()
	staged, err := backup.Stage(ctx, src, []byte(passphrase), cfg.DataDir, binarySchema)
	if err != nil {
		return fail(describeRestoreError(file, err))
	}
	defer staged.Discard()

	version := buildinfo.Get().Version
	if err := staged.Prepare(ctx, target, backup.PrepareOptions{Source: "cli", AppVersion: version}); err != nil {
		return fail(describeRestoreError(file, err))
	}
	snapshot, err := staged.Swap(ctx, cfg.DataDir, time.Now().UTC())
	if err != nil {
		return fail(err)
	}
	if snapshot != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "The previous database was saved as %s\n", snapshot)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Restored. Start Bayplan and sign in with an account from the backup.")
	return nil
}

func inspectRestoreFile(file string, passphrase []byte, binarySchema int64) (backup.Inspection, error) {
	f, err := os.Open(file) //nolint:gosec // the path the operator named
	if err != nil {
		return backup.Inspection{}, err
	}
	defer func() { _ = f.Close() }()
	return backup.Inspect(f, passphrase, binarySchema)
}

// describeRestoreError says what is wrong in the terms an operator needs.
func describeRestoreError(file string, err error) error {
	switch {
	case errors.Is(err, backup.ErrNotBackup):
		return fmt.Errorf("%s is not a Bayplan backup file", file)
	case errors.Is(err, backup.ErrNewerBackup), errors.Is(err, backup.ErrNewerFormat):
		return err
	case errors.Is(err, backup.ErrUnreadable), errors.Is(err, backup.ErrHeaderLimits):
		return fmt.Errorf("%s could not be opened: the passphrase is wrong, or the file is damaged or incomplete", file)
	}
	return err
}

func printBackupSummary(cmd *cobra.Command, file string, in backup.Inspection) {
	m := in.Manifest
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Backup file:   %s\n", filepath.Base(file))
	name := m.InstanceName
	if m.PublicURL != "" {
		name += " (" + m.PublicURL + ")"
	}
	fmt.Fprintf(out, "From instance: %s\n", name)
	fmt.Fprintf(out, "Made:          %s\n", m.Created.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "Bayplan:       %s (schema %d)\n", m.AppVersion, m.Schema)
	fmt.Fprintf(out, "Contains:      %d users, %d teams, %d environments, %d audit entries\n",
		m.Counts.Users, m.Counts.Teams, m.Counts.Environments, m.Counts.AuditEntries)
	if !m.ChainVerified {
		fmt.Fprintln(out, "Warning:       the audit chain did not verify when this backup was made")
	}
}
