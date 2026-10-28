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
