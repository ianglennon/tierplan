// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package config

import (
	"bytes"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"
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

// load runs Load and fails the test on an error: the helper of the CheckEnrolled
// tests, which care about what happens after the configuration is read.
func load(t *testing.T, m map[string]string) Config {
	t.Helper()
	cfg, err := Load(env(m), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCheckEnrolledWithoutATokenOrAnIdentityNamesBothVariables(t *testing.T) {
	cfg := load(t, map[string]string{"BAYPLAN_AGENT_URL": goodURL})
	err := cfg.CheckEnrolled(files(nil))
	if err == nil {
		t.Fatal("want an error when there is no token and no enrolled state")
	}
	for _, name := range []string{"BAYPLAN_AGENT_JOIN_TOKEN_FILE", "BAYPLAN_AGENT_JOIN_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error %q does not name %s", err, name)
		}
	}
	if cfg.Enrolled {
		t.Error("Enrolled = true after a refusal")
	}
}

func TestCheckEnrolledAcceptsAnIdentityWithoutAToken(t *testing.T) {
	cfg := load(t, map[string]string{"BAYPLAN_AGENT_URL": goodURL})
	if err := cfg.CheckEnrolled(files(map[string]string{"/var/lib/bayplan-agent/identity.json": "{}"})); err != nil {
		t.Fatal(err)
	}
	if !cfg.Enrolled {
		t.Error("Enrolled = false, want true when identity.json exists in the state directory")
	}
	if cfg.JoinToken != "" {
		t.Errorf("JoinToken = %q, want empty", cfg.JoinToken)
	}
}

func TestCheckEnrolledFindsTheIdentityInACustomStateDirectory(t *testing.T) {
	cfg := load(t, map[string]string{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_STATE_DIR": "/state"})
	if err := cfg.CheckEnrolled(files(map[string]string{"/state/identity.json": "{}"})); err != nil {
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

func TestLoadDefaultsTheReconnectBackoffToOneAndSixtySeconds(t *testing.T) {
	cfg, err := Load(env(map[string]string{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "t"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BackoffMin != time.Second || cfg.BackoffMax != time.Minute {
		t.Fatalf("backoff = %v..%v, want 1s..1m0s", cfg.BackoffMin, cfg.BackoffMax)
	}
}

func TestLoadReadsTheBackoffOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "t",
		"BAYPLAN_AGENT_BACKOFF_MIN": "250ms", "BAYPLAN_AGENT_BACKOFF_MAX": "5s",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BackoffMin != 250*time.Millisecond || cfg.BackoffMax != 5*time.Second {
		t.Fatalf("backoff = %v..%v, want 250ms..5s", cfg.BackoffMin, cfg.BackoffMax)
	}
}

func TestLoadRejectsABackoffThatIsInvalidOrInverted(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"min above max":             {"BAYPLAN_AGENT_BACKOFF_MIN": "30s", "BAYPLAN_AGENT_BACKOFF_MAX": "10s"},
		"min not a time":            {"BAYPLAN_AGENT_BACKOFF_MIN": "soon"},
		"max not a time":            {"BAYPLAN_AGENT_BACKOFF_MAX": "1 minute"},
		"min zero":                  {"BAYPLAN_AGENT_BACKOFF_MIN": "0s"},
		"max negative":              {"BAYPLAN_AGENT_BACKOFF_MAX": "-5s"},
		"min above the default max": {"BAYPLAN_AGENT_BACKOFF_MIN": "2m"},
	} {
		vars["BAYPLAN_AGENT_URL"], vars["BAYPLAN_AGENT_JOIN_TOKEN"] = goodURL, "t"
		_, err := Load(env(vars), files(nil))
		if err == nil || !strings.Contains(err.Error(), "BAYPLAN_AGENT_BACKOFF") {
			t.Errorf("%s: Load error = %v, want one naming BAYPLAN_AGENT_BACKOFF_*", name, err)
		}
	}
}

func TestLoadDefaultsToReadWriteAllowedAndSelfUpdateOn(t *testing.T) {
	cfg, err := Load(env(map[string]string{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "tok"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReadOnly || !cfg.SelfUpdate {
		t.Fatalf("ReadOnly %v SelfUpdate %v, want false and true", cfg.ReadOnly, cfg.SelfUpdate)
	}
}

func TestLoadReadsTheHostPolicyFlags(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "tok",
		"BAYPLAN_AGENT_READ_ONLY": "true", "BAYPLAN_AGENT_SELF_UPDATE": "false",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReadOnly || cfg.SelfUpdate {
		t.Fatalf("ReadOnly %v SelfUpdate %v, want true and false", cfg.ReadOnly, cfg.SelfUpdate)
	}
}

// A flag that is meant to restrict must not be silently ignored because it was
// misspelled: the agent refuses to start and names the variable.
func TestLoadRefusesAPolicyFlagItCannotRead(t *testing.T) {
	for _, name := range []string{"BAYPLAN_AGENT_READ_ONLY", "BAYPLAN_AGENT_SELF_UPDATE"} {
		_, err := Load(env(map[string]string{
			"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "tok", name: "sure",
		}), files(nil))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=sure: error = %v, want one naming the variable", name, err)
		}
	}
}

func TestLoadReadsTheServerPinsFromTheInstallCommand(t *testing.T) {
	a := "sha256/" + strings.Repeat("A", 43) + "="
	b := "sha256/" + strings.Repeat("B", 43) + "="
	base := map[string]string{"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "bpjt_x_y"} //nolint:gosec // a test fixture, not a credential
	load := func(pin string) (Config, error) {
		m := map[string]string{"BAYPLAN_AGENT_PIN": pin}
		for k, v := range base {
			m[k] = v
		}
		return Load(env(m), files(nil))
	}

	cfg, err := load("")
	if err != nil || len(cfg.Pins) != 0 {
		t.Fatalf("no pin: %v, %v; want none", cfg.Pins, err)
	}
	cfg, err = load(a)
	if err != nil || len(cfg.Pins) != 1 || cfg.Pins[0] != a {
		t.Fatalf("one pin: %v, %v", cfg.Pins, err)
	}
	cfg, err = load(" " + a + " , " + b + ",")
	if err != nil || len(cfg.Pins) != 2 || cfg.Pins[0] != a || cfg.Pins[1] != b {
		t.Fatalf("two pins: %v, %v", cfg.Pins, err)
	}
}

func TestLoadRefusesAPinThatIsNotASHA256Pin(t *testing.T) {
	for _, bad := range []string{"sha1/abc", "AAAA", "sha256/short", "sha256/" + strings.Repeat("A", 44), "sha256/" + strings.Repeat("!", 43) + "="} {
		_, err := Load(env(map[string]string{ //nolint:gosec // a test fixture, not a credential
			"BAYPLAN_AGENT_URL": goodURL, "BAYPLAN_AGENT_JOIN_TOKEN": "bpjt_x_y", "BAYPLAN_AGENT_PIN": bad,
		}), files(nil))
		if err == nil || !strings.Contains(err.Error(), "BAYPLAN_AGENT_PIN") {
			t.Errorf("pin %q: error = %v, want one naming BAYPLAN_AGENT_PIN", bad, err)
		}
	}
}

func TestLoadHelperNeedsNoURLAndNoToken(t *testing.T) {
	cfg, err := LoadHelper(env(nil))
	if err != nil {
		t.Fatalf("a helper needs neither a URL nor a token: %v", err)
	}
	if cfg.SocketPath != DefaultSocket || cfg.StateDir != DefaultStateDir || cfg.DockerGID != nil {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadHelperReadsTheSocketTheStateDirectoryAndTheGroup(t *testing.T) {
	cfg, err := LoadHelper(env(map[string]string{
		"BAYPLAN_AGENT_SOCKET": "/x/d.sock", "BAYPLAN_AGENT_STATE_DIR": "/x/state", "BAYPLAN_AGENT_DOCKER_GID": "7",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocketPath != "/x/d.sock" || cfg.StateDir != "/x/state" || cfg.DockerGID == nil || *cfg.DockerGID != 7 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadHelperRefusesAGroupThatIsNotANumber(t *testing.T) {
	if _, err := LoadHelper(env(map[string]string{"BAYPLAN_AGENT_DOCKER_GID": "docker"})); err == nil || !strings.Contains(err.Error(), "BAYPLAN_AGENT_DOCKER_GID") {
		t.Fatalf("err = %v", err)
	}
}
