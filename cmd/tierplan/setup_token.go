// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/ianglennon/tierplan/internal/auth"
)

// errNoSetupToken is returned when there is no active token to print.
var errNoSetupToken = errors.New("no active setup token: setup is already complete, or the TierPlan server has not started yet")

// newSetupTokenCmd prints the active setup token (D-01). It reads the file the
// running server wrote into the data directory and never opens the database, so
// it works while the server holds it and needs no encryption key.
func newSetupTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup-token",
		Short: "Print the active setup token, for example: docker exec <container> tierplan setup-token",
		Long: "Prints the one-time token that claims a new TierPlan instance. It is the same token the server logs at start.\n" +
			"Exits 1 when there is no active token because setup is complete or the server has not started.",
		Args: cobra.NoArgs,
		// The explanation below is the output; cobra's "Error:" prefix would only repeat it.
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			token, err := auth.ReadTokenFile(cfg.DataDir)
			if errors.Is(err, fs.ErrNotExist) || (err == nil && token == "") {
				fmt.Fprintln(cmd.ErrOrStderr(), errNoSetupToken)
				return errNoSetupToken
			}
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "setup-token:", err)
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), token)
			return nil
		},
	}
}
