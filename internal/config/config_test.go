// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package config_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/bayplan/internal/config"
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
	c, err := load(t, []string{"--listen", "127.0.0.1:1"}, envMap{"BAYPLAN_LISTEN": ":9000"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:1" {
		t.Errorf("Listen = %q, want the flag value", c.Listen)
	}
}

func TestEnvBeatsDefault(t *testing.T) {
	c, err := load(t, nil, envMap{"BAYPLAN_LISTEN": ":9000"}, files{})
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
	c, err := load(t, []string{"--data-dir", "/x"}, envMap{"BAYPLAN_LISTEN": ":9000"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.DataDir != "/x" {
		t.Errorf("Listen/DataDir = %q/%q", c.Listen, c.DataDir)
	}
}

func TestFileVariantTrimsTrailingNewline(t *testing.T) {
	c, err := load(t, nil,
		envMap{"BAYPLAN_ENCRYPTION_KEY_FILE": "/run/secrets/key"},
		files{"/run/secrets/key": "abc\n"})
	if err != nil {
		t.Fatal(err)
	}
	if c.EncryptionKey != "abc" {
		t.Errorf("EncryptionKey = %q, want abc", c.EncryptionKey)
	}
}

func TestEnvBeatsFileWhenOnlyOneIsSet(t *testing.T) {
	c, err := load(t, nil, envMap{"BAYPLAN_ENCRYPTION_KEY": "fromenv"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.EncryptionKey != "fromenv" {
		t.Errorf("EncryptionKey = %q", c.EncryptionKey)
	}
}

func TestBothSecretAndFileIsAnError(t *testing.T) {
	_, err := load(t, nil, envMap{
		"BAYPLAN_ENCRYPTION_KEY":      "x",
		"BAYPLAN_ENCRYPTION_KEY_FILE": "/run/secrets/key",
	}, files{"/run/secrets/key": "abc"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range []string{"BAYPLAN_ENCRYPTION_KEY", "BAYPLAN_ENCRYPTION_KEY_FILE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

func TestUnreadableOrEmptyFileIsAnError(t *testing.T) {
	_, err := load(t, nil, envMap{"BAYPLAN_ENCRYPTION_KEY_FILE": "/missing"}, files{})
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_ENCRYPTION_KEY_FILE") {
		t.Errorf("missing file error = %v", err)
	}
	_, err = load(t, nil, envMap{"BAYPLAN_ENCRYPTION_KEY_FILE": "/empty"}, files{"/empty": "\n"})
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_ENCRYPTION_KEY_FILE") {
		t.Errorf("empty file error = %v", err)
	}
}

func TestPublicURL(t *testing.T) {
	_, err := load(t, nil, envMap{"BAYPLAN_PUBLIC_URL": "example.com"}, files{})
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_PUBLIC_URL") {
		t.Fatalf("relative URL error = %v", err)
	}
	for in, want := range map[string]string{
		"https://bayplan.example.com/":       "https://bayplan.example.com",
		"HTTPS://Bayplan.Example.com":        "https://bayplan.example.com",
		"http://localhost:5173":              "http://localhost:5173",
		"https://example.com/bayplan/":       "https://example.com/bayplan",
		"https://example.com/sub/path":       "https://example.com/sub/path",
		"http://192.168.1.10:7480":           "http://192.168.1.10:7480",
		"https://bayplan.example.com:443///": "https://bayplan.example.com:443",
	} {
		c, err := load(t, nil, envMap{"BAYPLAN_PUBLIC_URL": in}, files{})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if c.PublicURL != want {
			t.Errorf("%q normalised to %q, want %q", in, c.PublicURL, want)
		}
	}
	for _, bad := range []string{"ftp://example.com", "https://", "https://u:p@example.com", "https://example.com/?a=b", "https://example.com/#f"} {
		if _, err := load(t, nil, envMap{"BAYPLAN_PUBLIC_URL": bad}, files{}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPublicURLRejectsAFirstSegmentBayplanServesItself(t *testing.T) {
	for _, seg := range []string{"api", "assets", "agent", "healthz", "readyz", "setup", "openapi", "docs", "schemas", "env", "environments", "settings", "audit", "login", "welcome"} {
		for _, in := range []string{"https://example.com/" + seg, "https://example.com/" + seg + "/x", "https://example.com/" + seg + "/"} {
			_, err := load(t, nil, envMap{"BAYPLAN_PUBLIC_URL": in}, files{})
			if err == nil {
				t.Errorf("%q should be rejected", in)
				continue
			}
			if !strings.Contains(err.Error(), "BAYPLAN_PUBLIC_URL") || !strings.Contains(err.Error(), "must not start with /"+seg+", which Bayplan serves itself") {
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
		"https://example.com/bayplan":       "https://example.com/bayplan",
		"https://example.com/tools/bayplan": "https://example.com/tools/bayplan",
		// Only the FIRST segment is Bayplan's to claim.
		"https://example.com/tools/api": "https://example.com/tools/api",
		"https://example.com/apis":      "https://example.com/apis",
		"https://example.com/API":       "https://example.com/API",
	} {
		c, err := load(t, nil, envMap{"BAYPLAN_PUBLIC_URL": in}, files{})
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
	c, err := load(t, nil, envMap{"BAYPLAN_TRUSTED_PROXIES": "10.0.0.0/8, 192.168.1.5"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 2 {
		t.Fatalf("got %d prefixes: %v", len(c.TrustedProxies), c.TrustedProxies)
	}
	if got := c.TrustedProxies[1].String(); got != "192.168.1.5/32" {
		t.Errorf("single IP prefix = %s, want 192.168.1.5/32", got)
	}
	_, err = load(t, nil, envMap{"BAYPLAN_TRUSTED_PROXIES": "10.0.0.0/8,nonsense"}, files{})
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_TRUSTED_PROXIES") {
		t.Errorf("invalid entry error = %v", err)
	}
}

func TestInvalidValuesNameTheEnvVar(t *testing.T) {
	for env, val := range map[string]string{
		"BAYPLAN_LISTEN":                    "no-port",
		"BAYPLAN_LOGIN_MAX_FAILURES":        "0",
		"BAYPLAN_LOGIN_LOCKOUT":             "soon",
		"BAYPLAN_SESSION_IDLE_TIMEOUT":      "-1h",
		"BAYPLAN_SESSION_REMEMBER_LIFETIME": "0s",
		"BAYPLAN_AUDIT_RETENTION_DAYS":      "-3",
		"BAYPLAN_SKIP_PRE_MIGRATE_BACKUP":   "maybe",
		"BAYPLAN_TLS_SELF_SIGNED":           "perhaps",
		"BAYPLAN_DOCKER_GID":                "abc",
		"BAYPLAN_LOG_LEVEL":                 "loud",
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
		"BAYPLAN_LOGIN_MAX_FAILURES":      "9",
		"BAYPLAN_LOGIN_LOCKOUT":           "45m",
		"BAYPLAN_AUDIT_RETENTION_DAYS":    "0",
		"BAYPLAN_SKIP_PRE_MIGRATE_BACKUP": "true",
		"BAYPLAN_DOCKER_GID":              "999",
		"BAYPLAN_LOG_LEVEL":               "debug",
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
	_, err := load(t, nil, envMap{"BAYPLAN_TLS_CERT_FILE": "/c.pem"}, files{})
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_TLS_KEY_FILE") {
		t.Errorf("cert without key error = %v", err)
	}
	_, err = load(t, nil, envMap{
		"BAYPLAN_TLS_CERT_FILE":   "/c.pem",
		"BAYPLAN_TLS_KEY_FILE":    "/k.pem",
		"BAYPLAN_TLS_SELF_SIGNED": "true",
	}, files{})
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_TLS_SELF_SIGNED") {
		t.Errorf("cert plus self-signed error = %v", err)
	}
}

func TestRedactedNeverContainsSecrets(t *testing.T) {
	const secret = "super-secret-key-value"
	c, err := load(t, nil, envMap{"BAYPLAN_ENCRYPTION_KEY": secret, "BAYPLAN_LISTEN": ":9000"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	red := c.Redacted()
	for k, v := range red {
		if strings.Contains(k, secret) || strings.Contains(v, secret) {
			t.Errorf("Redacted leaks the secret in %s=%s", k, v)
		}
	}
	if red["BAYPLAN_ENCRYPTION_KEY"] != "[redacted]" {
		t.Errorf("secret shown as %q, want [redacted]", red["BAYPLAN_ENCRYPTION_KEY"])
	}
	if red["BAYPLAN_LISTEN"] != ":9000" {
		t.Errorf("non-secret shown as %q", red["BAYPLAN_LISTEN"])
	}
	// An unset secret is reported as unset, not as redacted.
	c2, _ := load(t, nil, envMap{}, files{})
	if got := c2.Redacted()["BAYPLAN_ENCRYPTION_KEY"]; got != "" {
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
	for _, want := range []string{"BAYPLAN_AGENT_HEARTBEAT_INTERVAL", "BAYPLAN_AGENT_OFFLINE_AFTER", "BAYPLAN_AGENT_RECONNECT_GRACE"} {
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
		"BAYPLAN_AGENT_HEARTBEAT_INTERVAL": "5s", "BAYPLAN_AGENT_OFFLINE_AFTER": "20s", "BAYPLAN_AGENT_RECONNECT_GRACE": "2m",
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
		"offline window equal to the interval":             {"BAYPLAN_AGENT_HEARTBEAT_INTERVAL": "10s", "BAYPLAN_AGENT_OFFLINE_AFTER": "10s"},
		"offline window under twice the interval":          {"BAYPLAN_AGENT_HEARTBEAT_INTERVAL": "10s", "BAYPLAN_AGENT_OFFLINE_AFTER": "19s"},
		"an interval that leaves the default window short": {"BAYPLAN_AGENT_HEARTBEAT_INTERVAL": "20s"},
		"not a duration":   {"BAYPLAN_AGENT_HEARTBEAT_INTERVAL": "often"},
		"a zero window":    {"BAYPLAN_AGENT_OFFLINE_AFTER": "0s"},
		"a negative grace": {"BAYPLAN_AGENT_RECONNECT_GRACE": "-1m"},
	} {
		_, err := load(t, nil, vars, files{})
		if err == nil || !strings.Contains(err.Error(), "BAYPLAN_AGENT_") {
			t.Errorf("%s: error = %v, want one naming a BAYPLAN_AGENT_ variable", name, err)
		}
	}
	// Exactly twice is allowed.
	if _, err := load(t, nil, envMap{"BAYPLAN_AGENT_HEARTBEAT_INTERVAL": "10s", "BAYPLAN_AGENT_OFFLINE_AFTER": "20s"}, files{}); err != nil {
		t.Errorf("a window of exactly twice the interval was refused: %v", err)
	}
}

func TestNamesListsTheAgentEndpointSettings(t *testing.T) {
	names := strings.Join(config.Names(), " ")
	for _, want := range []string{"BAYPLAN_AGENT_ALLOW_INSECURE", "BAYPLAN_AGENT_CONNECT_BURST"} {
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
	c, err := load(t, nil, envMap{"BAYPLAN_AGENT_ALLOW_INSECURE": "true", "BAYPLAN_AGENT_CONNECT_BURST": "500"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.AgentAllowInsecure || c.AgentConnectBurst != 500 {
		t.Fatalf("allow insecure %v, burst %d; want true and 500", c.AgentAllowInsecure, c.AgentConnectBurst)
	}
	for name, vars := range map[string]envMap{
		"a zero burst":      {"BAYPLAN_AGENT_CONNECT_BURST": "0"},
		"a negative burst":  {"BAYPLAN_AGENT_CONNECT_BURST": "-5"},
		"a burst of text":   {"BAYPLAN_AGENT_CONNECT_BURST": "many"},
		"insecure not bool": {"BAYPLAN_AGENT_ALLOW_INSECURE": "sometimes"},
	} {
		if _, err := load(t, nil, vars, files{}); err == nil || !strings.Contains(err.Error(), "BAYPLAN_AGENT_") {
			t.Errorf("%s: error = %v, want one naming a BAYPLAN_AGENT_ variable", name, err)
		}
	}
}

func TestAgentURLIsNormalisedLikeThePublicURL(t *testing.T) {
	c, err := load(t, nil, envMap{}, files{})
	if err != nil || c.AgentURL != "" {
		t.Fatalf("unset: %q, %v; want empty", c.AgentURL, err)
	}
	for in, want := range map[string]string{
		"https://bayplan.internal:7480":  "https://bayplan.internal:7480",
		"HTTPS://Bayplan.Internal:7480/": "https://bayplan.internal:7480",
		"http://192.168.1.10:7480":       "http://192.168.1.10:7480",
		"https://example.com/bayplan/":   "https://example.com/bayplan",
	} {
		c, err := load(t, nil, envMap{"BAYPLAN_AGENT_URL": in}, files{})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if c.AgentURL != want {
			t.Errorf("%q normalised to %q, want %q", in, c.AgentURL, want)
		}
	}
	for _, bad := range []string{"example.com", "ftp://example.com", "https://", "https://u:p@example.com", "https://example.com/?a=b", "https://example.com/api"} {
		_, err := load(t, nil, envMap{"BAYPLAN_AGENT_URL": bad}, files{})
		if err == nil || !strings.Contains(err.Error(), "BAYPLAN_AGENT_URL") {
			t.Errorf("%q: error = %v, want one naming BAYPLAN_AGENT_URL", bad, err)
		}
	}
}

func TestAgentURLHasAFlagAndTheEnvironmentWinsOverNothingElse(t *testing.T) {
	c, err := load(t, []string{"--agent-url", "https://flag.example:7480/"}, envMap{}, files{})
	if err != nil || c.AgentURL != "https://flag.example:7480" {
		t.Fatalf("flag: %q, %v", c.AgentURL, err)
	}
	// The public URL is a separate setting.
	c, err = load(t, nil, envMap{"BAYPLAN_PUBLIC_URL": "https://pub.example", "BAYPLAN_AGENT_URL": "https://agent.example"}, files{})
	if err != nil || c.PublicURL != "https://pub.example" || c.AgentURL != "https://agent.example" {
		t.Fatalf("both: %q %q, %v", c.PublicURL, c.AgentURL, err)
	}
}

func TestAgentEndpointIsTheWebSocketAddressAgentsDial(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com":               "wss://example.com/agent/connect",
		"https://example.com/bayplan":       "wss://example.com/bayplan/agent/connect",
		"https://example.com/bayplan/":      "wss://example.com/bayplan/agent/connect",
		"https://bayplan.lan:7480":          "wss://bayplan.lan:7480/agent/connect",
		"http://192.168.1.10:7480":          "ws://192.168.1.10:7480/agent/connect",
		"https://u:p@bayplan.example/x?q=1": "wss://bayplan.example/x/agent/connect",
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
	if !strings.Contains(strings.Join(config.Names(), " "), "BAYPLAN_FAILING_SERVICE_GRACE") {
		t.Error("Names() does not list BAYPLAN_FAILING_SERVICE_GRACE")
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
	c, err := load(t, nil, envMap{"BAYPLAN_FAILING_SERVICE_GRACE": "5m"}, files{})
	if err != nil {
		t.Fatal(err)
	}
	if c.FailingServiceGrace != 5*time.Minute {
		t.Fatalf("FailingServiceGrace = %v, want 5m", c.FailingServiceGrace)
	}
	for name, v := range map[string]string{"not a duration": "soon", "zero": "0s", "negative": "-1m"} {
		_, err := load(t, nil, envMap{"BAYPLAN_FAILING_SERVICE_GRACE": v}, files{})
		if err == nil || !strings.Contains(err.Error(), "BAYPLAN_FAILING_SERVICE_GRACE") {
			t.Errorf("%s: error = %v, want one naming BAYPLAN_FAILING_SERVICE_GRACE", name, err)
		}
	}
}

// BAYPLAN_INVITE_TTL (plan 03-11, D-24): how long an invite or reset link works.
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
		c, err := load(t, nil, envMap{"BAYPLAN_INVITE_TTL": raw}, files{})
		if err != nil || c.InviteTTL != want {
			t.Errorf("%s: %v, %v, want %v", raw, c.InviteTTL, err, want)
		}
	}
}

func TestInviteTTLRefusesAnythingOutsideItsRangeAtStartup(t *testing.T) {
	for _, raw := range []string{"59m", "30s", "0s", "-72h", "721h", "8760h", "soon", "72"} {
		_, err := load(t, nil, envMap{"BAYPLAN_INVITE_TTL": raw}, files{})
		if err == nil || !strings.Contains(err.Error(), "BAYPLAN_INVITE_TTL") {
			t.Errorf("%q: error %v, want one naming BAYPLAN_INVITE_TTL", raw, err)
		}
	}
	// The flag is checked the same way.
	if _, err := load(t, []string{"--invite-ttl=2000h"}, envMap{}, files{}); err == nil {
		t.Error("--invite-ttl=2000h was accepted")
	}
}

// BAYPLAN_RESTORE_RESTART and BAYPLAN_BACKUP_MAX_UPLOAD (plan 03-25, D-30): how
// Bayplan restarts after a restore from the web UI, and how big a backup it takes.
func TestTheRestoreRestartModeDefaultsToExecAcceptsExitAndRefusesAnythingElse(t *testing.T) {
	names := strings.Join(config.Names(), " ")
	for _, want := range []string{"BAYPLAN_RESTORE_RESTART", "BAYPLAN_BACKUP_MAX_UPLOAD"} {
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
		c, err := load(t, nil, envMap{"BAYPLAN_RESTORE_RESTART": mode}, files{})
		if err != nil || c.RestoreRestart != mode {
			t.Errorf("BAYPLAN_RESTORE_RESTART=%s: %q, %v", mode, c.RestoreRestart, err)
		}
	}
	c, err = load(t, []string{"--restore-restart", "exit"}, envMap{"BAYPLAN_RESTORE_RESTART": "exec"}, files{})
	if err != nil || c.RestoreRestart != "exit" {
		t.Errorf("the flag does not win: %q, %v", c.RestoreRestart, err)
	}
	for _, bad := range []string{"restart", "EXEC", "kill", "true"} {
		_, err := load(t, nil, envMap{"BAYPLAN_RESTORE_RESTART": bad}, files{})
		if err == nil || !strings.Contains(err.Error(), "BAYPLAN_RESTORE_RESTART") || !strings.Contains(err.Error(), "exec") || !strings.Contains(err.Error(), "exit") {
			t.Errorf("BAYPLAN_RESTORE_RESTART=%q: error = %v, want one naming the variable and the two values", bad, err)
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
		c, err := load(t, nil, envMap{"BAYPLAN_BACKUP_MAX_UPLOAD": in}, files{})
		if err != nil || c.BackupMaxUpload != want {
			t.Errorf("BAYPLAN_BACKUP_MAX_UPLOAD=%q: %d, %v; want %d", in, c.BackupMaxUpload, err, want)
		}
	}
	for _, bad := range []string{"1048575", "1023KiB", "0", "-1GiB", "65GiB", "70000000000", "lots", "1TiB", "1.5GiB"} {
		_, err := load(t, nil, envMap{"BAYPLAN_BACKUP_MAX_UPLOAD": bad}, files{})
		if err == nil || !strings.Contains(err.Error(), "BAYPLAN_BACKUP_MAX_UPLOAD") {
			t.Errorf("BAYPLAN_BACKUP_MAX_UPLOAD=%q: error = %v, want one naming the variable", bad, err)
		}
	}
}

// BAYPLAN_BACKUP_DIR (plan 03-27, D-32): where scheduled backups are written.
func TestTheBackupFolderIsUnsetByDefaultSoItFollowsTheDataDirectoryAndCanBeSet(t *testing.T) {
	if !strings.Contains(strings.Join(config.Names(), " "), "BAYPLAN_BACKUP_DIR") {
		t.Error("Names() does not list BAYPLAN_BACKUP_DIR")
	}
	c, err := load(t, nil, envMap{}, files{})
	if err != nil || c.BackupDir != "" {
		t.Fatalf("default BackupDir = %q, %v; want empty", c.BackupDir, err)
	}
	c, err = load(t, nil, envMap{"BAYPLAN_BACKUP_DIR": "/mnt/backups"}, files{})
	if err != nil || c.BackupDir != "/mnt/backups" {
		t.Errorf("BAYPLAN_BACKUP_DIR: %q, %v", c.BackupDir, err)
	}
	c, err = load(t, []string{"--backup-dir", "/srv/b"}, envMap{"BAYPLAN_BACKUP_DIR": "/mnt/backups"}, files{})
	if err != nil || c.BackupDir != "/srv/b" {
		t.Errorf("the flag does not win: %q, %v", c.BackupDir, err)
	}
}

// BAYPLAN_REGISTRY_BLOCK_CIDRS (plan 05-12, D-14): ranges registry calls refuse on top of the
// built-in ones.
func TestRegistryBlockCIDRsParsesCommaSeparatedPrefixes(t *testing.T) {
	c, err := load(t, nil, envMap{"BAYPLAN_REGISTRY_BLOCK_CIDRS": "10.9.0.0/16, fd00::/8"}, files{})
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
	_, err := load(t, nil, envMap{"BAYPLAN_REGISTRY_BLOCK_CIDRS": "10.9.0.0/16,not-a-range"}, files{})
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_REGISTRY_BLOCK_CIDRS") || !strings.Contains(err.Error(), "not-a-range") {
		t.Fatalf("error = %v, want one naming BAYPLAN_REGISTRY_BLOCK_CIDRS and not-a-range", err)
	}
}
