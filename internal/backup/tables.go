// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

// Class says what a backup does with one database table (D-29).
type Class int

// Table classes.
const (
	// Included tables travel in a backup as they are, after sealed values are
	// re-wrapped and the audit chain is re-signed.
	Included Class = iota + 1
	// Excluded tables are never in a backup: they hold credentials that must
	// not leave the instance or state that only means something on it.
	Excluded
	// Special tables are written by the export and restore code itself, not
	// copied row by row.
	Special
)

// String names the class for test and log output.
func (c Class) String() string {
	switch c {
	case Included:
		return "included"
	case Excluded:
		return "excluded"
	case Special:
		return "special"
	default:
		return "unclassified"
	}
}

// Tables classifies every table of the migrated schema. A table missing here, or
// listed here but gone from the schema, fails TestEveryTableIsClassifiedForBackups,
// so a migration cannot add a table that a backup silently omits (T-03-12).
var Tables = map[string]Class{
	// Identity and access.
	"users":                    Included,
	"roles":                    Included,
	"role_assignments":         Included,
	"assignment_extras":        Included,
	"teams":                    Included,
	"team_members":             Included,
	"user_recent_environments": Included,
	"user_shell_prefs":         Included, // how each user likes to open a shell in an image; no secrets
	"owned_resources":          Included,

	// Environments and their agents.
	"environments": Included,
	"env_cas":      Included, // CA keys are sealed; RewrapAll moves them to the restoring instance's key
	"agents":       Included,
	"agent_blocks": Included,

	// Instance settings, the audit log and the backup schedule.
	"settings":        Included,
	"audit_log":       Included, // re-signed under the restoring instance's key
	"backup_schedule": Included, // the passphrase is sealed; RewrapAll moves it

	// Registry credentials (Phase 5): the secret column is sealed under the credential's ID and
	// registered in vault.SealedColumns, so RewrapAll moves it with the rest.
	"registry_credentials": Included,

	// Restart-loop mutes (Phase 6): which flagged services and containers a user chose to stop
	// being told about. They belong to their environment and hold no secret; the thresholds are
	// columns of environments and travel with it.
	"restart_loop_mutes": Included,

	// Compose stacks (Phase 7): the stacks, their draft and revision snapshots, the files and
	// variables of each snapshot and the deploy history. Secret variables and secret files are
	// sealed under row-bound contexts and registered in vault.SealedColumns, so RewrapAll moves
	// their sealed values with the rest.
	"stacks":               Included,
	"stack_snapshots":      Included,
	"stack_snapshot_files": Included, // content_sealed is re-wrapped by RewrapAll
	"stack_snapshot_vars":  Included, // value_sealed is re-wrapped by RewrapAll
	"stack_deploys":        Included,

	// Credentials and state that must not travel: a restored instance starts with
	// no sessions, no outstanding invite or reset links, no agent join tokens and
	// an empty run history.
	"sessions":        Excluded,
	"user_links":      Excluded,
	"env_join_tokens": Excluded,
	"backup_runs":     Excluded,

	// Bookkeeping written by the export and restore code itself.
	"app_meta":         Special, // instance identity and the audit-prune anchor; carried by hand
	"goose_db_version": Special, // the schema version; the backup records it in its header
	"sqlite_sequence":  Special, // SQLite's AUTOINCREMENT counters; present only if a table uses AUTOINCREMENT
}
