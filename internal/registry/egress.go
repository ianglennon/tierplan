// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrEgressBlocked is what a dial to an address the egress policy refuses fails with. The
// connection is never made.
var ErrEgressBlocked = errors.New("registry: the address is not allowed for registry calls")

// ErrResponseTooLarge is what a read of a registry's answer fails with when the answer is longer
// than the server reads (WR-A01): a tag list is read whole into memory by the registry client, so
// the bound is on the connection.
var ErrResponseTooLarge = errors.New("registry: the registry's answer is larger than the server reads")

// responseBytesMax is the most bytes read from a registry connection after one request was
// written: 32 MiB, well over a manifest (8 MiB at most) or a tag list of the server's own
// 10,000-name cap, and far under what a hostile host could stream in the 30 second call timeout.
const responseBytesMax = 32 << 20

// gitResponseBytesMax is the most bytes read from a git connection after one request was written:
// 1 GiB. A packfile of a large repository is far over the 32 MiB a registry answer may be, and one
// keep-alive connection carries several requests, so the count restarts at each write. It is still
// a bound, so a hostile host cannot stream without end (T-09-04).
const gitResponseBytesMax = 1 << 30

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
	// Other cloud metadata addresses: AWS over IPv6, Alibaba Cloud, Oracle Cloud, and the NAT64
	// spelling of the link-local range (WR-B01, WR-B02).
	netip.MustParsePrefix("fd00:ec2::254/128"),
	netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("192.0.0.192/32"),
	netip.MustParsePrefix("64:ff9b::a9fe:0/112"),
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

// poolWith returns the certificate authorities a connection trusts when the credential brings
// its own: the policy's roots, or the system's, plus caPEM. The pool is new each time, so the
// credential's CA is never added to anything shared (T-05-73).
func (p EgressPolicy) poolWith(caPEM string) *x509.CertPool {
	var pool *x509.CertPool
	if p.roots != nil {
		pool = p.roots.Clone()
	} else if sys, err := x509.SystemCertPool(); err == nil {
		pool = sys
	} else {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM([]byte(caPEM))
	return pool
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

// refusesLookupTimeout bounds the name lookup of RefusesHost.
const refusesLookupTimeout = 5 * time.Second

// RefusesHost reports whether the policy refuses a connection to hostport (host or host:port) as
// this server sees it: the host is an address the policy blocks, or a name that resolves to one.
// It is for a call the server does not make itself, such as a registry check that a node's engine
// makes, so the operator's ranges and the built-in ones are not open to it either. A name the
// server cannot resolve is not refused, because the node may be the only one that can (an
// internal DNS name); and DNS can change between this answer and the engine's own lookup, so it
// narrows the reach rather than closing it (WR-B02).
func (p EgressPolicy) RefusesHost(ctx context.Context, hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if a, err := netip.ParseAddr(host); err == nil {
		return p.Blocked(a)
	}
	ctx, cancel := context.WithTimeout(ctx, refusesLookupTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(addrs, p.Blocked)
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
//
// What it reads is bounded (WR-A01): connections are HTTP/1.1, where one request is answered at a
// time, and a connection fails with ErrResponseTooLarge once more than responseBytesMax bytes have
// come since the last request was written. Compression is off, so that wire bytes are the bytes
// the client reads and a small compressed answer cannot expand without limit.
func (p EgressPolicy) Transport() *http.Transport {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: p.control}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &boundedConn{Conn: c, limit: responseBytesMax}, nil
		},
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DisableCompression:    true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.roots},
	}
}

// egressKey is the context key of a policy that stands in for a git transport's own for one call.
type egressKey struct{}

// WithEgressPolicy returns ctx carrying p as the policy the git transport dials through for the
// calls made with it, in place of the policy the transport was built with. The transport is
// installed once in the process (go-git keeps its protocols in a package variable), so this is how
// a caller that holds its own policy applies it: the dial hook reads the policy from the context
// the HTTP client passes to the dialer. The replaced policy is never looser than the zero value:
// every EgressPolicy always refuses the built-in ranges.
func WithEgressPolicy(ctx context.Context, p EgressPolicy) context.Context {
	return context.WithValue(ctx, egressKey{}, p)
}

// DialControl returns the dialer hook of the policy: it refuses a connection to an address the
// policy blocks, after DNS has answered. It is for a dialer that is not the registry's or git's
// HTTP transport, such as an SSH client.
func (p EgressPolicy) DialControl() func(network, address string, c syscall.RawConn) error {
	return p.control
}

// GitTransport returns a new HTTP transport for git over HTTPS: the dialer, the timeouts and the
// refusal of a proxy are those of Transport, and certificates are always verified (there is no
// field here that turns it off). It differs in what it reads: a git answer is a packfile, so a
// connection fails with ErrResponseTooLarge only once more than gitResponseBytesMax bytes have
// come since the last request was written, not at 32 MiB. Compression stays off, so wire bytes
// are the bytes read and a small compressed answer cannot expand without limit.
//
// It must be an *http.Transport: go-git clones it to add a per-call certificate authority.
func (p EgressPolicy) GitTransport() *http.Transport {
	return p.GitTransportBounded(gitResponseBytesMax)
}

// GitTransportBounded is GitTransport with limit as the most bytes read from a connection after one
// request was written. A listing of a repository's refs is parsed into memory, several times its size
// on the wire, so it is read through a transport with a far smaller limit than a packfile is (WR-A06).
func (p EgressPolicy) GitTransportBounded(limit int64) *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			pol := p
			if o, ok := ctx.Value(egressKey{}).(EgressPolicy); ok {
				pol = o
			}
			d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: pol.control}
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &boundedConn{Conn: c, limit: limit}, nil
		},
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DisableCompression:    true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.roots},
	}
}

// boundedConn counts what is read from a connection since the last write and fails the read that
// would pass limit. HTTP/1.1 writes a request before it reads the answer, so the count is the size
// of one answer (and the TLS framing around it).
type boundedConn struct {
	net.Conn
	limit int64
	read  atomic.Int64
}

func (c *boundedConn) Read(p []byte) (int, error) {
	left := c.limit - c.read.Load()
	if left <= 0 {
		return 0, ErrResponseTooLarge
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *boundedConn) Write(p []byte) (int, error) {
	c.read.Store(0)
	return c.Conn.Write(p)
}
