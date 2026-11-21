// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Command tierplan-ingress is the supervisor of a TierPlan ingress node and PID 1 of the ingress image. It
// runs Traefik as a child, keeps the node's bundle cache on its volume, and holds the channel to TierPlan
// that delivers new bundles (D-13, D-20). It has three forms:
//
//	tierplan-ingress               run the node
//	tierplan-ingress healthcheck   exit 0 when the readiness endpoint answers 200, else 1 (D-18)
//	tierplan-ingress --version     print the version
//
// Logs are JSON on standard output. A configuration error is one line on standard error and exit status 2.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ianglennon/tierplan/ingress/internal/cache"
	"github.com/ianglennon/tierplan/ingress/internal/config"
	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/ingress/internal/identity"
	"github.com/ianglennon/tierplan/ingress/internal/ready"
	"github.com/ianglennon/tierplan/ingress/internal/supervisor"
	"github.com/ianglennon/tierplan/ingress/internal/traefikproc"
	"github.com/ianglennon/tierplan/ingress/internal/version"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

const usage = "usage: tierplan-ingress [healthcheck | --version]"

// deps are the process-level effects run uses, so a test can drive it without a volume, a signal or a
// server.
type deps struct {
	args   []string
	getenv func(string) string
	stdout io.Writer
	stderr io.Writer
	// nnp sets no-new-privileges on the process (D-06).
	nnp func() error
	// readyURL is the readiness endpoint healthcheck asks. Empty means ready.DefaultReadyURL.
	readyURL string
	// serve runs the node until ctx ends.
	serve func(ctx context.Context, cfg config.Config, log *slog.Logger) error
}

func run(d deps) int {
	for _, a := range d.args {
		if a == "--version" || a == "-version" {
			_, _ = fmt.Fprintln(d.stdout, version.Version)
			return 0
		}
	}

	if len(d.args) > 0 {
		if d.args[0] == "healthcheck" && len(d.args) == 1 {
			return healthcheck(d)
		}
		_, _ = fmt.Fprintln(d.stderr, "tierplan-ingress: unknown argument", fmt.Sprintf("%q", d.args[0]))
		_, _ = fmt.Fprintln(d.stderr, usage)
		return 2
	}

	// Before anything is read or started: Traefik inherits the flag (D-06). A node that cannot set it does not run.
	if err := d.nnp(); err != nil {
		_, _ = fmt.Fprintln(d.stderr, "tierplan-ingress: no-new-privileges:", err)
		return 1
	}

	cfg, err := config.Load(d.getenv)
	if err != nil {
		_, _ = fmt.Fprintln(d.stderr, "tierplan-ingress:", err)
		return 2
	}

	log := slog.New(slog.NewJSONHandler(d.stdout, nil))
	log.Info("starting tierplan-ingress", "version", version.Version, "config", cfg)

	// PID 1 ignores a signal it has no handler for, so without this a `docker stop` would wait out its grace
	// period and kill the node, Traefik included, instead of stopping it in order (D-21).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := d.serve(ctx, cfg, log); err != nil {
		log.Error("tierplan-ingress stopped", "error", err)
		return 1
	}
	return 0
}

// healthcheck is the image's HEALTHCHECK: exit 0 when the readiness endpoint answers 200, 1 for anything else.
// It prints nothing when healthy.
func healthcheck(d deps) int {
	url := d.readyURL
	if url == "" {
		url = ready.DefaultReadyURL()
	}
	ctx, cancel := context.WithTimeout(context.Background(), ready.ProbeTimeout)
	defer cancel()
	if err := ready.Probe(ctx, ready.NewProbeClient(), url); err != nil {
		_, _ = fmt.Fprintln(d.stderr, "tierplan-ingress: healthcheck:", err)
		return 1
	}
	return 0
}

// serve builds the real node over the volume at ingressbundle.NodeRoot and runs it.
func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	fsys := fsx.OS()
	return supervisor.Run(ctx, supervisor.Deps{
		Config:   cfg,
		Store:    cache.NewStore(fsys, ingressbundle.NodeRoot),
		Identity: identity.New(fsys, ingressbundle.NodeRoot),
		Traefik:  traefikproc.New(traefikproc.Options{Log: log}),
		Ready:    &ready.State{},
		Ping:     ready.NewPinger(&http.Client{}, ready.DefaultPingURL()),
		Version:  version.Version,
		Log:      log,
	})
}

func main() {
	os.Exit(run(deps{
		args:   os.Args[1:],
		getenv: os.Getenv,
		stdout: os.Stdout,
		stderr: os.Stderr,
		nnp:    traefikproc.SetNoNewPrivs,
		serve:  serve,
	}))
}
