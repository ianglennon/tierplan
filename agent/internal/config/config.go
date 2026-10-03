// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package config reads the agent's settings from its environment.
//
// Load runs while the process is still root (before privdrop.Drop), because a
// Docker secret is mounted root-owned with mode 0400 and the dropped user could
// not read it (D-03, D-22, Pitfall 9). Later plans add their keys here.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// DefaultSocket is the Docker socket path inside the image.
	DefaultSocket = "/var/run/docker.sock"
	// DefaultStateDir is the agent's state directory; the image pre-creates it
	// owned by UID and GID 1000.
	DefaultStateDir = "/var/lib/bayplan-agent"

	// identityFile is written into the state directory when the agent enrols.
	// Its presence means a join token is no longer needed.
	identityFile = "identity.json"
)

// Config is what the agent needs to start.
type Config struct {
	// URL is the server's agent endpoint (ws:// or wss://).
	URL string
	// JoinToken is the one-time enrolment secret. It is never logged.
	JoinToken string
	// SocketPath is the Docker socket.
	SocketPath string
	// StateDir is the agent's persistent state directory.
	StateDir string
	// DockerGID overrides the group detected from the Docker socket.
	DockerGID *int
	// Enrolled is true when the state directory already holds an identity, so
	// a join token is not required.
	Enrolled bool
}

// Load reads the configuration. env looks up an environment variable and
// readFile reads a file; both are injected so tests need neither. Error text
// names variables and paths, never the token's value.
//
//	BAYPLAN_AGENT_URL              required, ws:// or wss://
//	BAYPLAN_AGENT_JOIN_TOKEN_FILE  path of a Docker secret holding the token; wins over the variable
//	BAYPLAN_AGENT_JOIN_TOKEN       the token itself
//	BAYPLAN_AGENT_SOCKET           default /var/run/docker.sock
//	BAYPLAN_AGENT_STATE_DIR        default /var/lib/bayplan-agent
//	BAYPLAN_AGENT_DOCKER_GID       non-negative integer; overrides the socket's group
func Load(env func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	cfg := Config{
		URL:        env("BAYPLAN_AGENT_URL"),
		SocketPath: env("BAYPLAN_AGENT_SOCKET"),
		StateDir:   env("BAYPLAN_AGENT_STATE_DIR"),
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocket
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}

	if err := checkURL(cfg.URL); err != nil {
		return Config{}, err
	}

	gid, err := parseGID(env("BAYPLAN_AGENT_DOCKER_GID"))
	if err != nil {
		return Config{}, err
	}
	cfg.DockerGID = gid

	token, err := readToken(env, readFile)
	if err != nil {
		return Config{}, err
	}
	cfg.JoinToken = token

	if cfg.JoinToken == "" {
		if _, err := readFile(filepath.Join(cfg.StateDir, identityFile)); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return Config{}, fmt.Errorf("read the enrolled state in %s: %w", cfg.StateDir, err)
			}
			return Config{}, errors.New("no join token: set BAYPLAN_AGENT_JOIN_TOKEN_FILE (a Docker secret) or BAYPLAN_AGENT_JOIN_TOKEN; copy the install command from Bayplan's Add environment screen")
		}
		cfg.Enrolled = true
	}
	return cfg, nil
}

// checkURL requires a ws:// or wss:// address with a host. The URL itself can
// hold no secret: the token travels in the handshake, never in a URL (D-20).
func checkURL(raw string) error {
	if raw == "" {
		return errors.New("BAYPLAN_AGENT_URL must be set to the server's agent address, for example wss://bayplan.example/agent/connect")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("BAYPLAN_AGENT_URL must be a ws:// or wss:// address with a host, got %q", raw)
	}
	return nil
}

func parseGID(raw string) (*int, error) {
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("BAYPLAN_AGENT_DOCKER_GID must be a non-negative integer, got %q", raw)
	}
	return &n, nil
}

// readToken returns the token from the file when one is named, else from the
// variable, else "". A named file that cannot give a token is an error rather
// than a silent fall back to the variable.
func readToken(env func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	if path := env("BAYPLAN_AGENT_JOIN_TOKEN_FILE"); path != "" {
		b, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("BAYPLAN_AGENT_JOIN_TOKEN_FILE: %w", err)
		}
		token := strings.TrimSpace(string(b))
		if token == "" {
			return "", fmt.Errorf("BAYPLAN_AGENT_JOIN_TOKEN_FILE: %s is empty", path)
		}
		return token, nil
	}
	return strings.TrimSpace(env("BAYPLAN_AGENT_JOIN_TOKEN")), nil
}
