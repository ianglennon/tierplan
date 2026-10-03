// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package state is the agent's identity on its volume: the key it generated, the
// certificate Bayplan issued for it, and the IDs that name it. With these the
// agent reconnects in hello mode and no longer needs its join token (D-01, D-03).
package state

import (
	"crypto/ed25519"
	"time"
)

// File names inside the state directory.
const (
	KeyFile       = "key.pem"
	CertFile      = "cert.der"
	IdentityFile  = "identity.json"
	ConnectedFile = "connected"
)

// State is an enrolled agent's identity.
type State struct {
	AgentID, EnvID string
	// Key is the agent's Ed25519 private key. It never leaves the host.
	Key ed25519.PrivateKey
	// Cert is the DER certificate Bayplan issued for Key.
	Cert []byte
}

var _ = time.Second

// Load reads the identity in dir. The bool is false, with a nil error, when the
// agent has not enrolled yet.
func Load(dir string) (State, bool, error) { return State{}, false, nil }

// Save writes the identity to dir.
func Save(dir string, s State) error { return nil }

// MarkConnected records that the agent completed a handshake just now.
func MarkConnected(dir string) error { return nil }
