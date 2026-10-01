// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Command bayplan is the Bayplan server and its small command line.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/server"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the Bayplan server",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runServe(cmd) },
	}

	root := &cobra.Command{
		Use:           "bayplan",
		Short:         "Bayplan: manage Docker Swarm clusters and standalone hosts",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		// The default action is serve.
		RunE: func(cmd *cobra.Command, _ []string) error { return runServe(cmd) },
	}
	// Every setting is also a flag; precedence is in internal/config (D-11).
	config.BindFlags(root.PersistentFlags())

	root.AddCommand(
		serve,
		newAuditCmd(),
		&cobra.Command{
			Use:   "healthcheck",
			Short: "Exit 0 if the local server answers /healthz, else 1 (for the container HEALTHCHECK)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig(cmd)
				if err != nil {
					return err
				}
				return server.Healthcheck(cmd.Context(), cfg.Listen)
			},
		},
		&cobra.Command{
			Use:   "openapi",
			Short: "Print the OpenAPI 3.1 document as JSON",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return writeOpenAPI(cmd.OutOrStdout())
			},
		},
		&cobra.Command{
			Use:   "version",
			Short: "Print the version",
			Args:  cobra.NoArgs,
			Run: func(cmd *cobra.Command, _ []string) {
				fmt.Fprintln(cmd.OutOrStdout(), buildinfo.Get().Display)
			},
		},
	)
	return root
}

func loadConfig(cmd *cobra.Command) (config.Config, error) {
	return config.Load(cmd.Flags(), os.Getenv, os.ReadFile)
}

// writeOpenAPI needs no configuration, data directory or socket: it builds the
// router with no dependencies.
func writeOpenAPI(w io.Writer) error {
	doc, err := json.MarshalIndent(server.NewRouter(httpx.Options{}, api.SystemDeps{}).API().OpenAPI(), "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(doc, '\n'))
	return err
}

func runServe(cmd *cobra.Command) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
