// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package config reads the agent's settings from its environment.
package config

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

// Load is not implemented yet.
func Load(env func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	return Config{}, nil
}
