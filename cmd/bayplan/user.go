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

	"github.com/spf13/cobra"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users from the host",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "unlock <name>",
		Short: "Clear a locked account, for example: docker exec <container> bayplan user unlock admin",
		Long: "Clears the failed-sign-in count and the lock of a user, so they can sign in at once instead of waiting for the lock to expire.\n" +
			"This is the only way back in when the only admin has been locked out, and it needs access to the container, so it is audited as system:cli.\n" +
			"Exits 0 when the account was unlocked and 1 when there is no such user.",
		Args: cobra.ExactArgs(1),
		// The one-line answer below is the output; cobra's "Error:" prefix would only repeat it.
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, args []string) error { return runUserUnlock(cmd, args[0]) },
	})
	return cmd
}

var errNoSuchUser = errors.New("no such user")

func runUserUnlock(cmd *cobra.Command, name string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	fail := func(err error) error {
		fmt.Fprintln(cmd.ErrOrStderr(), "user unlock:", err)
		return err
	}

	// Load never generates a key: an audit entry signed with a fresh one would
	// break the chain the server keeps.
	v, err := vault.Load(cfg.EncryptionKey, cfg.DataDir)
	if errors.Is(err, vault.ErrNoKey) {
		return fail(fmt.Errorf("no encryption key: set BAYPLAN_ENCRYPTION_KEY (or _FILE), or point BAYPLAN_DATA_DIR at the "+
			"directory that holds %s (looked in %s)", vault.KeyFile, cfg.DataDir))
	}
	if err != nil {
		return fail(err)
	}

	// Open would create an empty database; unlocking in a path that has none is a mistake to report.
	dbPath := filepath.Join(cfg.DataDir, store.DatabaseFile)
	if _, err := os.Stat(dbPath); err != nil {
		return fail(fmt.Errorf("no database at %s: set BAYPLAN_DATA_DIR to the directory Bayplan stores its data in", dbPath))
	}
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = st.Close() }()

	// The audit writer logs every entry it appends; the command's own output is
	// the answer, so that log goes nowhere.
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil)
	switch err := auth.Unlock(ctx, st, w, name); {
	case errors.Is(err, auth.ErrUserNotFound):
		fmt.Fprintf(cmd.ErrOrStderr(), "no user named %s\n", name)
		return errNoSuchUser
	case err != nil:
		return fail(err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "unlocked %s\n", name)
	return nil
}
