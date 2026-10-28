// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
)

// Repositories over SSH and the host keys they are trusted by (plan 09-06, GIT-01, D-03). The router,
// policy, CSRF guard, vault and audit writer are the real ones; the repository is served by a real SSH
// server running git upload-pack.

// sshTestResp is the answer of a test, with the part about the host key.
type sshTestResp struct {
	Steps struct {
		Reachable struct{ State string } `json:"reachable"`
		SignedIn  struct{ State string } `json:"signedIn"`
		Refs      struct{ State string } `json:"refs"`
	} `json:"steps"`
	BranchCount int `json:"branchCount"`
	Failure     *struct {
		Code string `json:"code"`
		Host string `json:"host"`
	} `json:"failure"`
	HostKey struct {
		State            string `json:"state"`
		Type             string `json:"type"`
		Fingerprint      string `json:"fingerprint"`
		Key              string `json:"key"`
		SavedFingerprint string `json:"savedFingerprint"`
	} `json:"hostKey"`
}

func decodeSSHTest(t *testing.T, rec *httptest.ResponseRecorder) sshTestResp {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	var r sshTestResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return r
}

// sshRepoFix is an SSH server for one repository, the deploy key it signs in and a saved credential
// for that key.
type sshRepoFix struct {
	f      *envFix
	admin  credentialUser
	srv    *gittest.SSHServer
	credID string
}

func newSSHRepoFix(t *testing.T) *sshRepoFix {
	t.Helper()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	admin := f.asAdmin(t)
	key, authorized, _, _ := deployKey(t)
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		t.Fatal(err)
	}
	repo := gittest.NewRepo(t)
	repo.Commit(t, map[string]string{"compose.yaml": "services: {}\n"}, "first")
	srv := gittest.NewSSHServer(t, repo, gittest.SSHOptions{AuthorizedKey: pub})

	rec := f.credCall(t, admin, http.MethodPost, "/api/git/credentials", gitJSON(map[string]any{"name": "deploy", "type": "ssh", "secret": key}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create credential: %d %s", rec.Code, rec.Body)
	}
	return &sshRepoFix{f: f, admin: admin, srv: srv, credID: decodeGitCred(t, rec).ID}
}

func (s *sshRepoFix) testAddress(t *testing.T, extra map[string]any) sshTestResp {
	t.Helper()
	body := map[string]any{"url": s.srv.URL, "credentialId": s.credID}
	for k, v := range extra {
		body[k] = v
	}
	return decodeSSHTest(t, s.f.credCall(t, s.admin, http.MethodPost, "/api/git/repos/test", gitJSON(body)))
}

func (s *sshRepoFix) createRepo(t *testing.T, name string, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"name": name, "url": s.srv.URL, "credentialId": s.credID}
	for k, v := range extra {
		body[k] = v
	}
	return s.f.credCall(t, s.admin, http.MethodPost, "/api/git/repos", gitJSON(body))
}

func (s *sshRepoFix) testSaved(t *testing.T, repoID string) sshTestResp {
	t.Helper()
	return decodeSSHTest(t, s.f.credCall(t, s.admin, http.MethodPost, "/api/git/repos/"+repoID+"/test", ""))
}

func TestGitSSHTrustOnFirstUse(t *testing.T) {
	t.Parallel()
	s := newSSHRepoFix(t)

	// The first test of an unknown host stops at the handshake and shows the key to confirm.
	first := s.testAddress(t, nil)
	if first.Failure == nil || first.Failure.Code != "host_key_untrusted" || first.Failure.Host != "127.0.0.1" {
		t.Fatalf("first test failure = %+v, want host_key_untrusted at 127.0.0.1", first.Failure)
	}
	hk := first.HostKey
	if hk.State != "untrusted" || hk.Type != "ssh-ed25519" || hk.Fingerprint != ssh.FingerprintSHA256(s.srv.HostKey) || hk.Key != s.srv.HostKeyLine {
		t.Errorf("hostKey = %+v, want untrusted with the server's type, fingerprint and key", hk)
	}
	if n := s.srv.AuthAttempts(); n != 0 {
		t.Errorf("the server saw %d authentication attempts, want none: no credential is sent to a host nobody trusted", n)
	}

	// Sending the key back trusts it for that one test.
	second := s.testAddress(t, map[string]any{"hostKey": hk.Key})
	if second.Failure != nil || second.HostKey.State != "trusted" || second.Steps.SignedIn.State != "passed" || second.BranchCount != 1 {
		t.Errorf("test with the confirmed key = %+v, want it to pass and the host to be trusted", second)
	}
	if again := s.testAddress(t, nil); again.Failure == nil || again.Failure.Code != "host_key_untrusted" {
		t.Errorf("a test without the key after that = %+v, want host_key_untrusted again: nothing was saved", again.Failure)
	}

	// Saving the repository with the key stores it, with who trusted it and when, and audits it.
	rec := s.createRepo(t, "app", map[string]any{"hostKey": hk.Key})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID      string `json:"id"`
		HostKey *struct {
			Type, Fingerprint, TrustedAt string
		} `json:"hostKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.HostKey == nil || created.HostKey.Fingerprint != hk.Fingerprint || created.HostKey.Type != "ssh-ed25519" || created.HostKey.TrustedAt == "" {
		t.Errorf("repository hostKey = %+v, want the confirmed key with the time it was trusted", created.HostKey)
	}
	if strings.Contains(rec.Body.String(), "BEGIN OPENSSH") {
		t.Errorf("the response carries a private key: %s", rec.Body)
	}
	rows := s.f.auditRows(t, "git.host_key_trusted")
	if len(rows) != 1 || rows[0].TargetID != created.ID || rows[0].Details["fingerprint"] != hk.Fingerprint || rows[0].Details["host"] != "127.0.0.1" || rows[0].Details["name"] != "app" {
		t.Errorf("git.host_key_trusted audit rows = %+v, want one for the repository with the fingerprint and host", rows)
	}

	// The saved repository is tested with the saved key.
	saved := s.testSaved(t, created.ID)
	if saved.Failure != nil || saved.HostKey.State != "trusted" || saved.Steps.Refs.State != "passed" {
		t.Errorf("test of the saved repository = %+v, want it to pass with the host trusted", saved)
	}
}

func TestGitSSHHostKeyIsValidated(t *testing.T) {
	t.Parallel()
	s := newSSHRepoFix(t)

	rec := s.f.credCall(t, s.admin, http.MethodPost, "/api/git/repos/test", gitJSON(map[string]any{"url": s.srv.URL, "hostKey": "not a key"}))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "host_key_invalid") {
		t.Errorf("test with a bad key: %d %s, want 422 host_key_invalid", rec.Code, rec.Body)
	}
	rec = s.createRepo(t, "bad", map[string]any{"hostKey": "ssh-ed25519 AAAA"})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "host_key_invalid") {
		t.Errorf("create with a bad key: %d %s, want 422 host_key_invalid", rec.Code, rec.Body)
	}
	rec = s.f.credCall(t, s.admin, http.MethodPost, "/api/git/repos", gitJSON(map[string]any{"name": "https", "url": "https://git.example.org/a.git", "hostKey": s.srv.HostKeyLine}))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "host_key_not_ssh") {
		t.Errorf("create an https repository with a host key: %d %s, want 422 host_key_not_ssh", rec.Code, rec.Body)
	}
}

// savedSSHRepo saves the repository with the server's key trusted and returns its ID.
func (s *sshRepoFix) savedSSHRepo(t *testing.T, name string) string {
	t.Helper()
	rec := s.createRepo(t, name, map[string]any{"hostKey": s.srv.HostKeyLine})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return decodeGitRepo(t, rec).ID
}

func TestGitSSHUntrustedWithoutKey(t *testing.T) {
	t.Parallel()
	s := newSSHRepoFix(t)
	rec := s.createRepo(t, "app", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a repository saved without a key: %d %s", rec.Code, rec.Body)
	}
	id := decodeGitRepo(t, rec).ID

	for i := range 2 {
		got := s.testSaved(t, id)
		if got.Failure == nil || got.Failure.Code != "host_key_untrusted" || got.Failure.Host != "127.0.0.1" {
			t.Fatalf("test %d = %+v, want host_key_untrusted at 127.0.0.1", i, got.Failure)
		}
		if got.HostKey.State != "untrusted" || got.HostKey.Fingerprint != ssh.FingerprintSHA256(s.srv.HostKey) || got.HostKey.Key != s.srv.HostKeyLine {
			t.Errorf("test %d hostKey = %+v, want the key to confirm", i, got.HostKey)
		}
	}
	if n := s.srv.AuthAttempts(); n != 0 {
		t.Errorf("authentication attempts = %d, want none", n)
	}
	// The last test is on the repository.
	one := decodeGitRepo(t, s.f.credCall(t, s.admin, http.MethodGet, "/api/git/repos/"+id, ""))
	if one.LastTest == nil || one.LastTest.OK || one.LastTest.Code != "host_key_untrusted" {
		t.Errorf("last test = %+v, want host_key_untrusted", one.LastTest)
	}
}

func TestGitSSHChangedKeyStops(t *testing.T) {
	t.Parallel()
	s := newSSHRepoFix(t)
	id := s.savedSSHRepo(t, "app")
	if got := s.testSaved(t, id); got.Failure != nil {
		t.Fatalf("test before the key changed: %+v", got.Failure)
	}
	oldFP, attempts := ssh.FingerprintSHA256(s.srv.HostKey), s.srv.AuthAttempts()

	s.srv.RotateHostKey(t)
	newFP := ssh.FingerprintSHA256(s.srv.HostKey)
	for i := range 2 {
		got := s.testSaved(t, id)
		if got.Failure == nil || got.Failure.Code != "host_key_changed" {
			t.Fatalf("test %d after the key changed = %+v, want host_key_changed", i, got.Failure)
		}
		if got.HostKey.State != "changed" || got.HostKey.Fingerprint != newFP || got.HostKey.SavedFingerprint != oldFP {
			t.Errorf("test %d hostKey = %+v, want changed with the new fingerprint %s and the saved %s", i, got.HostKey, newFP, oldFP)
		}
	}
	// Never accepted on its own: no credential was sent, and the saved key is still the old one.
	if n := s.srv.AuthAttempts(); n != attempts {
		t.Errorf("authentication attempts went from %d to %d: a credential was sent to a host whose key changed", attempts, n)
	}
	one := decodeGitRepo(t, s.f.credCall(t, s.admin, http.MethodGet, "/api/git/repos/"+id, ""))
	if one.LastTest == nil || one.LastTest.Code != "host_key_changed" {
		t.Errorf("last test = %+v, want host_key_changed", one.LastTest)
	}
	if got := s.testAddress(t, map[string]any{"hostKey": s.srv.HostKeyLine}); got.Failure != nil || got.HostKey.State != "trusted" {
		t.Errorf("an address test sent with the new key = %+v: a person may confirm it for that one test", got)
	}
	if got := s.testSaved(t, id); got.Failure == nil || got.Failure.Code != "host_key_changed" {
		t.Errorf("the saved repository after that one confirmed test = %+v, want host_key_changed still", got.Failure)
	}
}

func TestGitSSHRetrust(t *testing.T) {
	t.Parallel()
	s := newSSHRepoFix(t)
	id := s.savedSSHRepo(t, "app")
	oldFP := ssh.FingerprintSHA256(s.srv.HostKey)
	s.srv.RotateHostKey(t)
	newFP := ssh.FingerprintSHA256(s.srv.HostKey)
	if got := s.testSaved(t, id); got.Failure == nil || got.Failure.Code != "host_key_changed" {
		t.Fatalf("test after the key changed = %+v", got.Failure)
	}

	for name, body := range map[string]string{
		"not a key":   gitJSON(map[string]any{"hostKey": "not a key"}),
		"no key":      gitJSON(map[string]any{}),
		"a truncated": gitJSON(map[string]any{"hostKey": "ssh-ed25519 AAAA"}),
	} {
		if rec := s.f.credCall(t, s.admin, http.MethodPost, "/api/git/repos/"+id+"/trust-host-key", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", name, rec.Code, rec.Body)
		}
	}
	if rec := s.f.credCall(t, s.admin, http.MethodPost, "/api/git/repos/nope/trust-host-key", gitJSON(map[string]any{"hostKey": s.srv.HostKeyLine})); rec.Code != http.StatusNotFound {
		t.Errorf("a repository that does not exist: %d %s, want 404", rec.Code, rec.Body)
	}

	rec := s.f.credCall(t, s.admin, http.MethodPost, "/api/git/repos/"+id+"/trust-host-key", gitJSON(map[string]any{"hostKey": s.srv.HostKeyLine}))
	if rec.Code != http.StatusOK {
		t.Fatalf("trust-host-key: %d %s", rec.Code, rec.Body)
	}
	var repo struct {
		HostKey  *struct{ Fingerprint string } `json:"hostKey"`
		LastTest *struct{}                     `json:"lastTest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil || repo.HostKey == nil || repo.HostKey.Fingerprint != newFP {
		t.Errorf("repository after trusting = %s (%v), want the new key", rec.Body, err)
	}
	if repo.LastTest != nil {
		t.Errorf("the last test was kept: it was seen against a key that no longer applies")
	}
	rows := s.f.auditRows(t, "git.host_key_retrusted")
	if len(rows) != 1 || rows[0].TargetID != id || rows[0].Details["fingerprint"] != newFP || rows[0].Details["fingerprint_from"] != oldFP {
		t.Errorf("git.host_key_retrusted audit rows = %+v, want one with both fingerprints", rows)
	}
	if got := s.testSaved(t, id); got.Failure != nil || got.HostKey.State != "trusted" {
		t.Errorf("test after re-trusting = %+v, want it to pass", got)
	}
}

func TestGitSSHRetrustNeedsGitManageOrGitInstance(t *testing.T) {
	t.Parallel()
	s := newSSHRepoFix(t)
	f := s.f
	env := f.add(t).ID
	envAdmin := f.asRoleOn(t, "envadmin", env, "admin")
	operator := f.asRoleOn(t, "operator", env, "operator")
	hk := gitJSON(map[string]any{"hostKey": s.srv.HostKeyLine})

	// An environment repository: its Admin may trust a key, an Operator may not.
	rec := f.credCall(t, envAdmin, http.MethodPost, "/api/env/"+env+"/git/repos",
		gitJSON(map[string]any{"name": "envrepo", "url": s.srv.URL, "credentialId": s.credID}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create environment repository: %d %s", rec.Code, rec.Body)
	}
	envRepo := decodeGitRepo(t, rec).ID
	if rec := f.credCall(t, operator, http.MethodPost, "/api/env/"+env+"/git/repos/"+envRepo+"/trust-host-key", hk); rec.Code != http.StatusForbidden {
		t.Errorf("operator: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, envAdmin, http.MethodPost, "/api/env/"+env+"/git/repos/"+envRepo+"/trust-host-key", hk); rec.Code != http.StatusOK {
		t.Errorf("env admin: %d %s, want 200", rec.Code, rec.Body)
	}
	rows := f.auditRows(t, "git.host_key_trusted")
	if len(rows) != 1 || rows[0].EnvID != env || rows[0].Details["layer"] != "environment" {
		t.Errorf("git.host_key_trusted rows = %+v, want one for the environment, the first key of a repository", rows)
	}

	// The instance twin needs git.instance, and each route answers only for its own layer.
	instRepo := s.savedSSHRepo(t, "instrepo")
	if rec := f.credCall(t, envAdmin, http.MethodPost, "/api/git/repos/"+instRepo+"/trust-host-key", hk); rec.Code != http.StatusForbidden {
		t.Errorf("env admin on the instance route: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, s.admin, http.MethodPost, "/api/git/repos/"+envRepo+"/trust-host-key", hk); rec.Code != http.StatusNotFound {
		t.Errorf("an environment repository on the instance route: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, envAdmin, http.MethodPost, "/api/env/"+env+"/git/repos/"+instRepo+"/trust-host-key", hk); rec.Code != http.StatusNotFound {
		t.Errorf("an instance repository on the environment route: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, s.admin, http.MethodPost, "/api/git/repos/"+instRepo+"/trust-host-key", hk); rec.Code != http.StatusOK {
		t.Errorf("instance admin on the instance route: %d %s, want 200", rec.Code, rec.Body)
	}
}
