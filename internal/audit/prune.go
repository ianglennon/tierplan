// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package audit

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/ianglennon/coaming/internal/store/sqlc"
)

const (
	// maxRetentionDays keeps retentionDays*24h inside time.Duration; anything
	// longer is a century of history and is treated as "keep everything".
	maxRetentionDays = 36500

	nightlyFirstDelay = time.Minute
	nightlyInterval   = 24 * time.Hour
)

// Prune deletes entries older than retentionDays and returns how many it
// deleted (D-41). It does nothing when retentionDays is 0 (keep forever) or
// when nothing is old enough.
//
// Everything happens in one transaction: find the newest entry past the
// cutoff, delete up to it, remember its HMAC in app_meta, and append an
// audit.pruned entry as SystemRetention whose details carry that HMAC and the
// count. Verification starts from the anchor, so the pruned prefix is
// accounted for rather than looking like tampering.
func Prune(ctx context.Context, w *Writer, retentionDays int) (int, error) {
	if retentionDays <= 0 || retentionDays > maxRetentionDays {
		return 0, nil
	}
	cutoff := w.now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	tx, err := w.s.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("audit prune: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := w.s.Q.WithTx(tx)

	maxSeq, err := q.MaxSeqBefore(ctx, cutoff.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("audit prune: find cutoff: %w", err)
	}
	if maxSeq == 0 {
		return 0, nil
	}
	last, err := q.GetAuditBySeq(ctx, maxSeq)
	if err != nil {
		return 0, fmt.Errorf("audit prune: read last pruned entry: %w", err)
	}
	deleted, err := q.DeleteAuditUpTo(ctx, maxSeq)
	if err != nil {
		return 0, fmt.Errorf("audit prune: delete: %w", err)
	}
	lastHex := hex.EncodeToString(last.Hmac)
	if err := q.UpsertMeta(ctx, sqlc.UpsertMetaParams{Key: metaLastPruned, Value: lastHex, UpdatedAt: w.now().UTC().UnixNano()}); err != nil {
		return 0, fmt.Errorf("audit prune: record anchor: %w", err)
	}

	rec, err := w.prepare(Entry{
		Actor:      SystemRetention,
		Action:     ActionAuditPruned,
		TargetType: "audit_log",
		Result:     ResultSuccess,
		Details: map[string]string{
			"last_pruned_hmac": lastHex,
			"count":            strconv.FormatInt(deleted, 10),
			"cutoff":           cutoff.Format(time.RFC3339),
		},
	})
	if err != nil {
		return 0, err
	}
	seq, err := w.appendTx(ctx, tx, rec)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("audit prune: commit: %w", err)
	}
	w.logEntry(seq, rec)
	return int(deleted), nil
}

// RunNightly prunes one minute after it starts and then every 24 hours, until
// ctx is done. It blocks; start it in a goroutine. It does nothing when
// retentionDays is 0.
func RunNightly(ctx context.Context, w *Writer, retentionDays int, log *slog.Logger) {
	runNightly(ctx, w, retentionDays, log, nightlyFirstDelay, nightlyInterval)
}

func runNightly(ctx context.Context, w *Writer, retentionDays int, log *slog.Logger, first, every time.Duration) {
	if retentionDays <= 0 {
		return
	}
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		n, err := Prune(ctx, w, retentionDays)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Error("audit retention prune failed", "error", err)
		case n > 0:
			log.Info("audit retention pruned old entries", "count", n, "retention_days", retentionDays)
		}
		timer.Reset(every)
	}
}
