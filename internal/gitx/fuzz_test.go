// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitx_test

import (
	"strings"
	"testing"
	"unicode"

	"github.com/ianglennon/tierplan/internal/gitx"
)

// FuzzValidateURL: whatever is typed, an accepted address is https or ssh, names a host, has no
// control character and carries no password. The seeds run in every go test.
func FuzzValidateURL(f *testing.F) {
	for _, seed := range []string{
		"https://git.example.org/acme/app.git", "https://git.example.org:8443/a/b.git", "ssh://git@git.example.org:2222/a.git",
		"git@git.example.org:a/b.git", "https://[2001:db8::1]:8443/x.git", "git@[::1]:acme/app.git",
		"file:///etc", "/etc/passwd", "./x", "git://host/x", "http://host/x", "https://user:pass@host/x",
		"-oProxyCommand=x", "https://host/x\n", "https:///x", "", "https://host/x?token=abc", "host:x.git",
		"user:pass@host:x.git", "ssh://git:pw@host/x", strings.Repeat("a", 3000),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		u, err := gitx.ValidateURL(in)
		if err != nil {
			return
		}
		if u.Scheme != "https" && u.Scheme != "ssh" {
			t.Fatalf("%q accepted with scheme %q", in, u.Scheme)
		}
		if u.Host == "" || strings.HasPrefix(u.Host, "-") {
			t.Fatalf("%q accepted with host %q", in, u.Host)
		}
		if u.Port < 1 || u.Port > 65535 {
			t.Fatalf("%q accepted with port %d", in, u.Port)
		}
		if len(u.Raw) > 2048 || strings.HasPrefix(u.Raw, "-") {
			t.Fatalf("%q accepted as %q", in, u.Raw)
		}
		for _, r := range u.Raw {
			if unicode.IsControl(r) || unicode.IsSpace(r) {
				t.Fatalf("%q accepted with a control or space character in %q", in, u.Raw)
			}
		}
		if strings.Contains(u.Raw, "://") && strings.Contains(strings.SplitN(u.Raw[strings.Index(u.Raw, "://")+3:], "/", 2)[0], ":") {
			// A colon in the authority is a port or an IPv6 literal; a password would sit before an '@'.
			auth := strings.SplitN(u.Raw[strings.Index(u.Raw, "://")+3:], "/", 2)[0]
			if at := strings.LastIndex(auth, "@"); at >= 0 && strings.Contains(auth[:at], ":") {
				t.Fatalf("%q accepted with a password: %q", in, u.Raw)
			}
		}
	})
}
