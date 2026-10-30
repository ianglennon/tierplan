// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
	"github.com/ianglennon/bayplan/internal/registry"
)

// SSH repositories (plan 09-06, GIT-01, D-03). The server is a real SSH server in this process that
// runs git upload-pack on a real repository.

// deployKey is a signer for a deploy key.
func deployKey(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.PublicKey()
}

// sshFixture is a repository with main and dev behind an SSH server that signs in one deploy key.
type sshFixture struct {
	srv    *gittest.SSHServer
	signer ssh.Signer
	dev    string
}

func newSSHFixture(t *testing.T) *sshFixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	repo.Commit(t, map[string]string{"compose.yaml": "services: {}\n"}, "first")
	dev := repo.Commit(t, map[string]string{"compose.yaml": "services: {a: {image: x}}\n"}, "second")
	repo.Branch(t, "dev", dev)
	signer, pub := deployKey(t)
	return &sshFixture{srv: gittest.NewSSHServer(t, repo, gittest.SSHOptions{AuthorizedKey: pub}), signer: signer, dev: dev}
}

func (f *sshFixture) remote(keys ...string) gitx.Remote {
	r := gitx.Remote{URL: f.srv.URL, Auth: gitx.SSHKeyAuth{User: "git", Signer: f.signer}}
	for _, k := range keys {
		r.HostKeys = append(r.HostKeys, gitx.HostKey{Line: k})
	}
	return r
}

func asError(t *testing.T, err error) *gitx.Error {
	t.Helper()
	var ge *gitx.Error
	if !errors.As(err, &ge) {
		t.Fatalf("error = %v, want a *gitx.Error", err)
	}
	return ge
}

func TestSSHListsTheRefsOfARepositoryWithATrustedHostKey(t *testing.T) {
	t.Parallel()
	f := newSSHFixture(t)
	g := gitx.NewGoGit(gitx.Options{})

	got, err := g.LsRemote(context.Background(), f.remote(f.srv.HostKeyLine))
	if err != nil {
		t.Fatalf("LsRemote: %v", err)
	}
	if len(got.Branches) != 2 || got.Branches[0].Name != "dev" || got.Branches[1].Name != "main" || got.Default != "main" {
		t.Errorf("refs = %+v, want branches dev and main with main the default", got)
	}
	if got.HostKey == nil || got.HostKey.Fingerprint != ssh.FingerprintSHA256(f.srv.HostKey) {
		t.Errorf("verified host key = %+v, want the server's", got.HostKey)
	}
}

func TestSSHTrustOnFirstUseStopsAtTheHandshake(t *testing.T) {
	t.Parallel()
	f := newSSHFixture(t)
	g := gitx.NewGoGit(gitx.Options{})

	_, err := g.LsRemote(context.Background(), f.remote())
	ge := asError(t, err)
	if ge.Code != gitx.CodeHostKeyUntrusted || ge.Host != "127.0.0.1" {
		t.Fatalf("error = %v, want host_key_untrusted at 127.0.0.1", err)
	}
	p := ge.Presented
	if p == nil || p.Type != "ssh-ed25519" || p.Fingerprint != ssh.FingerprintSHA256(f.srv.HostKey) || p.Line != f.srv.HostKeyLine {
		t.Errorf("presented = %+v, want the server's type, fingerprint and key line", p)
	}
	if n := f.srv.AuthAttempts(); n != 0 {
		t.Errorf("the server saw %d authentication attempts, want none: no credential is sent before the host key is trusted", n)
	}

	// Re-sending the test with that key trusts it for that call.
	if _, err := g.LsRemote(context.Background(), f.remote(p.Line)); err != nil {
		t.Errorf("LsRemote with the confirmed key: %v", err)
	}
	if n := f.srv.AuthAttempts(); n == 0 {
		t.Error("the server saw no sign-in with the confirmed key")
	}
}

func TestSSHWithoutACredentialStopsAtTheHandshakeToo(t *testing.T) {
	t.Parallel()
	f := newSSHFixture(t)
	g := gitx.NewGoGit(gitx.Options{})

	_, err := g.LsRemote(context.Background(), gitx.Remote{URL: f.srv.URL})
	if ge := asError(t, err); ge.Code != gitx.CodeHostKeyUntrusted {
		t.Errorf("error = %v, want host_key_untrusted so the key can be confirmed before a credential is picked", err)
	}
	if n := f.srv.AuthAttempts(); n != 0 {
		t.Errorf("authentication attempts = %d, want none", n)
	}
}

func TestSSHWrongKeyIsAuthRefusedAfterTheHostWasVerified(t *testing.T) {
	t.Parallel()
	f := newSSHFixture(t)
	g := gitx.NewGoGit(gitx.Options{})
	other, _ := deployKey(t)

	r := f.remote(f.srv.HostKeyLine)
	r.Auth = gitx.SSHKeyAuth{Signer: other}
	_, err := g.LsRemote(context.Background(), r)
	if ge := asError(t, err); ge.Code != gitx.CodeAuthRefused {
		t.Errorf("error = %v, want auth_refused", err)
	}
	r.Auth = gitx.TokenAuth{Username: "u", Token: "t"}
	_, err = g.LsRemote(context.Background(), r)
	if ge := asError(t, err); ge.Code != gitx.CodeAuthRefused {
		t.Errorf("a token on an ssh address: %v, want auth_refused", err)
	}
	if _, err := g.LsRemote(context.Background(), gitx.Remote{URL: "https://127.0.0.1:1/x.git", Auth: gitx.SSHKeyAuth{Signer: other}}); asError(t, err).Code != gitx.CodeAuthRefused {
		t.Errorf("a deploy key on an https address: %v, want auth_refused", err)
	}
}

func TestSSHAnAddressThePolicyBlocksIsRefusedBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	f := newSSHFixture(t)
	g := gitx.NewGoGit(gitx.Options{Policy: registry.NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})})

	_, err := g.LsRemote(context.Background(), f.remote(f.srv.HostKeyLine))
	if ge := asError(t, err); ge.Code != gitx.CodeEgressBlocked {
		t.Errorf("error = %v, want egress_blocked", err)
	}
	if n := f.srv.AuthAttempts(); n != 0 {
		t.Errorf("authentication attempts = %d, want none", n)
	}
}

// The server user's own ~/.ssh is never read: a host that is in its known_hosts is still untrusted,
// and go-git's ssh_config reader is off (RESEARCH Pitfall 2).
func TestSSHNeverReadsTheServersKnownHostsOrConfig(t *testing.T) {
	f := newSSHFixture(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	u, err := gitx.ValidateURL(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	known := "[127.0.0.1]:" + strconv.Itoa(u.Port) + " " + f.srv.HostKeyLine + "\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte(known), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	if gitssh.DefaultSSHConfig != nil {
		t.Error("DefaultSSHConfig is set: ~/.ssh/config would rewrite hosts and ports")
	}
	_, err = gitx.NewGoGit(gitx.Options{}).LsRemote(context.Background(), f.remote())
	if ge := asError(t, err); ge.Code != gitx.CodeHostKeyUntrusted {
		t.Errorf("error = %v, want host_key_untrusted: the known_hosts of the server's user must not be consulted", err)
	}
}
