// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// newTestStore is a Store over a migrated temporary database and a vault with a generated key. The
// store and vault are returned too, for tests that read raw columns or re-wrap.
func newTestStore(t *testing.T) (*Store, *store.Store, *vault.Vault) {
	t.Helper()
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.New(t)
	return NewStore(st, v), st, v
}

const testDirectory = "https://acme-staging-v02.api.letsencrypt.org/directory"

func TestACMEAccountKeyRoundTripsSealed(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	acct := Account{
		DirectoryURL: testDirectory, URL: "https://acme.example/acct/1", Contact: "mailto:admin@example.org",
		TermsOfService: "https://acme.example/tos", CreatedAt: time.Unix(1_700_000_000, 123),
	}

	if err := s.SaveAccount(ctx, acct, key); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	got, gotKey, found, err := s.Account(ctx, testDirectory)
	if err != nil || !found {
		t.Fatalf("Account = found %v, %v; want found", found, err)
	}
	if !gotKey.Equal(key) {
		t.Error("Account returned a different key than was saved")
	}
	if !got.CreatedAt.Equal(acct.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, acct.CreatedAt)
	}
	if got.DirectoryURL != acct.DirectoryURL || got.URL != acct.URL || got.Contact != acct.Contact || got.TermsOfService != acct.TermsOfService {
		t.Errorf("Account = %+v, want %+v", got, acct)
	}

	var raw string
	if err := st.Read.QueryRowContext(ctx, `SELECT key_sealed FROM acme_account WHERE directory_url = ?`, testDirectory).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "PRIVATE KEY") || !strings.HasPrefix(raw, "v1.") {
		t.Errorf("the key_sealed column is not a vault envelope: %.20q", raw)
	}

	// Another directory has no account, and that is not an error.
	if _, _, found, err := s.Account(ctx, "https://acme-v02.api.letsencrypt.org/directory"); found || err != nil {
		t.Errorf("an unknown directory = found %v, %v; want not found and no error", found, err)
	}

	// An envelope that does not open is an error and never "not found".
	if _, err := st.Write.ExecContext(ctx, `UPDATE acme_account SET key_sealed = 'v1.k1-nope.AAAA' WHERE directory_url = ?`, testDirectory); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := s.Account(ctx, testDirectory); err == nil || found {
		t.Errorf("a key that does not open = found %v, %v; want an error", found, err)
	}

	if err := s.ForgetAccount(ctx, testDirectory); err != nil {
		t.Fatalf("ForgetAccount: %v", err)
	}
	if _, _, found, err := s.Account(ctx, testDirectory); found || err != nil {
		t.Errorf("after ForgetAccount = found %v, %v; want not found", found, err)
	}
}

func newTestOrder(id string, created time.Time) Order {
	return Order{
		ID: id, DirectoryURL: testDirectory, URL: "https://acme.example/order/" + id, Kind: "issue",
		Names: []string{"tierplan.example.org", "www.example.org"}, Profile: "shortlived", Replaces: "",
		Stage: StageCreated, Status: "open", ExpiresAt: created.Add(7 * 24 * time.Hour),
		CreatedAt: created, UpdatedAt: created,
	}
}

func TestACMEOrderKeyIsSealedAndDurable(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 987_654_321)
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := s.OpenOrder(ctx); found || err != nil {
		t.Fatalf("OpenOrder on a fresh database = found %v, %v; want not found", found, err)
	}

	o := newTestOrder("order-1", t0)
	if err := s.InsertOrder(ctx, o, key); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	// Durable when InsertOrder returns: nothing is left in the write-ahead log to lose to a power cut.
	var busy, logFrames, checkpointed int
	if err := st.Write.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatal(err)
	}
	if busy != 0 || logFrames != checkpointed {
		t.Errorf("after InsertOrder the log holds %d frames and %d are checkpointed (busy %d); want all checkpointed", logFrames, checkpointed, busy)
	}

	var raw string
	if err := st.Read.QueryRowContext(ctx, `SELECT cert_key_sealed FROM acme_orders WHERE id = 'order-1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "PRIVATE KEY") || !strings.HasPrefix(raw, "v1.") {
		t.Errorf("cert_key_sealed is not a vault envelope: %.20q", raw)
	}

	got, gotKey, found, err := s.OpenOrder(ctx)
	if err != nil || !found {
		t.Fatalf("OpenOrder = found %v, %v; want found", found, err)
	}
	if !gotKey.Equal(key) {
		t.Error("OpenOrder returned a different key than was inserted")
	}
	if got.ID != o.ID || got.URL != o.URL || got.DirectoryURL != o.DirectoryURL || got.Kind != "issue" ||
		!slices.Equal(got.Names, o.Names) || got.Profile != "shortlived" || got.Stage != StageCreated ||
		got.Status != "open" || got.ResumeCount != 0 {
		t.Errorf("OpenOrder = %+v, want %+v", got, o)
	}
	if !got.ExpiresAt.Equal(o.ExpiresAt) || !got.CreatedAt.Equal(o.CreatedAt) || !got.UpdatedAt.Equal(o.UpdatedAt) {
		t.Errorf("OpenOrder instants = %v %v %v, want %v %v %v", got.ExpiresAt, got.CreatedAt, got.UpdatedAt, o.ExpiresAt, o.CreatedAt, o.UpdatedAt)
	}

	t1 := t0.Add(time.Minute)
	if err := s.UpdateOrder(ctx, "order-1", StageFinalizing, 2, t1); err != nil {
		t.Fatalf("UpdateOrder: %v", err)
	}
	got, _, found, err = s.OpenOrder(ctx)
	if err != nil || !found || got.Stage != StageFinalizing || got.ResumeCount != 2 || !got.UpdatedAt.Equal(t1) {
		t.Errorf("after UpdateOrder = %+v, found %v, %v; want stage finalizing, 2 resumes, updated at %v", got, found, err, t1)
	}

	// A key copied into another order's row does not open: it is bound to the order's ID.
	if err := s.InsertOrder(ctx, newTestOrder("order-2", t0.Add(time.Hour)), key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.ExecContext(ctx, `UPDATE acme_orders SET cert_key_sealed = (SELECT cert_key_sealed FROM acme_orders WHERE id = 'order-1') WHERE id = 'order-2'`); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := s.OpenOrder(ctx); err == nil || found {
		t.Errorf("a key sealed for another order = found %v, %v; want an error", found, err)
	}

	if err := s.CloseOrder(ctx, "order-2", StageFailed, t1); err != nil {
		t.Fatalf("CloseOrder: %v", err)
	}
	got, _, found, err = s.OpenOrder(ctx)
	if err != nil || !found || got.ID != "order-1" {
		t.Fatalf("after closing order-2, OpenOrder = %+v, found %v, %v; want order-1", got, found, err)
	}
	if err := s.CloseOrder(ctx, "order-1", StageFailed, t1); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := s.OpenOrder(ctx); found || err != nil {
		t.Errorf("after closing every order, OpenOrder = found %v, %v; want not found", found, err)
	}
}

// selfSignedRow is what the tls_certs row "selfsigned" holds, read raw.
func selfSignedRow(t *testing.T, st *store.Store) string {
	t.Helper()
	var cert, sealed string
	var created, updated int64
	if err := st.Read.QueryRow(`SELECT cert_pem, key_sealed, created_at, updated_at FROM tls_certs WHERE id = 'selfsigned'`).Scan(&cert, &sealed, &created, &updated); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(cert, "|", sealed, "|", created, "|", updated)
}

func TestStoreIssuedIsOneTransactionAndLeavesSelfSignedAlone(t *testing.T) {
	ctx := context.Background()
	s, st, v := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 1)

	selfKey, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	selfPEM, err := encodeKey(selfKey)
	if err != nil {
		t.Fatal(err)
	}
	sealedSelf, err := v.Seal(vault.ContextTLSKey+"selfsigned", selfPEM)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.ExecContext(ctx, `INSERT INTO tls_certs (id, cert_pem, key_sealed, created_at, updated_at) VALUES ('selfsigned', 'self-cert', ?, 5, 6)`, sealedSelf); err != nil {
		t.Fatal(err)
	}
	before := selfSignedRow(t, st)

	if _, _, found, err := s.Certificate(ctx); found || err != nil {
		t.Fatalf("Certificate before any issue = found %v, %v; want not found", found, err)
	}

	certKey, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertOrder(ctx, newTestOrder("order-1", t0), certKey); err != nil {
		t.Fatal(err)
	}
	chain := []byte("-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nissuer\n-----END CERTIFICATE-----\n")

	// The second statement fails: neither change is kept.
	if _, err := st.Write.ExecContext(ctx, `CREATE TRIGGER force_failure BEFORE UPDATE ON acme_orders BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreIssued(ctx, "order-1", chain, certKey, t0.Add(time.Second)); err == nil {
		t.Fatal("StoreIssued succeeded although closing the order failed")
	}
	var n int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM tls_certs WHERE id = 'acme'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a failed StoreIssued left %d acme certificate rows (%v); want none", n, err)
	}
	if _, _, found, _ := s.OpenOrder(ctx); !found {
		t.Error("a failed StoreIssued closed the order")
	}
	if _, err := st.Write.ExecContext(ctx, `DROP TRIGGER force_failure`); err != nil {
		t.Fatal(err)
	}

	t1 := t0.Add(time.Minute)
	if err := s.StoreIssued(ctx, "order-1", chain, certKey, t1); err != nil {
		t.Fatalf("StoreIssued: %v", err)
	}
	gotChain, gotKey, found, err := s.Certificate(ctx)
	if err != nil || !found {
		t.Fatalf("Certificate = found %v, %v; want found", found, err)
	}
	if !bytes.Equal(gotChain, chain) || !gotKey.Equal(certKey) {
		t.Error("Certificate returned another chain or key than was stored")
	}
	var raw string
	if err := st.Read.QueryRow(`SELECT key_sealed FROM tls_certs WHERE id = 'acme'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "PRIVATE KEY") {
		t.Error("the acme certificate key is stored in the clear")
	}
	if _, err := v.Open(vault.ContextTLSKey+CertID, raw); err != nil {
		t.Errorf("the acme key does not open under the tls-key context the server reads it with: %v", err)
	}
	if _, _, found, err := s.OpenOrder(ctx); found || err != nil {
		t.Errorf("the order is still open after StoreIssued: found %v, %v", found, err)
	}
	var stage, status string
	if err := st.Read.QueryRow(`SELECT stage, status FROM acme_orders WHERE id = 'order-1'`).Scan(&stage, &status); err != nil || stage != StageStored || status != "closed" {
		t.Errorf("the order is %q/%q (%v), want stored/closed", stage, status, err)
	}

	// A renewal replaces the row; the self-signed one never moves.
	key2, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertOrder(ctx, newTestOrder("order-2", t1), key2); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreIssued(ctx, "order-2", []byte("renewed"), key2, t1.Add(time.Hour)); err != nil {
		t.Fatalf("StoreIssued (renewal): %v", err)
	}
	gotChain, gotKey, _, err = s.Certificate(ctx)
	if err != nil || string(gotChain) != "renewed" || !gotKey.Equal(key2) {
		t.Errorf("after the renewal Certificate = %q, %v", gotChain, err)
	}
	if after := selfSignedRow(t, st); after != before {
		t.Errorf("the selfsigned row changed:\n before %s\n after  %s", before, after)
	}
}

func TestACMEStateRoundTripsToTheNanosecond(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)

	fresh, err := s.State(ctx)
	if err != nil {
		t.Fatalf("State on a fresh database: %v", err)
	}
	if fresh.Mode != ModeOff || fresh.PausedReason != "" || fresh.ConsecutiveFailures != 0 {
		t.Errorf("fresh state = %+v; want mode off, no pause, no failures", fresh)
	}
	for name, ts := range instants(&fresh) {
		if !ts.IsZero() {
			t.Errorf("fresh state: %s = %v; want the zero time (no backoff in force)", name, ts)
		}
	}

	base := time.Unix(1_800_000_000, 0)
	want := State{
		Mode: ModeActive, ChallengeType: "http-01", Profile: "shortlived", PausedReason: PausedRestore,
		ConsecutiveFailures: 3, RenewBasis: "ari", LastErrorClass: "rate_limited",
		LastErrorDetail: "too many certificates (5) already issued", LastSuccessKind: "renew",
		LastAttemptAt: base.Add(1), NextAttemptAt: base.Add(2*time.Hour + 22), RetryAfterUntil: base.Add(3*time.Hour + 333),
		RenewAt: base.Add(4*time.Hour + 4444), AriWindowStart: base.Add(5*time.Hour + 55555), AriWindowEnd: base.Add(6*time.Hour + 666666),
		AriCheckedAt: base.Add(7*time.Hour + 7777777), AriNextCheckAt: base.Add(8*time.Hour + 88888888),
		LastErrorAt: base.Add(9*time.Hour + 999999999), LastSuccessAt: base.Add(10*time.Hour + 123456789),
		UpdatedAt: base.Add(11*time.Hour + 1),
	}
	if err := s.SaveState(ctx, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gi, wi := instants(&got), instants(&want)
	for name, w := range wi {
		if !gi[name].Equal(w) || gi[name].UnixNano() != w.UnixNano() {
			t.Errorf("%s = %v (%d ns), want %v (%d ns)", name, gi[name], gi[name].UnixNano(), w, w.UnixNano())
		}
	}
	for _, p := range []*State{&got, &want} {
		zeroInstants(p)
	}
	if got != want {
		t.Errorf("State = %+v, want %+v", got, want)
	}

	// Clearing every instant reads back as the empty ledger, not as the Unix epoch.
	if err := s.SaveState(ctx, State{Mode: ModeOff, ChallengeType: "http-01"}); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, ts := range instants(&cleared) {
		if !ts.IsZero() {
			t.Errorf("cleared state: %s = %v; want the zero time", name, ts)
		}
	}
}

// instants names every instant of a State.
func instants(st *State) map[string]time.Time {
	return map[string]time.Time{
		"LastAttemptAt": st.LastAttemptAt, "NextAttemptAt": st.NextAttemptAt, "RetryAfterUntil": st.RetryAfterUntil,
		"RenewAt": st.RenewAt, "AriWindowStart": st.AriWindowStart, "AriWindowEnd": st.AriWindowEnd,
		"AriCheckedAt": st.AriCheckedAt, "AriNextCheckAt": st.AriNextCheckAt, "LastErrorAt": st.LastErrorAt,
		"LastSuccessAt": st.LastSuccessAt, "UpdatedAt": st.UpdatedAt,
	}
}

func zeroInstants(st *State) {
	st.LastAttemptAt, st.NextAttemptAt, st.RetryAfterUntil, st.RenewAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	st.AriWindowStart, st.AriWindowEnd, st.AriCheckedAt, st.AriNextCheckAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	st.LastErrorAt, st.LastSuccessAt, st.UpdatedAt = time.Time{}, time.Time{}, time.Time{}
}

func TestACMERunsFinishWithTheLedgerAndPrune(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	base := time.Unix(1_800_000_000, 0)

	const total = 52
	for i := range total {
		r := Run{
			ID: fmt.Sprintf("run-%02d", i), Kind: "issue", Trigger: "schedule", Names: []string{"a.example.org"},
			StartedAt: base.Add(time.Duration(i) * time.Minute), Outcome: "running",
		}
		if i == 0 {
			r.Actor = "user-1"
		}
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun %d: %v", i, err)
		}
	}

	// The finish of a run and the ledger move together: when the state write fails, the run stays running.
	ledger := State{Mode: ModeActive, ChallengeType: "http-01", ConsecutiveFailures: 1, LastErrorClass: "dns", LastErrorAt: base.Add(time.Hour + 5), UpdatedAt: base.Add(time.Hour + 5)}
	if _, err := st.Write.ExecContext(ctx, `CREATE TRIGGER force_failure BEFORE UPDATE ON acme_state BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
		t.Fatal(err)
	}
	last := fmt.Sprintf("run-%02d", total-1)
	if err := s.FinishRun(ctx, last, "failed", "dns", base.Add(time.Hour), ledger); err == nil {
		t.Fatal("FinishRun succeeded although the state write failed")
	}
	runs, err := s.Runs(ctx, 100)
	if err != nil || len(runs) != total {
		t.Fatalf("after a failed FinishRun %d runs (%v); want %d, none pruned", len(runs), err, total)
	}
	if runs[0].ID != last || runs[0].Outcome != "running" {
		t.Errorf("newest run = %q %q after a failed FinishRun; want %s still running", runs[0].ID, runs[0].Outcome, last)
	}
	if _, err := st.Write.ExecContext(ctx, `DROP TRIGGER force_failure`); err != nil {
		t.Fatal(err)
	}

	finished := base.Add(time.Hour)
	if err := s.FinishRun(ctx, last, "failed", "dns", finished, ledger); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	runs, err = s.Runs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 50 {
		t.Fatalf("%d runs kept, want 50", len(runs))
	}
	if runs[0].ID != last || runs[0].Outcome != "failed" || runs[0].ErrorClass != "dns" || !runs[0].FinishedAt.Equal(finished) {
		t.Errorf("newest run = %+v; want %s failed/dns finished at %v", runs[0], last, finished)
	}
	if oldest := runs[len(runs)-1].ID; oldest != "run-02" {
		t.Errorf("oldest kept run = %s; want run-02 (the two oldest are pruned first)", oldest)
	}
	if len(runs[0].Names) != 1 || runs[0].Names[0] != "a.example.org" || runs[0].Kind != "issue" || runs[0].Trigger != "schedule" {
		t.Errorf("run fields lost: %+v", runs[0])
	}
	if two, err := s.Runs(ctx, 2); err != nil || len(two) != 2 {
		t.Errorf("Runs(2) = %d runs, %v; want 2", len(two), err)
	}
	gotState, err := s.State(ctx)
	if err != nil || gotState.ConsecutiveFailures != 1 || gotState.LastErrorClass != "dns" || !gotState.LastErrorAt.Equal(ledger.LastErrorAt) {
		t.Errorf("ledger after FinishRun = %+v, %v", gotState, err)
	}

	// FailedRunsSince is the start times of the failed runs inside the window.
	if err := s.FinishRun(ctx, "run-49", "failed", "internal", finished, ledger); err != nil {
		t.Fatal(err)
	}
	since := base.Add(49 * time.Minute)
	starts, err := s.FailedRunsSince(ctx, since)
	want := []time.Time{base.Add(49 * time.Minute), base.Add(51 * time.Minute)}
	if err != nil || len(starts) != len(want) {
		t.Fatalf("FailedRunsSince = %v, %v; want %v", starts, err, want)
	}
	for i := range want {
		if !starts[i].Equal(want[i]) {
			t.Errorf("FailedRunsSince[%d] = %v, want %v", i, starts[i], want[i])
		}
	}

	// A run still running at startup was cut short.
	cut := base.Add(2 * time.Hour)
	if err := s.InterruptRunning(ctx, cut); err != nil {
		t.Fatalf("InterruptRunning: %v", err)
	}
	var running int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM acme_runs WHERE outcome = 'running'`).Scan(&running); err != nil || running != 0 {
		t.Errorf("%d runs still running after InterruptRunning (%v)", running, err)
	}
	runs, _ = s.Runs(ctx, 100)
	for _, r := range runs {
		if r.ID == "run-10" && (r.Outcome != "failed" || r.ErrorClass != "internal" || !r.FinishedAt.Equal(cut)) {
			t.Errorf("an interrupted run = %+v; want failed/internal finished at %v", r, cut)
		}
	}
}

func TestACMEKeysSurviveRewrap(t *testing.T) {
	ctx := context.Background()
	s, st, v := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 0)

	acctKey, _ := newKey()
	orderKey, _ := newKey()
	certKey, _ := newKey()
	if err := s.SaveAccount(ctx, Account{DirectoryURL: testDirectory, URL: "https://acme.example/acct/1", CreatedAt: t0}, acctKey); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertOrder(ctx, newTestOrder("order-1", t0), orderKey); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertOrder(ctx, newTestOrder("order-0", t0.Add(-time.Hour)), certKey); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreIssued(ctx, "order-0", []byte("chain"), certKey, t0); err != nil {
		t.Fatal(err)
	}

	next, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.RewrapAll(ctx, tx, v, next); err != nil {
		_ = tx.Rollback()
		t.Fatalf("RewrapAll: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	moved := NewStore(st, next)
	check := func(name string, got, want *ecdsa.PrivateKey, found bool, err error) {
		t.Helper()
		if err != nil || !found || !got.Equal(want) {
			t.Errorf("%s after the re-wrap: found %v, %v, same key %v", name, found, err, got != nil && got.Equal(want))
		}
	}
	_, k, found, err := moved.Account(ctx, testDirectory)
	check("the account key", k, acctKey, found, err)
	_, k, found, err = moved.OpenOrder(ctx)
	check("the order key", k, orderKey, found, err)
	_, k, found, err = moved.Certificate(ctx)
	check("the certificate key", k, certKey, found, err)

	// The old vault no longer opens them: the re-wrap really moved the envelopes.
	if _, _, _, err := s.Account(ctx, testDirectory); err == nil {
		t.Error("the old vault still opens the account key after the re-wrap")
	}
}

// A snapshot taken as a run ends shows the run's outcome and the state it wrote together or neither,
// even when the run ends between the read of the state row and the read of the runs (the status read
// them separately and could miss the last error).
func TestACMEReadSnapshotIsOneInstant(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	base := time.Unix(1_800_000_000, 0)
	if err := s.InsertRun(ctx, Run{ID: "run-1", Kind: "issue", Trigger: "admin", StartedAt: base, Outcome: OutcomeRunning}); err != nil {
		t.Fatal(err)
	}
	failedState := State{
		Mode: ModeActive, ChallengeType: "http-01", ConsecutiveFailures: 1,
		LastErrorClass: ClassValidationDNS, LastErrorAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute),
	}

	finishes := 0
	s.afterStateRead = func() {
		finishes++
		if err := s.FinishRun(ctx, "run-1", OutcomeFailed, ClassValidationDNS, base.Add(time.Minute), failedState); err != nil {
			t.Errorf("FinishRun: %v", err)
		}
	}
	snap, err := s.ReadSnapshot(ctx, 5, base)
	if err != nil {
		t.Fatal(err)
	}
	if finishes != 1 {
		t.Fatalf("the run was finished %d times between the reads; want once", finishes)
	}
	// The run ended after the snapshot began: the state row and the run are both from before.
	if snap.State.LastErrorClass != "" || len(snap.Runs) != 1 || snap.Runs[0].Outcome != OutcomeRunning || len(snap.FailedStarts) != 0 {
		t.Fatalf("snapshot mixes instants: error class %q, runs %+v, failed starts %v; want the state and the run from before the finish",
			snap.State.LastErrorClass, snap.Runs, snap.FailedStarts)
	}

	// The next snapshot sees both the outcome and the error.
	s.afterStateRead = nil
	snap, err = s.ReadSnapshot(ctx, 5, base)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State.LastErrorClass != ClassValidationDNS || snap.Runs[0].Outcome != OutcomeFailed || len(snap.FailedStarts) != 1 {
		t.Fatalf("snapshot after the finish: error class %q, runs %+v, failed starts %v; want the failure with its error",
			snap.State.LastErrorClass, snap.Runs, snap.FailedStarts)
	}
}
