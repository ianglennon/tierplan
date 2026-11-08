// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Command tierplan-agent connects one Docker host to a TierPlan server. It dials
// out, so the host needs no inbound port, and it passes a read-only set of
// Docker calls from the server to the local socket.
//
// It starts as root, in this order: read the configuration (which includes the
// join token, often a root-only Docker secret, and nothing in the state
// directory), drop to UID and GID 1000 in the Docker socket's group with
// no-new-privileges, check as UID 1000 that there is a join token or an identity on
// the state volume, then connect (D-21, D-22). The check waits for the drop because
// root, with every capability but SETUID and SETGID dropped, cannot open the state
// directory (G-02-5).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ianglennon/tierplan/agent/internal/config"
	"github.com/ianglennon/tierplan/agent/internal/connect"
	"github.com/ianglennon/tierplan/agent/internal/privdrop"
	"github.com/ianglennon/tierplan/agent/internal/selfmaint"
	"github.com/ianglennon/tierplan/agent/internal/state"
	"github.com/ianglennon/tierplan/agent/internal/version"
)

// deps are the process-level effects run uses, so a test can drive it without
// root, a socket or a server.
type deps struct {
	args     []string
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	drop     func(privdrop.Options) (privdrop.Result, error)
	connect  func(context.Context, connect.Config) error
	recreate func(context.Context, selfmaint.RecreateOptions) error
	// removeVolume is the removal helper of an uninstall.
	removeVolume func(context.Context, selfmaint.RemoveVolumeOptions) error
	getuid       func() int
	getgid       func() int
	stdout       io.Writer
	stderr       io.Writer
}

func run(d deps) int {
	for _, a := range d.args {
		if a == "--version" || a == "-version" {
			_, _ = fmt.Fprintln(d.stdout, version.Version)
			return 0
		}
	}

	log := slog.New(slog.NewTextHandler(d.stderr, nil))

	if len(d.args) > 0 && d.args[0] == "helper" {
		return runHelper(d, log)
	}

	// The configuration is read first, while the process can still read a
	// root-only token file. Nothing in the state directory is read yet: root
	// without capabilities cannot open it (G-02-5).
	cfg, err := config.Load(d.getenv, d.readFile)
	if err != nil {
		_, _ = fmt.Fprintln(d.stderr, "tierplan-agent:", err)
		return 2
	}

	// A variable removed from the agent only warns, so an old install command still starts.
	config.WarnRemoved(d.getenv, log)

	res, err := d.drop(privdrop.Options{SocketPath: cfg.SocketPath, DockerGID: cfg.DockerGID})
	if err != nil {
		log.Error("the agent could not drop its privileges, so it will not start", "error", err)
		return 1
	}

	// As UID 1000 now, the agent can read its own state: a token-less start needs
	// an identity there (D-03).
	if err := cfg.CheckEnrolled(d.readFile); err != nil {
		_, _ = fmt.Fprintln(d.stderr, "tierplan-agent:", err)
		return 2
	}

	log.Info("starting tierplan-agent",
		"version", version.Version,
		"server", hostOf(cfg.URL),
		"socket", cfg.SocketPath,
		"token_present", cfg.JoinToken != "",
		"enrolled", cfg.Enrolled,
		"install_pins", len(cfg.Pins),
		"read_only", cfg.ReadOnly,
		"self_update", cfg.SelfUpdate,
		"allow_privileged_services", cfg.AllowPrivilegedServices,
		"host_route_file", cfg.HostRouteFile,
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
		ReadOnly: cfg.ReadOnly, SelfUpdate: cfg.SelfUpdate, AllowPrivilegedServices: cfg.AllowPrivilegedServices, InstallPins: cfg.Pins, HostRouteFile: cfg.HostRouteFile,
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

// runHelper is the short-lived helper of a standalone self-update (D-24):
// `tierplan-agent helper recreate --target <container ID>`. The agent started it
// from the new image with the Docker socket and its state volume. It dials no
// server, so it reads no URL and no token, but it drops its privileges the same way
// the agent does. Exit 0 means the agent was replaced; 1 means it was not (the old
// agent is back, or the error says what is left to do); 2 is a bad command line.
func runHelper(d deps, log *slog.Logger) int {
	if len(d.args) >= 2 && d.args[1] == "remove-volume" {
		return runRemoveVolume(d, log)
	}
	if len(d.args) < 2 || d.args[1] != "recreate" {
		_, _ = fmt.Fprintln(d.stderr, "usage: tierplan-agent helper recreate --target <container ID>\n       tierplan-agent helper remove-volume --wait <container ID> [--volume <name>] [--remove-container] [--keep-volume]")
		return 2
	}
	flags := flag.NewFlagSet("helper recreate", flag.ContinueOnError)
	flags.SetOutput(d.stderr)
	target := flags.String("target", "", "ID of the agent container to replace")
	if err := flags.Parse(d.args[2:]); err != nil {
		return 2
	}
	if *target == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(d.stderr, "usage: tierplan-agent helper recreate --target <container ID>")
		return 2
	}
	cfg, err := config.LoadHelper(d.getenv)
	if err != nil {
		_, _ = fmt.Fprintln(d.stderr, "tierplan-agent:", err)
		return 2
	}
	if _, err := d.drop(privdrop.Options{SocketPath: cfg.SocketPath, DockerGID: cfg.DockerGID}); err != nil {
		log.Error("the helper could not drop its privileges, so it will not start", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = d.recreate(ctx, selfmaint.RecreateOptions{
		SocketPath: cfg.SocketPath, TargetID: *target, Marker: connectedMarker(cfg.StateDir),
		Timeout: selfmaint.ConnectTimeout, Log: log,
	})
	if err != nil {
		log.Error("the helper did not replace the agent", "error", err)
		return 1
	}
	return 0
}

// runRemoveVolume is the helper of an uninstall (D-30):
// `tierplan-agent helper remove-volume --wait <container ID> [--volume <name>]
// [--remove-container] [--keep-volume]`. The agent started it from its own image with
// the Docker socket and nothing else mounted. It waits until the agent's container no
// longer exists, removing it itself with --remove-container (a standalone agent), and
// then removes the named state volume. Exit 0 means it is done; 1 that something is left
// that the admin's manual cleanup command removes; 2 is a bad command line.
func runRemoveVolume(d deps, log *slog.Logger) int {
	usage := "usage: tierplan-agent helper remove-volume --wait <container ID> [--volume <name>] [--remove-container] [--keep-volume]"
	flags := flag.NewFlagSet("helper remove-volume", flag.ContinueOnError)
	flags.SetOutput(d.stderr)
	wait := flags.String("wait", "", "ID of the agent container to wait for")
	volume := flags.String("volume", "", "name of the agent's state volume to remove once the container is gone")
	removeContainer := flags.Bool("remove-container", false, "remove the agent container itself, after a moment for it to answer the server")
	keepVolume := flags.Bool("keep-volume", false, "leave the volume")
	if err := flags.Parse(d.args[2:]); err != nil {
		return 2
	}
	if *wait == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(d.stderr, usage)
		return 2
	}
	cfg, err := config.LoadHelper(d.getenv)
	if err != nil {
		_, _ = fmt.Fprintln(d.stderr, "tierplan-agent:", err)
		return 2
	}
	if _, err := d.drop(privdrop.Options{SocketPath: cfg.SocketPath, DockerGID: cfg.DockerGID}); err != nil {
		log.Error("the helper could not drop its privileges, so it will not start", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := selfmaint.RemoveVolumeOptions{
		SocketPath: cfg.SocketPath, ContainerID: *wait, Volume: *volume, KeepVolume: *keepVolume,
		RemoveContainer: *removeContainer, Timeout: selfmaint.RemoveTimeout, Log: log,
	}
	if *removeContainer {
		opts.Grace = selfmaint.RemoveGrace
	}
	if err := d.removeVolume(ctx, opts); err != nil {
		log.Error("the helper did not finish the removal", "error", err)
		return 1
	}
	return 0
}

// connectedMarker reads when the agent last connected: the modification time of the
// connected file in the state directory, which the agent writes by rename after each
// completed handshake. A missing file is the zero time, not an error: the agent has
// not connected yet.
func connectedMarker(dir string) func() (time.Time, error) {
	path := filepath.Join(dir, state.ConnectedFile)
	return func() (time.Time, error) {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, nil
		}
		if err != nil {
			return time.Time{}, err
		}
		return fi.ModTime(), nil
	}
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
// touchState makes the state directory private and writes the start marker.
// The image creates the directory 0700, but a builder that keeps the mode of
// the checkout (git stores none) leaves it 0755, so the agent, which owns it,
// tightens it itself. It never creates the directory.
func touchState(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs its search bit; 0700 is owner-only
		return err
	}
	return os.WriteFile(filepath.Join(dir, "started"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

func main() {
	os.Exit(run(deps{
		args:         os.Args[1:],
		getenv:       os.Getenv,
		readFile:     os.ReadFile,
		drop:         privdrop.Drop,
		connect:      connect.Run,
		recreate:     selfmaint.RecreateAgent,
		removeVolume: selfmaint.RemoveVolumeAfter,
		getuid:       os.Getuid,
		getgid:       os.Getgid,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
	}))
}
