// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gittest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

// SSHOptions are the choices of NewSSHServer.
type SSHOptions struct {
	// AuthorizedKey is the only public key the server signs in. Nil signs in any key.
	AuthorizedKey ssh.PublicKey
}

// SSHServer serves one repository over SSH, in this process, with the command line's
// git-upload-pack behind it.
type SSHServer struct {
	// URL is the address of the repository: ssh://git@127.0.0.1:port/path/repo.git.
	URL string
	// HostKey is the key the server shows now; RotateHostKey changes it.
	HostKey ssh.PublicKey
	// HostKeyLine is HostKey in authorized_keys form, without a trailing newline.
	HostKeyLine string

	repo     *Repo
	opts     SSHOptions
	ln       net.Listener
	signer   atomic.Pointer[ssh.Signer]
	attempts atomic.Int64

	mu    sync.Mutex
	conns []net.Conn
}

// AuthAttempts is how many times a client tried to authenticate, other than the "none" probe that
// asks which methods exist. A client that stops at the host key makes none.
func (s *SSHServer) AuthAttempts() int { return int(s.attempts.Load()) }

// RotateHostKey makes the server show a new host key of the same type from now on, as a server
// that was reinstalled would, and updates HostKey and HostKeyLine. It must not run while a client
// is connecting.
func (s *SSHServer) RotateHostKey(tb testing.TB) {
	tb.Helper()
	signer := newSigner(tb)
	s.signer.Store(&signer)
	s.HostKey = signer.PublicKey()
	s.HostKeyLine = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.HostKey)))
}

func newSigner(tb testing.TB) ssh.Signer {
	tb.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		tb.Fatal(err)
	}
	return signer
}

// NewSSHServer serves r over SSH on a free loopback port and stops it when the test ends. The
// server runs `git upload-pack` on the repository for the one command a client sends.
func NewSSHServer(tb testing.TB, r *Repo, o SSHOptions) *SSHServer {
	tb.Helper()
	requireGit(tb)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	s := &SSHServer{repo: r, opts: o, ln: ln}
	s.RotateHostKey(tb)
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		tb.Fatalf("listener address %v is not TCP", ln.Addr())
	}
	port := addr.Port
	s.URL = "ssh://git@127.0.0.1:" + strconv.Itoa(port) + "/" + strings.TrimPrefix(r.bare, "/")

	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.serve(c)
			}()
		}
	}()
	tb.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		wg.Wait()
	})
	return s
}

// config is the server configuration of one connection, with the host key of now.
func (s *SSHServer) config() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if s.opts.AuthorizedKey != nil && !bytes.Equal(s.opts.AuthorizedKey.Marshal(), key.Marshal()) {
				return nil, errors.New("key not authorized")
			}
			return nil, nil
		},
		AuthLogCallback: func(_ ssh.ConnMetadata, method string, _ error) {
			if method != "none" {
				s.attempts.Add(1)
			}
		},
	}
	cfg.AddHostKey(*s.signer.Load())
	return cfg
}

// serve handles one connection: the handshake, then exec requests on session channels.
func (s *SSHServer) serve(c net.Conn) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, s.config())
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.session(ch, creqs)
	}
}

// session runs the one exec request a git client sends.
func (s *SSHServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		name, path, ok := splitCommand(payload.Command)
		if !ok || name != "git-upload-pack" || strings.TrimPrefix(path, "/") != strings.TrimPrefix(s.repo.bare, "/") {
			_ = req.Reply(true, nil)
			_, _ = io.WriteString(ch.Stderr(), "fatal: '"+path+"' does not appear to be a git repository\n")
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{128}))
			return
		}
		_ = req.Reply(true, nil)
		cmd := exec.Command("git", "upload-pack", s.repo.bare) //nolint:gosec // G204: the path is the fixture's own repository
		cmd.Env = s.repo.env
		cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
		status := uint32(0)
		if err := cmd.Run(); err != nil {
			status = 1
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

// splitCommand reads `git-upload-pack '/path'`.
func splitCommand(cmd string) (name, path string, ok bool) {
	name, rest, found := strings.Cut(strings.TrimSpace(cmd), " ")
	if !found {
		return "", "", false
	}
	return name, strings.Trim(strings.TrimSpace(rest), `'"`), true
}
