// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/vault"
)

// chainOf20 writes 20 entries under the fixture's vault, including one
// audit.pruned anchor produced by a real prune.
func (f *fixture) chainOf20(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i := range 4 {
		f.clk.Set(t0.Add(-time.Duration(40-i) * 24 * time.Hour))
		if err := f.w.Record(ctx, ent("test.old")); err != nil {
			t.Fatal(err)
		}
	}
	f.record(t, 15)
	f.clk.Set(t0)
	if n, err := audit.Prune(ctx, f.w, 30); err != nil || n != 4 {
		t.Fatalf("Prune = %d, %v; want 4", n, err)
	}
	for i := range 4 {
		f.clk.Set(t0.Add(time.Hour + time.Duration(i)*time.Second))
		if err := f.w.Record(ctx, ent("test.late")); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.count(t); got != 20 {
		t.Fatalf("chain has %d entries, want 20", got)
	}
}

func (f *fixture) newestHMAC(t *testing.T) []byte {
	t.Helper()
	var mac []byte
	if err := f.s.Read.QueryRow("SELECT hmac FROM audit_log ORDER BY seq DESC LIMIT 1").Scan(&mac); err != nil {
		t.Fatal(err)
	}
	return mac
}

func verifyWith(t *testing.T, f *fixture, v *vault.Vault) audit.VerifyResult {
	t.Helper()
	res, err := audit.Verify(context.Background(), f.s, v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return res
}

// rekey re-keys the fixture's chain under to in one committed transaction.
func (f *fixture) rekey(t *testing.T, to *vault.Vault) audit.RekeyResult {
	t.Helper()
	ctx := context.Background()
	tx, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := audit.Rekey(ctx, tx, f.s.Q, to)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("Rekey: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRekeyResignsTheWholeChainSoItVerifiesUnderTheTargetKeyAlone(t *testing.T) {
	f := newFixture(t)
	f.chainOf20(t)
	b := newVault(t, 9)

	before := verifyWith(t, f, b)
	if before.Broken == nil || !strings.Contains(before.Broken.Reason, "unknown key id") {
		t.Fatalf("verify under B before Rekey = %+v, want an unknown key id break", before.Broken)
	}
	oldHead := f.newestHMAC(t)

	res := f.rekey(t, b)
	if res.Entries != 20 || hex.EncodeToString(res.OldHead) != hex.EncodeToString(oldHead) {
		t.Fatalf("RekeyResult = %d entries, head %x; want 20, %x", res.Entries, res.OldHead, oldHead)
	}
	if len(res.OldKIDs) != 1 || res.OldKIDs[0] != f.v.KeyID() {
		t.Fatalf("OldKIDs = %v, want [%s]", res.OldKIDs, f.v.KeyID())
	}

	var n int
	if err := f.s.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE key_id = ?`, b.KeyID()).Scan(&n); err != nil || n != 20 {
		t.Fatalf("%d rows carry key id B, want 20 (%v)", n, err)
	}
	after := verifyWith(t, f, b)
	if after.Broken != nil || after.Entries != 20 {
		t.Fatalf("verify under B alone after Rekey = %+v", after)
	}
	if got := verifyWith(t, f, f.v); got.Broken == nil {
		t.Fatal("the old key still verifies a re-keyed chain")
	}
}

func TestRekeyKeepsTheFirstEntrysPredecessorLink(t *testing.T) {
	f := newFixture(t)
	f.chainOf20(t)
	var prev []byte
	if err := f.s.Read.QueryRow(`SELECT prev_hmac FROM audit_log ORDER BY seq LIMIT 1`).Scan(&prev); err != nil {
		t.Fatal(err)
	}
	f.rekey(t, newVault(t, 9))
	var got []byte
	if err := f.s.Read.QueryRow(`SELECT prev_hmac FROM audit_log ORDER BY seq LIMIT 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(prev) {
		t.Fatalf("first prev_hmac changed from %x to %x", prev, got)
	}
}

func TestAppendReanchorTxRecordsWhereTheChainCameFromAndStillVerifies(t *testing.T) {
	f := newFixture(t)
	f.chainOf20(t)
	b := newVault(t, 9)
	ctx := context.Background()
	oldHead := f.newestHMAC(t)

	tx, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := audit.Rekey(ctx, tx, f.s.Q, b)
	if err != nil {
		t.Fatal(err)
	}
	w := audit.NewWriter(f.s, b, slog.New(slog.DiscardHandler), f.clk.Now)
	if err := w.AppendReanchorTx(ctx, tx, res, "instance-x", "2026-10-01T00:00:00Z", true); err != nil {
		_ = tx.Rollback()
		t.Fatalf("AppendReanchorTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var actor, action, details string
	if err := f.s.Read.QueryRow(`SELECT actor_id, action, details FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&actor, &action, &details); err != nil {
		t.Fatal(err)
	}
	if actor != "system:restore" || action != audit.ActionAuditReanchored {
		t.Fatalf("last entry = %s by %s", action, actor)
	}
	for _, want := range []string{
		`"source_instance":"instance-x"`, `"backup_created":"2026-10-01T00:00:00Z"`,
		`"old_kid":"` + f.v.KeyID() + `"`, `"old_head":"` + hex.EncodeToString(oldHead) + `"`,
		`"entries":"20"`, `"chain_verified":"true"`,
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details %s lack %s", details, want)
		}
	}
	if got := verifyWith(t, f, b); got.Broken != nil || got.Entries != 21 {
		t.Fatalf("verify after reanchor = %+v", got)
	}
}

func TestRekeyInsideARolledBackTransactionChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.chainOf20(t)
	ctx := context.Background()

	tx, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Rekey(ctx, tx, f.s.Q, newVault(t, 9)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := verifyWith(t, f, f.v); got.Broken != nil || got.Entries != 20 {
		t.Fatalf("verify under A after a rolled-back Rekey = %+v", got)
	}
}

func TestRekeyOfAnEmptyLogIsANoOp(t *testing.T) {
	f := newFixture(t)
	res := f.rekey(t, newVault(t, 9))
	if res.Entries != 0 || len(res.OldHead) != 0 || len(res.OldKIDs) != 0 {
		t.Fatalf("RekeyResult of an empty log = %+v", res)
	}
}

// rotate re-keys the fixture's chain under b and records the key.rotated entry
// with an attestation made by the old key, in one committed transaction.
func (f *fixture) rotate(t *testing.T, b *vault.Vault) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := audit.Rekey(ctx, tx, f.s.Q, b)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	w := audit.NewWriter(f.s, b, slog.New(slog.DiscardHandler), f.clk.Now)
	if err := w.RecordRotationTx(ctx, tx, f.v.Subkey(vault.LabelAuditHMAC), f.v.KeyID(), res, audit.SystemCLI); err != nil {
		_ = tx.Rollback()
		t.Fatalf("RecordRotationTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// bothKeys is a vault whose active key is b that can also read the fixture's key.
func (f *fixture) bothKeys(t *testing.T, b *vault.Vault, oldKey []byte) *vault.Vault {
	t.Helper()
	if _, err := b.AddKey(oldKey); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestKeyRotatedAttestationIsCheckedWhenTheOldKeyIsLoaded(t *testing.T) {
	f := newFixture(t)
	f.record(t, 5)
	b := newVault(t, 9)
	f.rotate(t, b)

	var details string
	if err := f.s.Read.QueryRow(`SELECT details FROM audit_log WHERE action = ?`, audit.ActionKeyRotated).Scan(&details); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"old_kid", "new_kid", "old_head", "entries", "attestation"} {
		if !strings.Contains(details, `"`+k+`"`) {
			t.Errorf("key.rotated details %s lack %s", details, k)
		}
	}

	res := verifyWith(t, f, f.bothKeys(t, b, bytes.Repeat([]byte{5}, 32)))
	if res.Broken != nil || res.AttestationsUnchecked != 0 || res.Entries != 6 {
		t.Fatalf("verify with both keys = %+v (broken %+v)", res, res.Broken)
	}
}

func TestAWrongRotationAttestationIsReportedAsBrokenAtItsSeq(t *testing.T) {
	f := newFixture(t)
	f.record(t, 5)
	b := newVault(t, 9)
	f.rotate(t, b)

	var seq int64
	var att string
	if err := f.s.Read.QueryRow(`SELECT seq, json_extract(details, '$.attestation') FROM audit_log WHERE action = ?`,
		audit.ActionKeyRotated).Scan(&seq, &att); err != nil {
		t.Fatal(err)
	}
	flipped := "0"
	if att[0] == '0' {
		flipped = "1"
	}
	f.exec(t, `UPDATE audit_log SET details = json_set(details, '$.attestation', ?) WHERE seq = ?`, flipped+att[1:], seq)
	// Re-sign the whole chain under B so only the attestation is wrong.
	f.rekey(t, b)

	res := verifyWith(t, f, f.bothKeys(t, b, bytes.Repeat([]byte{5}, 32)))
	if res.Broken == nil || res.Broken.Seq != seq || !strings.Contains(res.Broken.Reason, "rotation attestation mismatch") {
		t.Fatalf("verify = %+v (broken %+v), want a rotation attestation mismatch at seq %d", res, res.Broken, seq)
	}
}

func TestRotationAttestationIsCountedAsNotCheckedWhenTheOldKeyIsAbsent(t *testing.T) {
	f := newFixture(t)
	f.record(t, 5)
	b := newVault(t, 9)
	f.rotate(t, b)

	res := verifyWith(t, f, b)
	if res.Broken != nil || res.AttestationsUnchecked != 1 {
		t.Fatalf("verify with the new key alone = %+v (broken %+v), want intact and 1 unchecked", res, res.Broken)
	}
}

func TestRotationAttestationBindsEveryField(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	base := audit.RotationAttestation(key, "k1-a", "k1-b", []byte("head"), "id-1", 7)
	if len(base) != 64 {
		t.Fatalf("attestation = %q, want 64 hex characters", base)
	}
	for name, got := range map[string]string{
		"old kid":  audit.RotationAttestation(key, "k1-x", "k1-b", []byte("head"), "id-1", 7),
		"new kid":  audit.RotationAttestation(key, "k1-a", "k1-x", []byte("head"), "id-1", 7),
		"old head": audit.RotationAttestation(key, "k1-a", "k1-b", []byte("hexd"), "id-1", 7),
		"entry id": audit.RotationAttestation(key, "k1-a", "k1-b", []byte("head"), "id-2", 7),
		"time":     audit.RotationAttestation(key, "k1-a", "k1-b", []byte("head"), "id-1", 8),
		"key":      audit.RotationAttestation(bytes.Repeat([]byte{2}, 32), "k1-a", "k1-b", []byte("head"), "id-1", 7),
	} {
		if got == base {
			t.Errorf("changing the %s did not change the attestation", name)
		}
	}
	if again := audit.RotationAttestation(key, "k1-a", "k1-b", []byte("head"), "id-1", 7); again != base {
		t.Error("the attestation is not deterministic")
	}
}
