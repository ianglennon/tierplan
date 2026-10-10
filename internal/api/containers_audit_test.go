// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

// The row of an action that has been done is written even if the browser that asked
// for it has gone away (found by the end-to-end suite: a restart that waits out its
// stop timeout was audited as "context canceled" when the page was closed).
func TestContainerActionAuditRowsAreWrittenEvenWhenTheRequestWasCancelled(t *testing.T) {
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &rbac.Principal{Type: rbac.PrincipalUser, ID: "user-1", Name: "admin"}
	targets := []ContainerActionTarget{{NodeID: "node-1", ID: "abc"}}
	results := []ContainerActionResult{{NodeID: "node-1", ID: "abc", Name: "web", Result: actionDone}}
	auditTargets(ctx, ContainersDeps{Audit: w, Log: slog.New(slog.DiscardHandler)}, p, "env-1", "agent",
		audit.ActionContainerRestarted, "", nil, targets, results)

	var n int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND target_id = ?`,
		audit.ActionContainerRestarted, "abc").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows for the restart = %d, want 1", n)
	}
}

// The rows of network and volume changes, which are written after the change, survive
// a cancelled request the same way.
func TestNetworkAndVolumeAuditRowsAreWrittenEvenWhenTheRequestWasCancelled(t *testing.T) {
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil)
	log := slog.New(slog.DiscardHandler)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &rbac.Principal{Type: rbac.PrincipalUser, ID: "user-1", Name: "admin"}

	recordNetwork(ctx, NetworksDeps{Audit: w, Log: log}, p, "env-1", "agent",
		audit.ActionNetworkRemoved, "net-1", "app", codeRemoved, map[string]string{"name": "app"})
	auditVolumeRemoved(ctx, VolumesDeps{Audit: w, Log: log}, p, "env-1", "agent", "",
		VolumeDeleteTarget{NodeID: "node-1", Name: "data"}, "local", VolumeDeleteResult{NodeID: "node-1", Name: "data", Result: "done"})

	for action, target := range map[string]string{audit.ActionNetworkRemoved: "net-1", audit.ActionVolumeRemoved: "data"} {
		var n int
		if err := st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND target_id = ?`, action, target).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("audit rows for %s = %d, want 1", action, n)
		}
	}
}
