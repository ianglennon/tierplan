// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/access"
)

// Sessions: listing, revoking one, revoking the others, revoking all (plan
// 03-15, AUTH-04, D-23). Negative cases are mandatory (D-81).

func (f *ownedFixture) addSessionAt(t *testing.T, userID, publicID string, lastSeen int64, ip, ua string) []byte {
	t.Helper()
	h := []byte("hash-" + publicID)
	f.exec(t, `INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, absolute_expires_at, remember, ip, user_agent, public_id)
		VALUES (?, ?, 1, ?, NULL, 0, ?, ?, ?)`, h, userID, lastSeen, ip, ua, publicID)
	return h
}

func TestListSessionsAnswersTheUsersOwnNewestFirstAndMarksTheCurrentOne(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	cur := f.addSessionAt(t, "alice", "pub-alice-1", 100, "203.0.113.1", "UA-one")
	f.addSessionAt(t, "alice", "pub-alice-2", 200, "203.0.113.2", "UA-two")
	f.addSessionAt(t, "bob", "pub-bob-1", 300, "203.0.113.3", "UA-bob")

	got, err := f.svc.ListSessions(f.ctx, "alice", cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PublicID != "pub-alice-2" || got[1].PublicID != "pub-alice-1" {
		t.Fatalf("sessions = %+v, want alice's two, most recently active first", got)
	}
	if got[0].Current || !got[1].Current {
		t.Fatalf("current flags = %v %v, want only the second row current", got[0].Current, got[1].Current)
	}
	if got[0].IP != "203.0.113.2" || got[0].UserAgent != "UA-two" || got[0].LastSeenAt.UnixNano() != 200 {
		t.Fatalf("session = %+v", got[0])
	}
}

func TestListSessionsWithoutACurrentSessionMarksNone(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addSessionAt(t, "alice", "pub-1", 1, "", "")
	got, err := f.svc.ListSessions(f.ctx, "alice", nil)
	if err != nil || len(got) != 1 || got[0].Current {
		t.Fatalf("sessions = %+v, %v; want one, not current", got, err)
	}
}

func TestRevokeSessionDeletesOnlyThatSessionAndAuditsItWithTheHandleAndDevice(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	cur := f.addSessionAt(t, "alice", "pub-1", 1, "", "")
	f.addSessionAt(t, "alice", "pub-2", 2, "", strings.Repeat("x", 300))

	if err := f.svc.RevokeSession(f.ctx, testAdmin, "alice", "pub-2", cur); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'alice'`); n != 1 {
		t.Fatalf("sessions left = %d, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM sessions WHERE public_id = 'pub-1'`); n != 1 {
		t.Fatal("the current session was removed")
	}
	got := f.auditDetails(t, "session.revoked")
	if !strings.Contains(got, `"handle":"pub-2"`) || !strings.Contains(got, `"username":"alice"`) {
		t.Fatalf("details = %s, want the username and the handle", got)
	}
	if !strings.Contains(got, `"device":"`+strings.Repeat("x", 120)+`"`) || strings.Contains(got, strings.Repeat("x", 121)) {
		t.Fatalf("details = %s, want the device cut to 120 characters", got)
	}
	if strings.Contains(got, "session_id") || strings.Contains(got, "hash") {
		t.Fatalf("details = %s, must not use secret words", got)
	}
}

func TestRevokeSessionRefusesTheCurrentSessionAndAnotherUsersAndAnUnknownOne(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	cur := f.addSessionAt(t, "alice", "pub-1", 1, "", "")
	f.addSessionAt(t, "bob", "pub-bob", 1, "", "")

	if err := f.svc.RevokeSession(f.ctx, testAdmin, "alice", "pub-1", cur); !errors.Is(err, access.ErrCurrentSession) {
		t.Fatalf("revoking the current session = %v, want ErrCurrentSession", err)
	}
	if err := f.svc.RevokeSession(f.ctx, testAdmin, "alice", "pub-bob", cur); !errors.Is(err, access.ErrSessionNotFound) {
		t.Fatalf("revoking another user's session = %v, want ErrSessionNotFound", err)
	}
	if err := f.svc.RevokeSession(f.ctx, testAdmin, "alice", "no-such", cur); !errors.Is(err, access.ErrSessionNotFound) {
		t.Fatalf("revoking an unknown session = %v, want ErrSessionNotFound", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM sessions`); n != 2 {
		t.Fatalf("sessions = %d, want both untouched", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'session.revoked'`); n != 0 {
		t.Fatalf("audit entries = %d, want none for refusals", n)
	}
}

func TestRevokeOtherSessionsKeepsTheCurrentOneCountsAndAudits(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	cur := f.addSessionAt(t, "alice", "pub-1", 1, "", "")
	f.addSessionAt(t, "alice", "pub-2", 2, "", "")
	f.addSessionAt(t, "alice", "pub-3", 3, "", "")
	f.addSessionAt(t, "bob", "pub-bob", 1, "", "")

	n, err := f.svc.RevokeOtherSessions(f.ctx, testAdmin, "alice", cur)
	if err != nil || n != 2 {
		t.Fatalf("RevokeOtherSessions = %d, %v; want 2", n, err)
	}
	if c := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'alice'`); c != 1 {
		t.Fatalf("alice has %d sessions, want only the current one", c)
	}
	if c := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'bob'`); c != 1 {
		t.Fatal("another user's session was removed")
	}
	if got := f.auditDetails(t, "user.sessions_revoked"); got != `{"ended":"2","username":"alice"}` {
		t.Fatalf("details = %s", got)
	}
}

func TestRevokeOtherSessionsWithNothingElseWritesNoEntry(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	cur := f.addSessionAt(t, "alice", "pub-1", 1, "", "")
	n, err := f.svc.RevokeOtherSessions(f.ctx, testAdmin, "alice", cur)
	if err != nil || n != 0 {
		t.Fatalf("RevokeOtherSessions = %d, %v; want 0", n, err)
	}
	if c := f.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'user.sessions_revoked'`); c != 0 {
		t.Fatalf("audit entries = %d, want none when nothing ended", c)
	}
}

func TestRevokeAllSessionsEndsEveryOneOfTheUserAndAudits(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	f.addSessionAt(t, "alice", "pub-1", 1, "", "")
	f.addSessionAt(t, "alice", "pub-2", 2, "", "")
	f.addSessionAt(t, "bob", "pub-bob", 1, "", "")

	n, err := f.svc.RevokeAllSessions(f.ctx, testAdmin, "alice")
	if err != nil || n != 2 {
		t.Fatalf("RevokeAllSessions = %d, %v; want 2", n, err)
	}
	if c := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'alice'`); c != 0 {
		t.Fatalf("alice has %d sessions, want none", c)
	}
	if c := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'bob'`); c != 1 {
		t.Fatal("another user's session was removed")
	}
	if got := f.auditDetails(t, "user.sessions_revoked"); got != `{"ended":"2","username":"alice"}` {
		t.Fatalf("details = %s", got)
	}
}

func TestRevokeAllSessionsOfAnUnknownOrDeletedUserIsNotFound(t *testing.T) {
	f := newOwnedFixture(t)
	if _, err := f.svc.RevokeAllSessions(f.ctx, testAdmin, "nobody"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("RevokeAllSessions(unknown) = %v, want ErrNotFound", err)
	}
	f.addUser(t, "carol")
	f.exec(t, `UPDATE users SET deleted_at = 5 WHERE id = 'carol'`)
	if _, err := f.svc.RevokeAllSessions(f.ctx, testAdmin, "carol"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("RevokeAllSessions(deleted) = %v, want ErrNotFound", err)
	}
}
