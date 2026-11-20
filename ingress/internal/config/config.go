// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package config reads the ingress supervisor's settings from its environment.
//
// It mirrors the agent's configuration: the server's address must be wss://, the
// optional pins are checked when they are read, and an error names a variable or a
// path and never a secret. The state root is ingressbundle.NodeRoot and is not
// configurable (Phase 21 D-24).
//
// Load never reads the join token. A node that already enrolled holds an
// identity and needs no token, so whether a token is required is decided by the
// caller, which asks Config.JoinToken only when it has to enrol (D-22).
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/proto/ingresswire"
)

const (
	// EnvURL is the full address of the server's ingress channel, for example
	// wss://tierplan.example:7480/ingress/connect (D-33).
	EnvURL = "TIERPLAN_INGRESS_URL"
	// EnvPin is the pin of the server's certificate, or several separated by
	// commas, as "sha256/" and a base64 SPKI hash (D-32). Without it the server
	// must present a certificate the system trusts.
	EnvPin = "TIERPLAN_INGRESS_PIN"
	// EnvJoinTokenFile is the path of a Docker secret holding the join token. It
	// wins over EnvJoinToken.
	EnvJoinTokenFile = "TIERPLAN_INGRESS_JOIN_TOKEN_FILE" //nolint:gosec // the name of a variable, not a credential
	// EnvJoinToken is the join token itself.
	EnvJoinToken = "TIERPLAN_INGRESS_JOIN_TOKEN" //nolint:gosec // the name of a variable, not a credential
	// EnvNodeID is the swarm node ID of the host the supervisor runs on. A
	// standalone node has none, so it is optional (D-22).
	EnvNodeID = "TIERPLAN_INGRESS_NODE_ID"
)

// Config is what the supervisor needs to start. The join token is not a field
// anyone can read: JoinToken fetches it on demand, and no formatting verb prints
// it.
type Config struct {
	// URL is the server's ingress endpoint (wss:// only).
	URL string
	// Pins are the server certificate pins, validated.
	Pins []string
	// NodeID is the swarm node ID, or "" on a standalone node.
	NodeID string

	tokenFile, token string
}

// Load reads the configuration. env looks up an environment variable; it is
// injected so tests need no real environment. Error text names variables, never a
// value that could be a secret.
//
//	TIERPLAN_INGRESS_URL              required, the full wss:// address of the ingress channel
//	TIERPLAN_INGRESS_PIN              optional, comma-separated sha256/ pins of the server certificate
//	TIERPLAN_INGRESS_NODE_ID          optional, the swarm node ID
//	TIERPLAN_INGRESS_JOIN_TOKEN_FILE  path of a Docker secret holding the token; wins over the variable
//	TIERPLAN_INGRESS_JOIN_TOKEN       the token itself
//
// Load does not read the token variables' values into a check and never opens the
// token file; see Config.JoinToken.
func Load(env func(string) string) (Config, error) {
	cfg := Config{
		URL:       env(EnvURL),
		tokenFile: env(EnvJoinTokenFile),
		token:     env(EnvJoinToken),
	}
	if err := checkURL(cfg.URL); err != nil {
		return Config{}, err
	}
	var err error
	if cfg.Pins, err = parsePins(env(EnvPin)); err != nil {
		return Config{}, err
	}
	if cfg.NodeID, err = parseNodeID(env(EnvNodeID)); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// JoinToken returns the join token: from the file when EnvJoinTokenFile is set
// (it wins, and a file that cannot give a token is an error rather than a silent
// fall back to the variable), else from EnvJoinToken. Neither set is an error. It
// is called only when the node has to enrol, so an enrolled node needs no token
// (D-22). No error text holds the token.
func (c Config) JoinToken(readFile func(string) ([]byte, error)) (string, error) {
	if path := strings.TrimSpace(c.tokenFile); path != "" {
		b, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", EnvJoinTokenFile, err)
		}
		token := strings.TrimSpace(string(b))
		if token == "" {
			return "", fmt.Errorf("%s: %s is empty", EnvJoinTokenFile, path)
		}
		return token, nil
	}
	if token := strings.TrimSpace(c.token); token != "" {
		return token, nil
	}
	return "", errors.New("no join token: set " + EnvJoinTokenFile + " (a Docker secret) or " + EnvJoinToken + "; copy the install command from TierPlan's Add environment screen")
}

// String is the configuration without the token: the server's host, the number of
// pins, the node ID, and whether a token source is set.
func (c Config) String() string {
	return fmt.Sprintf("ingress config{host=%s pins=%d node=%q token_file_set=%t token_set=%t}",
		c.host(), len(c.Pins), c.NodeID, strings.TrimSpace(c.tokenFile) != "", strings.TrimSpace(c.token) != "")
}

// GoString is String, so %#v cannot print the token fields.
func (c Config) GoString() string { return c.String() }

// LogValue makes slog print the same token-free view.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("host", c.host()),
		slog.Int("pins", len(c.Pins)),
		slog.String("node_id", c.NodeID),
		slog.Bool("token_file_set", strings.TrimSpace(c.tokenFile) != ""),
		slog.Bool("token_set", strings.TrimSpace(c.token) != ""),
	)
}

// host is the URL's host, for display; the URL itself is not printed.
func (c Config) host() string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// checkURL requires an address the supervisor may dial: wss:// with a host and no
// user name, query or fragment. The token travels in the handshake, never in a
// URL; an error never repeats the value, since a mistaken one may carry a
// password. A plain ws:// address stops the supervisor here, before any dial, so a
// mistyped scheme cannot send the join token unencrypted (T-23-05, D-33).
func checkURL(raw string) error {
	if raw == "" {
		return errors.New(EnvURL + " must be set to the server's ingress address, for example wss://tierplan.example:7480/ingress/connect")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New(EnvURL + " is not a valid address; it must be a wss:// address with a host and no user name, query or fragment")
	}
	if !strings.EqualFold(u.Scheme, "wss") {
		return errors.New(EnvURL + " must start with wss://: the supervisor connects to TierPlan only over TLS")
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		msg := EnvURL + " must be a wss:// address with a host and no user name, query or fragment"
		if u.Hostname() != "" {
			msg += " (the host is " + u.Hostname() + ")"
		}
		return errors.New(msg)
	}
	return nil
}

// ValidPin reports whether s is a pin as TierPlan writes it: "sha256/" and the
// standard base64 of a 32-byte hash. It is the same check the agent makes.
func ValidPin(s string) bool {
	rest, ok := strings.CutPrefix(s, "sha256/")
	if !ok || len(rest) != 44 {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	return err == nil && len(raw) == 32
}

// parsePins reads a comma-separated list of pins. Each must be well formed, so a
// mistyped pin stops the supervisor instead of leaving it to trust the system
// roots alone (T-23-06). A pin is public, so the bad entry is quoted.
func parsePins(raw string) ([]string, error) {
	var pins []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !ValidPin(p) {
			return nil, fmt.Errorf("%s must be sha256/ followed by the 44-character base64 hash, as TierPlan's install command shows it; got %s", EnvPin, strconv.Quote(p))
		}
		pins = append(pins, p)
	}
	return pins, nil
}

// parseNodeID is the optional swarm node ID. An unexpanded service template such
// as {{.Node.ID}} is refused, since it means the template was never rendered.
func parseNodeID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.Contains(raw, "{{") {
		return "", errors.New(EnvNodeID + " holds an unexpanded service template; it must be the node ID the engine rendered")
	}
	if !ingresswire.ValidIdentity(raw) {
		return "", errors.New(EnvNodeID + " must be a swarm node ID: letters, digits, colon, underscore, dot and hyphen only")
	}
	return raw, nil
}
