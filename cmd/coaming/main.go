// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Command coaming is the Coaming server and its small command line.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ianglennon/coaming/internal/buildinfo"
	"github.com/ianglennon/coaming/internal/config"
	"github.com/ianglennon/coaming/internal/engine"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/privdrop"
	"github.com/ianglennon/coaming/internal/server"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		os.Exit(exitCode(err))
	}
}

func newRoot() *cobra.Command {
	// selfID is Coaming's own container ID, read before the privilege drop.
	var selfID string

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the Coaming server",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runServe(cmd, selfID) },
	}

	root := &cobra.Command{
		Use:           "coaming",
		Short:         "Coaming: manage Docker Swarm clusters and standalone hosts",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		// Runs before every subcommand, so `docker exec <c> coaming user unlock`
		// is as unprivileged as the server (D-17, RESEARCH A8).
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			selfID, err = dropPrivileges(cmd)
			return err
		},
		// The default action is serve.
		RunE: func(cmd *cobra.Command, _ []string) error { return runServe(cmd, selfID) },
	}
	// Every setting is also a flag; precedence is in internal/config (D-11).
	config.BindFlags(root.PersistentFlags())

	root.AddCommand(
		serve,
		newAuditCmd(),
		newSetupTokenCmd(),
		newUserCmd(),
		newBackupCmd(),
		newRestoreCmd(),
		newKeyCmd(),
		newPermissionsCmd(),
		&cobra.Command{
			Use:   "healthcheck",
			Short: "Exit 0 if the local server answers /healthz, else 1 (for the container HEALTHCHECK)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig(cmd)
				if err != nil {
					return err
				}
				return server.Healthcheck(cmd.Context(), cfg.Listen, server.HealthcheckCertFile(cfg))
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
	doc, err := json.MarshalIndent(server.NewRouter(server.RouterDeps{}).API().OpenAPI(), "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(doc, '\n'))
	return err
}

// noDataDir lists the commands that never read the data directory. They still
// drop privileges, but Drop neither creates nor chowns /data for them. The
// healthcheck is among them: the image runs it as root every 30 seconds, and
// it only probes /healthz (WR-08).
var noDataDir = map[string]bool{"version": true, "openapi": true, "permissions": true, "help": true, "completion": true, "healthcheck": true}

// dropPrivileges reads Coaming's own container ID and then, when started as
// root, joins the Docker socket's group, fixes /data and becomes UID and GID
// 1000. The order matters: after the user change the process is non-dumpable
// and some /proc/self entries stop being readable (RESEARCH A9, D-27).
func dropPrivileges(cmd *cobra.Command) (selfID string, err error) {
	selfID = envs.ReadSelfContainerID()
	if _, err := privdrop.Drop(dropOptions(cmd)); err != nil {
		return "", err
	}
	return selfID, nil
}

// dropOptions builds what the privilege drop needs from the configuration.
// A configuration error is reported by the command itself. The drop does not
// wait for it: the process must not stay root because a flag is wrong.
func dropOptions(cmd *cobra.Command) privdrop.Options {
	var opts privdrop.Options
	if cfg, cerr := loadConfig(cmd); cerr == nil {
		if !noDataDir[cmd.Name()] {
			opts.DataDir = cfg.DataDir
		}
		opts.GIDOverride = cfg.DockerGID
		// An unsupported DOCKER_HOST is reported by the server; no socket group is joined for it.
		if sock, perr := engine.ParseDockerHost(cfg.DockerHost); perr == nil {
			opts.SocketPath = sock
		}
	}
	return opts
}

func runServe(cmd *cobra.Command, selfID string) error {
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

	srv, err := server.Build(ctx, cfg, log, server.WithSelfContainerID(selfID))
	if err != nil {
		return err
	}
	return finishRun(srv.Run(ctx), cfg.RestoreRestart)
}

// finishRun turns what Run returned into the process's end. A restore asks for a
// restart by returning a *server.RestartError: the swap runs here, with the
// database closed and the data-directory lock still held, and then the process is
// replaced by a new Coaming ("exec") or ends with status 0 ("exit"), leaving the
// start to the container's restart policy.
func finishRun(err error, mode string) error {
	var re *server.RestartError
	if errors.As(err, &re) {
		return server.Restart(mode, re.Request)
	}
	return err
}
