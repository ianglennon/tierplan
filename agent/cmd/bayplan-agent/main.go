// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Command bayplan-agent connects one Docker host to a Bayplan server. It dials
// out, so the host needs no inbound port, and it passes a read-only set of
// Docker calls from the server to the local socket.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ianglennon/bayplan/agent/internal/connect"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Getenv, os.Stderr))
}

func run(getenv func(string) string, stderr *os.File) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))

	url := getenv("BAYPLAN_AGENT_URL")
	token := getenv("BAYPLAN_AGENT_JOIN_TOKEN")
	if url == "" || token == "" {
		_, _ = fmt.Fprintln(stderr, "bayplan-agent: BAYPLAN_AGENT_URL and BAYPLAN_AGENT_JOIN_TOKEN must both be set; copy the install command from Bayplan's Add environment screen")
		return 2
	}
	socket := getenv("BAYPLAN_AGENT_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting bayplan-agent", "version", version, "socket", socket)
	err := connect.Run(ctx, connect.Config{
		URL: url, JoinToken: token, SocketPath: socket, Version: version, Log: log,
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
	// A supervisor (the restart policy or the swarm) starts it again.
	return 1
}
