// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package config

import (
	"bytes"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
)

// env builds a lookup function from a map.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// files builds a readFile function over an in-memory file system; a missing
// name is fs.ErrNotExist.
func files(m map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if v, ok := m[name]; ok {
			return []byte(v), nil
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
}

const goodURL = "wss://bayplan.example/agent/connect"

func TestLoadTrimsTheTokenFileNewline(t *testing.T) {
	cfg, err := Load(
		env(map[string]string{
			"BAYPLAN_AGENT_URL":             goodURL,
			"BAYPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/bayplan-agent-token",
		}),
		files(map[string]string{"/run/secrets/bayplan-agent-token": "bpjt_x_y\n"}), //nolint:gosec // a test fixture, not a credential
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JoinToken != "bpjt_x_y" {
		t.Fatalf("JoinToken = %q, want bpjt_x_y", cfg.JoinToken)
	}
}

func TestLoadPrefersTheTokenFileOverTheEnvironmentVariable(t *testing.T) {
	cfg, err := Load(
		env(map[string]string{ //nolint:gosec // a test fixture, not a credential
			"BAYPLAN_AGENT_URL":             goodURL,
			"BAYPLAN_AGENT_JOIN_TOKEN":      "from-env",
			"BAYPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/t",
		}),
		files(map[string]string{"/run/secrets/t": "from-file\n"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JoinToken != "from-file" {
		t.Fatalf("JoinToken = %q, want the file's value", cfg.JoinToken)
	}
}

func TestLoadUsesTheEnvironmentTokenWhenThereIsNoFile(t *testing.T) {
	cfg, err := Load(
		env(map[string]string{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "bpjt_a_b"}), //nolint:gosec // a test fixture, not a credential
		files(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JoinToken != "bpjt_a_b" {
		t.Fatalf("JoinToken = %q, want bpjt_a_b", cfg.JoinToken)
	}
}

func TestLoadWithoutATokenOrAnEnrolledStateNamesBothVariables(t *testing.T) {
	_, err := Load(env(map[string]string{"BAYPLAN_AGENT_URL": goodURL}), files(nil))
	if err == nil {
		t.Fatal("want an error when there is no token and no enrolled state")
	}
	for _, name := range []string{"BAYPLAN_AGENT_JOIN_TOKEN_FILE", "BAYPLAN_AGENT_JOIN_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error %q does not name %s", err, name)
		}
	}
}

func TestLoadAcceptsAnEnrolledStateWithoutAToken(t *testing.T) {
	cfg, err := Load(
		env(map[string]string{"BAYPLAN_AGENT_URL": goodURL}),
		files(map[string]string{"/var/lib/bayplan-agent/identity.json": "{}"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enrolled {
		t.Error("Enrolled = false, want true when identity.json exists in the state directory")
	}
	if cfg.JoinToken != "" {
		t.Errorf("JoinToken = %q, want empty", cfg.JoinToken)
	}
}

func TestLoadFindsTheEnrolledStateInACustomStateDirectory(t *testing.T) {
	cfg, err := Load(
		env(map[string]string{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_STATE_DIR": "/state"}),
		files(map[string]string{"/state/identity.json": "{}"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enrolled || cfg.StateDir != "/state" {
		t.Fatalf("Enrolled=%v StateDir=%q, want true and /state", cfg.Enrolled, cfg.StateDir)
	}
}

func TestLoadRefusesAnEmptyOrUnreadableTokenFile(t *testing.T) {
	for name, fsys := range map[string]map[string]string{
		"empty":      {"/run/secrets/t": "\n"},
		"unreadable": {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(
				env(map[string]string{ //nolint:gosec // a test fixture, not a credential
					"BAYPLAN_AGENT_URL":             goodURL,
					"BAYPLAN_AGENT_JOIN_TOKEN":      "from-env",
					"BAYPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/t",
				}),
				files(fsys),
			)
			if err == nil {
				t.Fatal("want an error: a named token file that cannot give a token must not fall back silently")
			}
			if !strings.Contains(err.Error(), "BAYPLAN_AGENT_JOIN_TOKEN_FILE") {
				t.Errorf("the error %q does not name BAYPLAN_AGENT_JOIN_TOKEN_FILE", err)
			}
		})
	}
}

func TestLoadRequiresAWebSocketURL(t *testing.T) {
	for _, u := range []string{"", "https://bayplan.example/agent/connect", "bayplan.example", "ftp://x"} {
		_, err := Load(
			env(map[string]string{"BAYPLAN_AGENT_URL": u, "BAYPLAN_AGENT_JOIN_TOKEN": "tok"}),
			files(nil),
		)
		if err == nil {
			t.Errorf("URL %q: want an error", u)
			continue
		}
		if !strings.Contains(err.Error(), "BAYPLAN_AGENT_URL") {
			t.Errorf("URL %q: the error %q does not name BAYPLAN_AGENT_URL", u, err)
		}
	}
	for _, u := range []string{"ws://127.0.0.1:7480/agent/connect", goodURL} {
		if _, err := Load(
			env(map[string]string{"BAYPLAN_AGENT_URL": u, "BAYPLAN_AGENT_JOIN_TOKEN": "tok"}),
			files(nil),
		); err != nil {
			t.Errorf("URL %q: %v", u, err)
		}
	}
}

func TestLoadParsesTheDockerGID(t *testing.T) {
	base := map[string]string{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "tok"}
	with := func(v string) map[string]string {
		m := map[string]string{"BAYPLAN_AGENT_DOCKER_GID": v}
		for k, x := range base {
			m[k] = x
		}
		return m
	}

	cfg, err := Load(env(base), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DockerGID != nil {
		t.Errorf("DockerGID = %d, want nil when unset", *cfg.DockerGID)
	}

	for v, want := range map[string]int{"42": 42, "0": 0} {
		cfg, err := Load(env(with(v)), files(nil))
		if err != nil {
			t.Fatalf("GID %q: %v", v, err)
		}
		if cfg.DockerGID == nil || *cfg.DockerGID != want {
			t.Errorf("GID %q: DockerGID = %v, want %d", v, cfg.DockerGID, want)
		}
	}

	for _, v := range []string{"-1", "abc", "1.5", "99999999999999999999"} {
		_, err := Load(env(with(v)), files(nil))
		if err == nil {
			t.Errorf("GID %q: want an error", v)
			continue
		}
		if !strings.Contains(err.Error(), "BAYPLAN_AGENT_DOCKER_GID") {
			t.Errorf("GID %q: the error %q does not name BAYPLAN_AGENT_DOCKER_GID", v, err)
		}
	}
}

func TestLoadDefaultsAndOverrides(t *testing.T) {
	base := map[string]string{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "tok"}
	cfg, err := Load(env(base), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocketPath != "/var/run/docker.sock" || cfg.StateDir != "/var/lib/bayplan-agent" {
		t.Fatalf("defaults: SocketPath=%q StateDir=%q", cfg.SocketPath, cfg.StateDir)
	}
	base["BAYPLAN_AGENT_SOCKET"] = "/run/docker.sock"
	base["BAYPLAN_AGENT_STATE_DIR"] = "/data"
	cfg, err = Load(env(base), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocketPath != "/run/docker.sock" || cfg.StateDir != "/data" || cfg.URL != goodURL {
		t.Fatalf("overrides: %+v", cfg)
	}
}

// The secret must not leak through an error or a log line (T-02-18).
func TestLoadNeverPutsTheTokenInAnErrorOrALogLine(t *testing.T) {
	const secret = "bpjt_SECRETVALUE_zzz" //nolint:gosec // a test fixture, not a credential
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cases := []map[string]string{
		{"BAYPLAN_AGENT_URL": "https://wrong", "BAYPLAN_AGENT_JOIN_TOKEN": secret},
		{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": secret, "BAYPLAN_AGENT_DOCKER_GID": "nope"},
		{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": secret},
	}
	for _, c := range cases {
		_, err := Load(env(c), files(nil))
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("the error leaks the token: %v", err)
		}
	}
	// The file form, read successfully and then failing on the URL.
	_, err := Load(
		env(map[string]string{"BAYPLAN_AGENT_URL": "nope", "BAYPLAN_AGENT_JOIN_TOKEN_FILE": "/t"}),
		files(map[string]string{"/t": secret + "\n"}),
	)
	if err == nil {
		t.Fatal("want an error for the bad URL")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error leaks the token: %v", err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the log leaks the token: %s", logs.String())
	}
}
