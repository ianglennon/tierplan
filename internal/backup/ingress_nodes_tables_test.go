// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backup_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/backup"
)

// A restored server still recognises its ingress nodes by their stored keys, and never carries a join
// token (Phase 22, D-08, D-29).
func TestIngressNodeTablesAreClassifiedForBackups(t *testing.T) {
	for table, want := range map[string]backup.Class{
		"ingress_join_tokens": backup.Excluded,
		"ingress_nodes":       backup.Included,
	} {
		if got, ok := backup.Tables[table]; !ok || got != want {
			t.Errorf("backup.Tables[%q] = %v (classified: %v), want %v", table, got, ok, want)
		}
	}
}
