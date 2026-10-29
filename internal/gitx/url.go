// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// The refusals read as the screens word them (09-UI-SPEC "Shared refusals and failures"). The
// text carries no part of the address, so a password typed into one never comes back in a message.
var (
	// ErrURLUnsupported is what ValidateURL returns for an address that is not an https:// or ssh://
	// address or git@host:path.
	ErrURLUnsupported = errors.New("Use an https:// or ssh:// address, or git@host:path.") //nolint:staticcheck // ST1005: this is the text the screens show
	// ErrURLUserinfo is what ValidateURL returns for an address that carries a user name and
	// password (or an https user name alone).
	ErrURLUserinfo = errors.New("Remove the username and password from the address and add them as a credential.") //nolint:staticcheck // ST1005: this is the text the screens show
)

// maxURLLen is the longest address accepted.
const maxURLLen = 2048

var (
	// hostName is a DNS name or an IPv4 literal: labels of letters, digits, hyphens and
	// underscores, never starting with a hyphen.
	hostName = regexp.MustCompile(`^[a-z0-9_]([a-z0-9._-]*[a-z0-9_])?$`)
	// sshUser is a user name of an ssh address.
	sshUser = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// URL is a validated repository address.
type URL struct {
	// Raw is the address as it will be stored and used: the trimmed input, never a rewritten one.
	Raw string
	// Scheme is "https" or "ssh" (the scp-like git@host:path is ssh).
	Scheme string
	// User is the user name of an ssh address (git in git@host:path); empty for https.
	User string
	// Host is the lower-cased host without brackets or port.
	Host string
	// Port is the port, the scheme's default (443, 22) when the address has none.
	Port int
	// Path is the repository path as written: /acme/app.git, or acme/app.git for the scp-like form.
	Path string
}

// ValidateURL accepts the addresses Bayplan talks to and refuses everything else before go-git sees
// the address: transport.NewEndpoint accepts file://, bare paths and git:// (RESEARCH Pitfall 3).
//
// Accepted: https://host[:port]/path, ssh://[user@]host[:port]/path and the scp-like
// user@host:path; an IPv6 literal goes in brackets in the first two (go-git reads no brackets in the scp-like form).
// Refused with ErrURLUnsupported: any other
// scheme, a path, a value that starts with "-", a control or space character, a query or fragment,
// an empty host or a bad port, and more than 2048 bytes. Refused with ErrURLUserinfo: a password in
// the address, or any user in an https address (the credential is a separate object). An ssh user
// without a password (git@) is the way to name the account and is allowed.
func ValidateURL(raw string) (URL, error) {
	u, err := validateURL(raw)
	if err != nil {
		return URL{}, err
	}
	// Everything keyed on the host (the egress check, the host keys, the credential's destination) is judged on the
	// host this parser found, while go-git connects to the one its own parser finds in the same text. They must be
	// the same host, or the checks vouch for a place that is not the one dialled (WR-A07).
	if !agreesWithGoGit(u) {
		return URL{}, ErrURLUnsupported
	}
	return u, nil
}

// agreesWithGoGit says whether go-git reads the address u was made from as the same scheme, host and port.
func agreesWithGoGit(u URL) bool {
	ep, err := transport.NewEndpoint(u.Raw)
	if err != nil {
		return false
	}
	port := ep.Port
	if port == 0 {
		switch u.Scheme {
		case "https":
			port = 443
		case "ssh":
			port = 22
		}
	}
	return ep.Protocol == u.Scheme && strings.ToLower(strings.Trim(ep.Host, "[]")) == u.Host && port == u.Port
}

// validateURL is ValidateURL before the check against go-git's reading.
func validateURL(raw string) (URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > maxURLLen || strings.HasPrefix(s, "-") || strings.ContainsAny(s, "?#\\") {
		return URL{}, ErrURLUnsupported
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return URL{}, ErrURLUnsupported
		}
	}
	if scheme, rest, ok := strings.Cut(s, "://"); ok {
		switch strings.ToLower(scheme) {
		case "https":
			return parseHTTPS(s, rest)
		case "ssh":
			return parseSSH(s, rest)
		}
		return URL{}, ErrURLUnsupported
	}
	return parseSCP(s)
}

// parseHTTPS reads what follows https://.
func parseHTTPS(s, rest string) (URL, error) {
	authority, path := splitAuthority(rest)
	if strings.Contains(authority, "@") {
		return URL{}, ErrURLUserinfo
	}
	host, port, err := parseHostPort(authority, 443)
	if err != nil {
		return URL{}, err
	}
	return URL{Raw: s, Scheme: "https", Host: host, Port: port, Path: path}, nil
}

// parseSSH reads what follows ssh://.
func parseSSH(s, rest string) (URL, error) {
	authority, path := splitAuthority(rest)
	if len(path) < 2 {
		return URL{}, ErrURLUnsupported
	}
	user, hostport := "", authority
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		user, hostport = authority[:at], authority[at+1:]
		if strings.Contains(user, ":") {
			return URL{}, ErrURLUserinfo
		}
		if !sshUser.MatchString(user) {
			return URL{}, ErrURLUnsupported
		}
	}
	host, port, err := parseHostPort(hostport, 22)
	if err != nil {
		return URL{}, err
	}
	return URL{Raw: s, Scheme: "ssh", User: user, Host: host, Port: port, Path: path}, nil
}

// parseSCP reads the scp-like user@host:path.
func parseSCP(s string) (URL, error) {
	at := strings.Index(s, "@")
	if at <= 0 {
		return URL{}, ErrURLUnsupported
	}
	user, rest := s[:at], s[at+1:]
	if strings.Contains(user, ":") && !strings.Contains(user, "/") {
		return URL{}, ErrURLUserinfo
	}
	if !sshUser.MatchString(user) {
		return URL{}, ErrURLUnsupported
	}
	// A bracketed IPv6 host is refused here: go-git cuts user@[::1]:path at the first colon, so it would connect
	// to a host called "[" (WR-A07). ssh://user@[::1]/path is the form that works.
	i := strings.Index(rest, ":")
	if i < 0 || strings.HasPrefix(rest, "[") {
		return URL{}, ErrURLUnsupported
	}
	host, path := rest[:i], rest[i+1:]
	if path == "" {
		return URL{}, ErrURLUnsupported
	}
	host, port, err := parseHostPort(host, 22)
	if err != nil {
		return URL{}, err
	}
	return URL{Raw: s, Scheme: "ssh", User: user, Host: host, Port: port, Path: path}, nil
}

// splitAuthority splits what follows "scheme://" at the first slash.
func splitAuthority(rest string) (authority, path string) {
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i], rest[i:]
	}
	return rest, ""
}

// parseHostPort reads host[:port] or [ipv6][:port] and returns the lower-cased host without
// brackets and the port, def when there is none.
func parseHostPort(hp string, def int) (string, int, error) {
	host, portText := hp, ""
	if strings.HasPrefix(hp, "[") {
		end := strings.Index(hp, "]")
		if end < 0 {
			return "", 0, ErrURLUnsupported
		}
		host = hp[1:end]
		switch tail := hp[end+1:]; {
		case tail == "":
		case strings.HasPrefix(tail, ":"):
			portText = tail[1:]
		default:
			return "", 0, ErrURLUnsupported
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", 0, ErrURLUnsupported
		}
		host = addr.String()
	} else {
		if i := strings.LastIndex(hp, ":"); i >= 0 {
			host, portText = hp[:i], hp[i+1:]
		}
		host = strings.ToLower(host)
		if len(host) > 253 || !hostName.MatchString(host) {
			return "", 0, ErrURLUnsupported
		}
	}
	port := def
	if portText != "" {
		n, err := strconv.Atoi(portText)
		if err != nil || len(portText) > 5 || n < 1 || n > 65535 || strings.ContainsAny(portText, "+-") {
			return "", 0, ErrURLUnsupported
		}
		port = n
	} else if strings.HasSuffix(hp, ":") {
		return "", 0, ErrURLUnsupported
	}
	return host, port, nil
}
