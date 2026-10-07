// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

// rekeyBatch is how many rows Rekey reads per query.
const rekeyBatch = 1000

// RekeyResult is what Rekey changed: how many entries it re-signed, the HMAC the
// chain ended on before re-keying, and the key IDs that had signed it. A restore
// and a master-key rotation both record these in the entry that vouches for the
// change.
type RekeyResult struct {
	Entries int
	OldHead []byte
	OldKIDs []string
}

// Rekey re-signs every audit entry under the vault to's active key, inside the
// caller's write transaction (D-29, D-33). The first entry keeps its stored
// predecessor link, so a log whose prefix was pruned still starts at the same
// anchor, and every later entry chains from the entry before it as just
// re-signed. Entries are walked in sequence order, never timestamp order.
//
// Re-keying does not authenticate the chain it is given: the caller verifies
// first when it wants to know (restore records the outcome in audit.reanchored).
// An error leaves the transaction to be rolled back by the caller; nothing is
// changed outside it.
func Rekey(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, to *vault.Vault) (RekeyResult, error) {
	q = q.WithTx(tx)
	key := to.Subkey(vault.LabelAuditHMAC)
	var (
		res      RekeyResult
		prev     []byte
		afterSeq int64
		kids     = map[string]bool{}
	)
	for {
		rows, err := q.ListAuditChain(ctx, sqlc.ListAuditChainParams{Seq: afterSeq, Limit: rekeyBatch})
		if err != nil {
			return res, fmt.Errorf("audit rekey: read: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			afterSeq = row.Seq
			rec, err := rowRecord(row)
			if err != nil {
				return res, fmt.Errorf("audit rekey: entry %d: %w", row.Seq, err)
			}
			if res.Entries == 0 {
				prev = row.PrevHmac
			}
			kids[row.KeyID] = true
			res.OldHead = row.Hmac
			rec.KeyID = to.KeyID()
			mac := computeHMAC(key, prev, rec)
			if err := q.UpdateAuditChain(ctx, sqlc.UpdateAuditChainParams{
				KeyID: rec.KeyID, PrevHmac: prev, Hmac: mac, Seq: row.Seq,
			}); err != nil {
				return res, fmt.Errorf("audit rekey: entry %d: %w", row.Seq, err)
			}
			prev = mac
			res.Entries++
		}
	}
	for kid := range kids {
		res.OldKIDs = append(res.OldKIDs, kid)
	}
	slices.Sort(res.OldKIDs)
	return res, nil
}

// AppendReanchorTx records the audit.reanchored entry that follows a Rekey, as
// system:restore, inside tx. The Writer must be built over the store being
// re-keyed with the target vault. The entry's details say where the chain came
// from and how it stood before re-keying: old_head is the hex HMAC it ended on
// and chain_verified whether it verified under its own key first (D-29).
func (w *Writer) AppendReanchorTx(ctx context.Context, tx *sql.Tx, res RekeyResult, sourceInstance, backupCreated string, chainVerified bool) error {
	return w.RecordTx(ctx, tx, Entry{
		Actor:      SystemRestore,
		Action:     ActionAuditReanchored,
		TargetType: "audit_log",
		Result:     ResultSuccess,
		Details: map[string]string{
			"source_instance": sourceInstance,
			"backup_created":  backupCreated,
			"old_kid":         strings.Join(res.OldKIDs, ","),
			"old_head":        hex.EncodeToString(res.OldHead),
			"entries":         strconv.Itoa(res.Entries),
			"chain_verified":  strconv.FormatBool(chainVerified),
		},
	})
}

// rowRecord rebuilds the record a stored row was signed as.
func rowRecord(row sqlc.AuditLog) (record, error) {
	details, err := parseDetails(row.Details)
	if err != nil {
		return record{}, fmt.Errorf("details are not a JSON object of strings: %w", err)
	}
	return record{
		ID: row.ID, TS: row.Ts,
		ActorType: row.ActorType, ActorID: row.ActorID, ActorName: row.ActorName,
		Action: row.Action, TargetType: row.TargetType, TargetID: row.TargetID, EnvID: row.EnvID.String,
		Result: row.Result, Reason: row.Reason, IP: row.Ip, UserAgent: row.UserAgent,
		Details: details, KeyID: row.KeyID,
	}, nil
}
