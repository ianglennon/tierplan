// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitx

import (
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// builtinTable is the fingerprint of every key that ships with Coaming, as the vendors publish them
// (09-RESEARCH "Built-in pinned SSH host keys": GitHub's api.github.com/meta, GitLab's documentation
// checked against ssh-keyscan, Bitbucket's bitbucket.org/site/ssh). The table is typed out here, apart
// from the keys it checks: a typo in a key fails this test instead of locking everyone out.
var builtinTable = []struct {
	host, algorithm, fingerprint string
}{
	{"github.com", "ssh-ed25519", "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU"},
	{"github.com", "ecdsa-sha2-nistp256", "SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM"},
	{"github.com", "ssh-rsa", "SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s"},
	{"gitlab.com", "ssh-ed25519", "SHA256:eUXGGm1YGsMAS7vkcx6JOJdOGHPem5gQp4taiCfCLB8"},
	{"gitlab.com", "ecdsa-sha2-nistp256", "SHA256:HbW3g8zUjNSksFbqTiUWPWg2Bq1x8xdGUrliXFzSnUw"},
	{"gitlab.com", "ssh-rsa", "SHA256:ROQFvPThGrW4RuWLoL9tq9I9zJ42fK4XywyRtbOz/EQ"},
	{"bitbucket.org", "ssh-ed25519", "SHA256:ybgmFkzwOSotHTHLJgHO0QN8L0xErw6vd0VhFA9m3SM"},
	{"bitbucket.org", "ecdsa-sha2-nistp256", "SHA256:FC73VB6C4OQLSCrjEayhMp9UMxS97caD/Yyi2bhW/J0"},
	{"bitbucket.org", "ssh-rsa", "SHA256:46OSHA1Rmj8E8ERTC6xkNcmGOw9oFxYr0WF6zWW8l1E"},
}

func TestBuiltinHostKeyFingerprints(t *testing.T) {
	t.Parallel()
	if len(builtinTable) != 9 {
		t.Fatalf("the table has %d rows, want the nine of the research", len(builtinTable))
	}
	for _, row := range builtinTable {
		var found bool
		for _, l := range BuiltinHostKeys[row.host] {
			pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l))
			if err != nil {
				t.Errorf("%s: an embedded line does not parse: %v", row.host, err)
				continue
			}
			if pub.Type() != row.algorithm {
				continue
			}
			found = true
			if got := ssh.FingerprintSHA256(pub); got != row.fingerprint {
				t.Errorf("%s %s: fingerprint %s, want %s", row.host, row.algorithm, got, row.fingerprint)
			}
		}
		if !found {
			t.Errorf("%s has no embedded %s key", row.host, row.algorithm)
		}
	}
	// The alternate names carry the keys of their service, and nothing else is built in.
	for alias, host := range map[string]string{"ssh.github.com": "github.com", "altssh.gitlab.com": "gitlab.com", "ssh.bitbucket.org": "bitbucket.org"} {
		if strings.Join(BuiltinHostKeys[alias], "\n") != strings.Join(BuiltinHostKeys[host], "\n") || len(BuiltinHostKeys[alias]) != 3 {
			t.Errorf("%s does not carry the three keys of %s", alias, host)
		}
	}
	if len(BuiltinHostKeys) != 6 {
		t.Errorf("%d built-in hosts, want 6", len(BuiltinHostKeys))
	}
}

func TestBuiltinForIgnoresCaseAndTheTrailingDot(t *testing.T) {
	t.Parallel()
	for _, h := range []string{"github.com", "GitHub.COM", "github.com."} {
		if len(builtinFor(h)) != 3 {
			t.Errorf("builtinFor(%q) found %d keys, want 3", h, len(builtinFor(h)))
		}
	}
	if builtinFor("github.com.evil.example") != nil || builtinFor("notgithub.com") != nil {
		t.Error("a look-alike host has built-in keys")
	}
}

// A built-in host is known by its shipped keys alone: a key saved on a repository of that host is
// ignored, so it can neither widen nor replace the pin.
func TestBuiltinHostWins(t *testing.T) {
	t.Parallel()
	remote := &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 22}
	g := NewGoGit(Options{})
	stored := testKey(t)
	pinned := pinnedKeys("github.com", []HostKey{line(stored)})
	if len(pinned) != 3 {
		t.Fatalf("pinned %d keys for github.com with a saved key, want the 3 built-in ones only", len(pinned))
	}
	cb := g.hostKeyCallback("github.com", pinned, &hostKeyProbe{})

	// GitHub's own ed25519 key passes.
	real, _, _, _, err := ssh.ParseAuthorizedKey([]byte(BuiltinHostKeys["github.com"][0]))
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("github.com:22", remote, real); err != nil {
		t.Errorf("the built-in key: %v, want nil", err)
	}
	// The saved key is a stranger, of the same type as GitHub's: a changed key.
	var changed *HostKeyChangedError
	if err := cb("github.com:22", remote, stored); !errors.As(err, &changed) || changed.Saved != "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU" {
		t.Errorf("the saved key on github.com: %v, want a changed key against GitHub's pin", err)
	}
	if got := HostKeyStateOf(Remote{URL: "git@github.com:acme/app.git", HostKeys: []HostKey{line(stored)}}); got != HostKeyBuiltin {
		t.Errorf("state of github.com with a saved key = %q, want builtin", got)
	}
	// All three types are offered, ed25519 first, so a server may sign with any of them.
	algs := hostKeyAlgorithms(pinned)
	if len(algs) != 4 || algs[0] != ssh.KeyAlgoED25519 || algs[1] != ssh.KeyAlgoECDSA256 || algs[2] != ssh.KeyAlgoRSASHA512 || algs[3] != ssh.KeyAlgoRSASHA256 {
		t.Errorf("algorithms = %v, want ed25519, ecdsa, rsa-sha2-512, rsa-sha2-256", algs)
	}
}
