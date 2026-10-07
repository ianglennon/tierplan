// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
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
