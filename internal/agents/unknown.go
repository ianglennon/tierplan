// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

const (
	// unknownAgentsCap bounds the registry: a flood of refused agents cannot make
	// the server hold more than this many (T-03-116).
	unknownAgentsCap = 1024
	// unknownAgentsTTL is how long after its last attempt an agent stays listed.
	// An agent that is still trying is refreshed by every attempt, so the list
	// empties by itself 24 hours after the last one (D-31).
	unknownAgentsTTL = 24 * time.Hour
)

// UnknownAgent is an agent that presented a certificate its environment issued but
// that the environment has no record of, as after a restore from a backup taken
// before the agent enrolled (D-31).
type UnknownAgent struct {
	// EnvID is the environment whose CA signed the certificate.
	EnvID string
	// Node is the certificate's common name: the agent's ID.
	Node string
	// IP is the address of the last attempt.
	IP string
	// LastAttempt is when the agent last tried.
	LastAttempt time.Time
	// Key identifies the agent's key, for Forget. It is the hex SHA-256 of the
	// certificate's public key.
	Key string
}

// UnknownAgents is the in-memory list of refused unknown agents. It is not stored:
// it exists to tell an administrator what to reinstall after a restore, and it
// fills again by itself at the next attempt of every agent that is still trying.
// It is safe for concurrent use.
type UnknownAgents struct {
	mu      sync.Mutex
	entries map[unknownKey]UnknownAgent
}

type unknownKey struct{ env, key string }

// NewUnknownAgents returns an empty registry.
func NewUnknownAgents() *UnknownAgents {
	return &UnknownAgents{entries: map[unknownKey]UnknownAgent{}}
}

// KeyFingerprint is the identifier Record and Forget use for an agent's public key.
func KeyFingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// Record notes an attempt. A second attempt of the same key in the same
// environment replaces the first. When the registry is full the entry with the
// oldest attempt is dropped to make room.
func (u *UnknownAgents) Record(a UnknownAgent) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	k := unknownKey{env: a.EnvID, key: a.Key}
	if _, have := u.entries[k]; !have && len(u.entries) >= unknownAgentsCap {
		var oldest unknownKey
		var oldestAt time.Time
		first := true
		for key, e := range u.entries {
			if first || e.LastAttempt.Before(oldestAt) {
				oldest, oldestAt, first = key, e.LastAttempt, false
			}
		}
		delete(u.entries, oldest)
	}
	u.entries[k] = a
}

// List returns the agents whose last attempt was less than 24 hours before now,
// newest attempt first. It removes the older ones.
func (u *UnknownAgents) List(now time.Time) []UnknownAgent {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]UnknownAgent, 0, len(u.entries))
	for k, e := range u.entries {
		if now.Sub(e.LastAttempt) >= unknownAgentsTTL {
			delete(u.entries, k)
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastAttempt.Equal(out[j].LastAttempt) {
			return out[i].LastAttempt.After(out[j].LastAttempt)
		}
		if out[i].EnvID != out[j].EnvID {
			return out[i].EnvID < out[j].EnvID
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Forget removes the agent with this key in this environment, if listed.
func (u *UnknownAgents) Forget(envID, key string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.entries, unknownKey{env: envID, key: key})
}
