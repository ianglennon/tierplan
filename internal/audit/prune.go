// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit

import (
	"context"
	"log/slog"
	"time"
)

// Prune deletes entries older than retentionDays and appends an anchor.
func Prune(ctx context.Context, w *Writer, retentionDays int) (int, error) {
	return 0, errNotImplemented
}

// RunNightly prunes once a day until ctx is done.
func RunNightly(ctx context.Context, w *Writer, retentionDays int, log *slog.Logger) {}

func runNightly(ctx context.Context, w *Writer, retentionDays int, log *slog.Logger, first, every time.Duration) {
}
