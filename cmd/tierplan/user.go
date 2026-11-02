// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ianglennon/tierplan/internal/access"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/vault"
)

func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users from the host",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "unlock <name>",
		Short: "Clear a locked account, for example: docker exec <container> tierplan user unlock admin",
		Long: "Clears the failed-sign-in count and the lock of a user, so they can sign in at once instead of waiting for the lock to expire.\n" +
			"This is the only way back in when the only admin has been locked out, and it needs access to the container, so it is audited as system:cli.\n" +
			"Exits 0 when the account was unlocked and 1 when there is no such user.",
		Args: cobra.ExactArgs(1),
		// The one-line answer below is the output; cobra's "Error:" prefix would only repeat it.
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, args []string) error { return runUserUnlock(cmd, args[0]) },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "reset-password <name>",
		Short: "Print a one-time password reset link, for example: docker exec <container> tierplan user reset-password admin",
		Long: "Ends every session of a user and prints a one-time link that lets them choose a new password, built from the public URL and base path TierPlan has stored.\n" +
			"This is the way back in when the only instance admin has forgotten their password, and it needs access to the container, so it is audited as system:cli.\n" +
			"When no public URL is stored the link has no address in front of it: open it on your TierPlan address.\n" +
			"Exits 0 when the link was printed and 1 when there is no such user or the user is disabled.",
		Args: cobra.ExactArgs(1),
		// The one-line answer is the output; cobra's "Error:" prefix would only repeat it.
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, args []string) error { return runUserResetPassword(cmd, args[0]) },
	})
	return cmd
}

var errNoSuchUser = errors.New("no such user")

// errUserDisabled is a reset for a user who cannot sign in.
var errUserDisabled = errors.New("user is disabled")

// userTools is what a command that changes a user from the host needs: the
// store and an audit writer that signs with the server's own key.
type userTools struct {
	ctx   context.Context
	cfg   config.Config
	store *store.Store
	audit *audit.Writer
	// fail reports err on stderr, prefixed with the command, and returns it.
	fail func(err error) error
}

// openUserTools opens the data directory for a `tierplan user` subcommand. The
// returned func closes the store.
func openUserTools(cmd *cobra.Command, name string) (*userTools, func(), error) {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return nil, nil, err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	fail := func(err error) error {
		fmt.Fprintln(cmd.ErrOrStderr(), "user "+name+":", err)
		return err
	}

	// Load never generates a key: an audit entry signed with a fresh one would
	// break the chain the server keeps.
	v, err := vault.Load(cfg.EncryptionKey, cfg.DataDir)
	if errors.Is(err, vault.ErrNoKey) {
		return nil, nil, fail(fmt.Errorf("no encryption key: set TIERPLAN_ENCRYPTION_KEY (or _FILE), or point TIERPLAN_DATA_DIR at the "+
			"directory that holds %s (looked in %s)", vault.KeyFile, cfg.DataDir))
	}
	if err != nil {
		return nil, nil, fail(err)
	}

	// Open would create an empty database; working in a path that has none is a mistake to report.
	dbPath := filepath.Join(cfg.DataDir, store.DatabaseFile)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil, fail(fmt.Errorf("no database at %s: set TIERPLAN_DATA_DIR to the directory TierPlan stores its data in", dbPath))
	}
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, nil, fail(err)
	}

	// The audit writer logs every entry it appends; the command's own output is
	// the answer, so that log goes nowhere.
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil)
	return &userTools{ctx: ctx, cfg: cfg, store: st, audit: w, fail: fail}, func() { _ = st.Close() }, nil
}

func runUserUnlock(cmd *cobra.Command, name string) error {
	tools, closeTools, err := openUserTools(cmd, "unlock")
	if err != nil {
		return err
	}
	defer closeTools()

	switch err := auth.Unlock(tools.ctx, tools.store, tools.audit, name); {
	case errors.Is(err, auth.ErrUserNotFound):
		fmt.Fprintf(cmd.ErrOrStderr(), "no user named %s\n", name)
		return errNoSuchUser
	case err != nil:
		return tools.fail(err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "unlocked %s\n", name)
	return nil
}

// runUserResetPassword ends the user's sessions and prints a one-time reset
// link. It calls the same access.IssueReset as the API, and builds the address
// with the same access.LinkURL on the stored public URL and base path, so the
// two cannot disagree. It works for the last instance admin: no guard applies to
// a reset.
func runUserResetPassword(cmd *cobra.Command, name string) error {
	tools, closeTools, err := openUserTools(cmd, "reset-password")
	if err != nil {
		return err
	}
	defer closeTools()

	notFound := func() error {
		fmt.Fprintf(cmd.ErrOrStderr(), "no user named %s\n", name)
		return errNoSuchUser
	}
	norm, err := auth.NormalizeUsername(name)
	if err != nil {
		return notFound()
	}
	u, err := tools.store.RQ.GetUserByUsername(tools.ctx, norm)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && u.DeletedAt.Valid) {
		return notFound()
	}
	if err != nil {
		return tools.fail(fmt.Errorf("look up user: %w", err))
	}

	inst, err := instance.Load(tools.ctx, tools.store, tools.cfg)
	if err != nil {
		return tools.fail(err)
	}
	svc := access.New(tools.store, tools.audit, nil, tools.cfg.InviteTTL)
	_, link, err := svc.IssueReset(tools.ctx, audit.SystemCLI, u.ID)
	switch {
	case errors.Is(err, access.ErrNotFound):
		return notFound()
	case errors.Is(err, access.ErrUserDisabled):
		fmt.Fprintf(cmd.ErrOrStderr(), "%s is disabled: enable them in TierPlan first\n", name)
		return errUserDisabled
	case err != nil:
		return tools.fail(err)
	}

	url := access.LinkURL(inst.PublicURL(), inst.BasePath(), link.Token)
	if inst.PublicURL() == "" {
		// Without an address the link is only the part after it. Say where it goes.
		fmt.Fprintf(cmd.OutOrStdout(), "Open it on your TierPlan address: %s\n", url)
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), url)
	return nil
}
