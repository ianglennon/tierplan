// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/ianglennon/bayplan/internal/store"
)

// errNotImplemented is the RED-phase stub.
var errNotImplemented = errors.New("audit: not implemented")

// Filter narrows the audit log. The zero value matches every entry.
type Filter struct {
	Actor   string
	Actions []string
	From    time.Time
	To      time.Time
}

// Row is one audit entry as read.
type Row struct {
	Seq        int64             `json:"seq"`
	ID         string            `json:"id"`
	Time       time.Time         `json:"time"`
	ActorType  string            `json:"actorType"`
	ActorName  string            `json:"actor"`
	Action     string            `json:"action"`
	TargetType string            `json:"targetType"`
	TargetID   string            `json:"target"`
	EnvID      string            `json:"environment,omitempty"`
	Result     string            `json:"result"`
	Reason     string            `json:"reason"`
	IP         string            `json:"ip"`
	UserAgent  string            `json:"userAgent"`
	Details    map[string]string `json:"details"`
}

// Query is the RED-phase stub.
func Query(_ context.Context, _ *store.Store, _ Filter, _ int64, _ int) ([]Row, int64, error) {
	return nil, 0, errNotImplemented
}

// Actors is the RED-phase stub.
func Actors(_ context.Context, _ *store.Store) ([]string, error) { return nil, errNotImplemented }

// ExportCSV is the RED-phase stub.
func ExportCSV(_ context.Context, _ *store.Store, _ Filter, _ io.Writer) error {
	return errNotImplemented
}

// ExportJSON is the RED-phase stub.
func ExportJSON(_ context.Context, _ *store.Store, _ Filter, _ io.Writer) error {
	return errNotImplemented
}
