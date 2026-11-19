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

// An exported backup holds the node row, so a restored server still recognises the node, and holds no
// join token.
func TestExportCarriesIngressNodesAndNoIngressJoinTokens(t *testing.T) {
	f := newExportFixture(t)
	f.exec(t, `INSERT INTO ingress_nodes (id, env_id, name, swarm_node_id, public_key, status, created_at)
		VALUES ('inode-1', 'env-1', 'edge-a', NULL, ?, 'active', 1)`, make([]byte, 32))
	f.exec(t, `INSERT INTO ingress_join_tokens (id, env_id, token_hash, generation, created_at, created_by)
		VALUES ('ij1', 'env-1', ?, 1, 1, 'u1')`, make([]byte, 32))
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	o := openBackup(t, file, exportPass)
	if n := o.count(t, "ingress_nodes"); n != 1 {
		t.Errorf("the backup holds %d ingress_nodes rows, want 1", n)
	}
	if n := o.count(t, "ingress_join_tokens"); n != 0 {
		t.Errorf("the backup holds %d ingress_join_tokens rows, want 0", n)
	}
}
