// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package envs keeps the managed environments: which Docker hosts an admin
// opted in (the registry), and whether each is reachable right now (the
// monitor).
package envs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/engine"
)

// State is what the monitor currently believes about an environment.
type State string

// States.
const (
	StateUnknown     State = "unknown"
	StateOnline      State = "online"
	StateOffline     State = "offline"
	StateUnsupported State = "unsupported"
)

// Status is the monitor's view of one environment.
type Status struct {
	State       State
	Since       time.Time
	Reason      string
	LastAttempt time.Time
	Probe       dockerx.ProbeResult
}

// Errors from Monitor.Client.
var (
	ErrOffline     = errors.New("envs: the environment is offline")
	ErrUnsupported = errors.New("envs: the environment's engine is not supported")
)

var errNotImplemented = errors.New("envs: not implemented")

// Monitor probes each started environment.
type Monitor struct {
	// interval and probeTimeout are the online probe period and the per-probe
	// timeout; tests shorten them.
	interval, probeTimeout time.Duration
}

// NewMonitor returns a Monitor.
func NewMonitor(log *slog.Logger, now func() time.Time, sleep func(context.Context, time.Duration) error) *Monitor {
	return &Monitor{interval: 30 * time.Second, probeTimeout: 5 * time.Second}
}

// Start begins monitoring envID.
func (m *Monitor) Start(ctx context.Context, envID string, t engine.Transport) error {
	return errNotImplemented
}

// Stop ends monitoring envID.
func (m *Monitor) Stop(envID string) {}

// StopAll ends every monitor.
func (m *Monitor) StopAll() {}

// Status returns the current status of envID.
func (m *Monitor) Status(envID string) Status { return Status{} }

// Kick asks for an immediate probe.
func (m *Monitor) Kick(envID string) {}

// Client returns the environment's Docker client when it is online.
func (m *Monitor) Client(envID string) (*dockerx.Client, Status, error) {
	return nil, Status{}, errNotImplemented
}

func (m *Monitor) loopCount() int { return 0 }
