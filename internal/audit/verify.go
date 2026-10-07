// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

// verifyBatch is how many rows Verify reads per query, so a large log is
// streamed rather than loaded whole.
const verifyBatch = 1000

// Broken names the first entry that fails verification.
type Broken struct {
	Seq    int64
	ID     string
	Reason string
}

// VerifyResult is the outcome of Verify. Entries counts every entry examined,
// including anchors; Broken is nil when the chain is intact.
// AttestationsUnchecked counts key.rotated entries whose attestation could not be
// checked because the key that made it is not loaded: that is not a break, but it
// is not a pass either (D-33).
type VerifyResult struct {
	Entries               int
	Broken                *Broken
	AttestationsUnchecked int
}

// Verify walks the chain in sequence order (never timestamp order, because
// entries can share a nanosecond) and names the lowest-sequence entry that
// fails (D-42):
//
//   - its HMAC must recompute under the key named by its key_id;
//   - its prev_hmac must equal its predecessor's stored HMAC;
//   - the first remaining entry's prev_hmac must be the 32-byte genesis value,
//     or the last_pruned_hmac of a retained audit.pruned anchor;
//   - a key.rotated entry's attestation must recompute under the old key it
//     names, when this vault holds that key; otherwise it is counted, not failed.
//
// Truncating the newest entries cannot be detected without an external
// anchor; that residual risk is documented in docs/threat-model.md.
func Verify(ctx context.Context, s *store.Store, v *vault.Vault) (VerifyResult, error) {
	var (
		res        VerifyResult
		prevMAC    []byte
		afterSeq   int64
		first      *sqlc.AuditLog
		anchors    = map[string]bool{}
		keyForID   = map[string][]byte{}
		unknownKey = map[string]bool{}
	)
	setBroken := func(row sqlc.AuditLog, reason string) {
		if res.Broken == nil {
			res.Broken = &Broken{Seq: row.Seq, ID: row.ID, Reason: reason}
		}
	}

	for {
		rows, err := s.RQ.ListAuditForVerify(ctx, sqlc.ListAuditForVerifyParams{Seq: afterSeq, Limit: verifyBatch})
		if err != nil {
			return res, fmt.Errorf("read audit log: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			afterSeq = row.Seq
			res.Entries++
			if first == nil {
				r := row
				first = &r
			}

			key, ok := keyForID[row.KeyID]
			if !ok && !unknownKey[row.KeyID] {
				k, err := v.SubkeyFor(row.KeyID, vault.LabelAuditHMAC)
				switch {
				case err == nil:
					key = k
					keyForID[row.KeyID] = k
				case errors.Is(err, vault.ErrUnknownKey):
					unknownKey[row.KeyID] = true
				default:
					return res, err
				}
			}
			if key == nil {
				setBroken(row, fmt.Sprintf("unknown key id %s: this vault does not hold the key that signed the entry "+
					"(check BAYPLAN_ENCRYPTION_KEY or %s)", row.KeyID, vault.KeyFile))
				prevMAC = row.Hmac
				continue
			}

			details, derr := parseDetails(row.Details)
			rec := record{
				ID: row.ID, TS: row.Ts,
				ActorType: row.ActorType, ActorID: row.ActorID, ActorName: row.ActorName,
				Action: row.Action, TargetType: row.TargetType, TargetID: row.TargetID, EnvID: row.EnvID.String,
				Result: row.Result, Reason: row.Reason, IP: row.Ip, UserAgent: row.UserAgent,
				Details: details, KeyID: row.KeyID,
			}
			switch {
			case derr != nil:
				setBroken(row, "details are not a JSON object of strings")
			case !hmac.Equal(computeHMAC(key, row.PrevHmac, rec), row.Hmac):
				setBroken(row, "hmac mismatch")
			default:
				if row.Action == ActionAuditPruned {
					if last := details["last_pruned_hmac"]; last != "" {
						anchors[last] = true
					}
				}
				if prevMAC != nil && !bytes.Equal(row.PrevHmac, prevMAC) {
					setBroken(row, "prev link mismatch: the entry before this one is missing or was altered")
				}
				if row.Action == ActionKeyRotated {
					switch checkAttestation(v, row, details) {
					case attestationMismatch:
						setBroken(row, "rotation attestation mismatch: the entry does not carry a valid attestation from the old key")
					case attestationUnchecked:
						res.AttestationsUnchecked++
					case attestationOK:
					}
				}
			}
			prevMAC = row.Hmac
		}
	}

	// The first remaining entry must start the chain or follow a pruned prefix.
	// The anchors that account for a pruned prefix come later in the log, so
	// this check can only run after the walk. The first entry has the lowest
	// sequence number, so it wins over any break found above.
	if first != nil && !bytes.Equal(first.PrevHmac, make([]byte, genesisLen)) && !anchors[hex.EncodeToString(first.PrevHmac)] {
		if res.Broken == nil || res.Broken.Seq > first.Seq {
			res.Broken = &Broken{Seq: first.Seq, ID: first.ID, Reason: "prev link mismatch: the entries before this one " +
				"are missing and no audit.pruned anchor accounts for them"}
		}
	}
	return res, nil
}

type attestationOutcome int

const (
	attestationOK attestationOutcome = iota
	attestationUnchecked
	attestationMismatch
)

// checkAttestation recomputes a key.rotated row's attestation with the old key it
// names. A row with no old key ID or attestation cannot have been written by
// RecordRotationTx and is a mismatch.
func checkAttestation(v *vault.Vault, row sqlc.AuditLog, details map[string]string) attestationOutcome {
	oldKID, newKID, head, att := details["old_kid"], details["new_kid"], details["old_head"], details["attestation"]
	if oldKID == "" || att == "" {
		return attestationMismatch
	}
	oldKey, err := v.SubkeyFor(oldKID, vault.LabelAuditHMAC)
	if err != nil {
		return attestationUnchecked
	}
	oldHead, err := hex.DecodeString(head)
	if err != nil {
		return attestationMismatch
	}
	want := RotationAttestation(oldKey, oldKID, newKID, oldHead, row.ID, row.Ts)
	if !hmac.Equal([]byte(want), []byte(att)) {
		return attestationMismatch
	}
	return attestationOK
}

func parseDetails(s string) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}
