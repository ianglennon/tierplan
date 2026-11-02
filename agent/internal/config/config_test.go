// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package config

import (
	"bytes"
	"errors"
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

const goodURL = "wss://tierplan.example/agent/connect"

func TestLoadTrimsTheTokenFileNewline(t *testing.T) {
	cfg, err := Load(
		env(map[string]string{ //nolint:gosec // a test fixture, not a credential
			"TIERPLAN_AGENT_URL":             goodURL,
			"TIERPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/tierplan-agent-token",
		}),
		files(map[string]string{"/run/secrets/tierplan-agent-token": "cmjt_x_y\n"}), //nolint:gosec // a test fixture, not a credential
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JoinToken != "cmjt_x_y" {
		t.Fatalf("JoinToken = %q, want cmjt_x_y", cfg.JoinToken)
	}
}

func TestLoadPrefersTheTokenFileOverTheEnvironmentVariable(t *testing.T) {
	cfg, err := Load(
		env(map[string]string{ //nolint:gosec // a test fixture, not a credential
			"TIERPLAN_AGENT_URL":             goodURL,
			"TIERPLAN_AGENT_JOIN_TOKEN":      "from-env",
			"TIERPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/t",
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
		env(map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "cmjt_a_b"}), //nolint:gosec // a test fixture, not a credential
		files(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JoinToken != "cmjt_a_b" {
		t.Fatalf("JoinToken = %q, want cmjt_a_b", cfg.JoinToken)
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
	cfg := load(t, map[string]string{"TIERPLAN_AGENT_URL": goodURL})
	err := cfg.CheckEnrolled(files(nil))
	if err == nil {
		t.Fatal("want an error when there is no token and no enrolled state")
	}
	for _, name := range []string{"TIERPLAN_AGENT_JOIN_TOKEN_FILE", "TIERPLAN_AGENT_JOIN_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error %q does not name %s", err, name)
		}
	}
	if cfg.Enrolled {
		t.Error("Enrolled = true after a refusal")
	}
}

func TestCheckEnrolledAcceptsAnIdentityWithoutAToken(t *testing.T) {
	cfg := load(t, map[string]string{"TIERPLAN_AGENT_URL": goodURL})
	if err := cfg.CheckEnrolled(files(map[string]string{"/var/lib/tierplan-agent/identity.json": "{}"})); err != nil {
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
	cfg := load(t, map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_STATE_DIR": "/state"})
	if err := cfg.CheckEnrolled(files(map[string]string{"/state/identity.json": "{}"})); err != nil {
		t.Fatal(err)
	}
	if !cfg.Enrolled || cfg.StateDir != "/state" {
		t.Fatalf("Enrolled=%v StateDir=%q, want true and /state", cfg.Enrolled, cfg.StateDir)
	}
}

// recorded is a readFile that remembers every name asked for and reads nothing.
type recorded struct{ names []string }

func (r *recorded) read(name string) ([]byte, error) {
	r.names = append(r.names, name)
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// TestLoadReadsNothingInTheStateDirectory pins G-02-5: Load runs as root before
// the privilege drop, and root without capabilities cannot open the state
// directory, so Load must not try, with or without a token.
func TestLoadReadsNothingInTheStateDirectory(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"default": {"TIERPLAN_AGENT_URL": goodURL},
		"custom":  {"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_STATE_DIR": "/state"},
	} {
		t.Run(name, func(t *testing.T) {
			var r recorded
			cfg, err := Load(env(m), r.read)
			if err != nil {
				t.Fatalf("Load without a token must succeed and leave the check to CheckEnrolled: %v", err)
			}
			if cfg.Enrolled {
				t.Error("Enrolled = true straight after Load")
			}
			if len(r.names) != 0 {
				t.Errorf("Load read %v, want nothing", r.names)
			}
		})
	}
}

func TestCheckEnrolledWithATokenReadsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		env   map[string]string
		files map[string]string
	}{
		"variable": {map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "cmjt_a_b"}, nil}, //nolint:gosec // a test fixture, not a credential
		"file": {
			map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/t"}, //nolint:gosec // a test fixture, not a credential
			map[string]string{"/run/secrets/t": "cmjt_a_b\n"},                                                    //nolint:gosec // a test fixture, not a credential
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(env(tc.env), files(tc.files))
			if err != nil {
				t.Fatal(err)
			}
			var r recorded
			if err := cfg.CheckEnrolled(r.read); err != nil {
				t.Fatal(err)
			}
			if len(r.names) != 0 {
				t.Errorf("CheckEnrolled read %v, want nothing when a token is present", r.names)
			}
			if cfg.Enrolled {
				t.Error("Enrolled = true for a tokened start, want false")
			}
		})
	}
}

func TestCheckEnrolledReportsAStateDirectoryItCannotRead(t *testing.T) {
	cfg := load(t, map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_STATE_DIR": "/state"})
	err := cfg.CheckEnrolled(func(name string) ([]byte, error) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	})
	if err == nil {
		t.Fatal("want an error when the identity cannot be read")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("the error %v does not wrap fs.ErrPermission", err)
	}
	if !strings.Contains(err.Error(), "read the enrolled state in /state") {
		t.Errorf("the error %q does not name the state directory", err)
	}
	if strings.Contains(err.Error(), "cmjt_") {
		t.Errorf("the error carries a token: %q", err)
	}
	if cfg.Enrolled {
		t.Error("Enrolled = true after a read error")
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
					"TIERPLAN_AGENT_URL":             goodURL,
					"TIERPLAN_AGENT_JOIN_TOKEN":      "from-env",
					"TIERPLAN_AGENT_JOIN_TOKEN_FILE": "/run/secrets/t",
				}),
				files(fsys),
			)
			if err == nil {
				t.Fatal("want an error: a named token file that cannot give a token must not fall back silently")
			}
			if !strings.Contains(err.Error(), "TIERPLAN_AGENT_JOIN_TOKEN_FILE") {
				t.Errorf("the error %q does not name TIERPLAN_AGENT_JOIN_TOKEN_FILE", err)
			}
		})
	}
}

func TestLoadRequiresAWebSocketURL(t *testing.T) {
	for _, u := range []string{"", "https://tierplan.example/agent/connect", "tierplan.example", "ftp://x"} {
		_, err := Load(
			env(map[string]string{"TIERPLAN_AGENT_URL": u, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}),
			files(nil),
		)
		if err == nil {
			t.Errorf("URL %q: want an error", u)
			continue
		}
		if !strings.Contains(err.Error(), "TIERPLAN_AGENT_URL") {
			t.Errorf("URL %q: the error %q does not name TIERPLAN_AGENT_URL", u, err)
		}
	}
	for _, u := range []string{"ws://127.0.0.1:7480/agent/connect", goodURL} {
		if _, err := Load(
			env(map[string]string{"TIERPLAN_AGENT_URL": u, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}),
			files(nil),
		); err != nil {
			t.Errorf("URL %q: %v", u, err)
		}
	}
}

func TestLoadParsesTheDockerGID(t *testing.T) {
	base := map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}
	with := func(v string) map[string]string {
		m := map[string]string{"TIERPLAN_AGENT_DOCKER_GID": v}
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
		if !strings.Contains(err.Error(), "TIERPLAN_AGENT_DOCKER_GID") {
			t.Errorf("GID %q: the error %q does not name TIERPLAN_AGENT_DOCKER_GID", v, err)
		}
	}
}

func TestLoadDefaultsAndOverrides(t *testing.T) {
	base := map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}
	cfg, err := Load(env(base), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocketPath != "/var/run/docker.sock" || cfg.StateDir != "/var/lib/tierplan-agent" {
		t.Fatalf("defaults: SocketPath=%q StateDir=%q", cfg.SocketPath, cfg.StateDir)
	}
	base["TIERPLAN_AGENT_SOCKET"] = "/run/docker.sock"
	base["TIERPLAN_AGENT_STATE_DIR"] = "/data"
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
	const secret = "cmjt_SECRETVALUE_zzz" //nolint:gosec // a test fixture, not a credential
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cases := []map[string]string{
		{"TIERPLAN_AGENT_URL": "https://wrong", "TIERPLAN_AGENT_JOIN_TOKEN": secret},
		{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": secret, "TIERPLAN_AGENT_DOCKER_GID": "nope"},
		{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": secret},
		{"TIERPLAN_AGENT_URL": "ws://tierplan.example/agent/connect", "TIERPLAN_AGENT_JOIN_TOKEN": secret},
	}
	for _, c := range cases {
		_, err := Load(env(c), files(nil))
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("the error leaks the token: %v", err)
		}
	}
	// The file form, read successfully and then failing on the URL.
	_, err := Load(
		env(map[string]string{"TIERPLAN_AGENT_URL": "nope", "TIERPLAN_AGENT_JOIN_TOKEN_FILE": "/t"}),
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
	cfg, err := Load(env(map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "t"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BackoffMin != time.Second || cfg.BackoffMax != time.Minute {
		t.Fatalf("backoff = %v..%v, want 1s..1m0s", cfg.BackoffMin, cfg.BackoffMax)
	}
}

func TestLoadReadsTheBackoffOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "t",
		"TIERPLAN_AGENT_BACKOFF_MIN": "250ms", "TIERPLAN_AGENT_BACKOFF_MAX": "5s",
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
		"min above max":             {"TIERPLAN_AGENT_BACKOFF_MIN": "30s", "TIERPLAN_AGENT_BACKOFF_MAX": "10s"},
		"min not a time":            {"TIERPLAN_AGENT_BACKOFF_MIN": "soon"},
		"max not a time":            {"TIERPLAN_AGENT_BACKOFF_MAX": "1 minute"},
		"min zero":                  {"TIERPLAN_AGENT_BACKOFF_MIN": "0s"},
		"max negative":              {"TIERPLAN_AGENT_BACKOFF_MAX": "-5s"},
		"min above the default max": {"TIERPLAN_AGENT_BACKOFF_MIN": "2m"},
	} {
		vars["TIERPLAN_AGENT_URL"], vars["TIERPLAN_AGENT_JOIN_TOKEN"] = goodURL, "t"
		_, err := Load(env(vars), files(nil))
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_BACKOFF") {
			t.Errorf("%s: Load error = %v, want one naming TIERPLAN_AGENT_BACKOFF_*", name, err)
		}
	}
}

func TestLoadDefaultsToReadWriteAllowedAndSelfUpdateOn(t *testing.T) {
	cfg, err := Load(env(map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReadOnly || !cfg.SelfUpdate || cfg.AllowPrivilegedServices {
		t.Fatalf("ReadOnly %v SelfUpdate %v AllowPrivilegedServices %v, want false, true and false", cfg.ReadOnly, cfg.SelfUpdate, cfg.AllowPrivilegedServices)
	}
}

func TestLoadReadsTheHostPolicyFlags(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok",
		"TIERPLAN_AGENT_READ_ONLY": "true", "TIERPLAN_AGENT_SELF_UPDATE": "false",
		"TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES": "true",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReadOnly || cfg.SelfUpdate || !cfg.AllowPrivilegedServices {
		t.Fatalf("ReadOnly %v SelfUpdate %v AllowPrivilegedServices %v, want true, false and true", cfg.ReadOnly, cfg.SelfUpdate, cfg.AllowPrivilegedServices)
	}
}

// A flag that is meant to restrict must not be silently ignored because it was
// misspelled: the agent refuses to start and names the variable.
func TestLoadRefusesAPolicyFlagItCannotRead(t *testing.T) {
	for _, name := range []string{"TIERPLAN_AGENT_READ_ONLY", "TIERPLAN_AGENT_SELF_UPDATE", "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES"} {
		for _, value := range []string{"sure", "yes", "on", "TRUE-ish"} {
			_, err := Load(env(map[string]string{
				"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok", name: value,
			}), files(nil))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%s: error = %v, want one naming the variable", name, value, err)
			}
		}
	}
}

func TestLoadReadsTheServerPinsFromTheInstallCommand(t *testing.T) {
	a := "sha256/" + strings.Repeat("A", 43) + "="
	b := "sha256/" + strings.Repeat("B", 43) + "="
	base := map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "cmjt_x_y"} //nolint:gosec // a test fixture, not a credential
	load := func(pin string) (Config, error) {
		m := map[string]string{"TIERPLAN_AGENT_PIN": pin}
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
			"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "cmjt_x_y", "TIERPLAN_AGENT_PIN": bad,
		}), files(nil))
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_PIN") {
			t.Errorf("pin %q: error = %v, want one naming TIERPLAN_AGENT_PIN", bad, err)
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
		"TIERPLAN_AGENT_SOCKET": "/x/d.sock", "TIERPLAN_AGENT_STATE_DIR": "/x/state", "TIERPLAN_AGENT_DOCKER_GID": "7",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocketPath != "/x/d.sock" || cfg.StateDir != "/x/state" || cfg.DockerGID == nil || *cfg.DockerGID != 7 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadHelperRefusesAGroupThatIsNotANumber(t *testing.T) {
	if _, err := LoadHelper(env(map[string]string{"TIERPLAN_AGENT_DOCKER_GID": "docker"})); err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_DOCKER_GID") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRefusesPlainWSToAnotherHostWithoutTheSwitch(t *testing.T) {
	for _, u := range []string{"ws://tierplan.example/agent/connect", "ws://192.168.1.10:7480/agent/connect", "ws://[2001:db8::1]/agent/connect"} {
		_, err := Load(env(map[string]string{"TIERPLAN_AGENT_URL": u, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}), files(nil))
		if err == nil {
			t.Errorf("URL %q without the switch: want an error", u)
			continue
		}
		for _, want := range []string{"TIERPLAN_AGENT_URL", "TIERPLAN_AGENT_ALLOW_INSECURE", "wss://", "development"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("URL %q: the error %q does not mention %s", u, err, want)
			}
		}
		cfg, err := Load(env(map[string]string{"TIERPLAN_AGENT_URL": u, "TIERPLAN_AGENT_JOIN_TOKEN": "tok", "TIERPLAN_AGENT_ALLOW_INSECURE": "true"}), files(nil))
		if err != nil || !cfg.AllowInsecure {
			t.Errorf("URL %q with the switch: cfg.AllowInsecure %v, err %v; want it loaded", u, cfg.AllowInsecure, err)
		}
	}
	for _, u := range []string{"ws://127.0.0.1:7480/agent/connect", "ws://localhost/agent/connect", goodURL} {
		cfg, err := Load(env(map[string]string{"TIERPLAN_AGENT_URL": u, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}), files(nil))
		if err != nil || cfg.AllowInsecure {
			t.Errorf("URL %q without the switch: cfg.AllowInsecure %v, err %v; want it loaded with the switch off", u, cfg.AllowInsecure, err)
		}
	}
}

func TestLoadReadsTheAllowInsecureSwitchStrictly(t *testing.T) {
	base := map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}
	cfg, err := Load(env(base), files(nil))
	if err != nil || cfg.AllowInsecure {
		t.Fatalf("default: AllowInsecure %v, err %v; want false", cfg.AllowInsecure, err)
	}
	for v, want := range map[string]bool{"true": true, "TRUE": true, "1": true, "false": false, "0": false} {
		m := map[string]string{"TIERPLAN_AGENT_ALLOW_INSECURE": v}
		for k, x := range base {
			m[k] = x
		}
		cfg, err := Load(env(m), files(nil))
		if err != nil || cfg.AllowInsecure != want {
			t.Errorf("value %q: AllowInsecure %v, err %v; want %v", v, cfg.AllowInsecure, err, want)
		}
	}
	m := map[string]string{"TIERPLAN_AGENT_ALLOW_INSECURE": "yes please"}
	for k, x := range base {
		m[k] = x
	}
	_, err = Load(env(m), files(nil))
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_ALLOW_INSECURE") {
		t.Fatalf("a switch that is not a boolean: err = %v, want one naming the variable", err)
	}
}

func TestLoadRefusesAURLWithUserInfoAQueryOrAFragmentWithoutRepeatingIt(t *testing.T) {
	for _, u := range []string{
		"wss://user:hunter2@tierplan.example/agent/connect",
		"wss://hunter2@tierplan.example/agent/connect",
		"wss://tierplan.example/agent/connect?token=hunter2",
		"wss://tierplan.example/agent/connect#hunter2",
		"ws://user:hunter2@127.0.0.1/agent/connect",
	} {
		_, err := Load(env(map[string]string{"TIERPLAN_AGENT_URL": u, "TIERPLAN_AGENT_JOIN_TOKEN": "tok", "TIERPLAN_AGENT_ALLOW_INSECURE": "true"}), files(nil))
		if err == nil {
			t.Errorf("URL %q: want an error", u)
			continue
		}
		if !strings.Contains(err.Error(), "TIERPLAN_AGENT_URL") {
			t.Errorf("URL %q: the error %q does not name TIERPLAN_AGENT_URL", u, err)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "user:") {
			t.Errorf("URL %q: the error repeats part of the value: %v", u, err)
		}
	}
}

func TestLoadDefaultsTheHostRouteFile(t *testing.T) {
	cfg, err := Load(env(map[string]string{"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostRouteFile != "/host/route" {
		t.Fatalf("HostRouteFile = %q, want /host/route", cfg.HostRouteFile)
	}
}

func TestLoadReadsACustomHostRouteFile(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok", "TIERPLAN_AGENT_HOST_ROUTE_FILE": " /mnt/route ",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostRouteFile != "/mnt/route" {
		t.Fatalf("HostRouteFile = %q", cfg.HostRouteFile)
	}
}

func TestLoadRefusesAHostRouteFileThatIsNotAnAbsoluteCleanPath(t *testing.T) {
	for _, bad := range []string{"route", "./route", "../etc/route", "/host/../etc/route", "/host//route", "/host/route/"} {
		_, err := Load(env(map[string]string{
			"TIERPLAN_AGENT_URL": goodURL, "TIERPLAN_AGENT_JOIN_TOKEN": "tok", "TIERPLAN_AGENT_HOST_ROUTE_FILE": bad,
		}), files(nil))
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_HOST_ROUTE_FILE") {
			t.Errorf("%q: error = %v, want one naming the variable", bad, err)
		}
	}
}
