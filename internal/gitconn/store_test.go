// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitconn_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

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

// sealedOf is the stored sealed column of a credential.
func sealedOf(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var sealed string
	if err := st.Read.QueryRow(`SELECT secret_sealed FROM git_credentials WHERE id = ?`, id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestGitConnNameRace(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a")
	ctx := context.Background()

	race := func(t *testing.T, what string, create func() error) {
		t.Helper()
		const racers = 2
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, racers)
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = create()
			}()
		}
		close(start)
		wg.Wait()
		var ok, taken int
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, gitconn.ErrNameTaken):
				taken++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if ok != 1 || taken != 1 {
			t.Errorf("%s: two simultaneous creates of one name: %d created, %d name_taken, want exactly 1 and 1", what, ok, taken)
		}
	}
	for name, env := range map[string]*string{"instance": nil, "environment": ptr("env-a")} {
		race(t, name+" credential", func() error {
			_, err := s.CreateCredential(ctx, httpsCredential(env, "same"), "u", nil)
			return err
		})
		var n int
		if err := st.Read.QueryRow(`SELECT count(*) FROM git_credentials WHERE name = 'same' AND env_id IS ?`, env).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s credential rows = %d, %v; want exactly one", name, n, err)
		}
		race(t, name+" repository", func() error {
			_, err := s.CreateRepo(ctx, gitconn.RepoInput{EnvID: env, Name: "same", URL: "https://git.example.org/a.git"}, "u", nil)
			return err
		})
		if err := st.Read.QueryRow(`SELECT count(*) FROM git_repos WHERE name = 'same' AND env_id IS ?`, env).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s repository rows = %d, %v; want exactly one", name, n, err)
		}
	}
	// The same name in another layer is another row.
	if _, err := s.CreateCredential(ctx, httpsCredential(ptr("env-a"), "other-layer"), "u", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCredential(ctx, httpsCredential(nil, "other-layer"), "u", nil); err != nil {
		t.Errorf("the same name in the instance layer: %v, want it allowed", err)
	}
}

func TestGitConnListsEveryLayerInstanceFirst(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a")
	ctx := context.Background()
	for _, in := range []gitconn.CredentialInput{httpsCredential(ptr("env-a"), "a-first"), httpsCredential(nil, "z-last"), httpsCredential(nil, "m-mid")} {
		if _, err := s.CreateCredential(ctx, in, "u", nil); err != nil {
			t.Fatal(err)
		}
	}
	creds, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	var names []string
	for _, c := range creds {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"m-mid", "z-last", "a-first"}) {
		t.Errorf("credentials = %v, want the instance's by name, then the environment's", names)
	}
	for _, in := range []gitconn.RepoInput{
		{EnvID: ptr("env-a"), Name: "a-first", URL: "https://git.example.org/a.git"}, {Name: "z-last", URL: "https://git.example.org/z.git"},
	} {
		if _, err := s.CreateRepo(ctx, in, "u", nil); err != nil {
			t.Fatal(err)
		}
	}
	repos, err := s.ListRepos(ctx)
	if err != nil || len(repos) != 2 || repos[0].Name != "z-last" || repos[1].Name != "a-first" {
		t.Errorf("ListRepos = %+v, %v; want the instance repository first", repos, err)
	}
	for _, c := range creds {
		if c.Last4 == "" {
			t.Errorf("credential %s lost its last4", c.Name)
		}
	}
}

func TestGitConnUpdateRules(t *testing.T) {
	s, st, v := newStore(t)
	addEnvironment(t, st, "env-a")
	ctx := context.Background()
	c, err := s.CreateCredential(ctx, httpsCredential(nil, "gitea"), "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	before := sealedOf(t, st, c.ID)

	// An empty secret keeps the saved one byte for byte; the changed fields are named, sorted.
	got, changed, err := s.UpdateCredential(ctx, "", c.ID, gitconn.CredentialUpdate{Name: "gitea-2", Type: gitconn.TypeHTTPS, Username: "robot"}, nil)
	if err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	if got.Name != "gitea-2" || got.Username != "robot" || got.Last4 != "wxyz" || !slices.Equal(changed, []string{"name", "username"}) {
		t.Errorf("credential = %+v changed = %v", got, changed)
	}
	if sealedOf(t, st, c.ID) != before {
		t.Error("the sealed secret changed although no new secret was given")
	}

	// A form that changes nothing writes nothing and runs no hook.
	ran := false
	if _, changed, err := s.UpdateCredential(ctx, "", c.ID, gitconn.CredentialUpdate{Name: "gitea-2", Type: gitconn.TypeHTTPS, Username: "robot"},
		func(context.Context, *sql.Tx, gitconn.Credential, []string) error { ran = true; return nil }); err != nil || len(changed) != 0 || ran {
		t.Errorf("an unchanged form: changed %v, hook ran %v, err %v", changed, ran, err)
	}

	// A new secret is sealed again under the same context and replaces last4; the word secret is
	// all the change records.
	got, changed, err = s.UpdateCredential(ctx, "", c.ID, gitconn.CredentialUpdate{Name: "gitea-2", Type: gitconn.TypeHTTPS, Username: "robot", Secret: "glpat-another-token-ABCD"}, nil)
	if err != nil || got.Last4 != "ABCD" || !slices.Equal(changed, []string{"secret"}) {
		t.Fatalf("new secret: %+v %v %v", got, changed, err)
	}
	if plain, err := v.Open(vault.ContextGitCred+c.ID, sealedOf(t, st, c.ID)); err != nil || string(plain) != "glpat-another-token-ABCD" {
		t.Errorf("sealed value = %q, %v", plain, err)
	}

	var ve *gitconn.ValidationError

	// A credential of another layer is not found, and nothing changes.
	if _, _, err := s.UpdateCredential(ctx, "env-a", c.ID, gitconn.CredentialUpdate{Name: "taken-over", Type: gitconn.TypeHTTPS, Username: "x"}, nil); !errors.Is(err, gitconn.ErrNotFound) {
		t.Errorf("update through another layer: %v, want ErrNotFound", err)
	}
	if cur, _ := s.GetCredential(ctx, c.ID); cur.Name != "gitea-2" {
		t.Errorf("the name is %q after a refused update through another layer", cur.Name)
	}

	// A hook that fails rolls the change back.
	boom := errors.New("audit failed")
	if _, _, err := s.UpdateCredential(ctx, "", c.ID, gitconn.CredentialUpdate{Name: "rolled-back", Type: gitconn.TypeHTTPS, Username: "robot"},
		func(context.Context, *sql.Tx, gitconn.Credential, []string) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("hook failure: %v", err)
	}
	if cur, _ := s.GetCredential(ctx, c.ID); cur.Name != "gitea-2" {
		t.Errorf("the name is %q after a failed hook, want it rolled back", cur.Name)
	}

	// A clash within the layer is name_taken.
	if _, err := s.CreateCredential(ctx, httpsCredential(nil, "other"), "u", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpdateCredential(ctx, "", c.ID, gitconn.CredentialUpdate{Name: "other", Type: gitconn.TypeHTTPS, Username: "robot"}, nil); !errors.Is(err, gitconn.ErrNameTaken) {
		t.Errorf("rename onto another: %v, want ErrNameTaken", err)
	}

	// A repository whose address moves to another host keeps its credential only when the request
	// names it again.
	r, err := s.CreateRepo(ctx, gitconn.RepoInput{Name: "app", URL: "https://git.example.org/acme/app.git", CredentialID: &c.ID}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTest(ctx, r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	moved := gitconn.RepoUpdate{Name: "app", URL: "https://elsewhere.example.net/acme/app.git"}
	if _, _, err := s.UpdateRepo(ctx, "", r.ID, moved, nil); !errors.As(err, &ve) || ve.Fields[0].Field != "credentialId" || ve.Fields[0].Code != gitconn.CodeCredentialReselect {
		t.Errorf("a new host without naming the credential: %v, want credentialId: credential_reselect", err)
	}
	if cur, _ := s.GetRepo(ctx, r.ID); cur.URL != r.URL || cur.CredentialID != c.ID {
		t.Errorf("repository after a refused move = %+v, want it unchanged", cur)
	}
	// The same host (another path, a new name) keeps the credential without naming it.
	same, changed, err := s.UpdateRepo(ctx, "", r.ID, gitconn.RepoUpdate{Name: "app-renamed", URL: "https://git.example.org/acme/other.git"}, nil)
	if err != nil || same.CredentialID != c.ID || !slices.Equal(changed, []string{"name", "url"}) {
		t.Errorf("same host: %+v %v %v, want the credential kept and name, url changed", same, changed, err)
	}
	if same.LastTest != nil {
		t.Errorf("the last test survived an address change: %+v", same.LastTest)
	}
	// Naming the credential again lets the host move; naming none drops it.
	moved.Name = "app-renamed"
	moved.CredentialID = &c.ID
	again, changed, err := s.UpdateRepo(ctx, "", r.ID, moved, nil)
	if err != nil || again.CredentialID != c.ID || again.Host() != "elsewhere.example.net" || !slices.Equal(changed, []string{"url"}) {
		t.Errorf("a new host naming the credential: %+v %v %v", again, changed, err)
	}
	none := ""
	moved.CredentialID = &none
	if dropped, changed, err := s.UpdateRepo(ctx, "", r.ID, moved, nil); err != nil || dropped.CredentialID != "" || !slices.Equal(changed, []string{"credential"}) {
		t.Errorf("dropping the credential: %+v %v %v", dropped, changed, err)
	}
	// A repository of another layer is not found; a credential outside the layer is refused.
	if _, _, err := s.UpdateRepo(ctx, "env-a", r.ID, moved, nil); !errors.Is(err, gitconn.ErrNotFound) {
		t.Errorf("update through another layer: %v, want ErrNotFound", err)
	}
	inEnv, err := s.CreateCredential(ctx, httpsCredential(ptr("env-a"), "env-cred"), "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	moved.CredentialID = &inEnv.ID
	if _, _, err := s.UpdateRepo(ctx, "", r.ID, moved, nil); !errors.As(err, &ve) || ve.Fields[0].Code != gitconn.CodeCredentialScope {
		t.Errorf("an instance repository picking an environment credential: %v, want credential_scope", err)
	}
}

func TestGitCredentialInUse(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a")
	ctx := context.Background()
	c, err := s.CreateCredential(ctx, httpsCredential(nil, "shared"), "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	var repos []gitconn.Repo
	for _, name := range []string{"one", "two"} {
		r, err := s.CreateRepo(ctx, gitconn.RepoInput{Name: name, URL: "https://git.example.org/" + name + ".git", CredentialID: &c.ID}, "u", nil)
		if err != nil {
			t.Fatal(err)
		}
		repos = append(repos, r)
	}
	if got, _ := s.GetCredential(ctx, c.ID); got.UsedBy != 2 {
		t.Errorf("UsedBy = %d, want 2", got.UsedBy)
	}
	_, err = s.DeleteCredential(ctx, "", c.ID, nil)
	var inUse *gitconn.InUseError
	if !errors.As(err, &inUse) || inUse.Count != 2 || !errors.Is(err, gitconn.ErrCredentialInUse) {
		t.Fatalf("delete of a credential used by 2 repositories: %v, want an InUseError with count 2", err)
	}
	if _, err := s.GetCredential(ctx, c.ID); err != nil {
		t.Errorf("the credential is gone after a refused delete: %v", err)
	}
	// Another layer cannot delete it, even when unused.
	if _, err := s.DeleteCredential(ctx, "env-a", c.ID, nil); !errors.Is(err, gitconn.ErrNotFound) {
		t.Errorf("delete through another layer: %v, want ErrNotFound", err)
	}
	// Free it, then it can go; a repeated delete is not found.
	for _, r := range repos {
		if _, err := s.DeleteRepo(ctx, "", r.ID, nil); err != nil {
			t.Fatalf("DeleteRepo: %v", err)
		}
	}
	boom := errors.New("audit failed")
	if _, err := s.DeleteCredential(ctx, "", c.ID, func(context.Context, *sql.Tx, gitconn.Credential) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("hook failure: %v", err)
	}
	if _, err := s.GetCredential(ctx, c.ID); err != nil {
		t.Errorf("the credential is gone after a failed hook: %v", err)
	}
	gone, err := s.DeleteCredential(ctx, "", c.ID, nil)
	if err != nil || gone.Name != "shared" {
		t.Fatalf("DeleteCredential: %+v %v", gone, err)
	}
	if _, err := s.DeleteCredential(ctx, "", c.ID, nil); !errors.Is(err, gitconn.ErrNotFound) {
		t.Errorf("a repeated delete: %v, want ErrNotFound", err)
	}
	var sealed int
	if err := st.Read.QueryRow(`SELECT count(*) FROM git_credentials`).Scan(&sealed); err != nil || sealed != 0 {
		t.Errorf("credentials left = %d, %v", sealed, err)
	}
}

// pemKey is a private key in OpenSSH format, with the passphrase when there is one.
func pemKey(t *testing.T, pass string) (pemText string, pub ssh.PublicKey) {
	t.Helper()
	pubKey, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if pass == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(pass))
	}
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), sshPub
}

func TestSSHCredentialParse(t *testing.T) {
	ed, edPub := pemKey(t, "")

	got, err := gitconn.ParsePrivateKey(ed)
	if err != nil {
		t.Fatalf("ParsePrivateKey(ed25519): %v", err)
	}
	wantAuthorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(edPub)))
	if got.AuthorizedKey != wantAuthorized || !strings.HasPrefix(got.AuthorizedKey, "ssh-ed25519 ") || got.Fingerprint != ssh.FingerprintSHA256(edPub) ||
		!strings.HasPrefix(got.Fingerprint, "SHA256:") || got.Signer == nil {
		t.Errorf("parsed = %+v, want the authorized_keys line and the SHA256 fingerprint of the key", got)
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})
	if got, err := gitconn.ParsePrivateKey(string(rsaPEM)); err != nil || !strings.HasPrefix(got.AuthorizedKey, "ssh-rsa ") {
		t.Errorf("ParsePrivateKey(rsa) = %+v, %v", got, err)
	}

	protected, _ := pemKey(t, "correct horse")
	if _, err := gitconn.ParsePrivateKey(protected); !errors.Is(err, gitconn.ErrKeyPassphrase) {
		t.Errorf("a key with a passphrase: %v, want ErrKeyPassphrase", err)
	}
	for name, text := range map[string]string{
		"garbage":           "this is not a key",
		"empty":             "",
		"a public key":      got.AuthorizedKey,
		"a truncated key":   ed[:len(ed)/2],
		"a certificate PEM": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	} {
		if _, err := gitconn.ParsePrivateKey(text); !errors.Is(err, gitconn.ErrKeyInvalid) {
			t.Errorf("%s: %v, want ErrKeyInvalid", name, err)
		}
	}
	if _, err := gitconn.ParsePrivateKey(ed + strings.Repeat("\n#", gitconn.MaxPrivateKeyBytes)); !errors.Is(err, gitconn.ErrKeyTooLarge) {
		t.Errorf("a key over 16 KiB: %v, want ErrKeyTooLarge", err)
	}
	// No error carries any part of the text it refused.
	_, err = gitconn.ParsePrivateKey("BEGIN-SECRET-MARKER " + ed)
	if err == nil || strings.Contains(err.Error(), "SECRET-MARKER") {
		t.Errorf("the refusal of a key carries its text: %v", err)
	}
}

func TestSSHCredentialIsSealedAndShownAsPublicKeyAndFingerprint(t *testing.T) {
	s, st, v := newStore(t)
	ctx := context.Background()
	key, pub := pemKey(t, "")
	c, err := s.CreateCredential(ctx, gitconn.CredentialInput{Name: "deploy", Type: gitconn.TypeSSH, Secret: key}, "u", nil)
	if err != nil {
		t.Fatalf("CreateCredential(ssh): %v", err)
	}
	if c.Type != gitconn.TypeSSH || c.Username != "git" || c.Last4 != "" || c.Fingerprint != ssh.FingerprintSHA256(pub) ||
		c.PublicKey != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) {
		t.Errorf("credential = %+v, want user git, no last4, the public key and its fingerprint", c)
	}
	plain, err := v.Open(vault.ContextGitCred+c.ID, sealedOf(t, st, c.ID))
	if err != nil || !strings.Contains(string(plain), "OPENSSH PRIVATE KEY") {
		t.Errorf("the sealed value = %q, %v, want the private key", plain, err)
	}
	if strings.Contains(c.PublicKey+c.Fingerprint+c.Username, "PRIVATE KEY") {
		t.Errorf("the shown values carry the private key: %+v", c)
	}

	// An ssh address with the key: Remote gives the address and the key to sign in with, never a token.
	r, err := s.CreateRepo(ctx, gitconn.RepoInput{Name: "app", URL: "git@git.example.org:acme/app.git", CredentialID: &c.ID}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	rem, err := s.Remote(ctx, r)
	if _, isKey := rem.Auth.(gitx.SSHKeyAuth); err != nil || !isKey || rem.URL != r.URL {
		t.Errorf("Remote of an ssh repository = %+v, %v; want the deploy key as SSHKeyAuth, never a token", rem, err)
	}

	// Rotating the key replaces what is shown; keeping it (no secret) leaves both alone.
	key2, pub2 := pemKey(t, "")
	got, changed, err := s.UpdateCredential(ctx, "", c.ID, gitconn.CredentialUpdate{Name: "deploy", Type: gitconn.TypeSSH, Username: "git", Secret: key2}, nil)
	if err != nil || !slices.Equal(changed, []string{"secret"}) || got.Fingerprint != ssh.FingerprintSHA256(pub2) {
		t.Fatalf("rotate the key: %+v %v %v", got, changed, err)
	}
	kept, changed, err := s.UpdateCredential(ctx, "", c.ID, gitconn.CredentialUpdate{Name: "deploy-2", Type: gitconn.TypeSSH, Username: "git"}, nil)
	if err != nil || !slices.Equal(changed, []string{"name"}) || kept.Fingerprint != got.Fingerprint || kept.PublicKey != got.PublicKey {
		t.Errorf("rename without a key: %+v %v %v, want the key kept", kept, changed, err)
	}

	// Switching the type needs a secret typed now, either way.
	var ve *gitconn.ValidationError
	if _, _, err := s.UpdateCredential(ctx, "", c.ID, gitconn.CredentialUpdate{Name: "deploy-2", Type: gitconn.TypeHTTPS, Username: "bot"}, nil); !errors.As(err, &ve) ||
		ve.Fields[0].Field != "secret" || ve.Fields[0].Code != gitconn.CodeSecretRequired {
		t.Errorf("ssh to https without a token: %v, want secret: secret_required", err)
	}
	h, err := s.CreateCredential(ctx, httpsCredential(nil, "token"), "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpdateCredential(ctx, "", h.ID, gitconn.CredentialUpdate{Name: "token", Type: gitconn.TypeSSH, Username: "git"}, nil); !errors.As(err, &ve) ||
		ve.Fields[0].Field != "secret" || ve.Fields[0].Code != gitconn.CodeSecretRequired {
		t.Errorf("https to ssh without a key: %v, want secret: secret_required", err)
	}
	if conv, _, err := s.UpdateCredential(ctx, "", h.ID, gitconn.CredentialUpdate{Name: "token", Type: gitconn.TypeSSH, Username: "git", Secret: key}, nil); err != nil ||
		conv.Type != gitconn.TypeSSH || conv.Last4 != "" || conv.Fingerprint == "" {
		t.Errorf("https to ssh with a key: %+v %v", conv, err)
	}

	// The refusals the sheet words.
	for name, tc := range map[string]struct{ secret, code string }{
		"a passphrase": {func() string { k, _ := pemKey(t, "pw"); return k }(), gitconn.CodeKeyPassphrase},
		"garbage":      {"not a key", gitconn.CodeKeyInvalid},
		"too large":    {key + strings.Repeat("\n#", gitconn.MaxPrivateKeyBytes), gitconn.CodeKeyTooLarge},
		"nothing":      {"", gitconn.CodeSecretRequired},
	} {
		_, err := s.CreateCredential(ctx, gitconn.CredentialInput{Name: "bad", Type: gitconn.TypeSSH, Secret: tc.secret}, "u", nil)
		if !errors.As(err, &ve) || ve.Fields[0].Field != "secret" || ve.Fields[0].Code != tc.code {
			t.Errorf("%s: %v, want secret: %s", name, err, tc.code)
		}
	}
}

// A host key is stored with who trusted it and when, comes back in the Remote of the repository, and
// stays with the machine it was confirmed for (plan 09-06, D-03).
func TestSSHRepositoryStoresItsHostKeyAndUsesItsKeyToSignIn(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	key, pub := pemKey(t, "")
	c, err := s.CreateCredential(ctx, gitconn.CredentialInput{Name: "deploy", Type: gitconn.TypeSSH, Secret: key}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, hostPub := pemKey(t, "")
	hostLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPub)))

	r, err := s.CreateRepo(ctx, gitconn.RepoInput{
		Name: "app", URL: "ssh://git@git.example.org:2222/acme/app.git", CredentialID: &c.ID,
		HostKey: "  " + hostLine + " root@somewhere\n",
	}, "admin-1", nil)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if r.HostKey != hostLine || r.HostKeyFP != ssh.FingerprintSHA256(hostPub) || r.HostKeyTrustedBy != "admin-1" || r.HostKeyTrustedAt == nil || !r.HostKeyTrustedAt.Equal(now) {
		t.Errorf("repo = %+v, want the key normalised (no comment) with its fingerprint, who trusted it and when", r)
	}

	rem, err := s.Remote(ctx, r)
	if err != nil {
		t.Fatalf("Remote: %v", err)
	}
	auth, ok := rem.Auth.(gitx.SSHKeyAuth)
	if !ok || auth.User != "git" || auth.Signer == nil || ssh.FingerprintSHA256(auth.Signer.PublicKey()) != ssh.FingerprintSHA256(pub) {
		t.Errorf("Remote auth = %+v, want the deploy key as SSHKeyAuth", rem.Auth)
	}
	if len(rem.HostKeys) != 1 || rem.HostKeys[0].Line != hostLine {
		t.Errorf("Remote host keys = %+v, want the saved one", rem.HostKeys)
	}

	// Nothing else about the key is a field of the repository, and a key that is not a key is refused.
	for name, bad := range map[string]string{"garbage": "not a key", "truncated": "ssh-ed25519 AAAA", "two keys": hostLine + "\n" + hostLine} {
		_, err := s.CreateRepo(ctx, gitconn.RepoInput{Name: "x " + name, URL: "ssh://git@git.example.org/a.git", HostKey: bad}, "u", nil)
		var ve *gitconn.ValidationError
		if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0] != (gitconn.FieldError{Field: "hostKey", Code: gitconn.CodeHostKeyInvalid}) {
			t.Errorf("%s: %v, want hostKey host_key_invalid", name, err)
		}
	}
	_, err = s.CreateRepo(ctx, gitconn.RepoInput{Name: "https", URL: "https://git.example.org/a.git", HostKey: hostLine}, "u", nil)
	var ve *gitconn.ValidationError
	if !errors.As(err, &ve) || ve.Fields[0].Code != gitconn.CodeHostKeyNotSSH {
		t.Errorf("a host key on an https address: %v, want host_key_not_ssh", err)
	}
}

func TestAHostKeyStaysWithTheMachineItWasTrustedFor(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	_, hostPub := pemKey(t, "")
	hostLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPub)))
	r, err := s.CreateRepo(ctx, gitconn.RepoInput{Name: "app", URL: "ssh://git@git.example.org/acme/app.git", HostKey: hostLine}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Another path on the same host keeps the key.
	same, changed, err := s.UpdateRepo(ctx, "", r.ID, gitconn.RepoUpdate{Name: "app", URL: "ssh://git@git.example.org/acme/other.git"}, nil)
	if err != nil || same.HostKey != hostLine || slices.Contains(changed, "host_key") {
		t.Fatalf("update of the path: %+v changed %v, %v; want the key kept", same, changed, err)
	}
	// A different host forgets it: the person confirmed another machine.
	moved, changed, err := s.UpdateRepo(ctx, "", r.ID, gitconn.RepoUpdate{Name: "app", URL: "ssh://git@other.example.org/acme/app.git"}, nil)
	if err != nil || moved.HostKey != "" || moved.HostKeyFP != "" || moved.HostKeyTrustedAt != nil || !slices.Contains(changed, "host_key") {
		t.Fatalf("update of the host: %+v changed %v, %v; want the key forgotten", moved, changed, err)
	}
	// Sending a key with the move trusts that one, by whoever sent it.
	again, _, err := s.UpdateRepo(ctx, "", r.ID, gitconn.RepoUpdate{Name: "app", URL: "ssh://git@other.example.org/acme/app.git", HostKey: hostLine, TrustedBy: "admin-2"}, nil)
	if err != nil || again.HostKey != hostLine || again.HostKeyTrustedBy != "admin-2" {
		t.Fatalf("update with a key: %+v, %v; want it stored for admin-2", again, err)
	}
}

// addGitStack stores a stack of environment env that points at repoID, as the create of a git stack does.
func addGitStack(t *testing.T, st *store.Store, env, id, repoID string) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO stacks (id, env_id, name, target, source_kind, source_ref, created_by, created_at) VALUES ('` + id + `', '` + env + `', '` + id + `', 'standalone', 'git', '` + repoID + `', 'u', 1)`,
		`INSERT INTO stack_git (stack_id, repo_id, ref_kind, ref, folder, compose_files, created_at, updated_at) VALUES ('` + id + `', '` + repoID + `', 'branch', 'main', '', '["compose.yaml"]', 1, 1)`,
	} {
		if _, err := st.Write.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepoInUseByStacks(t *testing.T) {
	s, st, _ := newStore(t)
	ctx := context.Background()
	addEnvironment(t, st, "env-1")
	env := ptr("env-1")
	repo, err := s.CreateRepo(ctx, gitconn.RepoInput{EnvID: env, Name: "acme", URL: "https://git.example/acme/stacks.git"}, "admin-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	addGitStack(t, st, "env-1", "stack-a", repo.ID)
	addGitStack(t, st, "env-1", "stack-b", repo.ID)

	got, err := s.GetRepo(ctx, repo.ID)
	if err != nil || got.UsedBy != 2 {
		t.Errorf("GetRepo usedBy = %d, %v, want 2", got.UsedBy, err)
	}
	list, err := s.ListRepos(ctx)
	if err != nil || len(list) != 1 || list[0].UsedBy != 2 {
		t.Errorf("ListRepos = %+v, %v, want usedBy 2", list, err)
	}

	// A repository that stacks use cannot be deleted, and nothing is removed.
	_, err = s.DeleteRepo(ctx, "env-1", repo.ID, nil)
	var inUse *gitconn.RepoInUseError
	if !errors.Is(err, gitconn.ErrRepoInUse) || !errors.As(err, &inUse) || inUse.Count != 2 {
		t.Fatalf("DeleteRepo = %v, want ErrRepoInUse with a count of 2", err)
	}
	if _, err := s.GetRepo(ctx, repo.ID); err != nil {
		t.Errorf("the repository is gone after a refused delete: %v", err)
	}

	// Once the stacks are gone it can be deleted.
	if _, err := st.Write.Exec(`DELETE FROM stacks`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRepo(ctx, "env-1", repo.ID, nil); err != nil {
		t.Errorf("DeleteRepo after the stacks were removed: %v", err)
	}
}
