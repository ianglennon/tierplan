// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/rbac/compose"
)

// ApplyInput is a spec to apply, as ReviewInput.
type ApplyInput struct {
	EnvID     string
	Client    *dockerx.Client
	ServiceID string
	Version   uint64
	Spec      json.RawMessage
	Caller    Caller
}

// ApplyResult is what an apply did. It is filled as far as the work got, also when Apply returns an
// error, so the caller can audit the attempt.
type ApplyResult struct {
	ServiceID, Name string
	Image           ImageInfo
	Findings        []compose.Finding
	Warnings        []string
}

// Apply sends the spec once.
func Apply(ctx context.Context, deps Deps, in ApplyInput) (ApplyResult, error) {
	return ApplyResult{}, errors.New("swarmops: not implemented")
}
