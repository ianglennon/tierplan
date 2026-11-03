// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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

	// Git connections (Phase 9): credentials and the repositories that use them. The credential's
	// token or private key is sealed under the credential's ID and registered in
	// vault.SealedColumns, so RewrapAll moves it with the rest. A repository holds an address, a
	// public CA bundle and the host key a person trusted; no secret.
	"git_credentials": Included, // secret_sealed is re-wrapped by RewrapAll
	"git_repos":       Included,

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

	// Git-backed stacks (Phase 9): a stack's repository, ref, folder and sync settings travel with it; the
	// webhook secret is sealed under the stack's ID and registered in vault.SealedColumns. The state of the
	// last check is derived, so a restored instance checks again rather than trusting a stale answer.
	"stack_git":       Included, // webhook_secret_sealed is re-wrapped by RewrapAll
	"stack_git_state": Excluded, // derived state, re-checked after a restore

	// Networks and address pools (Phase 10): when TierPlan first saw each network and last saw it in use
	// (the orphan check's 24-hour grace clock, which the Phase 11 janitor reads too). It belongs to its
	// environment and holds no secret.
	"network_seen": Included,

	// What a person asked the janitor and the orphan check never to remove or list (Phase 11, D-12):
	// networks, containers and images in one table, which replaced the Phase 10 network_keeps table. An
	// older backup is migrated forward on restore, so its network keeps arrive here. It belongs to its
	// environment and holds no secret.
	"janitor_keeps": Included,

	// When the janitor first saw each image without a name (Phase 11, D-07), which its image grace period is
	// measured from. It is derived from what the engines show: a restored instance starts every clock again,
	// which can only delay a removal by one grace period, never bring one forward.
	"image_dangling_seen": Excluded,

	// The janitor's record (Phase 11): each run and every item it removed or failed to remove, so
	// what was cleaned up and by whom survives a restore. Both belong to their environment and hold
	// no secret.
	"janitor_runs":      Included,
	"janitor_run_items": Included,
	// The crash evidence saved before a crashed container was removed: why it stopped and the masked, capped tail
	// of its log. It belongs to its run and goes with it.
	"janitor_evidence": Included,

	// The janitor's policy (Phase 11): the instance defaults (one seeded row), each environment's
	// overrides and each swarm node's. A NULL field follows the level above, so nothing here is a
	// copy of a default. No secret.
	"janitor_defaults":    Included,
	"janitor_env_policy":  Included,
	"janitor_node_policy": Included,

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
