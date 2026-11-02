// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package audit_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func newVault(t *testing.T, fill byte) *vault.Vault {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
	v, err := vault.LoadOrCreate(key, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type fixture struct {
	s   *store.Store
	v   *vault.Vault
	w   *audit.Writer
	clk *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := storetest.New(t)
	v := newVault(t, 5)
	clk := &clock{t: t0}
	return &fixture{s: s, v: v, clk: clk,
		w: audit.NewWriter(s, v, slog.New(slog.DiscardHandler), clk.Now)}
}

func ent(action string) audit.Entry {
	return audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "alice"},
		Action: action, TargetType: "session", TargetID: "s-1", EnvID: "env-1",
		Result: audit.ResultSuccess, Reason: "because", IP: "203.0.113.9", UserAgent: "test/1.0",
		Details: map[string]string{"k": "v"},
	}
}

// record appends n entries, one second apart.
func (f *fixture) record(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		f.clk.Set(t0.Add(time.Duration(i) * time.Second))
		if err := f.w.Record(context.Background(), ent(fmt.Sprintf("test.action%d", i))); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
}

func (f *fixture) verify(t *testing.T) audit.VerifyResult {
	t.Helper()
	res, err := audit.Verify(context.Background(), f.s, f.v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return res
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.s.Write.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *fixture) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.Read.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM audit_log").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFirstEntryChainsFromZeroGenesis(t *testing.T) {
	f := newFixture(t)
	f.clk.Set(t0)
	if err := f.w.Record(context.Background(), ent(audit.ActionAuthLogin)); err != nil {
		t.Fatal(err)
	}
	var (
		id, keyID, actorType, ip string
		ts                       int64
		prev, mac                []byte
	)
	err := f.s.Read.QueryRow(`SELECT id, ts, key_id, prev_hmac, hmac, actor_type, ip FROM audit_log WHERE seq = 1`).
		Scan(&id, &ts, &keyID, &prev, &mac, &actorType, &ip)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prev, make([]byte, 32)) {
		t.Fatalf("first prev_hmac = %x, want 32 zero bytes", prev)
	}
	if keyID != f.v.KeyID() {
		t.Fatalf("key_id = %q, want %q", keyID, f.v.KeyID())
	}
	if len(mac) != 32 {
		t.Fatalf("hmac length = %d", len(mac))
	}
	if ts != t0.UnixNano() {
		t.Fatalf("ts = %d, want %d", ts, t0.UnixNano())
	}
	if u, err := uuid.Parse(id); err != nil || u.Version() != 7 {
		t.Fatalf("id %q is not a UUIDv7: %v", id, err)
	}
	if actorType != "user" || ip != "203.0.113.9" {
		t.Fatalf("stored fields wrong: %s %s", actorType, ip)
	}

	f.clk.Set(t0.Add(time.Second))
	if err := f.w.Record(context.Background(), ent(audit.ActionAuthLogout)); err != nil {
		t.Fatal(err)
	}
	var prev2 []byte
	if err := f.s.Read.QueryRow(`SELECT prev_hmac FROM audit_log WHERE seq = 2`).Scan(&prev2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prev2, mac) {
		t.Fatal("second entry does not chain from the first entry's hmac")
	}
}

func TestVerifyEmptyLog(t *testing.T) {
	f := newFixture(t)
	res := f.verify(t)
	if res.Entries != 0 || res.Broken != nil {
		t.Fatalf("empty log: %+v", res)
	}
}

func TestSameNanosecondEntriesVerifyInSequenceOrder(t *testing.T) {
	f := newFixture(t)
	f.clk.Set(t0)
	for i := range 5 {
		if err := f.w.Record(context.Background(), ent(fmt.Sprintf("test.same%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	res := f.verify(t)
	if res.Entries != 5 || res.Broken != nil {
		t.Fatalf("same-nanosecond entries: %+v", res)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	t.Run("every stored column is covered by the hmac", func(t *testing.T) {
		for _, set := range []string{
			"id = 'x' || substr(id, 2)",
			"ts = ts + 1",
			"actor_type = 'system'",
			"actor_id = 'u-2'",
			"actor_name = 'mallory'",
			"action = 'x' || substr(action, 2)",
			"target_type = 'x'",
			"target_id = 'x'",
			"env_id = 'other'",
			"env_id = NULL",
			"result = 'failure'",
			"reason = 'edited'",
			"ip = '198.51.100.1'",
			"user_agent = 'edited'",
			`details = '{"k":"edited"}'`,
			"prev_hmac = zeroblob(32)",
		} {
			f := newFixture(t)
			f.record(t, 6)
			f.exec(t, "UPDATE audit_log SET "+set+" WHERE seq = 3")
			res := f.verify(t)
			if res.Broken == nil || res.Broken.Seq != 3 {
				t.Errorf("%s: Broken = %+v, want seq 3", set, res.Broken)
				continue
			}
			if !strings.Contains(res.Broken.Reason, "hmac mismatch") {
				t.Errorf("%s: reason = %q", set, res.Broken.Reason)
			}
		}
	})

	t.Run("flipped byte names the entry, its id and the reason", func(t *testing.T) {
		f := newFixture(t)
		f.record(t, 6)
		var id string
		if err := f.s.Read.QueryRow("SELECT id FROM audit_log WHERE seq = 3").Scan(&id); err != nil {
			t.Fatal(err)
		}
		f.exec(t, "UPDATE audit_log SET action = 'x' || substr(action, 2) WHERE seq = 3")
		res := f.verify(t)
		if res.Broken == nil || res.Broken.Seq != 3 || res.Broken.ID != id || res.Broken.Reason != "hmac mismatch" {
			t.Fatalf("Broken = %+v", res.Broken)
		}
	})

	t.Run("deleted middle row", func(t *testing.T) {
		f := newFixture(t)
		f.record(t, 6)
		f.exec(t, "DELETE FROM audit_log WHERE seq = 3")
		res := f.verify(t)
		if res.Broken == nil || res.Broken.Seq != 4 || !strings.Contains(res.Broken.Reason, "prev link mismatch") {
			t.Fatalf("Broken = %+v, want seq 4 prev link mismatch", res.Broken)
		}
	})

	t.Run("deleted prefix without an anchor", func(t *testing.T) {
		f := newFixture(t)
		f.record(t, 6)
		f.exec(t, "DELETE FROM audit_log WHERE seq <= 3")
		res := f.verify(t)
		if res.Broken == nil || res.Broken.Seq != 4 || !strings.Contains(res.Broken.Reason, "prev link mismatch") {
			t.Fatalf("Broken = %+v, want seq 4 prev link mismatch", res.Broken)
		}
	})

	t.Run("the earliest break is reported even when later ones exist", func(t *testing.T) {
		f := newFixture(t)
		f.record(t, 6)
		f.exec(t, "UPDATE audit_log SET reason = 'a' WHERE seq = 5")
		f.exec(t, "UPDATE audit_log SET reason = 'b' WHERE seq = 2")
		res := f.verify(t)
		if res.Broken == nil || res.Broken.Seq != 2 {
			t.Fatalf("Broken = %+v, want seq 2", res.Broken)
		}
	})

	t.Run("an entry written by a key this vault does not hold", func(t *testing.T) {
		f := newFixture(t)
		f.record(t, 3)
		res, err := audit.Verify(context.Background(), f.s, newVault(t, 6))
		if err != nil {
			t.Fatal(err)
		}
		if res.Broken == nil || res.Broken.Seq != 1 || !strings.Contains(res.Broken.Reason, "key") {
			t.Fatalf("Broken = %+v, want seq 1 naming the key", res.Broken)
		}
	})
}

func (f *fixture) lastHMAC(t *testing.T, seq int) []byte {
	t.Helper()
	var mac []byte
	if err := f.s.Read.QueryRow("SELECT hmac FROM audit_log WHERE seq = ?", seq).Scan(&mac); err != nil {
		t.Fatal(err)
	}
	return mac
}

func TestPruneAppendsAnAnchorAndVerificationStillPasses(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, off := range []time.Duration{-40 * 24 * time.Hour, -35 * 24 * time.Hour, -24 * time.Hour} {
		f.clk.Set(t0.Add(off))
		if err := f.w.Record(ctx, ent(audit.ActionAuthLogin)); err != nil {
			t.Fatal(err)
		}
	}
	prunedHMAC := f.lastHMAC(t, 2)

	f.clk.Set(t0)
	n, err := audit.Prune(ctx, f.w, 30)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("Prune deleted %d, want 2", n)
	}

	res := f.verify(t)
	if res.Broken != nil || res.Entries != 2 {
		t.Fatalf("after prune: %+v", res)
	}

	var action, actorID, details, ts string
	err = f.s.Read.QueryRow(`SELECT action, actor_id, details, CAST(ts AS TEXT) FROM audit_log ORDER BY seq DESC LIMIT 1`).
		Scan(&action, &actorID, &details, &ts)
	if err != nil {
		t.Fatal(err)
	}
	if action != audit.ActionAuditPruned || actorID != audit.SystemRetention.ID {
		t.Fatalf("anchor = %s by %s", action, actorID)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(details), &d); err != nil {
		t.Fatal(err)
	}
	if d["count"] != "2" || d["last_pruned_hmac"] != hex.EncodeToString(prunedHMAC) {
		t.Fatalf("anchor details = %v, want count 2 and the hmac of the last pruned entry", d)
	}
	if _, err := time.Parse(time.RFC3339, d["cutoff"]); err != nil {
		t.Fatalf("anchor cutoff %q is not RFC 3339: %v", d["cutoff"], err)
	}
	var meta string
	if err := f.s.Read.QueryRow(`SELECT value FROM app_meta WHERE key = 'audit_last_pruned_hmac'`).Scan(&meta); err != nil || meta != hex.EncodeToString(prunedHMAC) {
		t.Fatalf("app_meta audit_last_pruned_hmac = %q (%v)", meta, err)
	}

	// The chain keeps growing from the anchor.
	f.clk.Set(t0.Add(time.Minute))
	if err := f.w.Record(ctx, ent(audit.ActionAuthLogout)); err != nil {
		t.Fatal(err)
	}
	if res := f.verify(t); res.Broken != nil || res.Entries != 3 {
		t.Fatalf("after another entry: %+v", res)
	}
}

func TestSecondPruneRemovesTheFirstAnchor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, off := range []time.Duration{-40 * 24 * time.Hour, -35 * 24 * time.Hour, -24 * time.Hour} {
		f.clk.Set(t0.Add(off))
		if err := f.w.Record(ctx, ent(audit.ActionAuthLogin)); err != nil {
			t.Fatal(err)
		}
	}
	f.clk.Set(t0)
	if _, err := audit.Prune(ctx, f.w, 30); err != nil {
		t.Fatal(err)
	}
	f.clk.Set(t0.Add(29 * 24 * time.Hour))
	if err := f.w.Record(ctx, ent(audit.ActionAuthLogin)); err != nil {
		t.Fatal(err)
	}
	f.clk.Set(t0.Add(31 * 24 * time.Hour))
	n, err := audit.Prune(ctx, f.w, 30)
	if err != nil || n != 2 {
		t.Fatalf("second Prune = %d, %v; want 2 (the old entry and the first anchor)", n, err)
	}
	if res := f.verify(t); res.Broken != nil || res.Entries != 2 {
		t.Fatalf("after second prune: %+v", res)
	}
}

func TestPruneDoesNothingWhenDisabledOrNothingIsOld(t *testing.T) {
	f := newFixture(t)
	f.record(t, 3)
	f.clk.Set(t0.Add(1000 * 24 * time.Hour))
	if n, err := audit.Prune(context.Background(), f.w, 0); err != nil || n != 0 {
		t.Fatalf("Prune(0) = %d, %v", n, err)
	}
	if f.count(t) != 3 {
		t.Fatal("retention 0 must keep everything and append nothing")
	}
	f.clk.Set(t0)
	if n, err := audit.Prune(context.Background(), f.w, 30); err != nil || n != 0 {
		t.Fatalf("Prune with nothing old = %d, %v", n, err)
	}
	if f.count(t) != 3 {
		t.Fatal("a prune that deletes nothing must not append an anchor")
	}
}

func TestRemovingEntriesBehindAnAnchorIsStillDetected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, off := range []time.Duration{-40 * 24 * time.Hour, -35 * 24 * time.Hour, -24 * time.Hour} {
		f.clk.Set(t0.Add(off))
		if err := f.w.Record(ctx, ent(audit.ActionAuthLogin)); err != nil {
			t.Fatal(err)
		}
	}
	f.clk.Set(t0)
	if _, err := audit.Prune(ctx, f.w, 30); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "DELETE FROM audit_log WHERE seq = 3") // the oldest retained entry
	if res := f.verify(t); res.Broken == nil || res.Broken.Seq != 4 {
		t.Fatalf("Broken = %+v, want the anchor at seq 4", res.Broken)
	}
}

func TestSecretDetailKeysAreRejected(t *testing.T) {
	f := newFixture(t)
	for _, key := range []string{"password", "Password", "setup_token", "TOKEN", "client_secret", "api_key", "cookie", "Set-Cookie", "session_id", "csrf", "X-CSRF-Token"} {
		e := ent(audit.ActionAuthLoginFailed)
		e.Details = map[string]string{"ok": "fine", key: "value"}
		err := f.w.Record(context.Background(), e)
		if !errors.Is(err, audit.ErrSecretField) {
			t.Errorf("detail key %q: err = %v, want ErrSecretField", key, err)
		}
	}
	if f.count(t) != 0 {
		t.Fatal("a rejected entry was written")
	}
	e := ent(audit.ActionSettingsUpdated)
	e.Details = map[string]string{"count": "2", "attempted_username": "bob"}
	if err := f.w.Record(context.Background(), e); err != nil {
		t.Fatalf("harmless details rejected: %v", err)
	}
}

func TestInvalidEntriesAreRejected(t *testing.T) {
	f := newFixture(t)
	for name, mutate := range map[string]func(*audit.Entry){
		"no action":      func(e *audit.Entry) { e.Action = "" },
		"unknown actor":  func(e *audit.Entry) { e.Actor.Type = "robot" },
		"unknown result": func(e *audit.Entry) { e.Result = "maybe" },
	} {
		e := ent(audit.ActionAuthLogin)
		mutate(&e)
		if err := f.w.Record(context.Background(), e); !errors.Is(err, audit.ErrInvalidEntry) {
			t.Errorf("%s: err = %v, want ErrInvalidEntry", name, err)
		}
	}
}

func TestTwoDatabaseHandlesAppendingConcurrentlyKeepOneChain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s2, err := store.Open(ctx, filepath.Dir(f.s.Path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	w2 := audit.NewWriter(s2, f.v, slog.New(slog.DiscardHandler), time.Now)
	f.w = audit.NewWriter(f.s, f.v, slog.New(slog.DiscardHandler), time.Now)

	var wg sync.WaitGroup
	errc := make(chan error, 100)
	for _, w := range []*audit.Writer{f.w, w2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				if err := w.Record(ctx, ent(fmt.Sprintf("test.concurrent%d", i))); err != nil {
					errc <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("concurrent Record: %v", err)
	}
	res := f.verify(t)
	if res.Entries != 100 || res.Broken != nil {
		t.Fatalf("concurrent chain: %+v", res)
	}
}

func TestRecordTxIsVisibleOnlyAfterTheCallerCommits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	tx, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.w.RecordTx(ctx, tx, ent(audit.ActionSettingsUpdated)); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t); n != 0 {
		t.Fatalf("entry visible before commit: %d", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t); n != 1 {
		t.Fatalf("after commit: %d entries, want exactly 1", n)
	}

	rolled, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.w.RecordTx(ctx, rolled, ent(audit.ActionSettingsUpdated)); err != nil {
		t.Fatal(err)
	}
	if err := rolled.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t); n != 1 {
		t.Fatalf("after rollback: %d entries, want 1", n)
	}
	if err := f.w.Record(ctx, ent(audit.ActionAuthLogout)); err != nil {
		t.Fatal(err)
	}
	if res := f.verify(t); res.Entries != 2 || res.Broken != nil {
		t.Fatalf("chain after a rolled back RecordTx: %+v", res)
	}
}

func TestFieldLengthsAreCappedAndStillVerify(t *testing.T) {
	f := newFixture(t)
	e := ent(audit.ActionAuthLoginFailed)
	e.UserAgent = strings.Repeat("a", 600)
	e.Actor.Name = strings.Repeat("n", 200)
	e.TargetID = strings.Repeat("t", 200)
	if err := f.w.Record(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e2 := ent(audit.ActionAuthLoginFailed)
	e2.UserAgent = strings.Repeat("é", 300) // 600 bytes
	e2.Reason = "bad \xff\xfe bytes \x00 and\nnewlines"
	if err := f.w.Record(context.Background(), e2); err != nil {
		t.Fatal(err)
	}

	var ua, name, target string
	if err := f.s.Read.QueryRow("SELECT user_agent, actor_name, target_id FROM audit_log WHERE seq = 1").Scan(&ua, &name, &target); err != nil {
		t.Fatal(err)
	}
	if len(ua) != 512 || len(name) != 128 || len(target) != 128 {
		t.Fatalf("stored lengths = %d, %d, %d; want 512, 128, 128", len(ua), len(name), len(target))
	}
	var ua2, reason2 string
	if err := f.s.Read.QueryRow("SELECT user_agent, reason FROM audit_log WHERE seq = 2").Scan(&ua2, &reason2); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(reason2, "\x00\n") {
		t.Fatalf("control characters survived into the stored reason: %q", reason2)
	}
	if len(ua2) > 512 || !utf8.ValidString(ua2) || !utf8.ValidString(reason2) {
		t.Fatalf("truncation broke UTF-8 or the cap: %d bytes valid=%v/%v", len(ua2), utf8.ValidString(ua2), utf8.ValidString(reason2))
	}
	if res := f.verify(t); res.Entries != 2 || res.Broken != nil {
		t.Fatalf("capped entries do not verify: %+v", res)
	}
}

func TestFailedAppendReturnsAnError(t *testing.T) {
	f := newFixture(t)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.w.Record(context.Background(), ent(audit.ActionAuthLogin)); err == nil {
		t.Fatal("Record on a closed database returned nil: the entry would be silently dropped")
	}
}

func TestRecordEmitsOneConstantMessageWithFieldsAsAttributes(t *testing.T) {
	s := storetest.New(t)
	var logs bytes.Buffer
	w := audit.NewWriter(s, newVault(t, 5), slog.New(slog.NewJSONHandler(&logs, nil)), func() time.Time { return t0 })
	e := ent(audit.ActionAuthLoginFailed)
	e.Reason = "line one\nFAKE LOG LINE"
	e.Actor = audit.Actor{Type: audit.ActorAnonymous, Name: "evil\nname"}
	if err := w.Record(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one log line, got %d: %q", len(lines), logs.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "audit entry" || rec["action"] != audit.ActionAuthLoginFailed {
		t.Fatalf("log record = %v", rec)
	}
}

// inspectEntry is a network.inspect_saved entry whose inspect detail is doc.
func inspectEntry(doc string) audit.Entry {
	e := ent(audit.ActionNetworkInspectSaved)
	e.TargetType, e.TargetID = "network", "net-1"
	e.Details = map[string]string{"name": "n", "inspect": doc}
	return e
}

// jsonOf40KiB is a JSON object a little over 40 KiB, with characters the JSON encoder escapes.
func jsonOf40KiB() string {
	var b strings.Builder
	b.WriteString(`{"Name":"n","Labels":{`)
	for i := range 160 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":"%s"`, i, strings.Repeat("<é>&", 60))
	}
	b.WriteString(`}}`)
	return b.String()
}

// The inspect detail of network.inspect_saved is evidence (D-12): stored byte for byte, and the
// chain still verifies over it.
func TestEvidenceStoredWhole(t *testing.T) {
	f := newFixture(t)
	doc := jsonOf40KiB()
	if len(doc) < 40<<10 || len(doc) > audit.MaxEvidenceValue {
		t.Fatalf("fixture is %d bytes", len(doc))
	}
	if err := f.w.Record(context.Background(), inspectEntry(doc)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rows, _, err := audit.Query(context.Background(), f.s, audit.Filter{AnyEnv: true}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Details["inspect"] != doc {
		t.Fatalf("the inspect was not read back byte for byte (%d rows)", len(rows))
	}
	if rows[0].Details["name"] != "n" {
		t.Errorf("name = %q", rows[0].Details["name"])
	}
	if got := f.verify(t); got.Broken != nil || got.Entries != 1 {
		t.Fatalf("verify = %+v", got)
	}
	// The limit itself is stored.
	full := strings.Repeat("a", audit.MaxEvidenceValue)
	if err := f.w.Record(context.Background(), inspectEntry(full)); err != nil {
		t.Fatalf("a value of exactly MaxEvidenceValue was refused: %v", err)
	}
}

// A value over the limit, or one cleaning would change, is refused and nothing is written.
func TestEvidenceRefusedNotCut(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.w.Record(ctx, inspectEntry(strings.Repeat("a", audit.MaxEvidenceValue+1))); !errors.Is(err, audit.ErrEvidenceTooLarge) {
		t.Fatalf("err = %v, want ErrEvidenceTooLarge", err)
	}
	for name, doc := range map[string]string{"DEL": "{\"a\":\"x\x7fy\"}", "NUL": "{\"a\":\"x\x00y\"}", "bad UTF-8": "{\"a\":\"x\xffy\"}"} {
		if err := f.w.Record(ctx, inspectEntry(doc)); !errors.Is(err, audit.ErrInvalidEntry) {
			t.Errorf("%s: err = %v, want ErrInvalidEntry", name, err)
		}
	}
	if n := f.count(t); n != 0 {
		t.Fatalf("the log holds %d entries after refusals, want 0", n)
	}
	tx, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.w.RecordTx(ctx, tx, inspectEntry(strings.Repeat("a", audit.MaxEvidenceValue+1))); !errors.Is(err, audit.ErrEvidenceTooLarge) {
		t.Fatalf("RecordTx err = %v, want ErrEvidenceTooLarge", err)
	}
}

// Only network.inspect_saved's inspect is evidence; every other value keeps the 512-byte cap.
func TestEvidenceOnlyForItsPair(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	doc := jsonOf40KiB()

	removed := ent(audit.ActionNetworkRemoved)
	removed.Details = map[string]string{"inspect": doc}
	if err := f.w.Record(ctx, removed); err != nil {
		t.Fatal(err)
	}
	other := ent(audit.ActionNetworkInspectSaved)
	other.Details = map[string]string{"name": doc, "inspect": "{}"}
	if err := f.w.Record(ctx, other); err != nil {
		t.Fatal(err)
	}
	rows, _, err := audit.Query(ctx, f.s, audit.Filter{AnyEnv: true}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, r := range rows {
		for k, v := range r.Details {
			if len(v) > 512 {
				t.Errorf("%s %s is %d bytes, want it cut to 512", r.Action, k, len(v))
			}
		}
	}
	if got := f.verify(t); got.Broken != nil || got.Entries != 2 {
		t.Fatalf("verify = %+v", got)
	}
}
