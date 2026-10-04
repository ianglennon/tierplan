// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Command bayplan-agent connects one Docker host to a Bayplan server. It dials
// out, so the host needs no inbound port, and it passes a read-only set of
// Docker calls from the server to the local socket.
//
// It starts as root, in this order: read the configuration (which includes the
// join token, often a root-only Docker secret), drop to UID and GID 1000 in the
// Docker socket's group with no-new-privileges, then connect (D-21, D-22).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ianglennon/bayplan/agent/internal/config"
	"github.com/ianglennon/bayplan/agent/internal/connect"
	"github.com/ianglennon/bayplan/agent/internal/privdrop"
	"github.com/ianglennon/bayplan/agent/internal/selfmaint"
	"github.com/ianglennon/bayplan/agent/internal/version"
)

// deps are the process-level effects run uses, so a test can drive it without
// root, a socket or a server.
type deps struct {
	args     []string
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	drop     func(privdrop.Options) (privdrop.Result, error)
	connect  func(context.Context, connect.Config) error
	getuid   func() int
	getgid   func() int
	stdout   io.Writer
	stderr   io.Writer
}

func run(d deps) int {
	for _, a := range d.args {
		if a == "--version" || a == "-version" {
			_, _ = fmt.Fprintln(d.stdout, version.Version)
			return 0
		}
	}

	log := slog.New(slog.NewTextHandler(d.stderr, nil))

	// The configuration is read first, while the process can still read a
	// root-only token file.
	cfg, err := config.Load(d.getenv, d.readFile)
	if err != nil {
		_, _ = fmt.Fprintln(d.stderr, "bayplan-agent:", err)
		return 2
	}

	res, err := d.drop(privdrop.Options{SocketPath: cfg.SocketPath, DockerGID: cfg.DockerGID})
	if err != nil {
		log.Error("the agent could not drop its privileges, so it will not start", "error", err)
		return 1
	}

	log.Info("starting bayplan-agent",
		"version", version.Version,
		"server", hostOf(cfg.URL),
		"socket", cfg.SocketPath,
		"token_present", cfg.JoinToken != "",
		"enrolled", cfg.Enrolled,
		"install_pins", len(cfg.Pins),
		"read_only", cfg.ReadOnly,
		"self_update", cfg.SelfUpdate,
		"dropped_privileges", res.Dropped,
		"uid", d.getuid(),
		"gid", d.getgid(),
		"docker_gid", res.GID,
	)

	// The state directory belongs to UID 1000 in the image. Say so now if it
	// is not writable, rather than when the first thing needs it.
	if err := touchState(cfg.StateDir); err != nil {
		log.Warn("the state directory is not writable; the agent cannot keep its identity across restarts", "dir", cfg.StateDir, "error", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = d.connect(ctx, connect.Config{
		URL: cfg.URL, JoinToken: cfg.JoinToken, SocketPath: cfg.SocketPath, Version: version.Version, Log: log,
		StateDir: cfg.StateDir, BackoffMin: cfg.BackoffMin, BackoffMax: cfg.BackoffMax,
		ReadOnly: cfg.ReadOnly, SelfUpdate: cfg.SelfUpdate, InstallPins: cfg.Pins,
		Maint: selfmaint.New(selfmaint.Config{
			SocketPath: cfg.SocketPath, SelfUpdate: cfg.SelfUpdate, StateDir: cfg.StateDir, Log: log,
		}),
	})
	if err == nil {
		return 0
	}
	var refused *connect.RefusedError
	if errors.As(err, &refused) {
		log.Error("the server refused this agent", "code", refused.Code, "detail", refused.Detail)
	} else {
		log.Error("the agent stopped", "error", err)
	}
	// Only a configuration the agent cannot work with, or a refusal of its very
	// first enrolment, ends it: a dropped connection or a refusal of an enrolled
	// agent is retried inside connect.Run. A supervisor (the restart policy or the
	// swarm) starts it again.
	return 1
}

// hostOf is the host part of the server URL, for the log. The URL carries no
// secret (the token travels in the handshake), but a log line needs only the host.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// touchState writes the start marker in the state directory. It proves the
// dropped user can write there (the image smoke checks the file), and the
// connected marker of a later plan lives beside it. It never creates the
// directory: the image provides it with the right owner.
func touchState(dir string) error {
	return os.WriteFile(filepath.Join(dir, "started"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

func main() {
	os.Exit(run(deps{
		args:     os.Args[1:],
		getenv:   os.Getenv,
		readFile: os.ReadFile,
		drop:     privdrop.Drop,
		connect:  connect.Run,
		getuid:   os.Getuid,
		getgid:   os.Getgid,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	}))
}
