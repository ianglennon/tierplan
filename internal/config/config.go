// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package config is the one place TierPlan reads its settings (D-11). Every
// setting is a row in the settings table below. Precedence, highest first:
//
//	command-line flag (only when explicitly set)
//	environment variable
//	<NAME>_FILE environment variable (secrets only, for Docker secrets)
//	built-in default
//
// Setting both X and X_FILE is a startup error. Every error names the
// environment variable, so an operator can find the setting from the message.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// The two agent timing settings are named in the table and in the check that
// ties them together.
const (
	envAgentHeartbeatInterval = "TIERPLAN_AGENT_HEARTBEAT_INTERVAL"
	envAgentOfflineAfter      = "TIERPLAN_AGENT_OFFLINE_AFTER"
)

// redactedMark replaces the value of a set secret in Redacted.
const redactedMark = "[redacted]"

// Config is the validated configuration.
type Config struct {
	Listen    string
	DataDir   string
	PublicURL string // normalised origin plus optional path, no trailing slash; empty when unset
	// AgentURL is where agents dial when that differs from the public URL, such as
	// an internal host name or a LAN address (D-18). Normalised like PublicURL;
	// empty when unset, and then agents dial the public URL.
	AgentURL string

	EncryptionKey string // secret
	// TrustedProxies are the peers whose forwarded headers are believed (D-39).
	TrustedProxies []netip.Prefix
	// RegistryBlockCIDRs are address ranges the server refuses to connect to when it talks to
	// a registry, on top of the built-in link-local, unspecified and multicast ranges.
	RegistryBlockCIDRs []netip.Prefix

	LoginMaxFailures        int
	LoginLockout            time.Duration
	SessionIdleTimeout      time.Duration
	SessionRememberLifetime time.Duration
	// InviteTTL is how long a one-time invite or reset link works (D-24).
	InviteTTL            time.Duration
	AuditRetentionDays   int // 0 means keep forever
	SkipPreMigrateBackup bool

	// RestoreRestart is how TierPlan restarts after a restore from the web UI:
	// "exec" replaces the process image so a container's PID 1 survives, "exit"
	// exits 0 and leaves the restart to the container's restart policy.
	RestoreRestart string
	// BackupMaxUpload caps an uploaded backup, in bytes.
	BackupMaxUpload int64
	// BackupDir is the folder scheduled backups are written into; empty means
	// the backups folder inside the data directory.
	BackupDir string
	// HostRouteFile is the host's /proc/1/net/route as bind-mounted read-only into
	// TierPlan's own container, which the subnet picker's route check reads for
	// TierPlan's own host. Optional: when it is not there the check says so.
	HostRouteFile string

	TLSCertFile string
	TLSKeyFile  string
	// TLSSelfSigned is the value of the deprecated TIERPLAN_TLS_SELF_SIGNED (true only for a
	// value that reads as true). TierPlan serves HTTPS with a certificate of its own without it, so it
	// changes nothing apart from the places that still ask EnvTLS. TLSSelfSignedSet is whether the
	// operator set it at all, to any value, so the server can say once that it is deprecated (D-14).
	TLSSelfSigned    bool
	TLSSelfSignedSet bool

	// DockerGID overrides the detected socket group; nil when unset (D-17).
	DockerGID *int
	LogLevel  slog.Level
	// DockerHost is the raw string; the Docker transport enforces unix-only (D-21).
	DockerHost string

	// AgentHeartbeatInterval is how often agents are told to heartbeat,
	// AgentOfflineAfter how long a session may be silent before it is closed,
	// and AgentReconnectGrace how long after a server start an environment shows
	// "Reconnecting" instead of offline (D-41).
	AgentHeartbeatInterval time.Duration
	AgentOfflineAfter      time.Duration
	AgentReconnectGrace    time.Duration
	// FailingServiceGrace is how long after a swarm service was updated it is not
	// counted as failing (D-34).
	FailingServiceGrace time.Duration
	// StackConvergeTimeout is how long a swarm stack deploy waits for each service to settle
	// after the swarm accepted it (D-13). A service that has not settled by then reads
	// "Timed out"; Docker may still finish it.
	StackConvergeTimeout time.Duration
	// AgentAllowInsecure lets agents connect over plain ws:// to an agent address
	// that is not this machine; for development only. Join tokens and Docker
	// answers then travel unencrypted, and install commands for a plain address
	// also set it on the agent. A plain address on this machine needs no switch.
	// AgentConnectBurst is how many connection attempts one client address may
	// make at once.
	AgentAllowInsecure bool
	AgentConnectBurst  int

	// raw holds the resolved text of every setting, for Redacted.
	raw map[string]string
}

// setting is one row of the table.
type setting struct {
	env    string
	flag   string // empty: no command-line flag (secrets, to keep them out of the process list)
	def    string
	desc   string
	secret bool
	apply  func(c *Config, raw string) error
}

// The two ways TierPlan restarts after a restore from the web UI.
const (
	RestoreRestartExec = "exec"
	RestoreRestartExit = "exit"
)

// The range the backup upload cap accepts: below a mebibyte no real backup
// would fit, and above 64 GiB a single request body is a way to fill the disk.
const (
	minUploadBytes = 1 << 20
	maxUploadBytes = 64 << 30
)

// The range the stack converge timeout accepts: under a minute a service that pulls an image
// would be called stuck while it works, and over two hours a stuck service holds the environment's
// one stack operation for a working day.
const (
	minConvergeTimeout = time.Minute
	maxConvergeTimeout = 2 * time.Hour
)

// The range TIERPLAN_INVITE_TTL accepts: an hour is the shortest a person can
// plausibly act on a link they were handed, and 30 days the longest a bearer
// secret should stay live.
const (
	minInviteTTL = time.Hour
	maxInviteTTL = 720 * time.Hour
)

// settings is the single source of truth for env names, flags, defaults and parsing.
var settings = []setting{
	{env: "TIERPLAN_LISTEN", flag: "listen", def: ":7480", desc: "address to listen on",
		apply: func(c *Config, raw string) error {
			_, port, err := net.SplitHostPort(raw)
			if err != nil {
				return fmt.Errorf("must be host:port or :port: %w", err)
			}
			n, err := strconv.Atoi(port)
			if err != nil || n < 0 || n > 65535 {
				return fmt.Errorf("port %q must be a number from 0 to 65535", port)
			}
			c.Listen = raw
			return nil
		}},
	{env: "TIERPLAN_DATA_DIR", flag: "data-dir", def: "/data", desc: "directory for the database, key and checkouts",
		apply: func(c *Config, raw string) error { c.DataDir = raw; return nil }},
	{env: "TIERPLAN_PUBLIC_URL", flag: "public-url", desc: "externally visible base URL; overrides and locks the stored value",
		apply: func(c *Config, raw string) (err error) { c.PublicURL, err = normalisePublicURL(raw); return err }},
	{env: "TIERPLAN_AGENT_URL", flag: "agent-url", desc: "address agents dial when it differs from the public URL; overrides and locks the stored value",
		apply: func(c *Config, raw string) (err error) { c.AgentURL, err = normalisePublicURL(raw); return err }},
	{env: "TIERPLAN_ENCRYPTION_KEY", secret: true, desc: "key for secrets at rest (also TIERPLAN_ENCRYPTION_KEY_FILE)",
		apply: func(c *Config, raw string) error { c.EncryptionKey = raw; return nil }},
	{env: "TIERPLAN_TRUSTED_PROXIES", flag: "trusted-proxies", desc: "comma-separated IPs or CIDRs of proxies whose forwarded headers are trusted",
		apply: func(c *Config, raw string) (err error) { c.TrustedProxies, err = parsePrefixes(raw); return err }},
	{env: "TIERPLAN_REGISTRY_BLOCK_CIDRS", flag: "registry-block-cidrs", desc: "comma-separated IPs or CIDRs the server refuses to connect to when it tests or browses a registry, on top of link-local, unspecified and multicast ranges; private and loopback ranges are allowed by default because self-hosted registries live there",
		apply: func(c *Config, raw string) (err error) { c.RegistryBlockCIDRs, err = parsePrefixes(raw); return err }},
	{env: "TIERPLAN_LOGIN_MAX_FAILURES", flag: "login-max-failures", def: "5", desc: "failed sign-ins before an account locks",
		apply: func(c *Config, raw string) (err error) { c.LoginMaxFailures, err = parseInt(raw, 1); return err }},
	{env: "TIERPLAN_LOGIN_LOCKOUT", flag: "login-lockout", def: "30m", desc: "how long an account stays locked",
		apply: func(c *Config, raw string) (err error) { c.LoginLockout, err = parseDuration(raw); return err }},
	{env: "TIERPLAN_SESSION_IDLE_TIMEOUT", flag: "session-idle-timeout", def: "12h", desc: "idle time after which a session ends",
		apply: func(c *Config, raw string) (err error) { c.SessionIdleTimeout, err = parseDuration(raw); return err }},
	{env: "TIERPLAN_SESSION_REMEMBER_LIFETIME", flag: "session-remember-lifetime", def: "720h", desc: "lifetime of a remember-me session",
		apply: func(c *Config, raw string) (err error) {
			c.SessionRememberLifetime, err = parseDuration(raw)
			return err
		}},
	{env: "TIERPLAN_INVITE_TTL", flag: "invite-ttl", def: "72h", desc: "how long an invite or password reset link works",
		apply: func(c *Config, raw string) (err error) {
			c.InviteTTL, err = parseDuration(raw)
			if err == nil && (c.InviteTTL < minInviteTTL || c.InviteTTL > maxInviteTTL) {
				err = fmt.Errorf("%q must be between %s and %s", raw, minInviteTTL, maxInviteTTL)
			}
			return err
		}},
	{env: "TIERPLAN_AUDIT_RETENTION_DAYS", flag: "audit-retention-days", def: "365", desc: "days to keep audit entries, 0 for forever",
		apply: func(c *Config, raw string) (err error) { c.AuditRetentionDays, err = parseInt(raw, 0); return err }},
	{env: "TIERPLAN_SKIP_PRE_MIGRATE_BACKUP", flag: "skip-pre-migrate-backup", def: "false", desc: "do not back up the database before a migration",
		apply: func(c *Config, raw string) (err error) {
			c.SkipPreMigrateBackup, err = strconv.ParseBool(raw)
			return err
		}},
	{env: "TIERPLAN_RESTORE_RESTART", flag: "restore-restart", def: "exec", desc: "how TierPlan restarts after a restore from the web UI: exec replaces the process so the container keeps running, exit stops with status 0 and needs a restart policy",
		apply: func(c *Config, raw string) error {
			if raw != RestoreRestartExec && raw != RestoreRestartExit {
				return fmt.Errorf("%q is not one of %s, %s", raw, RestoreRestartExec, RestoreRestartExit)
			}
			c.RestoreRestart = raw
			return nil
		}},
	{env: "TIERPLAN_BACKUP_MAX_UPLOAD", flag: "backup-max-upload", def: "1GiB", desc: "the largest backup file the web UI accepts for a restore, from 1MiB to 64GiB (bytes, or a number with KiB, MiB or GiB)",
		apply: func(c *Config, raw string) (err error) { c.BackupMaxUpload, err = parseUploadSize(raw); return err }},
	{env: "TIERPLAN_BACKUP_DIR", flag: "backup-dir", desc: "folder scheduled backups are written into (default: backups inside the data directory); mount a volume or host folder here to keep them off the data volume",
		apply: func(c *Config, raw string) error { c.BackupDir = raw; return nil }},
	{env: "TIERPLAN_TLS_CERT_FILE", flag: "tls-cert-file", desc: "path to a TLS certificate (PEM)",
		apply: func(c *Config, raw string) error { c.TLSCertFile = raw; return nil }},
	{env: "TIERPLAN_TLS_KEY_FILE", flag: "tls-key-file", desc: "path to the TLS private key (PEM)",
		apply: func(c *Config, raw string) error { c.TLSKeyFile = raw; return nil }},
	{env: "TIERPLAN_TLS_SELF_SIGNED", flag: "tls-self-signed", desc: "deprecated and without effect: TierPlan always serves HTTPS with a self-signed certificate unless a certificate file is supplied; any value is accepted and logged at start",
		apply: func(c *Config, raw string) error {
			// A stale value must never stop startup, so one that is not a boolean reads as not true.
			c.TLSSelfSignedSet = raw != ""
			on, err := strconv.ParseBool(raw)
			c.TLSSelfSigned = err == nil && on
			return nil
		}},
	{env: "TIERPLAN_DOCKER_GID", flag: "docker-gid", desc: "group ID of the Docker socket, overriding detection",
		apply: func(c *Config, raw string) error {
			if raw == "" {
				c.DockerGID = nil
				return nil
			}
			n, err := parseInt(raw, 0)
			if err != nil {
				return err
			}
			c.DockerGID = &n
			return nil
		}},
	{env: "TIERPLAN_HOST_ROUTE_FILE", flag: "host-route-file", def: "/host/route", desc: "the host's /proc/1/net/route bind-mounted read-only into TierPlan's container, used for the subnet picker's route check on TierPlan's own host; an absolute, clean path; optional, the check says when it is not mounted",
		apply: func(c *Config, raw string) error {
			if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
				return fmt.Errorf("%q must be an absolute, clean path", raw)
			}
			c.HostRouteFile = raw
			return nil
		}},
	{env: envAgentHeartbeatInterval, flag: "agent-heartbeat-interval", def: "10s", desc: "how often agents send a heartbeat; sent to each agent when it connects",
		apply: func(c *Config, raw string) (err error) {
			c.AgentHeartbeatInterval, err = parseDuration(raw)
			return err
		}},
	{env: envAgentOfflineAfter, flag: "agent-offline-after", def: "30s", desc: "silence after which an agent connection is closed and the agent counts as offline; at least twice the heartbeat interval",
		apply: func(c *Config, raw string) (err error) { c.AgentOfflineAfter, err = parseDuration(raw); return err }},
	{env: "TIERPLAN_AGENT_RECONNECT_GRACE", flag: "agent-reconnect-grace", def: "60s", desc: "after a start, how long an environment shows Reconnecting instead of offline while its agents come back",
		apply: func(c *Config, raw string) (err error) { c.AgentReconnectGrace, err = parseDuration(raw); return err }},
	{env: "TIERPLAN_FAILING_SERVICE_GRACE", flag: "failing-service-grace", def: "2m", desc: "how long after a swarm service was updated it is not counted as failing",
		apply: func(c *Config, raw string) (err error) { c.FailingServiceGrace, err = parseDuration(raw); return err }},
	{env: "TIERPLAN_STACK_CONVERGE_TIMEOUT", flag: "stack-converge-timeout", def: "10m", desc: "how long a swarm stack deploy waits for each service to settle, from 1m to 2h; a service that has not settled by then reads Timed out and the deploy stops",
		apply: func(c *Config, raw string) (err error) {
			c.StackConvergeTimeout, err = parseDuration(raw)
			if err == nil && (c.StackConvergeTimeout < minConvergeTimeout || c.StackConvergeTimeout > maxConvergeTimeout) {
				err = fmt.Errorf("%q must be between %s and %s", raw, minConvergeTimeout, maxConvergeTimeout)
			}
			return err
		}},
	{env: "TIERPLAN_AGENT_ALLOW_INSECURE", flag: "agent-allow-insecure", def: "false", desc: "development only: let agents connect over plain ws:// to an agent address that is not this machine, which sends join tokens and Docker answers unencrypted; install commands for a plain address then set it on the agent too",
		apply: func(c *Config, raw string) (err error) {
			c.AgentAllowInsecure, err = strconv.ParseBool(raw)
			return err
		}},
	{env: "TIERPLAN_AGENT_CONNECT_BURST", flag: "agent-connect-burst", def: "200", desc: "how many connection attempts one client address may make at once on the agent endpoint; sized so a whole swarm behind one NAT can reconnect together",
		apply: func(c *Config, raw string) (err error) { c.AgentConnectBurst, err = parseInt(raw, 1); return err }},
	{env: "TIERPLAN_LOG_LEVEL", flag: "log-level", def: "info", desc: "log level: debug, info, warn or error",
		apply: func(c *Config, raw string) error {
			switch strings.ToLower(raw) {
			case "debug":
				c.LogLevel = slog.LevelDebug
			case "info":
				c.LogLevel = slog.LevelInfo
			case "warn":
				c.LogLevel = slog.LevelWarn
			case "error":
				c.LogLevel = slog.LevelError
			default:
				return fmt.Errorf("%q is not one of debug, info, warn, error", raw)
			}
			return nil
		}},
	{env: "DOCKER_HOST", flag: "docker-host", def: "unix:///var/run/docker.sock", desc: "Docker daemon address (unix sockets only)",
		apply: func(c *Config, raw string) error { c.DockerHost = raw; return nil }},
}

// BindFlags registers one string flag for every setting that has a flag. The
// flags are all strings so that "explicitly set" is exactly pflag's Changed;
// Load does the parsing, so env vars and flags share one parser and one set of
// error messages.
func BindFlags(fs *pflag.FlagSet) {
	for _, s := range settings {
		if s.flag == "" {
			continue
		}
		fs.String(s.flag, s.def, fmt.Sprintf("%s (env %s)", s.desc, s.env))
	}
}

// Load resolves every setting. flags may be nil. env and readFile are injected
// so tests need neither a real environment nor real files.
func Load(flags *pflag.FlagSet, env func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	c := Config{raw: make(map[string]string, len(settings))}
	for _, s := range settings {
		raw, err := resolve(s, flags, env, readFile)
		if err != nil {
			return Config{}, err
		}
		if err := s.apply(&c, raw); err != nil {
			return Config{}, fmt.Errorf("invalid %s: %w", s.env, err)
		}
		c.raw[s.env] = raw
	}
	if err := c.crossCheck(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// resolve applies the precedence rules to one setting.
func resolve(s setting, flags *pflag.FlagSet, env func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	if s.flag != "" && flags != nil {
		if f := flags.Lookup(s.flag); f != nil && f.Changed {
			return f.Value.String(), nil
		}
	}
	direct := env(s.env)
	if !s.secret {
		if direct != "" {
			return direct, nil
		}
		return s.def, nil
	}
	fileVar := s.env + "_FILE"
	path := env(fileVar)
	switch {
	case direct != "" && path != "":
		return "", fmt.Errorf("both %s and %s are set; set only one", s.env, fileVar)
	case direct != "":
		return direct, nil
	case path != "":
		b, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: cannot read %s: %w", fileVar, path, err)
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return "", fmt.Errorf("%s: file %s is empty", fileVar, path)
		}
		return v, nil
	default:
		return s.def, nil
	}
}

// EnvTLS reports whether an environment variable (or its flag) turns TLS on for the
// whole process: the self-signed switch or a certificate file. variable names the
// setting for a message. It is the one place that decides this, so the listener, the
// redirect gate and the health check cannot disagree (D-18).
// A lone key file counts: Validate rejects it, but nothing here may treat it as plain.
func (c Config) EnvTLS() (variable string, ok bool) {
	switch {
	case c.TLSSelfSigned:
		return "TIERPLAN_TLS_SELF_SIGNED", true
	case c.TLSCertFile != "":
		return "TIERPLAN_TLS_CERT_FILE", true
	case c.TLSKeyFile != "":
		return "TIERPLAN_TLS_KEY_FILE", true
	}
	return "", false
}

// crossCheck validates combinations of settings.
func (c Config) crossCheck() error {
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		if c.TLSCertFile == "" {
			return errors.New("TIERPLAN_TLS_KEY_FILE is set without TIERPLAN_TLS_CERT_FILE; set both or neither")
		}
		return errors.New("TIERPLAN_TLS_CERT_FILE is set without TIERPLAN_TLS_KEY_FILE; set both or neither")
	}
	// A window shorter than two heartbeats would call an agent offline after one
	// late or lost heartbeat.
	if c.AgentOfflineAfter < 2*c.AgentHeartbeatInterval {
		return fmt.Errorf("%s (%s) must be at least twice %s (%s)",
			envAgentOfflineAfter, c.AgentOfflineAfter, envAgentHeartbeatInterval, c.AgentHeartbeatInterval)
	}
	return nil
}

// Redacted returns every setting as env name to display value, for the startup
// log. A set secret shows as "[redacted]" and an unset one as empty; the value
// itself never appears.
func (c Config) Redacted() map[string]string {
	out := make(map[string]string, len(settings))
	for _, s := range settings {
		v := c.raw[s.env]
		if s.secret && v != "" {
			v = redactedMark
		}
		out[s.env] = v
	}
	return out
}

// Names lists every environment variable the table defines, sorted. It exists
// so documentation and tests can check against the table.
func Names() []string {
	out := make([]string, 0, len(settings))
	for _, s := range settings {
		out = append(out, s.env)
	}
	sort.Strings(out)
	return out
}

func parseInt(raw string, minimum int) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a whole number", raw)
	}
	if n < minimum {
		return 0, fmt.Errorf("%d is below the minimum of %d", n, minimum)
	}
	return n, nil
}

// parseUploadSize reads a byte count: a whole number of bytes, or a whole number
// followed by KiB, MiB or GiB (K, M, G and a space before the unit are accepted;
// the case does not matter). The result must be between 1 MiB and 64 GiB.
func parseUploadSize(raw string) (int64, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > maxUploadBytes/mult {
		if err == nil && n > 0 {
			return 0, fmt.Errorf("%q is above the maximum of 64GiB", raw)
		}
		return 0, fmt.Errorf("%q is not a size such as 512MiB, 2GiB or 1073741824", raw)
	}
	n *= mult
	if n < minUploadBytes {
		return 0, fmt.Errorf("%q is below the minimum of 1MiB", raw)
	}
	return n, nil
}

func parseDuration(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 30m or 12h", raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q must be greater than zero", raw)
	}
	return d, nil
}

func parsePrefixes(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR", part)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// reservedSegments are the first path segments TierPlan serves itself: its API,
// assets, agent endpoint, probes and the SPA's own routes. A public URL whose
// path started with one would make the base path shadow a route (D-50, T-02-14).
var reservedSegments = map[string]bool{
	"api": true, "assets": true, "agent": true, "healthz": true, "readyz": true,
	"setup": true, "openapi": true, "docs": true, "schemas": true, "env": true,
	"environments": true, "settings": true, "audit": true, "login": true, "welcome": true,
}

// reservedSegment returns the first segment of the decoded URL path when
// TierPlan serves it itself, and "" otherwise. Only the first segment is
// TierPlan's to claim: /tools/api is a fine base. A name such as openapi.json is
// treated as the openapi segment, as the router does.
func reservedSegment(p string) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	if reservedSegments[first] {
		return first
	}
	if strings.HasPrefix(first, "openapi.") {
		return first
	}
	return ""
}

// NormalisePublicURL validates a public URL the same way TIERPLAN_PUBLIC_URL is
// validated, so a value entered in the setup wizard and one set in the
// environment are held to one rule. Empty stays empty.
func NormalisePublicURL(raw string) (string, error) { return normalisePublicURL(raw) }

// AgentEndpoint is the address an agent dials for the URL it is told to use, the
// agent URL or else the public URL: the same host and base path, https as wss and
// http as ws, with /agent/connect on the end (D-18). It is the one place that
// builds the address, so the install commands, the welcome and Settings agree.
func AgentEndpoint(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse the URL: %w", err)
	}
	var scheme string
	switch strings.ToLower(u.Scheme) {
	case "https":
		scheme = "wss"
	case "http":
		scheme = "ws"
	default:
		return "", fmt.Errorf("the URL %q is not http or https", baseURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("the URL %q has no host", baseURL)
	}
	out := url.URL{Scheme: scheme, Host: u.Host, Path: strings.TrimRight(u.Path, "/") + "/agent/connect"}
	return out.String(), nil
}

// normalisePublicURL accepts an absolute http or https URL and returns its
// origin plus an optional path with no trailing slash. Empty stays empty.
func normalisePublicURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("must be an absolute http or https URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("must be an absolute http or https URL, for example https://tierplan.example.com")
	}
	if u.User != nil {
		return "", errors.New("must not contain a user name or password")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", errors.New("must not contain a query or fragment")
	}
	if seg := reservedSegment(u.Path); seg != "" {
		return "", fmt.Errorf("must not start with /%s, which TierPlan serves itself", seg)
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	return u.Scheme + "://" + strings.ToLower(u.Host) + path, nil
}
