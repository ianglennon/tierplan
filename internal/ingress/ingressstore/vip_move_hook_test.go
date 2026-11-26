// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
)

// TestInsertVIPMoveWithHookRollsBack: the move, the prune and the hook are one transaction, so a hook that wrote an audit
// row and then failed leaves neither the move nor the row, and a hook that succeeds leaves both.
func TestInsertVIPMoveWithHookRollsBack(t *testing.T) {
	ctx := context.Background()
	s, st, v := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), func() time.Time { return fixedNow })
	m := VIPMove{EnvID: "env-a", At: fixedNow, FromNodeID: "n1", FromNodeName: "one", ToNodeID: "n2", ToNodeName: "two", Reason: "holder_failed"}
	record := func(ctx context.Context, tx *sql.Tx) error {
		return w.RecordTx(ctx, tx, audit.Entry{
			Actor: audit.SystemIngress, Action: audit.ActionIngressVIPMoved, TargetType: "environment", TargetID: "env-a",
			EnvID: "env-a", Result: audit.ResultSuccess,
			Details: map[string]string{"from": m.FromNodeName, "to": m.ToNodeName, "reason": m.Reason},
		})
	}

	boom := errors.New("the hook failed")
	err := s.InsertVIPMoveWithHook(ctx, m, func(ctx context.Context, tx *sql.Tx) error {
		if err := record(ctx, tx); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InsertVIPMoveWithHook with a failing hook = %v, want the hook's error", err)
	}
	if _, ok, err := s.LatestVIPMove(ctx, "env-a"); err != nil || ok {
		t.Fatalf("LatestVIPMove after a rolled-back insert = %v, %v; want none", ok, err)
	}
	if n := len(audittest.Rows(t, st, audit.ActionIngressVIPMoved)); n != 0 {
		t.Fatalf("%d ingress.vip_moved rows after a rolled-back insert, want 0", n)
	}

	if err := s.InsertVIPMoveWithHook(ctx, m, record); err != nil {
		t.Fatalf("InsertVIPMoveWithHook: %v", err)
	}
	if _, ok, err := s.LatestVIPMove(ctx, "env-a"); err != nil || !ok {
		t.Fatalf("LatestVIPMove after the insert = %v, %v; want the move", ok, err)
	}
	if n := len(audittest.Rows(t, st, audit.ActionIngressVIPMoved)); n != 1 {
		t.Fatalf("%d ingress.vip_moved rows after the insert, want 1", n)
	}
}
