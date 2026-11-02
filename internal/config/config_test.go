// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package config_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/tierplan/internal/config"
)

// envMap is a fake environment.
type envMap map[string]string

func (e envMap) get(k string) string { return e[k] }

// files is a fake file system for _FILE variables.
type files map[string]string

func (f files) read(name string) ([]byte, error) {
	if v, ok := f[name]; ok {
		return []byte(v), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// load parses args as flags, then loads with the fake env and files.
func load(t *testing.T, args []string, env envMap, f files) (config.Config, error) {
	t.Helper()
	fl := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.BindFlags(fl)
	if err := fl.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return config.Load(fl, env.get, f.read)
}

func TestDefaults(t *testing.T) {
	c, err := load(t, nil, envMap{}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7480" || c.DataDir != "/data" {
		t.Errorf("listen/datadir = %q/%q", c.Listen, c.DataDir)
	}
	if c.LoginMaxFailures != 5 || c.LoginLockout != 30*time.Minute {
		t.Errorf("login defaults = %d/%v", c.LoginMaxFailures, c.LoginLockout)
	}
	if c.SessionIdleTimeout != 12*time.Hour || c.SessionRememberLifetime != 720*time.Hour {
		t.Errorf("session defaults = %v/%v", c.SessionIdleTimeout, c.SessionRememberLifetime)
	}
	if c.AuditRetentionDays != 365 || c.SkipPreMigrateBackup || c.TLSSelfSigned {
		t.Errorf("audit/backup/tls defaults wrong: %+v", c)
	}
	if c.DockerGID != nil {
		t.Errorf("DockerGID should be unset, got %d", *c.DockerGID)
	}
	if c.LogLevel != slog.LevelInfo || c.DockerHost != "unix:///var/run/docker.sock" {
		t.Errorf("log level / docker host = %v / %q", c.LogLevel, c.DockerHost)
	}
}

func TestFlagBeatsEnv(t *testing.T) {
	c, err := load(t, []string{"--listen", "127.0.0.1:1"}, envMap{"TIERPLAN_LISTEN": ":9000"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:1" {
		t.Errorf("Listen = %q, want the flag value", c.Listen)
	}
}

func TestEnvBeatsDefault(t *testing.T) {
	c, err := load(t, nil, envMap{"TIERPLAN_LISTEN": ":9000"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" {
		t.Errorf("Listen = %q, want :9000", c.Listen)
	}
}

func TestFlagDefaultDoesNotOverrideEnv(t *testing.T) {
	// A flag that was not set on the command line must not beat the env var,
	// even though BindFlags registered it with a default.
	c, err := load(t, []string{"--data-dir", "/x"}, envMap{"TIERPLAN_LISTEN": ":9000"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.DataDir != "/x" {
		t.Errorf("Listen/DataDir = %q/%q", c.Listen, c.DataDir)
	}
}

func TestFileVariantTrimsTrailingNewline(t *testing.T) {
	c, err := load(t, nil,
		envMap{"TIERPLAN_ENCRYPTION_KEY_FILE": "/run/secrets/key"},
		files{"/run/secrets/key": "abc\n"})
	if err != nil {
		t.Fatal(err)
	}
	if c.EncryptionKey != "abc" {
		t.Errorf("EncryptionKey = %q, want abc", c.EncryptionKey)
	}
}

func TestEnvBeatsFileWhenOnlyOneIsSet(t *testing.T) {
	c, err := load(t, nil, envMap{"TIERPLAN_ENCRYPTION_KEY": "fromenv"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.EncryptionKey != "fromenv" {
		t.Errorf("EncryptionKey = %q", c.EncryptionKey)
	}
}

func TestBothSecretAndFileIsAnError(t *testing.T) {
	_, err := load(t, nil, envMap{
		"TIERPLAN_ENCRYPTION_KEY":      "x",
		"TIERPLAN_ENCRYPTION_KEY_FILE": "/run/secrets/key",
	}, files{"/run/secrets/key": "abc"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range []string{"TIERPLAN_ENCRYPTION_KEY", "TIERPLAN_ENCRYPTION_KEY_FILE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

func TestUnreadableOrEmptyFileIsAnError(t *testing.T) {
	_, err := load(t, nil, envMap{"TIERPLAN_ENCRYPTION_KEY_FILE": "/missing"}, files{})
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_ENCRYPTION_KEY_FILE") {
		t.Errorf("missing file error = %v", err)
	}
	_, err = load(t, nil, envMap{"TIERPLAN_ENCRYPTION_KEY_FILE": "/empty"}, files{"/empty": "\n"})
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_ENCRYPTION_KEY_FILE") {
		t.Errorf("empty file error = %v", err)
	}
}

func TestPublicURL(t *testing.T) {
	_, err := load(t, nil, envMap{"TIERPLAN_PUBLIC_URL": "example.com"}, files{})
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_PUBLIC_URL") {
		t.Fatalf("relative URL error = %v", err)
	}
	for in, want := range map[string]string{
		"https://tierplan.example.com/":       "https://tierplan.example.com",
		"HTTPS://TierPlan.Example.com":        "https://tierplan.example.com",
		"http://localhost:5173":              "http://localhost:5173",
		"https://example.com/tierplan/":       "https://example.com/tierplan",
		"https://example.com/sub/path":       "https://example.com/sub/path",
		"http://192.168.1.10:7480":           "http://192.168.1.10:7480",
		"https://tierplan.example.com:443///": "https://tierplan.example.com:443",
	} {
		c, err := load(t, nil, envMap{"TIERPLAN_PUBLIC_URL": in}, files{})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if c.PublicURL != want {
			t.Errorf("%q normalised to %q, want %q", in, c.PublicURL, want)
		}
	}
	for _, bad := range []string{"ftp://example.com", "https://", "https://u:p@example.com", "https://example.com/?a=b", "https://example.com/#f"} {
		if _, err := load(t, nil, envMap{"TIERPLAN_PUBLIC_URL": bad}, files{}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPublicURLRejectsAFirstSegmentTierPlanServesItself(t *testing.T) {
	for _, seg := range []string{"api", "assets", "agent", "healthz", "readyz", "setup", "openapi", "docs", "schemas", "env", "environments", "settings", "audit", "login", "welcome"} {
		for _, in := range []string{"https://example.com/" + seg, "https://example.com/" + seg + "/x", "https://example.com/" + seg + "/"} {
			_, err := load(t, nil, envMap{"TIERPLAN_PUBLIC_URL": in}, files{})
			if err == nil {
				t.Errorf("%q should be rejected", in)
				continue
			}
			if !strings.Contains(err.Error(), "TIERPLAN_PUBLIC_URL") || !strings.Contains(err.Error(), "must not start with /"+seg+", which TierPlan serves itself") {
				t.Errorf("%q: error %q does not name the reserved segment", in, err)
			}
		}
	}
	// NormalisePublicURL is what the setup wizard and Settings call.
	if _, err := config.NormalisePublicURL("https://example.com/api"); err == nil || !strings.Contains(err.Error(), "/api") {
		t.Errorf("NormalisePublicURL(/api) = %v", err)
	}
}

func TestPublicURLAcceptsAPathThatDoesNotCollide(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com/tierplan":       "https://example.com/tierplan",
		"https://example.com/tools/tierplan": "https://example.com/tools/tierplan",
		// Only the FIRST segment is TierPlan's to claim.
		"https://example.com/tools/api": "https://example.com/tools/api",
		"https://example.com/apis":      "https://example.com/apis",
		"https://example.com/API":       "https://example.com/API",
	} {
		c, err := load(t, nil, envMap{"TIERPLAN_PUBLIC_URL": in}, files{})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if c.PublicURL != want {
			t.Errorf("%q normalised to %q, want %q", in, c.PublicURL, want)
		}
	}
}

func TestTrustedProxies(t *testing.T) {
	c, err := load(t, nil, envMap{"TIERPLAN_TRUSTED_PROXIES": "10.0.0.0/8, 192.168.1.5"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 2 {
		t.Fatalf("got %d prefixes: %v", len(c.TrustedProxies), c.TrustedProxies)
	}
	if got := c.TrustedProxies[1].String(); got != "192.168.1.5/32" {
		t.Errorf("single IP prefix = %s, want 192.168.1.5/32", got)
	}
	_, err = load(t, nil, envMap{"TIERPLAN_TRUSTED_PROXIES": "10.0.0.0/8,nonsense"}, files{})
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_TRUSTED_PROXIES") {
		t.Errorf("invalid entry error = %v", err)
	}
}

func TestInvalidValuesNameTheEnvVar(t *testing.T) {
	for env, val := range map[string]string{
		"TIERPLAN_LISTEN":                    "no-port",
		"TIERPLAN_LOGIN_MAX_FAILURES":        "0",
		"TIERPLAN_LOGIN_LOCKOUT":             "soon",
		"TIERPLAN_SESSION_IDLE_TIMEOUT":      "-1h",
		"TIERPLAN_SESSION_REMEMBER_LIFETIME": "0s",
		"TIERPLAN_AUDIT_RETENTION_DAYS":      "-3",
		"TIERPLAN_SKIP_PRE_MIGRATE_BACKUP":   "maybe",
		"TIERPLAN_TLS_SELF_SIGNED":           "perhaps",
		"TIERPLAN_DOCKER_GID":                "abc",
		"TIERPLAN_LOG_LEVEL":                 "loud",
	} {
		_, err := load(t, nil, envMap{env: val}, files{})
		if err == nil {
			t.Errorf("%s=%q should be rejected", env, val)
			continue
		}
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not name %s", err, env)
		}
	}
}

func TestTypedValues(t *testing.T) {
	c, err := load(t, nil, envMap{
		"TIERPLAN_LOGIN_MAX_FAILURES":      "9",
		"TIERPLAN_LOGIN_LOCKOUT":           "45m",
		"TIERPLAN_AUDIT_RETENTION_DAYS":    "0",
		"TIERPLAN_SKIP_PRE_MIGRATE_BACKUP": "true",
		"TIERPLAN_DOCKER_GID":              "999",
		"TIERPLAN_LOG_LEVEL":               "debug",
		"DOCKER_HOST":                     "unix:///custom.sock",
	}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.LoginMaxFailures != 9 || c.LoginLockout != 45*time.Minute || c.AuditRetentionDays != 0 ||
		!c.SkipPreMigrateBackup || c.LogLevel != slog.LevelDebug || c.DockerHost != "unix:///custom.sock" {
		t.Errorf("typed values wrong: %+v", c)
	}
	if c.DockerGID == nil || *c.DockerGID != 999 {
		t.Errorf("DockerGID = %v, want 999", c.DockerGID)
	}
}

func TestTLSSettingsMustBeConsistent(t *testing.T) {
	_, err := load(t, nil, envMap{"TIERPLAN_TLS_CERT_FILE": "/c.pem"}, files{})
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_TLS_KEY_FILE") {
		t.Errorf("cert without key error = %v", err)
	}
	_, err = load(t, nil, envMap{
		"TIERPLAN_TLS_CERT_FILE":   "/c.pem",
		"TIERPLAN_TLS_KEY_FILE":    "/k.pem",
		"TIERPLAN_TLS_SELF_SIGNED": "true",
	}, files{})
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_TLS_SELF_SIGNED") {
		t.Errorf("cert plus self-signed error = %v", err)
	}
}

func TestRedactedNeverContainsSecrets(t *testing.T) {
	const secret = "super-secret-key-value"
	c, err := load(t, nil, envMap{"TIERPLAN_ENCRYPTION_KEY": secret, "TIERPLAN_LISTEN": ":9000"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	red := c.Redacted()
	for k, v := range red {
		if strings.Contains(k, secret) || strings.Contains(v, secret) {
			t.Errorf("Redacted leaks the secret in %s=%s", k, v)
		}
	}
	if red["TIERPLAN_ENCRYPTION_KEY"] != "[redacted]" {
		t.Errorf("secret shown as %q, want [redacted]", red["TIERPLAN_ENCRYPTION_KEY"])
	}
	if red["TIERPLAN_LISTEN"] != ":9000" {
		t.Errorf("non-secret shown as %q", red["TIERPLAN_LISTEN"])
	}
	// An unset secret is reported as unset, not as redacted.
	c2, _ := load(t, nil, envMap{}, files{})
	if got := c2.Redacted()["TIERPLAN_ENCRYPTION_KEY"]; got != "" {
		t.Errorf("unset secret shown as %q, want empty", got)
	}
}

func TestSecretSettingsHaveNoFlag(t *testing.T) {
	// A secret on the command line is visible in the process list, so secrets
	// are env or _FILE only.
	fl := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.BindFlags(fl)
	fl.VisitAll(func(f *pflag.Flag) {
		if strings.Contains(f.Name, "encryption") {
			t.Errorf("flag --%s exposes a secret", f.Name)
		}
	})
}

func TestLoadWithNilFlagsAndReadFileError(t *testing.T) {
	if _, err := config.Load(nil, envMap{}.get, func(string) ([]byte, error) { return nil, errors.New("boom") }); err != nil {
		t.Errorf("nil flag set with defaults only should load: %v", err)
	}
}

func TestNamesListsTheAgentTimingSettings(t *testing.T) {
	names := strings.Join(config.Names(), " ")
	for _, want := range []string{"TIERPLAN_AGENT_HEARTBEAT_INTERVAL", "TIERPLAN_AGENT_OFFLINE_AFTER", "TIERPLAN_AGENT_RECONNECT_GRACE"} {
		if !strings.Contains(names, want) {
			t.Errorf("Names() does not list %s", want)
		}
	}
}

func TestTheAgentTimingDefaultsAreTenThirtyAndSixtySeconds(t *testing.T) {
	c, err := load(t, nil, envMap{}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentHeartbeatInterval != 10*time.Second || c.AgentOfflineAfter != 30*time.Second || c.AgentReconnectGrace != 60*time.Second {
		t.Fatalf("heartbeat %v, offline after %v, grace %v; want 10s, 30s, 60s", c.AgentHeartbeatInterval, c.AgentOfflineAfter, c.AgentReconnectGrace)
	}
}

func TestTheAgentTimingSettingsCanBeOverridden(t *testing.T) {
	c, err := load(t, nil, envMap{
		"TIERPLAN_AGENT_HEARTBEAT_INTERVAL": "5s", "TIERPLAN_AGENT_OFFLINE_AFTER": "20s", "TIERPLAN_AGENT_RECONNECT_GRACE": "2m",
	}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentHeartbeatInterval != 5*time.Second || c.AgentOfflineAfter != 20*time.Second || c.AgentReconnectGrace != 2*time.Minute {
		t.Fatalf("heartbeat %v, offline after %v, grace %v", c.AgentHeartbeatInterval, c.AgentOfflineAfter, c.AgentReconnectGrace)
	}
}

func TestTheOfflineWindowMustBeAtLeastTwiceTheHeartbeatInterval(t *testing.T) {
	for name, vars := range map[string]envMap{
		"offline window equal to the interval":             {"TIERPLAN_AGENT_HEARTBEAT_INTERVAL": "10s", "TIERPLAN_AGENT_OFFLINE_AFTER": "10s"},
		"offline window under twice the interval":          {"TIERPLAN_AGENT_HEARTBEAT_INTERVAL": "10s", "TIERPLAN_AGENT_OFFLINE_AFTER": "19s"},
		"an interval that leaves the default window short": {"TIERPLAN_AGENT_HEARTBEAT_INTERVAL": "20s"},
		"not a duration":   {"TIERPLAN_AGENT_HEARTBEAT_INTERVAL": "often"},
		"a zero window":    {"TIERPLAN_AGENT_OFFLINE_AFTER": "0s"},
		"a negative grace": {"TIERPLAN_AGENT_RECONNECT_GRACE": "-1m"},
	} {
		_, err := load(t, nil, vars, files{})
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_") {
			t.Errorf("%s: error = %v, want one naming a TIERPLAN_AGENT_ variable", name, err)
		}
	}
	// Exactly twice is allowed.
	if _, err := load(t, nil, envMap{"TIERPLAN_AGENT_HEARTBEAT_INTERVAL": "10s", "TIERPLAN_AGENT_OFFLINE_AFTER": "20s"}, files{}); err != nil {
		t.Errorf("a window of exactly twice the interval was refused: %v", err)
	}
}

func TestNamesListsTheAgentEndpointSettings(t *testing.T) {
	names := strings.Join(config.Names(), " ")
	for _, want := range []string{"TIERPLAN_AGENT_ALLOW_INSECURE", "TIERPLAN_AGENT_CONNECT_BURST"} {
		if !strings.Contains(names, want) {
			t.Errorf("Names() does not list %s", want)
		}
	}
}

func TestTheAgentEndpointDefaultsAreSecureAndTwoHundred(t *testing.T) {
	c, err := load(t, nil, envMap{}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentAllowInsecure || c.AgentConnectBurst != 200 {
		t.Fatalf("allow insecure %v, burst %d; want false and 200", c.AgentAllowInsecure, c.AgentConnectBurst)
	}
}

func TestTheAgentEndpointSettingsCanBeOverriddenAndBadValuesAreRefused(t *testing.T) {
	c, err := load(t, nil, envMap{"TIERPLAN_AGENT_ALLOW_INSECURE": "true", "TIERPLAN_AGENT_CONNECT_BURST": "500"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.AgentAllowInsecure || c.AgentConnectBurst != 500 {
		t.Fatalf("allow insecure %v, burst %d; want true and 500", c.AgentAllowInsecure, c.AgentConnectBurst)
	}
	for name, vars := range map[string]envMap{
		"a zero burst":      {"TIERPLAN_AGENT_CONNECT_BURST": "0"},
		"a negative burst":  {"TIERPLAN_AGENT_CONNECT_BURST": "-5"},
		"a burst of text":   {"TIERPLAN_AGENT_CONNECT_BURST": "many"},
		"insecure not bool": {"TIERPLAN_AGENT_ALLOW_INSECURE": "sometimes"},
	} {
		if _, err := load(t, nil, vars, files{}); err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_") {
			t.Errorf("%s: error = %v, want one naming a TIERPLAN_AGENT_ variable", name, err)
		}
	}
}

func TestAgentURLIsNormalisedLikeThePublicURL(t *testing.T) {
	c, err := load(t, nil, envMap{}, files{})
	if err != nil || c.AgentURL != "" {
		t.Fatalf("unset: %q, %v; want empty", c.AgentURL, err)
	}
	for in, want := range map[string]string{
		"https://tierplan.internal:7480":  "https://tierplan.internal:7480",
		"HTTPS://TierPlan.Internal:7480/": "https://tierplan.internal:7480",
		"http://192.168.1.10:7480":       "http://192.168.1.10:7480",
		"https://example.com/tierplan/":   "https://example.com/tierplan",
	} {
		c, err := load(t, nil, envMap{"TIERPLAN_AGENT_URL": in}, files{})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if c.AgentURL != want {
			t.Errorf("%q normalised to %q, want %q", in, c.AgentURL, want)
		}
	}
	for _, bad := range []string{"example.com", "ftp://example.com", "https://", "https://u:p@example.com", "https://example.com/?a=b", "https://example.com/api"} {
		_, err := load(t, nil, envMap{"TIERPLAN_AGENT_URL": bad}, files{})
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_AGENT_URL") {
			t.Errorf("%q: error = %v, want one naming TIERPLAN_AGENT_URL", bad, err)
		}
	}
}

func TestAgentURLHasAFlagAndTheEnvironmentWinsOverNothingElse(t *testing.T) {
	c, err := load(t, []string{"--agent-url", "https://flag.example:7480/"}, envMap{}, files{})
	if err != nil || c.AgentURL != "https://flag.example:7480" {
		t.Fatalf("flag: %q, %v", c.AgentURL, err)
	}
	// The public URL is a separate setting.
	c, err = load(t, nil, envMap{"TIERPLAN_PUBLIC_URL": "https://pub.example", "TIERPLAN_AGENT_URL": "https://agent.example"}, files{})
	if err != nil || c.PublicURL != "https://pub.example" || c.AgentURL != "https://agent.example" {
		t.Fatalf("both: %q %q, %v", c.PublicURL, c.AgentURL, err)
	}
}

func TestAgentEndpointIsTheWebSocketAddressAgentsDial(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com":               "wss://example.com/agent/connect",
		"https://example.com/tierplan":       "wss://example.com/tierplan/agent/connect",
		"https://example.com/tierplan/":      "wss://example.com/tierplan/agent/connect",
		"https://tierplan.lan:7480":          "wss://tierplan.lan:7480/agent/connect",
		"http://192.168.1.10:7480":          "ws://192.168.1.10:7480/agent/connect",
		"https://u:p@tierplan.example/x?q=1": "wss://tierplan.example/x/agent/connect",
	} {
		got, err := config.AgentEndpoint(in)
		if err != nil || got != want {
			t.Errorf("AgentEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "not a url", "https://"} {
		if got, err := config.AgentEndpoint(bad); err == nil {
			t.Errorf("AgentEndpoint(%q) = %q, want an error", bad, got)
		}
	}
}

func TestTheFailingServiceGraceDefaultsToTwoMinutesAndIsListed(t *testing.T) {
	if !strings.Contains(strings.Join(config.Names(), " "), "TIERPLAN_FAILING_SERVICE_GRACE") {
		t.Error("Names() does not list TIERPLAN_FAILING_SERVICE_GRACE")
	}
	c, err := load(t, nil, envMap{}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.FailingServiceGrace != 2*time.Minute {
		t.Fatalf("FailingServiceGrace = %v, want 2m", c.FailingServiceGrace)
	}
}

func TestTheFailingServiceGraceCanBeOverriddenAndMustBeAPositiveDuration(t *testing.T) {
	c, err := load(t, nil, envMap{"TIERPLAN_FAILING_SERVICE_GRACE": "5m"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.FailingServiceGrace != 5*time.Minute {
		t.Fatalf("FailingServiceGrace = %v, want 5m", c.FailingServiceGrace)
	}
	for name, v := range map[string]string{"not a duration": "soon", "zero": "0s", "negative": "-1m"} {
		_, err := load(t, nil, envMap{"TIERPLAN_FAILING_SERVICE_GRACE": v}, files{})
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_FAILING_SERVICE_GRACE") {
			t.Errorf("%s: error = %v, want one naming TIERPLAN_FAILING_SERVICE_GRACE", name, err)
		}
	}
}

// TIERPLAN_STACK_CONVERGE_TIMEOUT (plan 07-18, D-13): how long a swarm stack deploy waits for a
// service to settle.
func TestTheStackConvergeTimeoutDefaultsToTenMinutesAndIsListed(t *testing.T) {
	if !strings.Contains(strings.Join(config.Names(), " "), "TIERPLAN_STACK_CONVERGE_TIMEOUT") {
		t.Error("Names() does not list TIERPLAN_STACK_CONVERGE_TIMEOUT")
	}
	c, err := load(t, nil, envMap{}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.StackConvergeTimeout != 10*time.Minute {
		t.Fatalf("StackConvergeTimeout = %v, want 10m", c.StackConvergeTimeout)
	}
}

func TestTheStackConvergeTimeoutAcceptsOneMinuteToTwoHoursAndRefusesTheRest(t *testing.T) {
	for raw, want := range map[string]time.Duration{"1m": time.Minute, "45m": 45 * time.Minute, "2h": 2 * time.Hour} {
		c, err := load(t, nil, envMap{"TIERPLAN_STACK_CONVERGE_TIMEOUT": raw}, files{})
		if err != nil || c.StackConvergeTimeout != want {
			t.Errorf("%s: %v, %v, want %v", raw, c.StackConvergeTimeout, err, want)
		}
	}
	for name, v := range map[string]string{"under a minute": "30s", "over two hours": "121m", "zero": "0s", "negative": "-5m", "not a duration": "soon"} {
		_, err := load(t, nil, envMap{"TIERPLAN_STACK_CONVERGE_TIMEOUT": v}, files{})
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_STACK_CONVERGE_TIMEOUT") {
			t.Errorf("%s: error = %v, want one naming TIERPLAN_STACK_CONVERGE_TIMEOUT", name, err)
		}
	}
}

// TIERPLAN_INVITE_TTL (plan 03-11, D-24): how long an invite or reset link works.
func TestInviteTTLDefaultsToSeventyTwoHours(t *testing.T) {
	c, err := load(t, nil, envMap{}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.InviteTTL != 72*time.Hour {
		t.Fatalf("InviteTTL = %v, want 72h", c.InviteTTL)
	}
}

func TestInviteTTLAcceptsFromOneHourToThirtyDays(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"1h": time.Hour, "24h": 24 * time.Hour, "168h": 168 * time.Hour, "720h": 720 * time.Hour, "90m": 90 * time.Minute,
	} {
		c, err := load(t, nil, envMap{"TIERPLAN_INVITE_TTL": raw}, files{})
		if err != nil || c.InviteTTL != want {
			t.Errorf("%s: %v, %v, want %v", raw, c.InviteTTL, err, want)
		}
	}
}

func TestInviteTTLRefusesAnythingOutsideItsRangeAtStartup(t *testing.T) {
	for _, raw := range []string{"59m", "30s", "0s", "-72h", "721h", "8760h", "soon", "72"} {
		_, err := load(t, nil, envMap{"TIERPLAN_INVITE_TTL": raw}, files{})
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_INVITE_TTL") {
			t.Errorf("%q: error %v, want one naming TIERPLAN_INVITE_TTL", raw, err)
		}
	}
	// The flag is checked the same way.
	if _, err := load(t, []string{"--invite-ttl=2000h"}, envMap{}, files{}); err == nil {
		t.Error("--invite-ttl=2000h was accepted")
	}
}

// TIERPLAN_RESTORE_RESTART and TIERPLAN_BACKUP_MAX_UPLOAD (plan 03-25, D-30): how
// TierPlan restarts after a restore from the web UI, and how big a backup it takes.
func TestTheRestoreRestartModeDefaultsToExecAcceptsExitAndRefusesAnythingElse(t *testing.T) {
	names := strings.Join(config.Names(), " ")
	for _, want := range []string{"TIERPLAN_RESTORE_RESTART", "TIERPLAN_BACKUP_MAX_UPLOAD"} {
		if !strings.Contains(names, want) {
			t.Errorf("Names() does not list %s", want)
		}
	}
	c, err := load(t, nil, envMap{}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.RestoreRestart != "exec" {
		t.Fatalf("RestoreRestart default = %q, want exec", c.RestoreRestart)
	}
	for _, mode := range []string{"exec", "exit"} {
		c, err := load(t, nil, envMap{"TIERPLAN_RESTORE_RESTART": mode}, files{})
		if err != nil || c.RestoreRestart != mode {
			t.Errorf("TIERPLAN_RESTORE_RESTART=%s: %q, %v", mode, c.RestoreRestart, err)
		}
	}
	c, err = load(t, []string{"--restore-restart", "exit"}, envMap{"TIERPLAN_RESTORE_RESTART": "exec"}, files{})
	if err != nil || c.RestoreRestart != "exit" {
		t.Errorf("the flag does not win: %q, %v", c.RestoreRestart, err)
	}
	for _, bad := range []string{"restart", "EXEC", "kill", "true"} {
		_, err := load(t, nil, envMap{"TIERPLAN_RESTORE_RESTART": bad}, files{})
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_RESTORE_RESTART") || !strings.Contains(err.Error(), "exec") || !strings.Contains(err.Error(), "exit") {
			t.Errorf("TIERPLAN_RESTORE_RESTART=%q: error = %v, want one naming the variable and the two values", bad, err)
		}
	}
}

func TestTheBackupUploadCapDefaultsToOneGiBAndTakesByteSizesFromOneMiBToSixtyFourGiB(t *testing.T) {
	c, err := load(t, nil, envMap{}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.BackupMaxUpload != 1<<30 {
		t.Fatalf("BackupMaxUpload default = %d, want 1 GiB", c.BackupMaxUpload)
	}
	for in, want := range map[string]int64{
		"1MiB": 1 << 20, "512MiB": 512 << 20, "2GiB": 2 << 30, "64GiB": 64 << 30,
		"1048576": 1 << 20, "5368709120": 5 << 30, " 1 GiB ": 1 << 30, "1gib": 1 << 30,
		"1024KiB": 1 << 20, "1M": 1 << 20, "3G": 3 << 30,
	} {
		c, err := load(t, nil, envMap{"TIERPLAN_BACKUP_MAX_UPLOAD": in}, files{})
		if err != nil || c.BackupMaxUpload != want {
			t.Errorf("TIERPLAN_BACKUP_MAX_UPLOAD=%q: %d, %v; want %d", in, c.BackupMaxUpload, err, want)
		}
	}
	for _, bad := range []string{"1048575", "1023KiB", "0", "-1GiB", "65GiB", "70000000000", "lots", "1TiB", "1.5GiB"} {
		_, err := load(t, nil, envMap{"TIERPLAN_BACKUP_MAX_UPLOAD": bad}, files{})
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_BACKUP_MAX_UPLOAD") {
			t.Errorf("TIERPLAN_BACKUP_MAX_UPLOAD=%q: error = %v, want one naming the variable", bad, err)
		}
	}
}

// TIERPLAN_BACKUP_DIR (plan 03-27, D-32): where scheduled backups are written.
func TestTheBackupFolderIsUnsetByDefaultSoItFollowsTheDataDirectoryAndCanBeSet(t *testing.T) {
	if !strings.Contains(strings.Join(config.Names(), " "), "TIERPLAN_BACKUP_DIR") {
		t.Error("Names() does not list TIERPLAN_BACKUP_DIR")
	}
	c, err := load(t, nil, envMap{}, files{})
	if err != nil || c.BackupDir != "" {
		t.Fatalf("default BackupDir = %q, %v; want empty", c.BackupDir, err)
	}
	c, err = load(t, nil, envMap{"TIERPLAN_BACKUP_DIR": "/mnt/backups"}, files{})
	if err != nil || c.BackupDir != "/mnt/backups" {
		t.Errorf("TIERPLAN_BACKUP_DIR: %q, %v", c.BackupDir, err)
	}
	c, err = load(t, []string{"--backup-dir", "/srv/b"}, envMap{"TIERPLAN_BACKUP_DIR": "/mnt/backups"}, files{})
	if err != nil || c.BackupDir != "/srv/b" {
		t.Errorf("the flag does not win: %q, %v", c.BackupDir, err)
	}
}

// TIERPLAN_REGISTRY_BLOCK_CIDRS (plan 05-12, D-14): ranges registry calls refuse on top of the
// built-in ones.
func TestRegistryBlockCIDRsParsesCommaSeparatedPrefixes(t *testing.T) {
	c, err := load(t, nil, envMap{"TIERPLAN_REGISTRY_BLOCK_CIDRS": "10.9.0.0/16, fd00::/8"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.RegistryBlockCIDRs) != 2 || c.RegistryBlockCIDRs[0].String() != "10.9.0.0/16" || c.RegistryBlockCIDRs[1].String() != "fd00::/8" {
		t.Fatalf("RegistryBlockCIDRs = %v, want [10.9.0.0/16 fd00::/8]", c.RegistryBlockCIDRs)
	}
}

func TestRegistryBlockCIDRsIsEmptyByDefault(t *testing.T) {
	c, err := load(t, nil, envMap{}, files{})
	if err != nil || len(c.RegistryBlockCIDRs) != 0 {
		t.Fatalf("RegistryBlockCIDRs = %v, %v, want none", c.RegistryBlockCIDRs, err)
	}
}

func TestRegistryBlockCIDRsRefusesAnInvalidEntryAtStartupAndNamesIt(t *testing.T) {
	_, err := load(t, nil, envMap{"TIERPLAN_REGISTRY_BLOCK_CIDRS": "10.9.0.0/16,not-a-range"}, files{})
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_REGISTRY_BLOCK_CIDRS") || !strings.Contains(err.Error(), "not-a-range") {
		t.Fatalf("error = %v, want one naming TIERPLAN_REGISTRY_BLOCK_CIDRS and not-a-range", err)
	}
}

// TIERPLAN_HOST_ROUTE_FILE (plan 10-05, NET-05): the host's route table as mounted into
// TierPlan's own container, for the subnet picker's route check on TierPlan's own host.
func TestConfigHostRouteFile(t *testing.T) {
	if !strings.Contains(strings.Join(config.Names(), " "), "TIERPLAN_HOST_ROUTE_FILE") {
		t.Error("Names() does not list TIERPLAN_HOST_ROUTE_FILE")
	}
	c, err := load(t, nil, envMap{}, files{})
	if err != nil || c.HostRouteFile != "/host/route" {
		t.Fatalf("default HostRouteFile = %q, %v; want /host/route", c.HostRouteFile, err)
	}
	c, err = load(t, nil, envMap{"TIERPLAN_HOST_ROUTE_FILE": "/proc/host/net/route"}, files{})
	if err != nil || c.HostRouteFile != "/proc/host/net/route" {
		t.Errorf("TIERPLAN_HOST_ROUTE_FILE: %q, %v", c.HostRouteFile, err)
	}
	c, err = load(t, []string{"--host-route-file", "/srv/route"}, envMap{"TIERPLAN_HOST_ROUTE_FILE": "/proc/host/net/route"}, files{})
	if err != nil || c.HostRouteFile != "/srv/route" {
		t.Errorf("the flag does not win: %q, %v", c.HostRouteFile, err)
	}
	for _, bad := range []string{"host/route", "route", "/host/../route", "/host//route", "/host/route/"} {
		_, err := load(t, nil, envMap{"TIERPLAN_HOST_ROUTE_FILE": bad}, files{})
		if err == nil || !strings.Contains(err.Error(), "TIERPLAN_HOST_ROUTE_FILE") {
			t.Errorf("TIERPLAN_HOST_ROUTE_FILE=%q: error = %v, want one naming the variable", bad, err)
		}
	}
}
