// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/vault"
)

func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Work with the audit log",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "verify",
		Short: "Check the audit log's HMAC chain and name the first broken entry",
		Long: "Walks the audit log in order and recomputes every entry's HMAC with the key derived from the master key.\n" +
			"Exits 0 when the chain is intact and 1 when an entry fails, naming its sequence number and ID.",
		Args: cobra.NoArgs,
		// The failure line below is the output; cobra's own "Error:" prefix would only repeat it.
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runAuditVerify(cmd) },
	})
	return cmd
}

var errAuditBroken = errors.New("audit chain is broken")

func runAuditVerify(cmd *cobra.Command) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	fail := func(err error) error {
		fmt.Fprintln(cmd.ErrOrStderr(), "audit verify:", err)
		return err
	}

	// Load never generates a key: a freshly minted one could not match the
	// entries the server signed.
	v, err := vault.Load(cfg.EncryptionKey, cfg.DataDir)
	if errors.Is(err, vault.ErrNoKey) {
		return fail(fmt.Errorf("no encryption key: set TIERPLAN_ENCRYPTION_KEY (or _FILE), or point TIERPLAN_DATA_DIR at the "+
			"directory that holds %s (looked in %s)", vault.KeyFile, cfg.DataDir))
	}
	if err != nil {
		return fail(err)
	}

	// Open would create an empty database; verifying a path that has none is a mistake to report.
	dbPath := filepath.Join(cfg.DataDir, store.DatabaseFile)
	if _, err := os.Stat(dbPath); err != nil {
		return fail(fmt.Errorf("no database at %s: set TIERPLAN_DATA_DIR to the directory TierPlan stores its data in", dbPath))
	}
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = st.Close() }()

	res, err := audit.Verify(ctx, st, v)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			err = errors.New("the database has no audit log yet: start TierPlan once so it can migrate the database")
		}
		return fail(err)
	}
	if b := res.Broken; b != nil {
		msg := fmt.Sprintf("audit chain BROKEN at seq %d (id %s): %s", b.Seq, b.ID, b.Reason)
		fmt.Fprintln(cmd.OutOrStdout(), msg)
		return fmt.Errorf("%w: %s", errAuditBroken, msg)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "audit chain OK: %d entries\n", res.Entries)
	if res.AttestationsUnchecked > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%d key rotation attestation(s) not checked: the previous key is not loaded\n",
			res.AttestationsUnchecked)
	}
	return nil
}
