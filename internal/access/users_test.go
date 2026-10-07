// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

var testAdmin = audit.Actor{Type: audit.ActorUser, ID: "id-admin", Name: "admin"}

var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// clock is a mutable fake clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fixture struct {
	svc *access.Service
	st  *store.Store
	clk *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: testNow}
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), clk.now)
	svc := access.New(st, w, rbac.NewChecker(st), 0)
	svc.Now = clk.now
	return &fixture{svc: svc, st: st, clk: clk}
}

func TestCreateUserStoresAnInvitedUserWithNoPasswordAndOnlyTheHashOfTheToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	u, link, err := f.svc.CreateUser(ctx, testAdmin, access.CreateUserInput{Username: "Alice", DisplayName: "Alice L", Email: "alice@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" || u.PasswordHash != "" || u.DisplayName != "Alice L" || u.Email != "alice@example.test" || u.InstanceAdmin != 0 {
		t.Fatalf("user = %+v", u)
	}
	if len(link.Token) != 43 || !link.ExpiresAt.Equal(testNow.Add(72*time.Hour)) {
		t.Fatalf("link = %+v, want a 43 character token valid for 72 hours", link)
	}

	var stored []byte
	var kind, createdBy string
	if err := f.st.Read.QueryRow(`SELECT token_hash, kind, created_by FROM user_links WHERE user_id = ?`, u.ID).Scan(&stored, &kind, &createdBy); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(link.Token))
	if !bytes.Equal(stored, sum[:]) || kind != "invite" || createdBy != "id-admin" {
		t.Fatalf("stored link = %x %q %q, want the SHA-256 of the token, kind invite, created by the admin", stored, kind, createdBy)
	}
	if bytes.Contains(stored, []byte(link.Token)) {
		t.Fatal("the raw token is stored")
	}
}

func TestAcceptLinkSetsAPasswordThatVerifiesAndSpendsTheLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u, link, err := f.svc.CreateUser(ctx, testAdmin, access.CreateUserInput{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	info, err := f.svc.LookupLink(ctx, link.Token)
	if err != nil || info.Kind != "invite" || info.Username != "alice" {
		t.Fatalf("lookup = %+v, %v", info, err)
	}
	const chosen = "a long chosen passphrase"
	if err := f.svc.AcceptLink(ctx, link.Token, chosen, "203.0.113.9", "ua"); err != nil {
		t.Fatal(err)
	}

	got, err := f.st.RQ.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.VerifyPassword(chosen, got.PasswordHash); err != nil || !ok {
		t.Fatalf("the chosen password does not verify: %v, %v", ok, err)
	}
	if strings.Contains(got.PasswordHash, chosen) {
		t.Fatal("the password is stored in the clear")
	}
	var links int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM user_links`).Scan(&links); err != nil || links != 0 {
		t.Fatalf("links after accept = %d, %v, want the link spent", links, err)
	}
}

func mustUserID(t *testing.T, f *fixture, name string) string {
	t.Helper()
	u, err := f.st.RQ.GetUserByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// reissue replaces a user's link with a fresh invite, the way regenerating it
// will (a later plan), straight through the store.
func (f *fixture) reissue(ctx context.Context, userID string) (access.Link, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return access.Link{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	expires := f.clk.now().Add(72 * time.Hour)
	if err := f.st.Q.DeleteUserLinks(ctx, userID); err != nil {
		return access.Link{}, err
	}
	err := f.st.Q.InsertUserLink(ctx, sqlc.InsertUserLinkParams{
		ID: "link-" + token[:8], UserID: userID, Kind: "invite", TokenHash: sum[:],
		CreatedAt: f.clk.now().UnixNano(), ExpiresAt: expires.UnixNano(), CreatedBy: "id-admin",
	})
	return access.Link{Token: token, ExpiresAt: expires}, err
}

func TestReservedAUsernameOfADeletedUserCannotBeUsedAgain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, _ := f.invite(t, "alice")
	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = ? WHERE id = ?`, testNow.UnixNano(), id); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"alice", "ALICE"} {
		if _, _, err := f.svc.CreateUser(ctx, testAdmin, access.CreateUserInput{Username: name}); !errors.Is(err, access.ErrUsernameReserved) {
			t.Fatalf("create %q over a tombstone = %v, want ErrUsernameReserved", name, err)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'alice'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users named alice = %d, %v, want the tombstone only", n, err)
	}
}

func TestTakenALiveUsernameIsRefusedWhateverItsCase(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "alice")
	if _, _, err := f.svc.CreateUser(context.Background(), testAdmin, access.CreateUserInput{Username: "Alice"}); !errors.Is(err, access.ErrUsernameTaken) {
		t.Fatalf("create Alice over alice = %v, want ErrUsernameTaken", err)
	}
}

func TestCreateUserRefusesBadFieldsAndNamesEveryOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tooLong := strings.Repeat("é", 65)
	cases := []struct {
		name string
		in   access.CreateUserInput
		want []error
	}{
		{"bad username", access.CreateUserInput{Username: "a!"}, []error{access.ErrInvalidUsername}},
		{"short username", access.CreateUserInput{Username: "ab"}, []error{access.ErrInvalidUsername}},
		{"email not an address", access.CreateUserInput{Username: "alice", Email: "not-an-email"}, []error{access.ErrInvalidEmail}},
		{"email with a display part", access.CreateUserInput{Username: "alice", Email: "Alice <a@example.test>"}, []error{access.ErrInvalidEmail}},
		{"email over 254 characters", access.CreateUserInput{Username: "alice", Email: strings.Repeat("a", 250) + "@example.test"}, []error{access.ErrInvalidEmail}},
		{"display name over 64 characters", access.CreateUserInput{Username: "alice", DisplayName: tooLong}, []error{access.ErrDisplayNameTooLong}},
		{"all three", access.CreateUserInput{Username: "!", Email: "x", DisplayName: tooLong}, []error{access.ErrInvalidUsername, access.ErrInvalidEmail, access.ErrDisplayNameTooLong}},
	}
	for _, c := range cases {
		_, _, err := f.svc.CreateUser(ctx, testAdmin, c.in)
		for _, w := range c.want {
			if !errors.Is(err, w) {
				t.Errorf("%s: %v, want it to include %v", c.name, err, w)
			}
		}
	}
	// A display name of exactly 64 characters is fine.
	if _, _, err := f.svc.CreateUser(ctx, testAdmin, access.CreateUserInput{Username: "alice", DisplayName: strings.Repeat("é", 64)}); err != nil {
		t.Fatalf("64 character display name: %v", err)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users = %d, %v; the refused submissions must create none", n, err)
	}
}
