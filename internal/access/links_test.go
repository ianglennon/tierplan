// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package access_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/access"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/auth"
)

// One-time links (plan 03-11, AUTH-04, D-22a). Negative cases are mandatory (D-81).

const chosenPassword = "a long chosen passphrase"

func (f *fixture) invite(t *testing.T, name string) (userID string, link access.Link) {
	t.Helper()
	u, link, err := f.svc.CreateUser(context.Background(), testAdmin, access.CreateUserInput{Username: name})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID, link
}

func TestSingleUseSecondAcceptOfTheSameLinkIsInvalid(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, link := f.invite(t, "alice")

	if err := f.svc.AcceptLink(ctx, link.Token, chosenPassword, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AcceptLink(ctx, link.Token, "another long passphrase", "", ""); !errors.Is(err, access.ErrInvalidLink) {
		t.Fatalf("second accept = %v, want ErrInvalidLink", err)
	}
	if _, err := f.svc.LookupLink(ctx, link.Token); !errors.Is(err, access.ErrInvalidLink) {
		t.Fatalf("look-up of a spent link = %v, want ErrInvalidLink", err)
	}
	// The first password stayed.
	var hash string
	if err := f.st.Read.QueryRow(`SELECT password_hash FROM users WHERE username = 'alice'`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if ok, _ := auth.VerifyPassword(chosenPassword, hash); !ok {
		t.Fatal("the second accept replaced the password")
	}
}

func TestConcurrentAcceptsOfOneLinkLeaveExactlyOneSuccess(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, link := f.invite(t, "alice")

	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = f.svc.AcceptLink(ctx, link.Token, chosenPassword+strings.Repeat("x", i), "", "")
		}()
	}
	close(start)
	wg.Wait()

	var ok, invalid int
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, access.ErrInvalidLink):
			invalid++
		default:
			t.Errorf("accept %d: %v, want nil or ErrInvalidLink", i, err)
		}
	}
	if ok != 1 || invalid != n-1 {
		t.Fatalf("%d succeeded and %d were invalid, want 1 and %d", ok, invalid, n-1)
	}
	var accepted int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'user.invite_accepted'`).Scan(&accepted); err != nil || accepted != 1 {
		t.Fatalf("user.invite_accepted rows = %d, %v, want exactly one", accepted, err)
	}
}

func TestUniformEveryUnusableLinkIsTheSameError(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, expired := f.invite(t, "expired")
	_, replacedOld := f.invite(t, "replaced")
	_, disabled := f.invite(t, "disabled")
	deletedID, deleted := f.invite(t, "deleted")

	// A newer link for the same user replaces the old one.
	replacedID := mustUserID(t, f, "replaced")
	newer, err := f.reissue(ctx, replacedID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE username = 'disabled'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = ? WHERE id = ?`, testNow.UnixNano(), deletedID); err != nil {
		t.Fatal(err)
	}
	// Past the 72 hours of the first link only; the rest are re-dated to still be live.
	f.clk.t = testNow.Add(73 * time.Hour)
	if _, err := f.st.Write.Exec(`UPDATE user_links SET expires_at = ? WHERE user_id != (SELECT id FROM users WHERE username = 'expired')`, f.clk.t.Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"expired":  expired.Token,
		"replaced": replacedOld.Token,
		"unknown":  strings.Repeat("A", 43),
		"short":    "abc",
		"alphabet": strings.Repeat("!", 43),
		"empty":    "",
		"disabled": disabled.Token,
		"deleted":  deleted.Token,
	}
	for name, token := range cases {
		_, lookupErr := f.svc.LookupLink(ctx, token)
		acceptErr := f.svc.AcceptLink(ctx, token, chosenPassword, "", "")
		if lookupErr != access.ErrInvalidLink || acceptErr != access.ErrInvalidLink { //nolint:errorlint // the point is that it is the one value, not a wrapped one
			t.Errorf("%s: lookup %v, accept %v, want exactly ErrInvalidLink from both", name, lookupErr, acceptErr)
		}
	}
	// The newer link of the replaced user still works, so the refusals above were about the old one.
	if err := f.svc.AcceptLink(ctx, newer.Token, chosenPassword, "", ""); err != nil {
		t.Fatalf("the replacing link: %v", err)
	}
}

func TestAcceptRefusesAPasswordTheLinkIsNotSpentBy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, link := f.invite(t, "alice")

	if err := f.svc.AcceptLink(ctx, link.Token, "short", "", ""); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Fatalf("short password = %v, want ErrPasswordTooShort", err)
	}
	if err := f.svc.AcceptLink(ctx, link.Token, "xx alice xx xx", "", ""); !errors.Is(err, auth.ErrPasswordCommon) {
		t.Fatalf("password with the username = %v, want ErrPasswordCommon", err)
	}
	if err := f.svc.AcceptLink(ctx, link.Token, chosenPassword, "", ""); err != nil {
		t.Fatalf("accept after refused passwords: %v", err)
	}
}

func TestNoAuditEntryHoldsTheTokenOrThePassword(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, link := f.invite(t, "alice")
	if err := f.svc.AcceptLink(ctx, link.Token, chosenPassword, "203.0.113.9", "ua"); err != nil {
		t.Fatal(err)
	}
	_ = f.svc.AcceptLink(ctx, link.Token, chosenPassword, "203.0.113.9", "ua") // refused

	rows, err := f.st.Read.Query(`SELECT action, actor_id, actor_name, target_type, target_id, reason, ip, user_agent, details FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var action string
		cols := make([]string, 8)
		if err := rows.Scan(&action, &cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5], &cols[6], &cols[7]); err != nil {
			t.Fatal(err)
		}
		n++
		for _, c := range cols {
			if strings.Contains(c, link.Token) || strings.Contains(c, chosenPassword) {
				t.Fatalf("audit entry %s holds a secret: %q", action, c)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("scanned %d audit rows, want at least user.created, user.invite_created and user.invite_accepted", n)
	}
}

func TestInviteTTLIsHowLongALinkWorks(t *testing.T) {
	f := newFixture(t)
	f.svc.InviteTTL = time.Hour
	ctx := context.Background()
	_, link := f.invite(t, "alice")
	if !link.ExpiresAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("expires %v, want one hour after creation", link.ExpiresAt)
	}
	f.clk.t = testNow.Add(time.Hour) // expires_at == now is no longer valid
	if _, err := f.svc.LookupLink(ctx, link.Token); !errors.Is(err, access.ErrInvalidLink) {
		t.Fatalf("look-up at the expiry instant = %v, want ErrInvalidLink", err)
	}
}

// Regenerating an invite (plan 03-12, D-22a).

func TestRegenerateInviteReplacesTheLinkSoTheOldTokenIsInvalid(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, old := f.invite(t, "alice")
	f.clk.t = f.clk.t.Add(time.Hour)

	username, fresh, err := f.svc.RegenerateInvite(ctx, testAdmin, id)
	if err != nil {
		t.Fatalf("RegenerateInvite: %v", err)
	}
	if username != "alice" || fresh.Token == "" || fresh.Token == old.Token || len(fresh.Token) != 43 {
		t.Fatalf("got %q and %+v, want alice and a new 43 character token", username, fresh)
	}
	if want := f.clk.t.Add(72 * time.Hour); !fresh.ExpiresAt.Equal(want) {
		t.Fatalf("expires %v, want a full TTL from now (%v)", fresh.ExpiresAt, want)
	}
	if _, err := f.svc.LookupLink(ctx, old.Token); !errors.Is(err, access.ErrInvalidLink) {
		t.Fatalf("the old link answers %v, want ErrInvalidLink", err)
	}
	if err := f.svc.AcceptLink(ctx, old.Token, chosenPassword, "", ""); !errors.Is(err, access.ErrInvalidLink) {
		t.Fatalf("accepting the old link = %v, want ErrInvalidLink", err)
	}
	if info, err := f.svc.LookupLink(ctx, fresh.Token); err != nil || info.Username != "alice" || info.Kind != access.LinkInvite {
		t.Fatalf("the new link: %+v, %v", info, err)
	}
	var links int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM user_links WHERE user_id = ?`, id).Scan(&links); err != nil || links != 1 {
		t.Fatalf("links held = %d, %v, want exactly one", links, err)
	}
}

func TestRegenerateInviteRevivesAnExpiredInvite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, _ := f.invite(t, "alice")
	f.clk.t = f.clk.t.Add(100 * time.Hour)
	if list, _ := f.svc.ListUsers(ctx); len(list) != 1 || list[0].Status != access.StatusInviteExpired {
		t.Fatalf("before: %+v, want invite_expired", list)
	}
	if _, _, err := f.svc.RegenerateInvite(ctx, testAdmin, id); err != nil {
		t.Fatalf("RegenerateInvite: %v", err)
	}
	if list, _ := f.svc.ListUsers(ctx); len(list) != 1 || list[0].Status != access.StatusInvitePending {
		t.Fatalf("after: %+v, want invite_pending", list)
	}
}

func TestRegenerateInviteIsAuditedWithTheUsernameAndExpiryAndNoToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, old := f.invite(t, "alice")
	_, fresh, err := f.svc.RegenerateInvite(ctx, testAdmin, id)
	if err != nil {
		t.Fatalf("RegenerateInvite: %v", err)
	}

	var actor, target, details string
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'user.invite_regenerated'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("user.invite_regenerated rows = %d, %v, want one", n, err)
	}
	if err := f.st.Read.QueryRow(`SELECT actor_id, target_id, details FROM audit_log WHERE action = 'user.invite_regenerated'`).Scan(&actor, &target, &details); err != nil {
		t.Fatal(err)
	}
	if actor != "id-admin" || target != id {
		t.Fatalf("actor %q target %q, want the admin and the user", actor, target)
	}
	for _, want := range []string{"alice", "expires_at"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details %q lack %q", details, want)
		}
	}
	if strings.Contains(details, fresh.Token) || strings.Contains(details, old.Token) {
		t.Fatalf("details %q hold a token", details)
	}
}

func TestRegenerateInviteRefusesAUserWhoHasSetAPasswordAndLeavesTheirStateAlone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, link := f.invite(t, "alice")
	if err := f.svc.AcceptLink(ctx, link.Token, chosenPassword, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.RegenerateInvite(ctx, testAdmin, id); !errors.Is(err, access.ErrNotInvited) {
		t.Fatalf("RegenerateInvite for a user with a password = %v, want ErrNotInvited", err)
	}
	var links, audited int
	_ = f.st.Read.QueryRow(`SELECT COUNT(*) FROM user_links WHERE user_id = ?`, id).Scan(&links)
	_ = f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'user.invite_regenerated'`).Scan(&audited)
	if links != 0 || audited != 0 {
		t.Fatalf("links = %d, audit rows = %d, want a refusal to change nothing", links, audited)
	}
}

func TestRegenerateInviteRefusesAnUnknownOrDeletedUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.svc.RegenerateInvite(ctx, testAdmin, "no-such-id"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("unknown id = %v, want ErrNotFound", err)
	}
	id, _ := f.invite(t, "gone")
	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = ? WHERE id = ?`, f.clk.t.UnixNano(), id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.RegenerateInvite(ctx, testAdmin, id); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("deleted user = %v, want ErrNotFound", err)
	}
}

// --- password reset (plan 03-15, AUTH-04, D-22b) ------------------------------

func TestLinkURLPutsTheTokenInTheFragmentAfterThePublicURLOrTheBasePath(t *testing.T) {
	const tok = "TOKEN"
	for _, c := range []struct{ public, base, want string }{
		{"https://tierplan.example.test", "", "https://tierplan.example.test/invite#token=TOKEN"},
		{"https://tierplan.example.test/", "", "https://tierplan.example.test/invite#token=TOKEN"},
		{"https://example.test/tierplan", "/tierplan", "https://example.test/tierplan/invite#token=TOKEN"},
		{"", "", "/invite#token=TOKEN"},
		{"", "/tierplan", "/tierplan/invite#token=TOKEN"},
	} {
		if got := access.LinkURL(c.public, c.base, tok); got != c.want {
			t.Errorf("LinkURL(%q, %q) = %q, want %q", c.public, c.base, got, c.want)
		}
	}
}

func (f *fixture) addSession(t *testing.T, userID, id string) {
	t.Helper()
	if _, err := f.st.Write.Exec(`INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, absolute_expires_at, remember, ip, user_agent, public_id)
		VALUES (?, ?, 1, 1, NULL, 0, '', '', ?)`, []byte(id), userID, id); err != nil {
		t.Fatal(err)
	}
}

// activeUser is a user who has set a password, with two sessions.
func (f *fixture) activeUser(t *testing.T, name string) string {
	t.Helper()
	id, link := f.invite(t, name)
	if err := f.svc.AcceptLink(context.Background(), link.Token, chosenPassword, "", ""); err != nil {
		t.Fatal(err)
	}
	f.addSession(t, id, name+"-1")
	f.addSession(t, id, name+"-2")
	return id
}

func TestIssueResetEndsEverySessionAndIssuesAResetLinkThatSetsANewPassword(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.activeUser(t, "alice")
	other := f.activeUser(t, "bob")

	username, link, err := f.svc.IssueReset(ctx, testAdmin, id)
	if err != nil {
		t.Fatalf("IssueReset: %v", err)
	}
	if username != "alice" || len(link.Token) != 43 || !link.ExpiresAt.Equal(testNow.Add(72*time.Hour)) {
		t.Fatalf("got %q and %+v, want alice and a 43 character token valid for 72 hours", username, link)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the user has %d sessions left (%v), want none", n, err)
	}
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, other).Scan(&n); err != nil || n != 2 {
		t.Fatalf("another user's sessions = %d (%v), want them untouched", n, err)
	}
	info, err := f.svc.LookupLink(ctx, link.Token)
	if err != nil || info.Kind != access.LinkReset || info.Username != "alice" {
		t.Fatalf("LookupLink = %+v, %v; want a reset link for alice", info, err)
	}

	const next = "a brand new passphrase 99"
	if err := f.svc.AcceptLink(ctx, link.Token, next, "", ""); err != nil {
		t.Fatalf("AcceptLink: %v", err)
	}
	var hash string
	if err := f.st.Read.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if ok, _ := auth.VerifyPassword(next, hash); !ok {
		t.Fatal("the new password does not verify")
	}
	if ok, _ := auth.VerifyPassword(chosenPassword, hash); ok {
		t.Fatal("the old password still verifies")
	}
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'user.password_reset_completed'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("user.password_reset_completed entries = %d (%v), want 1", n, err)
	}
}

func TestIssueResetIsAuditedWithTheSessionsEndedAndTheExpiryAndNeverTheToken(t *testing.T) {
	f := newFixture(t)
	id := f.activeUser(t, "alice")
	_, link, err := f.svc.IssueReset(context.Background(), testAdmin, id, access.WithOrigin("203.0.113.9", "ua/1"))
	if err != nil {
		t.Fatal(err)
	}
	var actor, target, ip, details string
	if err := f.st.Read.QueryRow(`SELECT actor_id, target_id, ip, details FROM audit_log WHERE action = 'user.password_reset_issued'`).Scan(&actor, &target, &ip, &details); err != nil {
		t.Fatalf("no user.password_reset_issued entry: %v", err)
	}
	if actor != "id-admin" || target != id || ip != "203.0.113.9" {
		t.Fatalf("entry = actor %q target %q ip %q", actor, target, ip)
	}
	want := `{"ended":"2","expires_at":"` + link.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00") + `","username":"alice"}`
	if details != want {
		t.Fatalf("details = %s, want %s", details, want)
	}
	if strings.Contains(details, link.Token) {
		t.Fatal("the audit entry holds the token")
	}
}

func TestIssueResetReplacesTheUsersEarlierLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.activeUser(t, "alice")
	_, first, err := f.svc.IssueReset(ctx, testAdmin, id)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := f.svc.IssueReset(ctx, testAdmin, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.LookupLink(ctx, first.Token); !errors.Is(err, access.ErrInvalidLink) {
		t.Fatalf("the earlier reset link answers %v, want ErrInvalidLink", err)
	}
	if _, err := f.svc.LookupLink(ctx, second.Token); err != nil {
		t.Fatalf("the newer link: %v", err)
	}
}

func TestIssueResetRefusesAnUnknownOrDeletedUserAndADisabledOneAndChangesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.svc.IssueReset(ctx, testAdmin, "no-such-user"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
	gone := f.activeUser(t, "gone")
	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = 5 WHERE id = ?`, gone); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.IssueReset(ctx, testAdmin, gone); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("deleted user = %v, want ErrNotFound", err)
	}
	off := f.activeUser(t, "off")
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, off); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.IssueReset(ctx, testAdmin, off); !errors.Is(err, access.ErrUserDisabled) {
		t.Fatalf("disabled user = %v, want ErrUserDisabled", err)
	}
	var sessions, links, entries int
	if err := f.st.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM sessions WHERE user_id IN (?, ?)), (SELECT COUNT(*) FROM user_links), (SELECT COUNT(*) FROM audit_log WHERE action = 'user.password_reset_issued')`, gone, off).Scan(&sessions, &links, &entries); err != nil {
		t.Fatal(err)
	}
	if sessions != 4 || links != 0 || entries != 0 {
		t.Fatalf("after refusals: sessions %d, links %d, entries %d; want 4, 0, 0", sessions, links, entries)
	}
}

func TestIssueResetWorksForTheOnlyInstanceAdmin(t *testing.T) {
	f := newFixture(t)
	id := f.activeUser(t, "root")
	if _, err := f.st.Write.Exec(`UPDATE users SET instance_admin = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.IssueReset(context.Background(), audit.SystemCLI, id); err != nil {
		t.Fatalf("resetting the only instance admin: %v", err)
	}
}
