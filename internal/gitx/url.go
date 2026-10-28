// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"errors"
	"net/url"
	"strings"
)

// ErrURLUnsupported is what ValidateURL returns for an address that is not an https:// or ssh://
// address or git@host:path.
var ErrURLUnsupported = errors.New("gitx: use an https:// or ssh:// address, or git@host:path")

// ErrURLUserinfo is what ValidateURL returns for an address that carries a user name and password.
var ErrURLUserinfo = errors.New("gitx: remove the username and password from the address and add them as a credential")

// URL is a validated repository address.
type URL struct {
	// Raw is the address as it will be stored and used: the trimmed input.
	Raw string
	// Scheme is "https" or "ssh".
	Scheme string
	// Host is the lower-cased host without brackets or port.
	Host string
	// Port is the port, or 0 for the scheme's default.
	Port int
	// Path is the repository path.
	Path string
}

// ValidateURL accepts the addresses Bayplan talks to. This is the minimal form of the tracer; the
// full validator follows.
func ValidateURL(raw string) (URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return URL{}, ErrURLUnsupported
	}
	return URL{Raw: raw, Scheme: "https", Host: strings.ToLower(u.Hostname()), Path: u.Path}, nil
}
