// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Phase 22, D-23: the server mints the instance ID when it boots, before the hub serves a node, and a server that boots
// over the same data directory again keeps it.
func TestServerMintsTheInstanceIDAtBoot(t *testing.T) {
	ctx := testsupport.Context(t)
	dir := filepath.Join(t.TempDir(), "data")
	cfg := func() map[string]string { return map[string]string{"TIERPLAN_DATA_DIR": dir} }

	first, err := Build(ctx, testConfig(t, cfg()), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	id, err := instance.InstanceID(ctx, first.Store())
	if err != nil {
		_ = first.Close()
		t.Fatalf("the booted server holds no instance ID: %v", err)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Errorf("the instance ID %q is not a UUID: %v", id, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Build(ctx, testConfig(t, cfg()), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	again, err := instance.InstanceID(ctx, second.Store())
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Errorf("a second boot changed the instance ID from %q to %q", id, again)
	}
}

// Phase 22, D-29: the dial hosts the certificate preflight answers for are every source's, in order, and a nil source adds none.
func TestJoinDialHostsJoinsEverySource(t *testing.T) {
	a := func() []string { return []string{"agents.example.com", "b.example.com"} }
	b := func() []string { return []string{"ingress.example.com"} }
	empty := func() []string { return nil }

	got := joinDialHosts(a, nil, empty, b)()
	want := []string{"agents.example.com", "b.example.com", "ingress.example.com"}
	if !slices.Equal(got, want) {
		t.Errorf("joinDialHosts = %v, want %v", got, want)
	}
	if got := joinDialHosts()(); len(got) != 0 {
		t.Errorf("joinDialHosts with no sources = %v, want none", got)
	}
}
