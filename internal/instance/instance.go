// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package instance holds the instance settings every request reads: the display
// name and the public URL (D-03, D-04).
package instance

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// RED stub: signatures only, so the tests compile and fail on their assertions.

// ErrPublicURLLocked is returned by SetTx when BAYPLAN_PUBLIC_URL fixes the URL.
var ErrPublicURLLocked = errors.New("the public URL is fixed by BAYPLAN_PUBLIC_URL")

// Instance caches the instance settings.
type Instance struct{}

// Load reads the stored settings.
func Load(ctx context.Context, s *store.Store, cfg config.Config) (*Instance, error) {
	return &Instance{}, nil
}

func (i *Instance) Name() string         { return "" }
func (i *Instance) PublicURL() string    { return "" }
func (i *Instance) PublicOrigin() string { return "" }
func (i *Instance) Secure() bool         { return false }
func (i *Instance) Locked() bool         { return false }
func (i *Instance) SetTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, name, publicURL string) error {
	return nil
}
func (i *Instance) Apply(name, publicURL string) {}
