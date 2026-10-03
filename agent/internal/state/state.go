// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package state is the agent's identity on its volume: the key it generated, the
// certificate Bayplan issued for it, and the IDs that name it. With these the
// agent reconnects in hello mode and no longer needs its join token (D-01, D-03).
//
// Every file is private to the agent user (0600) and is replaced atomically, so
// a crash in the middle of a save leaves the previous identity usable.
package state

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File names inside the state directory.
const (
	KeyFile       = "key.pem"
	CertFile      = "cert.der"
	IdentityFile  = "identity.json"
	ConnectedFile = "connected"
	// PinsFile holds the server certificate pins the agent keeps (D-06).
	PinsFile = "pins.json"

	fileMode  = 0o600
	tmpSuffix = ".tmp"
)

// State is an enrolled agent's identity.
type State struct {
	AgentID, EnvID string
	// Key is the agent's Ed25519 private key. It never leaves the host.
	Key ed25519.PrivateKey
	// Cert is the DER certificate Bayplan issued for Key.
	Cert []byte
}

// identity is the content of identity.json.
type identity struct {
	AgentID string `json:"agentId"`
	EnvID   string `json:"envId"`
}

// Load reads the identity in dir. The bool is false, with a nil error, when the
// agent has not enrolled yet: identity.json is the marker, so a directory that
// does not exist and an empty one read the same. An identity whose key or
// certificate is missing or unreadable is an error, because it cannot be used
// and must not be silently replaced by a second enrolment.
func Load(dir string) (State, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, IdentityFile)) //nolint:gosec // the name is a constant joined to the agent's own state directory
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read the agent identity: %w", err)
	}
	var id identity
	if err := json.Unmarshal(raw, &id); err != nil || id.AgentID == "" || id.EnvID == "" {
		return State{}, false, fmt.Errorf("the agent identity in %s is damaged", filepath.Join(dir, IdentityFile))
	}

	keyPEM, err := os.ReadFile(filepath.Join(dir, KeyFile)) //nolint:gosec // the name is a constant joined to the agent's own state directory
	if err != nil {
		return State{}, false, fmt.Errorf("the agent is enrolled but its key cannot be read: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return State{}, false, fmt.Errorf("%s is not a PEM file", filepath.Join(dir, KeyFile))
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return State{}, false, fmt.Errorf("parse %s: %w", filepath.Join(dir, KeyFile), err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return State{}, false, fmt.Errorf("%s is not an Ed25519 key", filepath.Join(dir, KeyFile))
	}

	cert, err := os.ReadFile(filepath.Join(dir, CertFile)) //nolint:gosec // the name is a constant joined to the agent's own state directory
	if err != nil {
		return State{}, false, fmt.Errorf("the agent is enrolled but its certificate cannot be read: %w", err)
	}
	if len(cert) == 0 {
		return State{}, false, fmt.Errorf("%s is empty", filepath.Join(dir, CertFile))
	}
	return State{AgentID: id.AgentID, EnvID: id.EnvID, Key: key, Cert: cert}, true, nil
}

// Save writes the identity to dir. All three files are written to temporary
// names first and renamed into place afterwards, so a failure while writing
// changes nothing. The directory must exist: the image creates it with the
// right owner and Save never makes one.
func Save(dir string, s State) error {
	if s.AgentID == "" || s.EnvID == "" || len(s.Key) != ed25519.PrivateKeySize || len(s.Cert) == 0 {
		return errors.New("state: an identity needs an agent ID, an environment ID, a key and a certificate")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(s.Key)
	if err != nil {
		return fmt.Errorf("marshal the agent key: %w", err)
	}
	idJSON, err := json.Marshal(identity{AgentID: s.AgentID, EnvID: s.EnvID})
	if err != nil {
		return fmt.Errorf("marshal the agent identity: %w", err)
	}
	// The certificate goes first: if its rename fails nothing has changed. The
	// identity goes last, because it is the marker Load looks for.
	files := []struct {
		name string
		data []byte
	}{
		{CertFile, s.Cert},
		{KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})},
		{IdentityFile, append(idJSON, '\n')},
	}

	var written []string
	cleanup := func() {
		for _, tmp := range written {
			_ = os.Remove(tmp)
		}
	}
	for _, f := range files {
		tmp := filepath.Join(dir, f.name+tmpSuffix)
		if err := writeFile(tmp, f.data); err != nil {
			cleanup()
			return fmt.Errorf("save %s: %w", f.name, err)
		}
		written = append(written, tmp)
	}
	for i, f := range files {
		if err := os.Rename(written[i], filepath.Join(dir, f.name)); err != nil {
			cleanup()
			return fmt.Errorf("save %s: %w", f.name, err)
		}
	}
	return nil
}

// MarkConnected records that the agent completed a handshake just now. The
// self-update helper reads it to learn that a new version came up healthy.
func MarkConnected(dir string) error {
	tmp := filepath.Join(dir, ConnectedFile+tmpSuffix)
	if err := writeFile(tmp, []byte(time.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
		return fmt.Errorf("mark connected: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, ConnectedFile)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mark connected: %w", err)
	}
	return nil
}

// writeFile creates a file that did not exist, with mode 0600 from the first
// byte, writes data and flushes it to disk.
func writeFile(path string, data []byte) error {
	_ = os.Remove(path)                                                      // a leftover from a crash, never trusted
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode) //nolint:gosec // path is a constant name joined to the agent state directory
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// PinOverlap is how long a pin that the server stopped listing stays valid. A
// server that changes its key therefore does not cut off an agent that has not
// yet heard about the new one (D-06).
const PinOverlap = 30 * 24 * time.Hour

// MaxPins bounds the pins kept. A server lists one or two; the cap keeps a
// misbehaving one from growing the file.
const MaxPins = 16

// PinEntry is one server certificate pin. ExpiresAt is zero while the server
// lists the pin, and is the end of its overlap window once it stopped.
type PinEntry struct {
	Pin       string    `json:"pin"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

// ValidPin reports whether s is a pin as authn.SPKIPin writes it: "sha256/" and
// the standard base64 of a 32-byte hash.
func ValidPin(s string) bool {
	rest, ok := strings.CutPrefix(s, "sha256/")
	if !ok || len(rest) != 44 {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	return err == nil && len(raw) == 32
}

// live reports whether e still counts at now.
func (e PinEntry) live(now time.Time) bool {
	return e.ExpiresAt.IsZero() || now.Before(e.ExpiresAt)
}

// MergePins is the pins to keep after a welcome that listed fromServer, given
// the current ones. Every well-formed pin the server lists is kept with no
// expiry. A current pin it no longer lists stays until PinOverlap after the first
// welcome that left it out, and is then dropped; later welcomes do not extend that
// deadline. Malformed entries are ignored and the result is capped at MaxPins,
// the server's own pins first.
func MergePins(current []PinEntry, fromServer []string, now time.Time) []PinEntry {
	var out []PinEntry
	listed := map[string]bool{}
	for _, p := range fromServer {
		if !ValidPin(p) || listed[p] {
			continue
		}
		listed[p] = true
		out = append(out, PinEntry{Pin: p})
	}
	for _, e := range current {
		if listed[e.Pin] || !ValidPin(e.Pin) || !e.live(now) {
			continue
		}
		listed[e.Pin] = true
		if e.ExpiresAt.IsZero() {
			e.ExpiresAt = now.Add(PinOverlap)
		}
		out = append(out, e)
	}
	if len(out) > MaxPins {
		out = out[:MaxPins]
	}
	return out
}

// ActivePins is the pins that count at now, in order.
func ActivePins(entries []PinEntry, now time.Time) []string {
	var out []string
	for _, e := range entries {
		if e.live(now) {
			out = append(out, e.Pin)
		}
	}
	return out
}

// EqualPins reports whether two lists hold the same pins and deadlines.
func EqualPins(a, b []PinEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Pin != b[i].Pin || !a[i].ExpiresAt.Equal(b[i].ExpiresAt) {
			return false
		}
	}
	return true
}

// LoadPins reads the pins in dir. A directory with no pins.json has none.
func LoadPins(dir string) ([]PinEntry, error) {
	raw, err := os.ReadFile(filepath.Join(dir, PinsFile)) //nolint:gosec // the name is a constant joined to the agent's own state directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", PinsFile, err)
	}
	var entries []PinEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", filepath.Join(dir, PinsFile), err)
	}
	return entries, nil
}

// SavePins replaces pins.json with entries, atomically and private to the agent.
func SavePins(dir string, entries []PinEntry) error {
	if entries == nil {
		entries = []PinEntry{}
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal the pins: %w", err)
	}
	tmp := filepath.Join(dir, PinsFile+tmpSuffix)
	if err := writeFile(tmp, append(data, '\n')); err != nil {
		return fmt.Errorf("save %s: %w", PinsFile, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, PinsFile)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save %s: %w", PinsFile, err)
	}
	return nil
}
