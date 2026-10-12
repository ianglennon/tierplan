// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"syscall"
	"time"
)

// ErrEgressBlocked is what a dial to an address the egress policy refuses fails with. The
// connection is never made.
var ErrEgressBlocked = errors.New("registry: the address is not allowed for registry calls")

const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

// alwaysBlocked are the ranges no registry lives in: link-local (which holds the cloud
// metadata services), unspecified and multicast. Private and loopback ranges are allowed,
// because self-hosted registries live there (RESEARCH Open Question 4, T-05-38).
var alwaysBlocked = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("ff00::/8"),
}

// EgressPolicy decides which addresses the server may connect to when it talks to a
// registry. It is checked at dial time, after DNS has answered, so a name that resolves to
// a refused address is refused too, and every redirect and token endpoint is covered.
//
// The zero value is the default policy: the built-in ranges are always refused, so a policy
// that was never constructed cannot be the permissive one.
type EgressPolicy struct {
	extra []netip.Prefix
	roots *x509.CertPool
}

// NewEgressPolicy returns the policy that always refuses link-local, unspecified and
// multicast addresses, and also refuses extra (the operator's own ranges).
func NewEgressPolicy(extra []netip.Prefix) EgressPolicy {
	return EgressPolicy{extra: slices.Clone(extra)}
}

// WithRootCAs returns the policy with pool as the certificate authorities that registry
// connections trust, instead of the host's. It is for a private CA the whole server trusts
// and for tests.
func (p EgressPolicy) WithRootCAs(pool *x509.CertPool) EgressPolicy {
	p.roots = pool
	return p
}

// Blocked reports whether the policy refuses a connection to a.
func (p EgressPolicy) Blocked(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, list := range [][]netip.Prefix{alwaysBlocked, p.extra} {
		for _, pr := range list {
			if pr.Contains(a) {
				return true
			}
		}
	}
	return false
}

// control is the dialer hook: it sees the address the dialer is about to connect to, which
// is always an IP address by then.
func (p EgressPolicy) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || p.Blocked(ap.Addr()) {
		return ErrEgressBlocked
	}
	return nil
}

// Transport returns a new HTTP transport that dials through the policy, with a 10 second
// dial and TLS handshake and 30 seconds to the first response byte. It never uses a proxy
// from the environment: a proxy would be dialled instead of the registry and the check
// would see only the proxy's address.
func (p EgressPolicy) Transport() *http.Transport {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: p.control}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.roots},
	}
}
