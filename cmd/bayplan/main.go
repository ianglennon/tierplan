// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Command bayplan is the Bayplan server and its small command line.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/webui"
	"github.com/ianglennon/bayplan/web"
)

// defaultListen is the default listen address (D-10).
const defaultListen = ":7480"

func main() {
	if err := newRoot().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	listen := envOr("BAYPLAN_LISTEN", defaultListen)

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the Bayplan server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), listen)
		},
	}
	serve.Flags().StringVar(&listen, "listen", listen, "address to listen on (env BAYPLAN_LISTEN)")

	root := &cobra.Command{
		Use:           "bayplan",
		Short:         "Bayplan: manage Docker Swarm clusters and standalone hosts",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		// The default action is serve.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), listen)
		},
	}
	root.Flags().StringVar(&listen, "listen", listen, "address to listen on (env BAYPLAN_LISTEN)")

	root.AddCommand(
		serve,
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

// buildRouter wires every operation. It takes no dependencies so `openapi` can
// build the same handlers without a database or a socket.
func buildRouter(fallback http.Handler) *httpx.Router {
	r := httpx.New(httpx.Options{Fallback: fallback})
	api.RegisterSystem(r)
	return r
}

func writeOpenAPI(w io.Writer) error {
	doc, err := json.MarshalIndent(buildRouter(nil).API().OpenAPI(), "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(doc, '\n'))
	return err
}

func runServe(ctx context.Context, listen string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	router := buildRouter(webui.Handler(web.FS()))
	srv := &http.Server{
		Addr:              listen,
		Handler:           router.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("bayplan listening", "addr", listen, "version", buildinfo.Get().Display)

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
