// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package server assembles Bayplan from its configuration. Later plans add
// their dependencies (database, sessions, Docker transport) in Build.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/webui"
	"github.com/ianglennon/bayplan/web"
)

const (
	// readHeaderTimeout and idleTimeout bound slow clients (T-01-17, Pitfall 8).
	// There is deliberately no global WriteTimeout: SSE streams in plan 21 set
	// per-handler deadlines instead.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Server is the assembled application.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	dataDir datadir.Info
	store   *store.Store
	router  *httpx.Router
}

// NewRouter wires every operation. It takes plain dependencies so the
// `openapi` subcommand can build the same handlers without a database or a
// socket.
func NewRouter(fallback http.Handler, deps api.SystemDeps) *httpx.Router {
	r := httpx.New(httpx.Options{Fallback: fallback})
	api.RegisterSystem(r, deps)
	return r
}

// Build prepares the data directory, opens and migrates the database and
// assembles the server. It logs the effective configuration with secrets
// redacted. A migration failure (or a database newer than this binary) is
// returned as an error so `serve` exits non-zero before it listens (D-82, D-84).
func Build(ctx context.Context, cfg config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	log.Info("starting bayplan",
		"version", buildinfo.Get().Display,
		"config", cfg.Redacted(),
	)

	info, err := datadir.Prepare(cfg.DataDir, datadir.Options{})
	if err != nil {
		return nil, fmt.Errorf("prepare data directory: %w", err)
	}
	if !info.Persisted {
		log.Warn("the data directory is not persisted: data written to it is lost when the container is removed; mount a named volume or bind mount at "+info.Path,
			"path", info.Path, "reason", info.Reason)
	}

	st, err := store.Open(ctx, info.Path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	res, err := store.Migrate(ctx, st, store.MigrateOptions{
		AppVersion: buildinfo.Get().Version,
		DataDir:    info.Path,
		SkipBackup: cfg.SkipPreMigrateBackup,
	})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("migration failed: %w", err)
	}
	attrs := []any{"from", res.From, "to", res.To}
	if res.SnapshotPath != "" {
		attrs = append(attrs, "snapshot", res.SnapshotPath)
	}
	log.Info("database ready", attrs...)

	// /readyz needs a usable data directory and a database at this binary's
	// schema version (D-86).
	ready := func(ctx context.Context) error {
		if err := datadir.CheckWritable(info.Path); err != nil {
			return err
		}
		return st.Ready(ctx)
	}
	return &Server{
		cfg:     cfg,
		log:     log,
		dataDir: info,
		store:   st,
		router:  NewRouter(webui.Handler(web.FS()), api.SystemDeps{Ready: ready}),
	}, nil
}

// Store is the migrated database, for the dependencies later plans add.
func (s *Server) Store() *store.Store { return s.store }

// Close releases the database. Run calls it on exit; it is safe to call again.
func (s *Server) Close() error { return s.store.Close() }

// DataDir reports the prepared data directory, for the admin banner (plan 14).
func (s *Server) DataDir() datadir.Info { return s.dataDir }

// Handler is the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.router.Handler() }

// Run serves until ctx is cancelled, then shuts down gracefully within 10s.
func (s *Server) Run(ctx context.Context) error {
	defer func() { _ = s.Close() }()
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s (BAYPLAN_LISTEN): %w", s.cfg.Listen, err)
	}
	return s.serve(ctx, ln)
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.Info("bayplan listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}
