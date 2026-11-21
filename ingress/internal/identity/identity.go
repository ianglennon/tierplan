// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package identity keeps the node's identity on its volume: the Ed25519 key and the enrolment record
// (node ID, environment ID and the instance ID the node pinned).
//
// The key is made once and is never replaced by the supervisor. The server refuses a second enrolment of
// the same node until an administrator revokes the first (node_already_enrolled), so a lost or
// regenerated key would lock the node out. For the same reason a file that exists but cannot be read back
// is an error that names the file and leaves it alone; it is never treated as absent.
//
// No error here carries the key's bytes or any text of a file, and the package writes no log (D-23).
package identity

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

const (
	keyFile    = "state/node.key"
	recordFile = "state/identity.json"
	tmpDir     = "tmp"
	fileMode   = fs.FileMode(0o600)
	pemType    = "PRIVATE KEY"
)

// Record is the enrolment of this node.
type Record struct {
	NodeID     string `json:"nodeId"`
	EnvID      string `json:"envId"`
	InstanceID string `json:"instanceId"`
}

// Store is the node's identity files under one root, which is ingressbundle.NodeRoot in production.
// The root's state/ and tmp/ directories must exist (the cache's Prepare makes them).
type Store struct {
	fsys fsx.FS
	root string
}

// New returns a Store over root.
func New(fsys fsx.FS, root string) *Store { return &Store{fsys: fsys, root: root} }

func (s *Store) abs(rel string) string { return filepath.Join(s.root, filepath.FromSlash(rel)) }

// LoadOrCreateKey returns the node's key. When state/node.key does not exist it makes the key, writes it
// as a PKCS#8 PEM file of mode 0600 by an atomic write and returns it. A file that exists but does not
// hold an Ed25519 key is an error and is left as it is.
func (s *Store) LoadOrCreateKey() (ed25519.PrivateKey, error) {
	raw, err := s.fsys.ReadFile(s.abs(keyFile))
	switch {
	case err == nil:
		return parseKey(raw)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("identity: read %s: %w", keyFile, err)
	}
	key, err := ingresswire.NewNodeKey()
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("identity: encode the key for %s", keyFile)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der})
	if err := fsx.WriteAtomic(s.fsys, s.abs(tmpDir), s.abs(keyFile), data, fileMode); err != nil {
		return nil, fmt.Errorf("identity: write %s: %w", keyFile, err)
	}
	return key, nil
}

// parseKey reads a PKCS#8 PEM Ed25519 key. Its errors say which file is damaged, never what it holds.
func parseKey(raw []byte) (ed25519.PrivateKey, error) {
	damaged := fmt.Errorf("identity: %s exists but does not hold an Ed25519 private key; it is left untouched, and a new key would be refused until the node is revoked on the server", keyFile)
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != pemType {
		return nil, damaged
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, damaged
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, damaged
	}
	return key, nil
}

// Load returns the enrolment record, and false when the node has not enrolled (no state/identity.json).
// A file that exists but is not a complete record is an error and is left as it is.
func (s *Store) Load() (Record, bool, error) {
	raw, err := s.fsys.ReadFile(s.abs(recordFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("identity: read %s: %w", recordFile, err)
	}
	var r Record
	if json.Unmarshal(raw, &r) != nil || r.NodeID == "" || r.InstanceID == "" {
		return Record{}, false, fmt.Errorf("identity: %s exists but is not a complete enrolment record; it is left untouched", recordFile)
	}
	return r, true, nil
}

// Save writes the enrolment record to state/identity.json atomically with mode 0600. A record with no
// node ID or no instance ID is refused.
func (s *Store) Save(r Record) error {
	if r.NodeID == "" || r.InstanceID == "" {
		return errors.New("identity: refusing to save an enrolment record without a node ID and an instance ID")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("identity: encode the record for %s", recordFile)
	}
	if err := fsx.WriteAtomic(s.fsys, s.abs(tmpDir), s.abs(recordFile), data, fileMode); err != nil {
		return fmt.Errorf("identity: write %s: %w", recordFile, err)
	}
	return nil
}

// Enrolled is true exactly when state/identity.json holds a node ID: it decides whether the supervisor
// reads the join token.
func (s *Store) Enrolled() (bool, error) {
	_, ok, err := s.Load()
	return ok, err
}
