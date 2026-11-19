// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"log/slog"
	"testing"

	"github.com/ianglennon/tierplan/internal/store/storetest"
)

// This file is package ingresshub so it can read the unexported clock. It must not import ingresstest, which
// imports this package and would form an import cycle.

// TestNewHubNilClockDefaultsToTimeNow pins D-36: a hub built with a nil clock reads time.Now, as NewService does,
// and the gate built from that clock works on the first handshake instead of dereferencing nil.
func TestNewHubNilClockDefaultsToTimeNow(t *testing.T) {
	h := NewHub(NewService(storetest.New(t), nil, nil), nil, slog.New(slog.DiscardHandler), nil)
	if h.now == nil {
		t.Fatal("NewHub left the clock nil")
	}
	if h.now().IsZero() {
		t.Fatal("the default clock returned the zero time")
	}
	release, refuse := h.gate.admit("192.0.2.1")
	if refuse != nil {
		t.Fatalf("the gate refused the first handshake of an address: %+v", refuse)
	}
	release()
}
