// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// SettingInstanceID is the settings key of the TierPlan instance ID: the random ID every ingress
// node pins at enrolment (D-14). It is a settings row, so a backup carries it and a restore keeps it
// (settings is Included). It is never an app_meta row: the ACME install ID lives there, and a restore
// drops that on purpose, the opposite of what a node pin needs.
const SettingInstanceID = "instance_id"

// ErrNoInstanceID is returned by InstanceID when no ID has been minted yet.
var ErrNoInstanceID = errors.New("no instance ID has been minted yet")

// EnsureInstanceID returns the instance ID, minting a UUIDv7 into the settings table only when the
// key is absent. Concurrent first calls all return the one ID that won the insert, and leave one row.
//
// A nil now is time.Now (D-36), as ingresshub.NewService treats a nil clock.
func EnsureInstanceID(ctx context.Context, s *store.Store, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	minted, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("mint the instance ID: %w", err)
	}
	if err := s.Q.InsertSettingIfAbsent(ctx, sqlc.InsertSettingIfAbsentParams{
		Key: SettingInstanceID, Value: minted.String(), UpdatedAt: now().UnixNano(),
	}); err != nil {
		return "", fmt.Errorf("store the instance ID: %w", err)
	}
	// Read back on the write pool: a concurrent first call that lost the insert reads the winner's row.
	stored, err := s.Q.GetSetting(ctx, SettingInstanceID)
	if err != nil {
		return "", fmt.Errorf("read the instance ID: %w", err)
	}
	if _, err := uuid.Parse(stored); err != nil {
		return "", fmt.Errorf("the setting %s is not a UUID: %w", SettingInstanceID, err)
	}
	return stored, nil
}

// InstanceID reads the instance ID on every call and never caches it, so a restore that reaches a
// running server through Reload is seen at once. It returns ErrNoInstanceID when none is stored.
func InstanceID(ctx context.Context, s *store.Store) (string, error) {
	v, err := s.RQ.GetSetting(ctx, SettingInstanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInstanceID
	}
	if err != nil {
		return "", fmt.Errorf("read the instance ID: %w", err)
	}
	return v, nil
}
