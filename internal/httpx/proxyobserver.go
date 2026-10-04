// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package httpx

import (
	"net/http"
	"time"
)

// Kinds of proxy problem the observer records.
const (
	SignalUntrustedForwarded = "untrusted_forwarded"
	SignalURLMismatch        = "url_mismatch"
	SignalWebSocketFailing   = "websocket_failing"
)

// Signal is one kind of proxy problem seen from one peer, host and scheme.
type Signal struct {
	Kind      string
	Peer      string
	Host      string
	Scheme    string
	FirstSeen time.Time
	LastSeen  time.Time
	Count     int
}

// ProxyObserver is a stub for the RED run.
type ProxyObserver struct{}

// NewProxyObserver is a stub for the RED run.
func NewProxyObserver(func() string, func() time.Time) *ProxyObserver { return &ProxyObserver{} }

// OnNew is a stub for the RED run.
func (o *ProxyObserver) OnNew(func(kind string)) *ProxyObserver { return o }

// Observe is a stub for the RED run.
func (o *ProxyObserver) Observe(next http.Handler) http.Handler { return next }

// RecordUpgradeFailure is a stub for the RED run.
func (o *ProxyObserver) RecordUpgradeFailure(string) {}

// Signals is a stub for the RED run.
func (o *ProxyObserver) Signals() []Signal { return nil }
