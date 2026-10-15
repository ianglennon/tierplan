// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

func migratedTables(t *testing.T) []string {
	t.Helper()
	s := storetest.New(t)
	rows, err := s.Read.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// TestEveryTableIsClassifiedForBackups fails, naming the table, when a migration
// adds a table nobody decided about: a backup would silently omit it or carry
// something it should not (D-29, T-03-12).
func TestEveryTableIsClassifiedForBackups(t *testing.T) {
	tables := migratedTables(t)
	for _, name := range tables {
		if strings.HasPrefix(name, "sqlite_") && name != "sqlite_sequence" {
			continue // SQLite's own bookkeeping
		}
		if _, ok := backup.Tables[name]; !ok {
			t.Errorf("table %q is in the migrated schema but not classified in backup.Tables: "+
				"decide whether a backup carries it", name)
		}
	}
	for name := range backup.Tables {
		if !slices.Contains(tables, name) {
			t.Errorf("backup.Tables classifies %q, which no migration creates (renamed or dropped?)", name)
		}
	}
}

func TestTheTablesThatMustNeverTravelAreExcludedAndTheRestIncluded(t *testing.T) {
	for _, name := range []string{"sessions", "user_links", "env_join_tokens", "backup_runs"} {
		if got, ok := backup.Tables[name]; !ok || got != backup.Excluded {
			t.Errorf("%s = %v (present %v), want Excluded", name, got, ok)
		}
	}
	for _, name := range []string{
		"users", "teams", "team_members", "role_assignments", "assignment_extras", "environments", "env_cas",
		"agents", "agent_blocks", "settings", "audit_log", "owned_resources", "user_recent_environments", "user_shell_prefs",
		"roles", "backup_schedule", "registry_credentials", "restart_loop_mutes",
	} {
		if got, ok := backup.Tables[name]; !ok || got != backup.Included {
			t.Errorf("%s = %v (present %v), want Included", name, got, ok)
		}
	}
}

// TestEverySealedColumnIsRegisteredForRewrap fails, naming the column, when a
// table gains a *_sealed column that vault.SealedColumns does not list: a backup
// or a key rotation would leave that secret sealed under the old key (D-28, T-03-11).
func TestEverySealedColumnIsRegisteredForRewrap(t *testing.T) {
	s := storetest.New(t)
	registered := map[string]bool{}
	for _, sc := range vault.SealedColumns {
		registered[sc.Table+"."+sc.Column] = true
	}
	found := map[string]bool{}
	for _, table := range migratedTables(t) {
		rows, err := s.Read.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			if strings.HasSuffix(col, "_sealed") {
				found[table+"."+col] = true
				if !registered[table+"."+col] {
					t.Errorf("column %s.%s holds sealed values but is not listed in vault.SealedColumns", table, col)
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	for key := range registered {
		if !found[key] {
			t.Errorf("vault.SealedColumns lists %s, which is not a column of the migrated schema", key)
		}
	}
}
