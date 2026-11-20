// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package identity keeps the node's identity on its volume: the Ed25519 key and the enrolment record.
package identity

import (
	"crypto/ed25519"
	"errors"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
)

// Record is the enrolment of this node.
type Record struct {
	NodeID     string `json:"nodeId"`
	EnvID      string `json:"envId"`
	InstanceID string `json:"instanceId"`
}

// Store is the node's identity files under one root.
type Store struct {
	fsys fsx.FS
	root string
}

var errTODO = errors.New("identity: not implemented")

// New returns a Store over root.
func New(fsys fsx.FS, root string) *Store { return &Store{fsys: fsys, root: root} }

// LoadOrCreateKey is a stub.
func (s *Store) LoadOrCreateKey() (ed25519.PrivateKey, error) { return nil, errTODO }

// Load is a stub.
func (s *Store) Load() (Record, bool, error) { return Record{}, false, errTODO }

// Save is a stub.
func (s *Store) Save(r Record) error { return errTODO }

// Enrolled is a stub.
func (s *Store) Enrolled() (bool, error) { return false, errTODO }
