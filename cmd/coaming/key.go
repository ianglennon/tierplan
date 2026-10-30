// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/backup"
	"github.com/ianglennon/coaming/internal/datadir"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/vault"
)

func newKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Work with Coaming's master encryption key",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "rotate",
		Short: "Replace the master key, with Coaming stopped",
		Long: "Generates a new master key, re-encrypts every stored secret under it, re-signs the whole audit log with it and " +
			"records a key.rotated audit entry that the old key attests. Coaming must not be running: stop it, then run this as a " +
			"one-off container on the same volume, for example:\n" +
			"  docker run --rm -v coaming:/data ghcr.io/ianglennon/coaming key rotate\n" +
			"The old key is kept as coaming.key.old so the attestation can be checked; delete it once `coaming audit verify` passes.\n" +
			"With a key supplied through COAMING_ENCRYPTION_KEY the new key is written to coaming.key.new instead, and you set the " +
			"variable from that file before starting Coaming again. The key is never printed.",
		Args: cobra.NoArgs,
		// The lines below are the output; cobra's "Error:" prefix would only repeat them.
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runKeyRotate(cmd) },
	})
	return cmd
}

func runKeyRotate(cmd *cobra.Command) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	stderr := cmd.ErrOrStderr()
	fail := func(err error) error {
		fmt.Fprintln(stderr, "key rotate:", err)
		return err
	}

	// Nothing may write the database or the key files while they are replaced:
	// the running server holds this lock for as long as it runs. It is taken
	// before anything is read, so a refusal changes nothing.
	release, err := datadir.Lock(cfg.DataDir)
	if errors.Is(err, datadir.ErrLocked) {
		fmt.Fprintln(stderr, "key rotate: Coaming is running on this data directory ("+err.Error()+")")
		fmt.Fprintln(stderr, "Stop it, then run: docker run --rm -v <volume>:/data <same image> key rotate")
		return fmt.Errorf("key rotate: %w", err)
	}
	if err != nil {
		return fail(err)
	}
	defer func() { _ = release() }()

	// Finish or discard what an earlier, interrupted rotation left, exactly as
	// the server would on its next start.
	dbPath := filepath.Join(cfg.DataDir, store.DatabaseFile)
	changed, err := vault.RecoverRotation(ctx, cfg.DataDir, cfg.EncryptionKey != "", func(ctx context.Context) (string, error) {
		return backup.StoredActiveKeyID(ctx, cfg.DataDir)
	})
	if err != nil {
		return fail(err)
	}
	if changed {
		fmt.Fprintln(stderr, "key rotate: finished or discarded an earlier interrupted rotation")
	}

	// Load never generates a key: a freshly minted one could not open anything.
	v, err := vault.Load(cfg.EncryptionKey, cfg.DataDir)
	if errors.Is(err, vault.ErrNoKey) {
		return fail(fmt.Errorf("no encryption key: set COAMING_ENCRYPTION_KEY (or _FILE), or point COAMING_DATA_DIR at the "+
			"directory that holds %s (looked in %s)", vault.KeyFile, cfg.DataDir))
	}
	if err != nil {
		return fail(err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		return fail(fmt.Errorf("no database at %s: set COAMING_DATA_DIR to the directory Coaming stores its data in", dbPath))
	}
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = st.Close() }()

	res, err := backup.RotateKey(ctx, st, cfg.DataDir, v, audit.SystemCLI)
	if err != nil {
		return fail(err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Rotated the master key: %s -> %s\n", res.OldKID, res.NewKID)
	fmt.Fprintf(out, "Re-encrypted %d stored secret(s) and re-signed %d audit entries.\n", res.Rewrapped, res.Entries)
	if res.KeyFileSwapped {
		fmt.Fprintf(out, "The new key is in %s. The old key is kept as %s; delete it once `coaming audit verify` passes.\n",
			vault.KeyFile, vault.OldKeyFile)
		fmt.Fprintln(out, "Back up the new key file now: without it stored secrets cannot be decrypted.")
	} else {
		fmt.Fprintf(out, "Set COAMING_ENCRYPTION_KEY to the contents of %s, restart Coaming, then delete the file.\n", res.NewKeyPath)
	}
	return nil
}
