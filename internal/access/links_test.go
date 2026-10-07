// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/auth"
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
