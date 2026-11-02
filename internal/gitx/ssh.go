// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/tierplan/internal/registry"
)

// sshHandshakeTimeout is how long the connect and the handshake of an ssh call may take, to the moment
// the server has answered the key. go-git's dial ignores the operation's context, so this is a bound
// of the connection itself (see sshdial.go).
const sshHandshakeTimeout = 15 * time.Second

// defaultSSHUser is the user name every large git host expects.
const defaultSSHUser = "git"

// HostKeyState is how TierPlan knows an SSH host: from the keys that ship with it, from a key a
// person trusted, or not at all.
type HostKeyState string

// The host key states of a test.
const (
	HostKeyNone      HostKeyState = "none"
	HostKeyBuiltin   HostKeyState = "builtin"
	HostKeyTrusted   HostKeyState = "trusted"
	HostKeyUntrusted HostKeyState = "untrusted"
	HostKeyChanged   HostKeyState = "changed"
)

// HostKeyStateOf is what TierPlan knows of the host of r before it connects: none for an https
// address or an ssh host with no built-in and no saved key, builtin when the host ships with keys
// (saved keys are ignored then), trusted when a key was saved. A test that finds a different key
// than these is untrusted or changed instead, from the failure it returns.
func HostKeyStateOf(r Remote) HostKeyState {
	u, err := ValidateURL(r.URL)
	if err != nil || u.Scheme != "ssh" {
		return HostKeyNone
	}
	switch {
	case len(builtinFor(u.Host)) > 0:
		return HostKeyBuiltin
	case len(r.HostKeys) > 0:
		return HostKeyTrusted
	}
	return HostKeyNone
}

// pinnedKey is a key the host is known by.
type pinnedKey struct {
	key         ssh.PublicKey
	fingerprint string
}

// pinnedKeys parses the keys the host is trusted by: the built-in ones when the host has any (a
// saved key is ignored for such a host), else the saved ones. A line that does not parse is
// dropped, which leaves the host less trusted, never more.
func pinnedKeys(host string, saved []HostKey) []pinnedKey {
	keys := builtinFor(host)
	if len(keys) == 0 {
		keys = saved
	}
	var out []pinnedKey
	for _, k := range keys {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Line))
		if err != nil {
			continue
		}
		out = append(out, pinnedKey{key: pub, fingerprint: ssh.FingerprintSHA256(pub)})
	}
	return out
}

// hostKeyAlgorithms is what the client offers the server to sign with, from the types of the
// pinned keys: a server must show a key of a type TierPlan holds, else it could present a type it
// has no pin for. An RSA key is signed with SHA-2 by every server in use. Empty means no
// restriction (nothing is pinned).
func hostKeyAlgorithms(pinned []pinnedKey) []string {
	var out []string
	add := func(alg ...string) {
		for _, a := range alg {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	// Preference order: ed25519, ecdsa, rsa.
	for _, pref := range []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSA} {
		if !slices.ContainsFunc(pinned, func(p pinnedKey) bool { return p.key.Type() == pref }) {
			continue
		}
		if pref == ssh.KeyAlgoRSA {
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
			continue
		}
		add(pref)
	}
	return out
}

// presentedOf describes a key a server showed.
func presentedOf(key ssh.PublicKey) PresentedKey {
	return PresentedKey{
		Type:        key.Type(),
		Fingerprint: ssh.FingerprintSHA256(key),
		Line:        strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
	}
}

// hostKeyProbe remembers what the host-key callback decided, so that the reason a connection was
// refused does not depend on how go-git and x/crypto wrap the error.
type hostKeyProbe struct {
	mu       sync.Mutex
	err      error
	verified *PresentedKey
}

func (p *hostKeyProbe) refuse(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
	return err
}

func (p *hostKeyProbe) accept(k PresentedKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verified = &k
}

// refusal is the error the callback refused with, or nil.
func (p *hostKeyProbe) refusal() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// presented is the key that was verified, or nil.
func (p *hostKeyProbe) presented() *PresentedKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verified
}

// hostKeyCallback is the one trust decision of an ssh call; nothing reads a file. It runs during
// the key exchange, before any credential is sent. It refuses, in order: a connected address the
// egress policy blocks, a key different from the pinned key of its type (HostKeyChangedError), and
// a key when nothing is pinned or no pinned key has its type (HostKeyUntrustedError).
func (g *GoGit) hostKeyCallback(host string, pinned []pinnedKey, probe *hostKeyProbe) ssh.HostKeyCallback {
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		ap, err := netip.ParseAddrPort(remote.String())
		if err != nil || g.policy.Blocked(ap.Addr()) {
			// An address that cannot be read is refused: the policy cannot vouch for it.
			return probe.refuse(registry.ErrEgressBlocked)
		}
		presented := presentedOf(key)
		for _, p := range pinned {
			if bytes.Equal(p.key.Marshal(), key.Marshal()) {
				probe.accept(presented)
				return nil
			}
		}
		for _, p := range pinned {
			if p.key.Type() == key.Type() {
				return probe.refuse(&HostKeyChangedError{Host: host, Saved: p.fingerprint, Presented: presented})
			}
		}
		return probe.refuse(&HostKeyUntrustedError{Host: host, Presented: presented})
	}
}

// sshMethod is the go-git auth method of one call: a public key, a host-key callback that is never
// nil (go-git would read the server user's known_hosts then) and a connect timeout.
type sshMethod struct {
	*gitssh.PublicKeys
}

func (m *sshMethod) ClientConfig() (*ssh.ClientConfig, error) {
	cfg, err := m.PublicKeys.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = sshHandshakeTimeout
	return cfg, nil
}

// sshAuth builds the auth method of one ssh call and the probe that records the host-key decision.
// With no credential it offers a throwaway key, never the server's ssh agent (go-git's default when
// no method is given): the handshake and the host key are checked before it is ever used.
func (g *GoGit) sshAuth(r Remote, u URL, call *sshCall) (*sshMethod, *hostKeyProbe, error) {
	var signer ssh.Signer
	user := u.User
	switch a := r.Auth.(type) {
	case nil:
	case SSHKeyAuth:
		signer = a.Signer
		if user == "" {
			user = a.User
		}
	default:
		return nil, nil, &Error{Code: CodeAuthRefused, Host: u.Host}
	}
	if signer == nil {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		if signer, err = ssh.NewSignerFromKey(priv); err != nil {
			return nil, nil, err
		}
	}
	if user == "" {
		user = defaultSSHUser
	}
	signer = markSigner{Signer: signer, call: call}
	pinned := pinnedKeys(u.Host, r.HostKeys)
	probe := &hostKeyProbe{}
	return &sshMethod{&gitssh.PublicKeys{
		User:   user,
		Signer: signer,
		HostKeyCallbackHelper: gitssh.HostKeyCallbackHelper{
			HostKeyCallback:   g.hostKeyCallback(u.Host, pinned, probe),
			HostKeyAlgorithms: hostKeyAlgorithms(pinned),
		},
	}}, probe, nil
}

// lsRemoteSSH lists an ssh remote. The address is refused when the policy blocks the host before
// anything is dialled, the host key is checked before authentication, and nothing of the server
// user's ~/.ssh is read.
func (g *GoGit) lsRemoteSSH(ctx context.Context, r Remote, u URL) (RefList, error) {
	ctx, cancel := context.WithTimeout(registry.WithEgressPolicy(ctx, g.policy), lsRemoteDeadline)
	defer cancel()
	if g.policy.RefusesHost(ctx, net.JoinHostPort(u.Host, strconv.Itoa(u.Port))) {
		return RefList{}, &Error{Code: CodeEgressBlocked, Host: u.Host}
	}
	call, proxyOpts, done := newSSHCall(ctx, g.policy, g.advertisementMax, g.handshake)
	defer done()
	method, probe, err := g.sshAuth(r, u, call)
	if err != nil {
		return RefList{}, Classify(err, u.Host)
	}
	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{u.Raw}})

	// go-git does not look at the context while it connects, but the connection is TierPlan's own and
	// is closed when the context ends, so the call returns then.
	refs, err := rem.ListContext(ctx, &git.ListOptions{PeelingOption: git.AppendPeeled, Auth: method, ProxyOptions: proxyOpts})
	if err != nil {
		if refusal := probe.refusal(); refusal != nil {
			return RefList{}, Classify(refusal, u.Host)
		}
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return RefList{HostKey: probe.presented()}, nil
		}
		return RefList{}, Classify(sshFailure(ctx, call, err), u.Host)
	}
	out := refList(refs)
	out.HostKey = probe.presented()
	return out, nil
}

// sshFailure is the error to classify when an ssh call failed with err: the size refusal when the
// connection was cut for going over its limit (ssh reports that as a closed connection), and the
// context's own error when the call ran out of time or was cancelled (the connection was closed under
// go-git, whose error then says only that it was).
func sshFailure(ctx context.Context, call *sshCall, err error) error {
	if call.tooLarge.Load() {
		return registry.ErrResponseTooLarge
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// isSSHAuthFailure reports whether err is the key exchange having finished and the server having
// refused every key. x/crypto has no typed error for it.
func isSSHAuthFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unable to authenticate")
}
