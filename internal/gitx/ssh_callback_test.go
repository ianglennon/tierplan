// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitx

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/tierplan/internal/registry"
)

func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func line(k ssh.PublicKey) HostKey { return HostKey{Line: string(ssh.MarshalAuthorizedKey(k))} }

// The callback checks the address the connection actually reached, not the one it was told to
// reach: a name that resolved to a blocked address after the first check is refused here, before
// any credential is sent (RESEARCH Pitfall 2).
func TestHostKeyCallbackRefusesAConnectedAddressThePolicyBlocks(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	g := NewGoGit(Options{Policy: registry.NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})})
	probe := &hostKeyProbe{}
	cb := g.hostKeyCallback("git.example.org", pinnedKeys("git.example.org", []HostKey{line(key)}), probe)

	// Even the pinned key is refused: the address decides first.
	err := cb("git.example.org:22", &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 22}, key)
	if !errors.Is(err, registry.ErrEgressBlocked) {
		t.Errorf("a blocked connected address: %v, want ErrEgressBlocked", err)
	}
	if err := cb("git.example.org:22", &net.TCPAddr{IP: net.ParseIP("169.254.169.254"), Port: 22}, key); !errors.Is(err, registry.ErrEgressBlocked) {
		t.Errorf("the cloud metadata address: %v, want ErrEgressBlocked", err)
	}
	if err := cb("git.example.org:22", &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 22}, key); err != nil {
		t.Errorf("an allowed address with the pinned key: %v, want nil", err)
	}
	// An address that cannot be read is refused: the policy cannot vouch for it.
	if err := cb("git.example.org:22", &net.UnixAddr{Name: "/x", Net: "unix"}, key); !errors.Is(err, registry.ErrEgressBlocked) {
		t.Errorf("an unreadable address: %v, want ErrEgressBlocked", err)
	}
}

func TestHostKeyCallbackDistinguishesChangedFromUntrusted(t *testing.T) {
	t.Parallel()
	saved, other := testKey(t), testKey(t)
	g := NewGoGit(Options{})
	remote := &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 22}

	var changed *HostKeyChangedError
	cb := g.hostKeyCallback("h", pinnedKeys("h", []HostKey{line(saved)}), &hostKeyProbe{})
	if err := cb("h:22", remote, other); !errors.As(err, &changed) || changed.Saved != ssh.FingerprintSHA256(saved) || changed.Presented.Fingerprint != ssh.FingerprintSHA256(other) {
		t.Errorf("a different key of the same type: %v, want a HostKeyChangedError with both fingerprints", err)
	}
	var untrusted *HostKeyUntrustedError
	cb = g.hostKeyCallback("h", pinnedKeys("h", nil), &hostKeyProbe{})
	if err := cb("h:22", remote, other); !errors.As(err, &untrusted) || untrusted.Presented.Line == "" {
		t.Errorf("a host with nothing pinned: %v, want a HostKeyUntrustedError carrying the key", err)
	}
}
