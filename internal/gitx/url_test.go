// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitx"
)

// The address validator runs before go-git sees an address: transport.NewEndpoint accepts file://,
// bare paths and git:// (RESEARCH Pitfall 3).

func TestValidateURLAcceptsTheAddressesBayplanTalksTo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want gitx.URL
	}{
		{"https://git.example.org/acme/app.git", gitx.URL{Scheme: "https", Host: "git.example.org", Port: 443, Path: "/acme/app.git"}},
		{"https://git.example.org:8443/a/b.git", gitx.URL{Scheme: "https", Host: "git.example.org", Port: 8443, Path: "/a/b.git"}},
		{"ssh://git@git.example.org:2222/a.git", gitx.URL{Scheme: "ssh", User: "git", Host: "git.example.org", Port: 2222, Path: "/a.git"}},
		{"ssh://git.example.org/a.git", gitx.URL{Scheme: "ssh", Host: "git.example.org", Port: 22, Path: "/a.git"}},
		{"git@git.example.org:a/b.git", gitx.URL{Scheme: "ssh", User: "git", Host: "git.example.org", Port: 22, Path: "a/b.git"}},
		{"git@git.example.org:/srv/git/b.git", gitx.URL{Scheme: "ssh", User: "git", Host: "git.example.org", Port: 22, Path: "/srv/git/b.git"}},
		{"https://[2001:db8::1]:8443/x.git", gitx.URL{Scheme: "https", Host: "2001:db8::1", Port: 8443, Path: "/x.git"}},
		{"ssh://git@[2001:db8::1]/x.git", gitx.URL{Scheme: "ssh", User: "git", Host: "2001:db8::1", Port: 22, Path: "/x.git"}},
		{"https://192.168.1.10/x.git", gitx.URL{Scheme: "https", Host: "192.168.1.10", Port: 443, Path: "/x.git"}},
		// The host is lower-cased; the stored address is the input, trimmed, never rewritten.
		{"  https://GitHub.COM/Acme/App.git \n", gitx.URL{Scheme: "https", Host: "github.com", Port: 443, Path: "/Acme/App.git"}},
	} {
		got, err := gitx.ValidateURL(tc.in)
		if err != nil {
			t.Errorf("ValidateURL(%q): %v, want it accepted", tc.in, err)
			continue
		}
		tc.want.Raw = strings.TrimSpace(tc.in)
		if got != tc.want {
			t.Errorf("ValidateURL(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestValidateURLRefusesWhatGoGitWouldHappilyOpen(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"file:///etc", "file:///srv/git/repo.git", "/etc/passwd", "./x", "../x", "x", "",
		"git://git.example.org/x.git", "http://git.example.org/x.git", "ftp://git.example.org/x.git",
		"-oProxyCommand=touch /tmp/x", "--upload-pack=x", "https://-host.example.org/x.git",
		"https://git.example.org/x\n.git", "https://git.example.org/x\x00.git", "https://git.example.org/ x.git", "https://git.exam\tple.org/x.git",
		"https:///x.git", "https://:443/x.git", "ssh:///x.git", "ssh://git@/x.git", "git@:x.git", "git@host",
		"host:x.git", "localhost:8080/x.git", "C:/repo.git",
		// go-git reads the host of the bracketed scp-like form as "[" (WR-A07); ssh://git@[::1]/x.git is the working form.
		"git@[2001:db8::1]:x.git", "git@[::1]:acme/app.git",
		"https://git.example.org:0/x.git", "https://git.example.org:99999/x.git", "ssh://git@git.example.org:abc/x.git",
		"https://git.example.org/x.git?token=abc", "https://git.example.org/x.git#frag",
		"https://exa mple.org/x.git", "https://[::1/x.git", "https://[fe80::1%25eth0]/x.git",
		"https://" + strings.Repeat("a", 2048) + ".org/x.git",
	} {
		_, err := gitx.ValidateURL(in)
		if !errors.Is(err, gitx.ErrURLUnsupported) {
			t.Errorf("ValidateURL(%q) = %v, want ErrURLUnsupported", in, err)
		}
	}
}

func TestValidateURLRefusesACredentialInTheAddress(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"https://user:pass@git.example.org/x.git", "https://token@git.example.org/x.git", "https://:pass@git.example.org/x.git",
		"ssh://git:pw@git.example.org/x.git", "user:pass@git.example.org:x.git",
	} {
		_, err := gitx.ValidateURL(in)
		if !errors.Is(err, gitx.ErrURLUserinfo) {
			t.Errorf("ValidateURL(%q) = %v, want ErrURLUserinfo", in, err)
		}
		if err != nil && (strings.Contains(err.Error(), "git.example.org") || strings.Contains(err.Error(), ":pass") || strings.Contains(err.Error(), ":pw")) {
			t.Errorf("the error for %q carries part of the address: %v", in, err)
		}
	}
}

func TestTheRefusalsReadAsTheScreensWordThem(t *testing.T) {
	t.Parallel()
	_, err := gitx.ValidateURL("file:///etc")
	if err == nil || !strings.Contains(err.Error(), "Use an https:// or ssh:// address, or git@host:path.") {
		t.Errorf("unsupported address: %v, want the text 'Use an https:// or ssh:// address, or git@host:path.'", err)
	}
	_, err = gitx.ValidateURL("https://u:p@h.example.org/x")
	if err == nil || !strings.Contains(err.Error(), "Remove the username and password from the address and add them as a credential.") {
		t.Errorf("userinfo: %v, want the text 'Remove the username and password from the address and add them as a credential.'", err)
	}
}
