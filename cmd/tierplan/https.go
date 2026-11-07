// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ianglennon/tierplan/internal/instance"
)

func newHTTPSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "https",
		Short: "Manage TierPlan's HTTPS from the host",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "disable",
		Short: "Switch TierPlan back to plain HTTP, for example: docker exec <container> tierplan https disable",
		Long: "Rewrites the stored public URL to http with an explicit port, so you can sign in again over plain HTTP when HTTPS has locked you out.\n" +
			"The running server applies the change within a few seconds. It needs access to the container, so it is audited as system:cli.\n" +
			"It refuses, changing nothing, when TIERPLAN_PUBLIC_URL fixes the URL, and when TIERPLAN_TLS_SELF_SIGNED or TIERPLAN_TLS_CERT_FILE sets TLS. " +
			"In that second case HTTPS is always served, so it names the https:// address to sign in at, or says to remove the variable and restart.\n" +
			"Exits 0 when the URL was changed or there was nothing to change, and 1 when TIERPLAN_PUBLIC_URL fixes it, an environment variable sets TLS, or the data directory cannot be used.",
		Args: cobra.NoArgs,
		// The one-line answers below are the output; cobra's "Error:" prefix would only repeat them.
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runHTTPSDisable(cmd) },
	})
	return cmd
}

func runHTTPSDisable(cmd *cobra.Command) error {
	tools, closeTools, err := openHostTools(cmd, "https disable")
	if err != nil {
		return err
	}
	defer closeTools()

	res, err := instance.DisableHTTPS(tools.ctx, tools.store, tools.cfg, tools.audit)
	switch {
	case errors.Is(err, instance.ErrPublicURLLocked):
		fmt.Fprintln(cmd.ErrOrStderr(), "https disable: the public URL is fixed by TIERPLAN_PUBLIC_URL; nothing was changed. "+
			"Change or unset TIERPLAN_PUBLIC_URL on the container and restart it.")
		return err
	case errors.Is(err, instance.ErrTLSSetByEnv):
		// Nothing goes to stdout: the answer is a refusal, and a script reads the exit code.
		for i, note := range res.Notes {
			if i == 0 {
				note = "https disable: " + note
			}
			fmt.Fprintln(cmd.ErrOrStderr(), note)
		}
		return err
	case err != nil:
		return tools.fail(err)
	}

	out := cmd.OutOrStdout()
	for _, line := range res.Changes {
		fmt.Fprintln(out, line)
	}
	for _, line := range res.Notes {
		fmt.Fprintln(out, line)
	}
	return nil
}
