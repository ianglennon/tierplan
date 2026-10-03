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
	"time"
)

const (
	// DefaultSocket is the Docker socket path inside the image.
	DefaultSocket = "/var/run/docker.sock"
	// DefaultStateDir is the agent's state directory; the image pre-creates it
	// owned by UID and GID 1000.
	DefaultStateDir = "/var/lib/bayplan-agent"

	// DefaultBackoffMin and DefaultBackoffMax bound the reconnect delay (D-41).
	DefaultBackoffMin = time.Second
	DefaultBackoffMax = time.Minute

	// envReadOnly makes the agent refuse every Docker call that is not a GET or
	// HEAD, whatever the server's allowlist says (D-28). The default is false.
	envReadOnly = "BAYPLAN_AGENT_READ_ONLY"
	// envSelfUpdate says whether the agent may update or uninstall itself when the
	// server asks (D-28). The default is true; a later plan implements what it gates.
	envSelfUpdate = "BAYPLAN_AGENT_SELF_UPDATE"

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
	// BackoffMin and BackoffMax bound the reconnect delay (D-41).
	BackoffMin, BackoffMax time.Duration
	// Enrolled is true when the state directory already holds an identity, so
	// a join token is not required.
	Enrolled bool
	// ReadOnly refuses every Docker call that is not a read, and SelfUpdate says
	// whether the agent may update itself (D-28).
	ReadOnly, SelfUpdate bool
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
//	BAYPLAN_AGENT_BACKOFF_MIN      first reconnect delay, default 1s
//	BAYPLAN_AGENT_BACKOFF_MAX      longest reconnect delay, default 60s; at least the minimum
//
// The two host policy flags (envReadOnly and envSelfUpdate) are true or false; a
// value that is neither stops the agent, so that a restriction is never dropped
// because it was misspelled.
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

	cfg.BackoffMin, cfg.BackoffMax, err = parseBackoff(env("BAYPLAN_AGENT_BACKOFF_MIN"), env("BAYPLAN_AGENT_BACKOFF_MAX"))
	if err != nil {
		return Config{}, err
	}

	if cfg.ReadOnly, err = parseFlag(env, envReadOnly, false); err != nil {
		return Config{}, err
	}
	if cfg.SelfUpdate, err = parseFlag(env, envSelfUpdate, true); err != nil {
		return Config{}, err
	}

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

// parseFlag reads a true/false variable, or its default when it is unset.
func parseFlag(env func(string) string, name string, def bool) (bool, error) {
	raw := strings.TrimSpace(env(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", name, raw)
	}
	return v, nil
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

// parseBackoff reads the two reconnect bounds. Both must be positive durations
// and the minimum may not exceed the maximum, whichever of them was defaulted.
func parseBackoff(rawMin, rawMax string) (lo, hi time.Duration, err error) {
	lo, hi = DefaultBackoffMin, DefaultBackoffMax
	if rawMin != "" {
		if lo, err = time.ParseDuration(rawMin); err != nil || lo <= 0 {
			return 0, 0, fmt.Errorf("BAYPLAN_AGENT_BACKOFF_MIN must be a positive duration such as 1s, got %q", rawMin)
		}
	}
	if rawMax != "" {
		if hi, err = time.ParseDuration(rawMax); err != nil || hi <= 0 {
			return 0, 0, fmt.Errorf("BAYPLAN_AGENT_BACKOFF_MAX must be a positive duration such as 60s, got %q", rawMax)
		}
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("BAYPLAN_AGENT_BACKOFF_MIN (%s) must not be longer than BAYPLAN_AGENT_BACKOFF_MAX (%s)", lo, hi)
	}
	return lo, hi, nil
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
