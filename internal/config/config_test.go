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
