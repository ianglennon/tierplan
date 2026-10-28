// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitconn_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

// A token of 12 or more characters, so its last 4 are kept.
const token = "ghp_0123456789abcdefghijklmnopqrstuvwxyz" //nolint:gosec // G101: a made-up token for the test

var now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (*gitconn.Store, *store.Store, *vault.Vault) {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	return gitconn.NewStore(st, v, func() time.Time { return now }), st, v
}

func ptr(s string) *string { return &s }

func httpsCredential(env *string, name string) gitconn.CredentialInput {
	return gitconn.CredentialInput{EnvID: env, Name: name, Type: gitconn.TypeHTTPS, Username: "bot", Secret: token}
}

func addEnvironment(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES (?, 'agent', ?, '', 1, 1, 1)`, id, "env "+id); err != nil {
		t.Fatal(err)
	}
}

func TestGitCredentialSealedAtRest(t *testing.T) {
	s, st, v := newStore(t)
	ctx := context.Background()
	c, err := s.CreateCredential(ctx, httpsCredential(nil, "gitea-homelab"), "admin-1", nil)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if c.ID == "" || c.Layer() != gitconn.LayerInstance || c.Last4 != "wxyz" || c.Username != "bot" || c.UsedBy != 0 {
		t.Errorf("credential = %+v", c)
	}

	var sealed string
	if err := st.Read.QueryRow(`SELECT secret_sealed FROM git_credentials WHERE id = ?`, c.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if sealed == "" || strings.Contains(sealed, token) {
		t.Fatalf("the stored column is not sealed: %q", sealed)
	}
	plain, err := v.Open(vault.ContextGitCred+c.ID, sealed)
	if err != nil || string(plain) != token {
		t.Fatalf("Open under its own ID = %q, %v", plain, err)
	}
	if _, err := v.Open(vault.ContextGitCred+"another-credential", sealed); !errors.Is(err, vault.ErrDecrypt) {
		t.Errorf("Open under another credential's ID: %v, want ErrDecrypt", err)
	}

	// A repository that picks the credential gets the token back only through Remote, in memory.
	r, err := s.CreateRepo(ctx, gitconn.RepoInput{Name: "app", URL: "https://git.example.org/acme/app.git", CredentialID: &c.ID}, "admin-1", nil)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if r.CredentialID != c.ID || r.CredentialName != "gitea-homelab" {
		t.Errorf("repo = %+v, want it to name the credential it picked", r)
	}
	rem, err := s.Remote(ctx, r)
	if err != nil {
		t.Fatalf("Remote: %v", err)
	}
	auth, ok := rem.Auth.(gitx.TokenAuth)
	if !ok || auth.Username != "bot" || auth.Token != token || rem.URL != r.URL {
		t.Errorf("Remote = %+v, want the saved token as TokenAuth", rem)
	}

	// A repository with no credential connects anonymously: nothing is inferred from its address.
	pub, err := s.CreateRepo(ctx, gitconn.RepoInput{Name: "public", URL: "https://git.example.org/acme/public.git"}, "admin-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rem, err := s.Remote(ctx, pub); err != nil || rem.Auth != nil {
		t.Errorf("Remote of a repository with no credential = %+v, %v; want no Auth", rem, err)
	}
}

func TestCreateRollsBackWhenTheHookFails(t *testing.T) {
	s, st, _ := newStore(t)
	boom := errors.New("audit failed")
	_, err := s.CreateCredential(context.Background(), httpsCredential(nil, "gitea"), "admin-1",
		func(context.Context, *sql.Tx, gitconn.Credential) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("CreateCredential error = %v, want the hook's", err)
	}
	var n int
	if err := st.Read.QueryRow(`SELECT count(*) FROM git_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("credentials after a failed hook = %d, %v; an interrupted create leaves nothing", n, err)
	}
}

func TestACredentialIsOnlyUsedByARepositoryThatPickedItInTheRightLayer(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a")
	addEnvironment(t, st, "env-b")
	ctx := context.Background()
	inst, err := s.CreateCredential(ctx, httpsCredential(nil, "inst"), "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	inA, err := s.CreateCredential(ctx, httpsCredential(ptr("env-a"), "in-a"), "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	url := "https://git.example.org/acme/app.git"
	for name, tc := range map[string]struct {
		env  *string
		cred string
		ok   bool
	}{
		"instance repository, instance credential":               {nil, inst.ID, true},
		"instance repository, environment credential":            {nil, inA.ID, false},
		"environment repository, its own credential":             {ptr("env-a"), inA.ID, true},
		"environment repository, instance credential":            {ptr("env-a"), inst.ID, true},
		"environment repository, another environment":            {ptr("env-b"), inA.ID, false},
		"environment repository, a credential that is not there": {ptr("env-a"), "no-such-credential", false},
	} {
		_, err := s.CreateRepo(ctx, gitconn.RepoInput{EnvID: tc.env, Name: strings.ReplaceAll(name, ",", ""), URL: url, CredentialID: &tc.cred}, "u", nil)
		var ve *gitconn.ValidationError
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v, want it saved", name, err)
		case !tc.ok && (!errors.As(err, &ve) || ve.Fields[0].Field != "credentialId" || ve.Fields[0].Code != gitconn.CodeCredentialScope):
			t.Errorf("%s: %v, want credentialId: credential_scope", name, err)
		}
	}
}

func TestRecordTestRemembersTheOutcomeAndWhatItShowedOfTheCredential(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	c, _ := s.CreateCredential(ctx, httpsCredential(nil, "gitea"), "u", nil)
	r, err := s.CreateRepo(ctx, gitconn.RepoInput{Name: "app", URL: "https://git.example.org/acme/app.git", CredentialID: &c.ID}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.LastTest != nil {
		t.Fatalf("a new repository was never tested, got %+v", r.LastTest)
	}
	if err := s.RecordTest(ctx, r.ID, false, gitx.CodeAuthRefused); err != nil {
		t.Fatal(err)
	}
	r, _ = s.GetRepo(ctx, r.ID)
	cred, _ := s.GetCredential(ctx, c.ID)
	if r.LastTest == nil || r.LastTest.OK || r.LastTest.Code != "auth_refused" || !r.LastTest.At.Equal(now) {
		t.Errorf("LastTest = %+v, want a failure with auth_refused", r.LastTest)
	}
	if cred.LastFailure == nil || cred.LastFailure.Code != "auth_refused" || cred.LastUseAt != nil {
		t.Errorf("credential after a refusal = %+v", cred)
	}
	if err := s.RecordTest(ctx, r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	r, _ = s.GetRepo(ctx, r.ID)
	cred, _ = s.GetCredential(ctx, c.ID)
	if r.LastTest == nil || !r.LastTest.OK || r.LastTest.Code != "" {
		t.Errorf("LastTest = %+v, want a pass", r.LastTest)
	}
	if cred.LastFailure != nil || cred.LastUseAt == nil {
		t.Errorf("a pass clears the refusal and shows the use: %+v", cred)
	}
	if err := s.RecordTest(ctx, "gone", true, ""); err != nil {
		t.Errorf("RecordTest of a repository deleted meanwhile: %v, want nil", err)
	}
}
