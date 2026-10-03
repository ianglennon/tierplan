// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/tunnel"
)

// Types of the events on the health stream (D-36, D-37). failover and
// proxy.warning are reserved for later plans.
const (
	EventEnvStatus         = "env.status"
	EventAgentConnected    = "agent.connected"
	EventAgentDisconnected = "agent.disconnected"
	EventAgentRefused      = "agent.refused"
	EventFailover          = "failover"
	EventProxyWarning      = "proxy.warning"
)

// HealthEvent is one event of the health stream.
type HealthEvent struct {
	ID    uint64
	Type  string
	EnvID string
	At    time.Time
	Data  any
}

// AgentConnectedData is the data of an agent.connected event.
type AgentConnectedData struct {
	AgentID, Node, Type, Version string
}

// AgentDisconnectedData is the data of an agent.disconnected event.
type AgentDisconnectedData struct {
	AgentID, Node, Reason string
}

// AgentRefusedData is the data of an agent.refused event.
type AgentRefusedData struct {
	Code, Mode, Node, IP string
}

// refusalEvery is how often one address may make one environment publish a refusal.
const refusalEvery = 10 * time.Second

// Broker is the health broker (stub).
type Broker struct {
	mu   sync.Mutex
	last map[string]string
}

// NewBroker returns a broker over svc.
func NewBroker(svc *Service, now func() time.Time) *Broker { return &Broker{} }

// Publish adds an event to the stream.
func (b *Broker) Publish(ev HealthEvent) HealthEvent { return ev }

// Subscribe registers a subscriber.
func (b *Broker) Subscribe(ctx context.Context) (backlog []HealthEvent, ch <-chan HealthEvent, cancel func()) {
	return nil, make(chan HealthEvent), func() {}
}

// SnapshotAll returns every environment.
func (b *Broker) SnapshotAll(ctx context.Context) ([]Environment, error) { return nil, nil }

// Notify asks for an environment's health to be recomputed.
func (b *Broker) Notify(envID string) {}

// Recompute recomputes the health of every environment once.
func (b *Broker) Recompute(ctx context.Context) {}

// Run recomputes on every tick and notification until ctx ends.
func (b *Broker) Run(ctx context.Context) {}

// Close ends every subscription.
func (b *Broker) Close() {}

// EventsAfter trims backlog to the events after lastID.
func EventsAfter(backlog []HealthEvent, lastID string) []HealthEvent { return backlog }

// AgentConnected implements tunnel.Notifier.
func (b *Broker) AgentConnected(s *tunnel.Session) {}

// AgentDisconnected implements tunnel.Notifier.
func (b *Broker) AgentDisconnected(s *tunnel.Session, code string) {}

// AgentRefused implements tunnel.Notifier.
func (b *Broker) AgentRefused(envID, code, mode, node, ip string) {}

var _ tunnel.Notifier = (*Broker)(nil)
