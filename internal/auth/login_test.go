// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/store/storetest"
)

// Sign-in, session lookup, CSRF and unlock (plan 12, AUTH-02, AUTH-03, AUTH-08).
// Negative cases are mandatory here (D-81).

const (
	loginPassword = "correct horse battery"
	loginIP       = "203.0.113.9"
	loginUA       = "login-test/1.0"
	maxFailures   = 5
	lockout       = 30 * time.Minute
)

// loginHash is hashed once: argon2id is slow and every test needs a user.
var loginHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(loginPassword)
	if err != nil {
		panic(err)
	}
	return h
})

type loginFixture struct {
	st    *store.Store
	sess  *auth.Sessions
	aud   *audit.Writer
	lg    *auth.Login
	lim   *auth.IPLimiter
	clk   *clock
	logs  *bytes.Buffer
	dummy atomic.Int32
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	f := &loginFixture{st: storetest.New(t), clk: newClock(), logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	v := newVault(t, 7)
	f.sess = auth.NewSessions(f.st, v, testIdle, testRemember, f.clk.now)
	f.aud = audit.NewWriter(f.st, v, log, f.clk.now)
	f.lim = auth.NewIPLimiter(time.Second, 1000, 10*time.Minute, f.clk.now)
	f.lg = auth.NewLogin(f.st, f.sess, f.aud, maxFailures, lockout, f.lim, log, f.clk.now)
	f.lg.SetDummyVerify(func(string) { f.dummy.Add(1) })
	return f
}

// addUser inserts a user with loginPassword and returns its ID.
func (f *loginFixture) addUser(t *testing.T, name string) string {
	t.Helper()
	n := f.clk.now().UnixNano()
	id := "id-" + name
	if err := f.st.Q.InsertUser(context.Background(), sqlc.InsertUserParams{
		ID: id, Username: name, PasswordHash: loginHash(), CreatedAt: n, UpdatedAt: n, PasswordChangedAt: n,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *loginFixture) user(t *testing.T, name string) sqlc.User {
	t.Helper()
	u, err := f.st.RQ.GetUserByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *loginFixture) attempt(name, password string) (auth.LoginResult, error) {
	return f.attemptFrom(name, password, loginIP, false, nil)
}

func (f *loginFixture) attemptFrom(name, password, ip string, remember bool, prev []byte) (auth.LoginResult, error) {
	return f.lg.Login(context.Background(), auth.LoginInput{
		Username: name, Password: password, Remember: remember, IP: ip, UserAgent: loginUA, PreviousSessionHash: prev,
	})
}

func (f *loginFixture) mustLogin(t *testing.T, name string, remember bool) auth.LoginResult {
	t.Helper()
	res, err := f.attemptFrom(name, loginPassword, loginIP, remember, nil)
	if err != nil {
		t.Fatalf("sign-in as %s: %v", name, err)
	}
	return res
}

func (f *loginFixture) sessionRow(t *testing.T, hash []byte) (sqlc.Session, bool) {
	t.Helper()
	row, err := f.st.RQ.GetSessionByHash(context.Background(), hash)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.Session{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row, true
}

func (f *loginFixture) sessionCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type auditRow struct {
	Action, ActorType, ActorID, ActorName, TargetType, TargetID, Result, Reason, IP, UserAgent, Details string
}

func (f *loginFixture) audits(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, actor_type, actor_id, actor_name, target_type, target_id, result, reason, ip, user_agent, details
		FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Action, &r.ActorType, &r.ActorID, &r.ActorName, &r.TargetType, &r.TargetID, &r.Result, &r.Reason, &r.IP, &r.UserAgent, &r.Details); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func hashOf(raw string) []byte {
	s := sha256.Sum256([]byte(raw))
	return s[:]
}

// --- Login -----------------------------------------------------------------

func TestLoginWithTheRightPasswordCreatesASessionAndResetsFailures(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	for range 2 {
		if _, err := f.attempt("admin", "wrong wrong wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("wrong password: %v", err)
		}
	}
	if got := f.user(t, "admin").FailedCount; got != 2 {
		t.Fatalf("failed_count = %d, want 2", got)
	}

	res, err := f.attempt("Admin", loginPassword) // the name is case-insensitive
	if err != nil {
		t.Fatal(err)
	}
	if res.RawSessionID == "" || len(res.IDHash) != 32 || res.User.Username != "admin" {
		t.Fatalf("result = %+v", res)
	}
	if string(hashOf(res.RawSessionID)) != string(res.IDHash) {
		t.Fatal("IDHash is not the SHA-256 of the raw session ID")
	}
	row, ok := f.sessionRow(t, res.IDHash)
	if !ok || row.UserID != "id-admin" || row.Ip != loginIP || row.UserAgent != loginUA || row.Remember != 0 {
		t.Fatalf("session row = %+v, %v", row, ok)
	}
	if got := f.user(t, "admin").FailedCount; got != 0 {
		t.Fatalf("failed_count after success = %d, want 0", got)
	}

	entries := f.audits(t, audit.ActionAuthLogin)
	if len(entries) != 1 {
		t.Fatalf("auth.login entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.ActorType != "user" || e.ActorID != "id-admin" || e.ActorName != "admin" || e.Result != "success" || e.IP != loginIP || e.UserAgent != loginUA {
		t.Fatalf("auth.login entry = %+v", e)
	}
}

func TestLoginRememberGivesTheSessionAnAbsoluteExpiry(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", true)
	row, _ := f.sessionRow(t, res.IDHash)
	if row.Remember != 1 || !row.AbsoluteExpiresAt.Valid || row.AbsoluteExpiresAt.Int64 != f.clk.now().Add(testRemember).UnixNano() {
		t.Fatalf("remembered session = %+v", row)
	}
}

func TestLoginRotatesThePreviousSession(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	old := f.mustLogin(t, "admin", false)

	res, err := f.attemptFrom("admin", loginPassword, loginIP, false, old.IDHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.sessionRow(t, old.IDHash); ok {
		t.Fatal("the previous session survived a new sign-in (D-32)")
	}
	if _, ok := f.sessionRow(t, res.IDHash); !ok {
		t.Fatal("the new session was not stored")
	}
	if res.RawSessionID == old.RawSessionID {
		t.Fatal("the session ID was reused")
	}
}

func TestLoginFailedAttemptDoesNotRotateThePreviousSession(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	old := f.mustLogin(t, "admin", false)
	if _, err := f.attemptFrom("admin", "wrong wrong wrong", loginIP, false, old.IDHash); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatal(err)
	}
	if _, ok := f.sessionRow(t, old.IDHash); !ok {
		t.Fatal("a failed sign-in deleted the session the browser held")
	}
}

func TestLoginFifthFailureLocksTheAccountForThirtyMinutes(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	for i := 1; i <= 4; i++ {
		if _, err := f.attempt("admin", "wrong wrong wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if u := f.user(t, "admin"); u.LockedUntil.Valid || u.FailedCount != int64(i) {
			t.Fatalf("after %d failures: %+v, want counted and unlocked", i, u)
		}
	}
	if len(f.audits(t, audit.ActionAuthLocked)) != 0 {
		t.Fatal("auth.locked recorded before the fifth failure")
	}

	if _, err := f.attempt("admin", "wrong wrong wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatal(err)
	}
	u := f.user(t, "admin")
	if want := f.clk.now().Add(lockout).UnixNano(); !u.LockedUntil.Valid || u.LockedUntil.Int64 != want {
		t.Fatalf("locked_until = %+v, want %d", u.LockedUntil, want)
	}
	locked := f.audits(t, audit.ActionAuthLocked)
	if len(locked) != 1 || locked[0].TargetType != "user" || locked[0].TargetID != "id-admin" || locked[0].IP != loginIP {
		t.Fatalf("auth.locked entries = %+v", locked)
	}
	if !strings.Contains(locked[0].Details, `"failures":"5"`) {
		t.Fatalf("auth.locked details = %s, want the failure count", locked[0].Details)
	}
	if !strings.Contains(f.logs.String(), "account locked") {
		t.Fatalf("the lock is not in the container log: %s", f.logs)
	}
}

func TestLoginRightPasswordWhileLockedIsRefusedUntilTheLockExpires(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	for range maxFailures {
		_, _ = f.attempt("admin", "wrong wrong wrong")
	}
	before := f.dummy.Load()

	if _, err := f.attempt("admin", loginPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("right password while locked: %v, want ErrInvalidCredentials", err)
	}
	if got := f.dummy.Load() - before; got != 1 {
		t.Fatalf("a locked attempt paid %d dummy verifications, want 1", got)
	}
	// It did not extend the lock or count as a failure.
	if u := f.user(t, "admin"); u.FailedCount != int64(maxFailures) || u.LockedUntil.Int64 != f.clk.now().Add(lockout).UnixNano() {
		t.Fatalf("a refused attempt changed the lock state: %+v", u)
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a session was created for a locked account")
	}

	f.clk.advance(lockout - time.Second)
	if _, err := f.attempt("admin", loginPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("one second before the lock ends: %v, want ErrInvalidCredentials", err)
	}
	f.clk.advance(time.Second) // locked_until == now: no longer locked
	res, err := f.attempt("admin", loginPassword)
	if err != nil {
		t.Fatalf("after the lock expired: %v", err)
	}
	if u := f.user(t, "admin"); u.FailedCount != 0 || u.LockedUntil.Valid {
		t.Fatalf("a successful sign-in left lock state: %+v", u)
	}
	if _, ok := f.sessionRow(t, res.IDHash); !ok {
		t.Fatal("no session after the lock expired")
	}
}

func TestLoginWrongPasswordAfterTheLockExpiredStartsANewCount(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	for range maxFailures {
		_, _ = f.attempt("admin", "wrong wrong wrong")
	}
	f.clk.advance(lockout + time.Second)
	if _, err := f.attempt("admin", "wrong wrong wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatal(err)
	}
	if u := f.user(t, "admin"); u.FailedCount != 1 || u.LockedUntil.Valid {
		t.Fatalf("one wrong password after an expired lock: %+v, want count 1 and no lock", u)
	}
}

func TestLoginUnknownUserPaysExactlyOneDummyVerifyAndIsAudited(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	if _, err := f.attempt("nobody", "whatever whatever"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	if got := f.dummy.Load(); got != 1 {
		t.Fatalf("dummy verifications = %d, want exactly 1 (D-30)", got)
	}
	entries := f.audits(t, audit.ActionAuthLoginFailed)
	if len(entries) != 1 {
		t.Fatalf("auth.login_failed entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.ActorType != "anonymous" || e.TargetType != "user" || e.TargetID != "(unknown user)" || e.Reason != "unknown_user" || e.Result != "failure" || e.IP != loginIP || e.UserAgent != loginUA {
		t.Fatalf("entry = %+v", e)
	}
}

// What a refused sign-in records as its target is the stored username of an
// account that matched, or a fixed placeholder. Never the typed string, which is
// often a password (CR-01, D-39).
func TestLoginRecordsOnlyAKnownAccountNameAsTheTarget(t *testing.T) {
	long := strings.Repeat("x", 500)
	for _, tc := range []string{"nobody", "Tr0ub4dor&3 hunter", "hunter2hunter2", long} {
		f := newLoginFixture(t)
		f.addUser(t, "admin")
		if _, err := f.attempt(tc, "wrong wrong wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("username %q: %v", tc, err)
		}
		e := f.audits(t, audit.ActionAuthLoginFailed)
		if len(e) != 1 {
			t.Fatalf("username %q: %d entries, want 1", tc, len(e))
		}
		if e[0].TargetID != "(unknown user)" || e[0].Reason != "unknown_user" {
			t.Errorf("username %q: entry = %+v, want target (unknown user) and reason unknown_user", tc, e[0])
		}
		if want := `"name_chars":"` + strconv.Itoa(utf8.RuneCountInString(tc)) + `"`; !strings.Contains(e[0].Details, want) {
			t.Errorf("username %q: details = %s, want %s", tc, e[0].Details, want)
		}
		if strings.Contains(e[0].Details, "user_id") {
			t.Errorf("username %q: details = %s name an account that does not exist", tc, e[0].Details)
		}
		if strings.Contains(f.logs.String(), tc) {
			t.Errorf("username %q reached the container log", tc)
		}
	}

	// An account that exists keeps its stored name and ID, whatever case was typed.
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	if _, err := f.attempt("Admin", "wrong wrong wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatal(err)
	}
	e := f.audits(t, audit.ActionAuthLoginFailed)
	if len(e) != 1 || e[0].TargetID != "admin" || e[0].Reason != "bad_password" ||
		!strings.Contains(e[0].Details, `"user_id":"id-admin"`) || strings.Contains(e[0].Details, "name_chars") {
		t.Fatalf("entries = %+v, want target admin, reason bad_password, the user ID and no name_chars", e)
	}
}

func TestLoginRefusedByTheAddressLimitRecordsNoNameAtAll(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	tight := auth.NewIPLimiter(time.Hour, 1, time.Hour, f.clk.now)
	lg := auth.NewLogin(f.st, f.sess, f.aud, maxFailures, lockout, tight, slog.New(slog.DiscardHandler), f.clk.now)
	_ = tight.Allow("198.51.100.1") // spend the single token
	in := auth.LoginInput{Username: "Tr0ub4dor&3 hunter", Password: "wrong wrong wrong", IP: "198.51.100.1", UserAgent: loginUA}
	if _, err := lg.Login(context.Background(), in); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatal(err)
	}
	e := f.audits(t, audit.ActionAuthLoginFailed)
	if len(e) != 1 || e[0].TargetID != "(not checked)" || e[0].Reason != "rate_limited" || strings.Contains(e[0].Details, in.Username) {
		t.Fatalf("entries = %+v, want target (not checked) and reason rate_limited", e)
	}
}

func TestLoginMalformedUsernameIsTreatedAsUnknown(t *testing.T) {
	f := newLoginFixture(t)
	for _, name := range []string{"", "a", "bad name", "ünï", strings.Repeat("x", 500)} {
		before := f.dummy.Load()
		if _, err := f.attempt(name, loginPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("username %q: %v", name, err)
		}
		if got := f.dummy.Load() - before; got != 1 {
			t.Fatalf("username %q paid %d dummy verifications, want 1", name, got)
		}
	}
	for _, e := range f.audits(t, audit.ActionAuthLoginFailed) {
		if e.Reason != "unknown_user" || e.TargetID != "(unknown user)" {
			t.Fatalf("entry %+v: want reason unknown_user and the fixed target (unknown user)", e)
		}
	}
}

func TestLoginDisabledUserCannotSignInEvenWithTheRightPassword(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE username = 'admin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.attempt("admin", loginPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("disabled user: %v, want ErrInvalidCredentials", err)
	}
	if got := f.dummy.Load(); got != 1 {
		t.Fatalf("dummy verifications = %d, want 1", got)
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a session was created for a disabled user")
	}
	if e := f.audits(t, audit.ActionAuthLoginFailed); len(e) != 1 || e[0].Reason != "disabled" {
		t.Fatalf("entries = %+v, want one with reason disabled", e)
	}
}

func TestLoginEveryFailureReturnsTheSameError(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	f.addUser(t, "locked")
	f.addUser(t, "off")
	for range maxFailures {
		_, _ = f.attempt("locked", "wrong wrong wrong")
	}
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE username = 'off'`); err != nil {
		t.Fatal(err)
	}
	tight := auth.NewIPLimiter(time.Hour, 1, time.Hour, f.clk.now)
	limited := auth.NewLogin(f.st, f.sess, f.aud, maxFailures, lockout, tight, slog.New(slog.DiscardHandler), f.clk.now)
	limited.SetDummyVerify(func(string) {})
	_ = tight.Allow("198.51.100.1") // spend the single token

	cases := map[string]error{}
	_, cases["unknown"] = f.attempt("nobody", loginPassword)
	_, cases["wrong password"] = f.attempt("admin", "wrong wrong wrong")
	_, cases["locked"] = f.attempt("locked", loginPassword)
	_, cases["disabled"] = f.attempt("off", loginPassword)
	_, cases["rate limited"] = limited.Login(context.Background(), auth.LoginInput{
		Username: "admin", Password: loginPassword, IP: "198.51.100.1", UserAgent: loginUA,
	})
	for name, err := range cases {
		if err != auth.ErrInvalidCredentials { //nolint:errorlint // the point is that it is the one sentinel, unwrapped
			t.Errorf("%s: %v, want the bare ErrInvalidCredentials", name, err)
		}
	}
}

func TestLoginPerIPLimitRefusesEvenTheRightPasswordAndIsAudited(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	f.lim = auth.NewIPLimiter(time.Minute, 2, time.Hour, f.clk.now)
	lg := auth.NewLogin(f.st, f.sess, f.aud, maxFailures, lockout, f.lim, slog.New(slog.DiscardHandler), f.clk.now)
	in := auth.LoginInput{Username: "admin", Password: "wrong wrong wrong", IP: "198.51.100.7", UserAgent: loginUA}
	for range 2 {
		if _, err := lg.Login(context.Background(), in); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	in.Password = loginPassword
	if _, err := lg.Login(context.Background(), in); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("third attempt in a minute: %v, want ErrInvalidCredentials", err)
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a rate-limited attempt signed in")
	}
	var limited int
	for _, e := range f.audits(t, audit.ActionAuthLoginFailed) {
		if e.Reason == "rate_limited" {
			limited++
		}
	}
	if limited != 1 {
		t.Fatalf("rate_limited entries = %d, want 1", limited)
	}
	// Another address is unaffected.
	in.IP = "198.51.100.8"
	if _, err := lg.Login(context.Background(), in); err != nil {
		t.Fatalf("a different address: %v", err)
	}
}

func TestLoginNeverWritesThePasswordToTheAuditLogOrTheLog(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	const secret = "s3cret-attempt-value-123"
	_, _ = f.attempt("admin", secret)
	_, _ = f.attempt("nobody", secret)
	// The secret typed into the username box, as people do (CR-01).
	_, _ = f.attemptFrom(secret, loginPassword, loginIP, false, nil)
	for range maxFailures {
		_, _ = f.attempt("admin", secret)
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Fatal("an attempted password reached the log")
	}
	var hits int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE details LIKE ?1 OR reason LIKE ?1 OR target_id LIKE ?1 OR actor_name LIKE ?1`,
		"%"+secret+"%").Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatal("an attempted password reached the audit log")
	}
}

func TestLoginBadPasswordIsAuditedWithAReason(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	_, _ = f.attempt("admin", "wrong wrong wrong")
	e := f.audits(t, audit.ActionAuthLoginFailed)
	if len(e) != 1 || e[0].Reason != "bad_password" || e[0].ActorType != "anonymous" || e[0].TargetID != "admin" || e[0].IP != loginIP {
		t.Fatalf("entries = %+v", e)
	}
}

// --- Logout ----------------------------------------------------------------

func TestLogoutEndsOnlyTheCurrentSessionAndIsAudited(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	a := f.mustLogin(t, "admin", false)
	b := f.mustLogin(t, "admin", true)

	p := &rbac.Principal{Type: rbac.PrincipalUser, ID: "id-admin", Name: "admin", SessionIDHash: a.IDHash}
	if err := f.lg.Logout(context.Background(), p, loginIP, loginUA); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.sessionRow(t, a.IDHash); ok {
		t.Fatal("the signed-out session still exists")
	}
	if _, ok := f.sessionRow(t, b.IDHash); !ok {
		t.Fatal("logging out one device ended another (D-33)")
	}
	e := f.audits(t, audit.ActionAuthLogout)
	if len(e) != 1 || e[0].ActorID != "id-admin" || e[0].ActorType != "user" || e[0].IP != loginIP || e[0].UserAgent != loginUA {
		t.Fatalf("auth.logout entries = %+v", e)
	}
}

func TestLogoutWithoutASessionIsRefused(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	if err := f.lg.Logout(context.Background(), nil, loginIP, loginUA); err == nil {
		t.Fatal("logout of a nil principal succeeded")
	}
	if err := f.lg.Logout(context.Background(), &rbac.Principal{Type: rbac.PrincipalUser, ID: "id-admin"}, loginIP, loginUA); err == nil {
		t.Fatal("logout of a principal without a session succeeded")
	}
	if len(f.audits(t, audit.ActionAuthLogout)) != 0 {
		t.Fatal("a refused logout was audited as a logout")
	}
}

// --- Authenticator ---------------------------------------------------------

func (f *loginFixture) authenticator() interface {
	Authenticate(*http.Request) (*rbac.Principal, error)
} {
	return auth.NewAuthenticator(f.st, testIdle, f.clk.now)
}

func addCookie(req *http.Request, name, value string) {
	req.AddCookie(&http.Cookie{Name: name, Value: value}) //nolint:gosec // a request cookie as a browser sends it: only name and value travel
}

func cookieRequest(name, value string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	if name != "" {
		addCookie(req, name, value)
	}
	return req
}

func TestAuthenticateFreshSessionReturnsThePrincipal(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", false)
	for _, name := range []string{auth.CookieName(true), auth.CookieName(false)} {
		p, err := f.authenticator().Authenticate(cookieRequest(name, res.RawSessionID))
		if err != nil || p == nil {
			t.Fatalf("cookie %s: %v, %v", name, p, err)
		}
		if p.Type != rbac.PrincipalUser || p.ID != "id-admin" || p.Name != "admin" || string(p.SessionIDHash) != string(res.IDHash) {
			t.Fatalf("principal = %+v", p)
		}
	}
}

func TestAuthenticateWithoutAValidCookieIsAnonymous(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", false)
	for name, req := range map[string]*http.Request{
		"no cookie":        cookieRequest("", ""),
		"empty value":      cookieRequest(auth.CookieName(true), ""),
		"unknown id":       cookieRequest(auth.CookieName(true), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
		"garbage":          cookieRequest(auth.CookieName(false), "!!!not base64!!!"),
		"the hash itself":  cookieRequest(auth.CookieName(true), string(res.IDHash)),
		"a different name": cookieRequest("other_session", res.RawSessionID),
	} {
		p, err := f.authenticator().Authenticate(req)
		if err != nil || p != nil {
			t.Errorf("%s: principal %+v, error %v; want anonymous", name, p, err)
		}
	}
}

func TestAuthenticateBrowserSessionExpiresAfterTwelveHoursIdleAndIsDeleted(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", false)

	f.clk.advance(testIdle) // exactly 12h: still valid
	if p, err := f.authenticator().Authenticate(cookieRequest(auth.CookieName(true), res.RawSessionID)); err != nil || p == nil {
		t.Fatalf("at exactly 12h idle: %v, %v; want a principal", p, err)
	}
	// That call touched the session, so measure idleness from now.
	f.clk.advance(testIdle + time.Second)
	p, err := f.authenticator().Authenticate(cookieRequest(auth.CookieName(true), res.RawSessionID))
	if err != nil || p != nil {
		t.Fatalf("12h and 1s idle: %v, %v; want anonymous", p, err)
	}
	if _, ok := f.sessionRow(t, res.IDHash); ok {
		t.Fatal("an expired session was not deleted")
	}
}

func TestAuthenticateRememberedSessionIgnoresIdleTimeAndEndsAtTheAbsoluteLimit(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", true)
	req := func() *http.Request { return cookieRequest(auth.CookieName(true), res.RawSessionID) }

	f.clk.advance(13 * time.Hour) // idle for longer than 12h: still fine (D-31)
	if p, err := f.authenticator().Authenticate(req()); err != nil || p == nil {
		t.Fatalf("remembered, 13h idle: %v, %v; want a principal", p, err)
	}
	f.clk.advance(testRemember - 13*time.Hour) // exactly the absolute limit
	if p, err := f.authenticator().Authenticate(req()); err != nil || p == nil {
		t.Fatalf("at exactly 30 days: %v, %v; want a principal", p, err)
	}
	f.clk.advance(time.Second)
	if p, err := f.authenticator().Authenticate(req()); err != nil || p != nil {
		t.Fatalf("past 30 days: %v, %v; want anonymous", p, err)
	}
	if _, ok := f.sessionRow(t, res.IDHash); ok {
		t.Fatal("a session past its absolute expiry was not deleted")
	}
}

func TestAuthenticateTouchesLastSeenAtMostOncePerMinute(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", false)
	t0 := f.clk.now().UnixNano()
	seen := func() int64 {
		row, ok := f.sessionRow(t, res.IDHash)
		if !ok {
			t.Fatal("session vanished")
		}
		return row.LastSeenAt
	}
	check := func() {
		t.Helper()
		if p, err := f.authenticator().Authenticate(cookieRequest(auth.CookieName(true), res.RawSessionID)); err != nil || p == nil {
			t.Fatalf("authenticate: %v, %v", p, err)
		}
	}

	f.clk.advance(30 * time.Second)
	check()
	if got := seen(); got != t0 {
		t.Fatalf("a use 30s after the last one wrote last_seen_at (%d != %d)", got, t0)
	}
	f.clk.advance(2 * time.Minute)
	check()
	touched := seen()
	if touched != f.clk.now().UnixNano() {
		t.Fatalf("a use 2m30s later did not touch: %d, want %d", touched, f.clk.now().UnixNano())
	}
	f.clk.advance(10 * time.Second)
	check()
	if got := seen(); got != touched {
		t.Fatalf("a second use 10s after a touch wrote again (%d != %d)", got, touched)
	}
}

func TestAuthenticateDisabledUserIsAnonymous(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", false)
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE username = 'admin'`); err != nil {
		t.Fatal(err)
	}
	p, err := f.authenticator().Authenticate(cookieRequest(auth.CookieName(true), res.RawSessionID))
	if err != nil || p != nil {
		t.Fatalf("disabled user's session: %v, %v; want anonymous", p, err)
	}
}

func TestAuthenticatePrefersTheHostPrefixedCookie(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	f.addUser(t, "other")
	good := f.mustLogin(t, "admin", false)
	planted := f.mustLogin(t, "other", false)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	addCookie(req, auth.CookieName(false), planted.RawSessionID)
	addCookie(req, auth.CookieName(true), good.RawSessionID)
	p, err := f.authenticator().Authenticate(req)
	if err != nil || p == nil || p.Name != "admin" {
		t.Fatalf("both cookies present: %+v, %v; want the __Host- one (admin)", p, err)
	}

	// A bare cookie never overrides a __Host- cookie that does not resolve.
	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	addCookie(req, auth.CookieName(true), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	addCookie(req, auth.CookieName(false), planted.RawSessionID)
	if p, err := f.authenticator().Authenticate(req); err != nil || p != nil {
		t.Fatalf("a bad __Host- cookie fell through to the bare one: %+v, %v", p, err)
	}
}

// --- CSRF guard ------------------------------------------------------------

func TestCSRFGuardRequiresTheSessionsOwnToken(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	a := f.mustLogin(t, "admin", false)
	b := f.mustLogin(t, "admin", false)
	guard := auth.NewCSRFGuard(f.sess)
	p := &rbac.Principal{Type: rbac.PrincipalUser, ID: "id-admin", Name: "admin", SessionIDHash: a.IDHash}

	req := func(token string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
		if token != "" {
			r.Header.Set("X-CSRF-Token", token)
		}
		return r
	}
	if err := guard.Check(req(f.sess.CSRFToken(a.IDHash)), p); err != nil {
		t.Fatalf("the right token: %v", err)
	}
	for name, token := range map[string]string{
		"missing":                    "",
		"wrong":                      "not-the-token",
		"another session's token":    f.sess.CSRFToken(b.IDHash),
		"the right token plus extra": f.sess.CSRFToken(a.IDHash) + "x",
		"the right token truncated":  f.sess.CSRFToken(a.IDHash)[:10],
	} {
		if err := guard.Check(req(token), p); !errors.Is(err, auth.ErrCSRF) {
			t.Errorf("%s: %v, want ErrCSRF", name, err)
		}
	}
}

func TestCSRFGuardRefusesAPrincipalWithoutASessionAndSkipsAnonymous(t *testing.T) {
	f := newLoginFixture(t)
	guard := auth.NewCSRFGuard(f.sess)
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	if err := guard.Check(r, nil); err != nil {
		t.Fatalf("anonymous caller: %v, want no check (the Origin guard covers public mutations)", err)
	}
	r.Header.Set("X-CSRF-Token", "anything")
	if err := guard.Check(r, &rbac.Principal{Type: rbac.PrincipalUser, ID: "u"}); !errors.Is(err, auth.ErrCSRF) {
		t.Fatalf("a principal with no session: %v, want ErrCSRF", err)
	}
}

// --- Unlock ----------------------------------------------------------------

func TestUnlockClearsTheLockAndAuditsAsSystemCLI(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	for range maxFailures {
		_, _ = f.attempt("admin", "wrong wrong wrong")
	}
	if !f.user(t, "admin").LockedUntil.Valid {
		t.Fatal("setup: the account is not locked")
	}
	if err := auth.Unlock(context.Background(), f.st, f.aud, "Admin"); err != nil {
		t.Fatal(err)
	}
	if u := f.user(t, "admin"); u.FailedCount != 0 || u.LockedUntil.Valid {
		t.Fatalf("after unlock: %+v", u)
	}
	e := f.audits(t, audit.ActionAuthUnlocked)
	if len(e) != 1 || e[0].ActorType != "system" || e[0].ActorID != "system:cli" || e[0].TargetType != "user" || e[0].TargetID != "id-admin" {
		t.Fatalf("auth.unlocked entries = %+v", e)
	}
	if _, err := f.attempt("admin", loginPassword); err != nil {
		t.Fatalf("sign-in right after unlock: %v", err)
	}
}

func TestUnlockOfAnUnknownUserIsErrUserNotFoundAndWritesNothing(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	for _, name := range []string{"nobody", "", "bad name", strings.Repeat("x", 200)} {
		if err := auth.Unlock(context.Background(), f.st, f.aud, name); !errors.Is(err, auth.ErrUserNotFound) {
			t.Errorf("Unlock(%q) = %v, want ErrUserNotFound", name, err)
		}
	}
	if n := len(f.audits(t, audit.ActionAuthUnlocked)); n != 0 {
		t.Fatalf("%d auth.unlocked entries for unknown users", n)
	}
}

// --- cleanup ---------------------------------------------------------------

func TestRunSessionCleanupDeletesExpiredSessionsAndStopsWithItsContext(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	stale := f.mustLogin(t, "admin", false)
	f.clk.advance(testIdle + time.Minute)
	fresh := f.mustLogin(t, "admin", false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		auth.RunSessionCleanup(ctx, f.st, testIdle, f.clk.now)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := f.sessionRow(t, stale.IDHash); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle session was not cleaned up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := f.sessionRow(t, fresh.IDHash); !ok {
		t.Fatal("cleanup deleted a live session")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSessionCleanup did not return after its context was cancelled")
	}
}
