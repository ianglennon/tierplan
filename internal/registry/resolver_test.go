// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Which credential a pull is sent with, and the header that carries it (plan 05-09, IMG-02,
// D-14, D-25).

type authJSON struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
}

func decodeAuth(t *testing.T, header string) authJSON {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("the header is not base64url: %v", err)
	}
	var a authJSON
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("the header is not JSON: %v", err)
	}
	return a
}

func TestEncodeAuthIsTheBase64URLJSONTheEngineReads(t *testing.T) {
	got := decodeAuth(t, registry.EncodeAuth("bot", "s3cret", "ghcr.io"))
	if got != (authJSON{"bot", "s3cret", "ghcr.io"}) {
		t.Errorf("decoded = %+v", got)
	}
	hub := decodeAuth(t, registry.EncodeAuth("me", "tok", "docker.io"))
	if hub.ServerAddress != "https://index.docker.io/v1/" {
		t.Errorf("a docker.io credential names %q, want the address the Docker CLI uses", hub.ServerAddress)
	}
	// A password with characters that standard base64 would write as + or / still decodes.
	odd := decodeAuth(t, registry.EncodeAuth("u", "??>>~~", "r.example:5000"))
	if odd.Password != "??>>~~" {
		t.Errorf("password = %q", odd.Password)
	}
}

func TestAuthForSendsNothingWhenNoCredentialMatches(t *testing.T) {
	s, _, _ := newStore(t)
	create(t, s, nil) // ghcr.io/acme
	ref, _ := registry.ParseRef("localhost:5000/private/busybox:1")
	header, used, err := registry.NewResolver(s).AuthFor(context.Background(), "env-1", ref)
	if header != "" || used != nil || err != nil {
		t.Errorf("AuthFor = %q, %v, %v; want nothing", header, used, err)
	}
}

func TestAuthForOpensTheMatchingSecretIntoTheHeader(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)
	ref, _ := registry.ParseRef("ghcr.io/acme/app:1")
	header, used, err := registry.NewResolver(s).AuthFor(context.Background(), "env-1", ref)
	if err != nil || used == nil || used.ID != c.ID || used.Name != "ghcr-acme" {
		t.Fatalf("AuthFor = %q, %+v, %v", header, used, err)
	}
	if got := decodeAuth(t, header); got != (authJSON{"bot", longSecret, "ghcr.io"}) {
		t.Errorf("decoded = %+v", got)
	}
}

func TestAuthForNamesTheCredentialAndNeverTheValueWhenTheVaultCannotOpenIt(t *testing.T) {
	s, st, _ := newStore(t)
	c := create(t, s, nil)
	// A secret sealed for another credential does not open under this one's ID.
	other, _ := vault.New(make([]byte, 32), "other")
	bad, err := other.Seal(vault.ContextRegistryCred+c.ID, []byte(longSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.Exec(`UPDATE registry_credentials SET secret_sealed = ? WHERE id = ?`, bad, c.ID); err != nil {
		t.Fatal(err)
	}
	ref, _ := registry.ParseRef("ghcr.io/acme/app:1")
	header, used, err := registry.NewResolver(s).AuthFor(context.Background(), "env-1", ref)
	if err == nil || header != "" {
		t.Fatalf("AuthFor = %q, %v, %v; want an error", header, used, err)
	}
	if !strings.Contains(err.Error(), c.ID) || strings.Contains(err.Error(), longSecret) || strings.Contains(err.Error(), bad) {
		t.Errorf("error = %q: must name the ID and never a value", err)
	}
}

func TestAFailureIsRecordedWithItsCodeAndASuccessClearsIt(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)
	ctx := context.Background()
	if err := s.RecordFailure(ctx, c.ID, "pull_unauthorized"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, c.ID)
	if err != nil || got.LastFailure == nil || got.LastFailure.Code != "pull_unauthorized" || !got.LastFailure.At.Equal(now) {
		t.Fatalf("credential = %+v, %v", got, err)
	}
	if err := s.ClearFailure(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, c.ID)
	if got.LastFailure != nil {
		t.Errorf("failure = %+v, want cleared", got.LastFailure)
	}
	// Clearing when there is none, or for a credential deleted meanwhile, is not an error.
	if err := s.ClearFailure(ctx, c.ID); err != nil {
		t.Errorf("a second clear: %v", err)
	}
	if err := s.RecordFailure(ctx, "gone", "pull_denied"); err != nil {
		t.Errorf("a failure for a deleted credential: %v", err)
	}
}
