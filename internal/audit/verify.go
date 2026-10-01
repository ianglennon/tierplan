// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit

import (
	"context"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Broken names the first entry that fails verification.
type Broken struct {
	Seq    int64
	ID     string
	Reason string
}

// VerifyResult is the outcome of Verify.
type VerifyResult struct {
	Entries int
	Broken  *Broken
}

// Verify walks the chain.
func Verify(ctx context.Context, s *store.Store, v *vault.Vault) (VerifyResult, error) {
	return VerifyResult{}, errNotImplemented
}
