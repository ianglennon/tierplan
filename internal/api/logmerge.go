// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"time"
)

type svcRec struct {
	T, S, M, TaskID, NodeID string
	seq                     uint64
}

type taskInfo struct {
	ID                  string
	Slot                int
	NodeID, Node, State string
}

func mergeBurst(recs []svcRec, tasks map[string]taskInfo, n int) []svcRec { return nil }

type reorderWindow struct{}

func newReorderWindow(hold time.Duration, now func() time.Time, slotOf func(string) int) *reorderWindow {
	return &reorderWindow{}
}
func (w *reorderWindow) add(r svcRec)                {}
func (w *reorderWindow) release() []svcRec           { return nil }
func (w *reorderWindow) wait() (time.Duration, bool) { return 0, false }
func (w *reorderWindow) drain() []svcRec             { return nil }

type taskMeta struct{}

func newTaskMeta(load func(context.Context) ([]taskInfo, error), now func() time.Time) *taskMeta {
	return &taskMeta{}
}
func (m *taskMeta) lookup(ctx context.Context, id string) (taskInfo, bool) { return taskInfo{}, false }
func (m *taskMeta) all(ctx context.Context) []taskInfo                     { return nil }
