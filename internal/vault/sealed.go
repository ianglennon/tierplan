// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SealedColumn names one database column that holds vault envelopes, and how to
// rebuild the context each value was sealed with. Re-wrapping (backup export and
// restore, master-key rotation) walks SealedColumns, so a secret column that is
// not listed would stay sealed under the old key. internal/backup's guard test
// fails the build when a column named *_sealed is missing from this list (D-28).
type SealedColumn struct {
	Table, Column string
	// KeyColumns identify the row; their values (as text) are passed to Context.
	KeyColumns []string
	// Context returns the context the row's value was sealed with.
	Context func(keys []string) string
}

// SealedColumns is every column that holds a vault envelope.
var SealedColumns = []SealedColumn{
	{
		Table: "env_cas", Column: "key_sealed", KeyColumns: []string{"env_id"},
		Context: func(keys []string) string { return ContextAgentCA + keys[0] },
	},
	{
		Table: "backup_schedule", Column: "passphrase_sealed", KeyColumns: []string{"id"},
		Context: func([]string) string { return ContextBackupSchedule },
	},
	{
		Table: "registry_credentials", Column: "secret_sealed", KeyColumns: []string{"id"},
		Context: func(keys []string) string { return ContextRegistryCred + keys[0] },
	},
	{
		Table: "git_credentials", Column: "secret_sealed", KeyColumns: []string{"id"},
		Context: func(keys []string) string { return ContextGitCred + keys[0] },
	},
	{
		Table: "stack_snapshot_vars", Column: "value_sealed", KeyColumns: []string{"snapshot_id", "name"},
		Context: func(keys []string) string { return ContextStackVar + keys[0] + "/" + keys[1] },
	},
	{
		Table: "stack_snapshot_files", Column: "content_sealed", KeyColumns: []string{"snapshot_id", "path"},
		Context: func(keys []string) string { return ContextStackFile + keys[0] + "/" + keys[1] },
	},
	{
		Table: "stack_git", Column: "webhook_secret_sealed", KeyColumns: []string{"stack_id"},
		Context: func(keys []string) string { return ContextGitWebhook + keys[0] },
	},
	{
		Table: "tls_certs", Column: "key_sealed", KeyColumns: []string{"id"},
		Context: func(keys []string) string { return ContextTLSKey + keys[0] },
	},
	{
		Table: "acme_account", Column: "key_sealed", KeyColumns: []string{"directory_url"},
		Context: func(keys []string) string { return ContextACMEAccountKey + keys[0] },
	},
	{
		Table: "acme_orders", Column: "cert_key_sealed", KeyColumns: []string{"id"},
		Context: func(keys []string) string { return ContextACMEOrderKey + keys[0] },
	},
	{
		Table: "acme_dns_credentials", Column: "token_sealed", KeyColumns: []string{"provider"},
		Context: func(keys []string) string { return ContextACMEDNSToken + keys[0] },
	},
}

// RewrapAll moves every sealed value in tx from the vault from to the vault to:
// each is opened with from under its own context and sealed again with to's
// active key under the same context. Empty values (a column that holds no secret
// yet) are skipped. It returns how many values it re-wrapped. Any value that does
// not open aborts with an error naming the table and column; the caller rolls
// the transaction back, so a partial re-wrap is never committed.
func RewrapAll(ctx context.Context, tx *sql.Tx, from, to *Vault) (int, error) {
	total := 0
	for _, sc := range SealedColumns {
		n, err := rewrapColumn(ctx, tx, sc, from, to)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

type sealedRow struct {
	keys   []any
	sealed string
}

func rewrapColumn(ctx context.Context, tx *sql.Tx, sc SealedColumn, from, to *Vault) (int, error) {
	cols := append(append([]string{}, sc.KeyColumns...), sc.Column)
	query := "SELECT " + strings.Join(cols, ", ") + " FROM " + sc.Table + //nolint:gosec // identifiers come from the static registry
		" WHERE " + sc.Column + " <> ''"
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("vault: read %s.%s: %w", sc.Table, sc.Column, err)
	}
	var pending []sealedRow
	for rows.Next() {
		r := sealedRow{keys: make([]any, len(sc.KeyColumns))}
		dest := make([]any, 0, len(cols))
		for i := range r.keys {
			dest = append(dest, &r.keys[i])
		}
		dest = append(dest, &r.sealed)
		if err := rows.Scan(dest...); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("vault: read %s.%s: %w", sc.Table, sc.Column, err)
		}
		pending = append(pending, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("vault: read %s.%s: %w", sc.Table, sc.Column, err)
	}

	where := make([]string, len(sc.KeyColumns))
	for i, k := range sc.KeyColumns {
		where[i] = k + " = ?"
	}
	update := "UPDATE " + sc.Table + " SET " + sc.Column + " = ? WHERE " + strings.Join(where, " AND ") //nolint:gosec // identifiers come from the static registry

	for _, r := range pending {
		keys := make([]string, len(r.keys))
		for i, k := range r.keys {
			if b, ok := k.([]byte); ok {
				k = string(b)
			}
			keys[i] = fmt.Sprint(k)
		}
		sealCtx := sc.Context(keys)
		plain, err := from.Open(sealCtx, r.sealed)
		if err != nil {
			return 0, fmt.Errorf("vault: open %s.%s of row %s: %w", sc.Table, sc.Column, strings.Join(keys, "/"), err)
		}
		resealed, err := to.Seal(sealCtx, plain)
		if err != nil {
			return 0, fmt.Errorf("vault: seal %s.%s of row %s: %w", sc.Table, sc.Column, strings.Join(keys, "/"), err)
		}
		args := append([]any{resealed}, r.keys...)
		if _, err := tx.ExecContext(ctx, update, args...); err != nil {
			return 0, fmt.Errorf("vault: write %s.%s of row %s: %w", sc.Table, sc.Column, strings.Join(keys, "/"), err)
		}
	}
	return len(pending), nil
}
