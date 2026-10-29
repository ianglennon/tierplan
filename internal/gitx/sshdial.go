// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"context"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	gotransport "github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"

	"github.com/ianglennon/bayplan/internal/registry"
)

// go-git dials an ssh remote itself, without looking at the operation's context: it applies the
// connect timeout to the TCP connect only and then runs the whole SSH handshake on a connection with
// no deadline (WR-A01). A server that accepts the connection and then says nothing would hold the call,
// and the mirror's write lock with it, for good. go-git does let a call name a proxy, and dials through
// the golang.org/x/net/proxy dialer registered for the proxy URL's scheme. Each ssh call registers one
// such dialer for itself (see sshCall) and names it as its proxy, so the connection is opened here:
//
//   - through the egress policy's dial hook, so the TCP connect itself is checked (and no proxy of the
//     server's environment is used);
//   - closed when the call's context ends, which unblocks go-git wherever it is waiting;
//   - under a deadline until the handshake is done;
//   - counted, so a server cannot stream without end (WR-A06).

// sshProxyScheme is the scheme of the proxy URL that names a call's dialer.
const sshProxyScheme = "bayplan-ssh"

// The most bytes one ssh call reads from the server: 32 MiB for a listing, which go-git parses into
// memory, and 1 GiB for a fetch, which is a packfile that goes to the mirror's disk.
const (
	advertisementBytesMax = 32 << 20
	packBytesMax          = 1 << 30
)

var (
	sshCalls   sync.Map // call id -> *sshCall
	sshCallSeq atomic.Uint64
)

// sshCall is what one ssh call shares with the connection go-git opens for it.
type sshCall struct {
	ctx       context.Context
	policy    registry.EgressPolicy
	limit     int64
	handshake time.Duration

	// signed is set once the key has signed the authentication request: after that the server's next
	// message ends the handshake.
	signed atomic.Bool
	// tooLarge is set when a read was refused for going over limit.
	tooLarge atomic.Bool
}

// newSSHCall registers a call that reads at most limit bytes and lives as long as ctx. The returned
// proxy options go in the FetchOptions or ListOptions of the call, and done is called when it ends.
func newSSHCall(ctx context.Context, p registry.EgressPolicy, limit int64, handshake time.Duration) (*sshCall, gotransport.ProxyOptions, func()) {
	c := &sshCall{ctx: ctx, policy: p, limit: limit, handshake: handshake}
	id := strconv.FormatUint(sshCallSeq.Add(1), 10)
	sshCalls.Store(id, c)
	return c, gotransport.ProxyOptions{URL: sshProxyScheme + "://call-" + id}, func() { sshCalls.Delete(id) }
}

// registerSSHDialer makes the dialers of calls known to go-git. It runs once, from InstallTransport,
// before any call: the x/net registry is a plain map.
func registerSSHDialer() {
	proxy.RegisterDialerType(sshProxyScheme, func(u *url.URL, _ proxy.Dialer) (proxy.Dialer, error) {
		id := u.Hostname()
		if len(id) > len("call-") {
			if v, ok := sshCalls.Load(id[len("call-"):]); ok {
				if c, ok := v.(*sshCall); ok {
					return sshDialer{call: c}, nil
				}
			}
		}
		return nil, registry.ErrEgressBlocked
	})
}

// sshDialer is the dialer of one call. It is a proxy.ContextDialer, which go-git requires.
type sshDialer struct{ call *sshCall }

func (d sshDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d sshDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := d.call.ctx.Err(); err != nil {
		return nil, err
	}
	nd := net.Dialer{Timeout: d.call.handshake, KeepAlive: 30 * time.Second, Control: d.call.policy.DialControl()}
	c, err := nd.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn := &sshConn{Conn: c, call: d.call}
	conn.armed.Store(true)
	if d.call.handshake > 0 {
		_ = c.SetDeadline(time.Now().Add(d.call.handshake))
	}
	conn.stop = context.AfterFunc(d.call.ctx, func() { _ = c.Close() })
	return conn, nil
}

// sshConn is the connection of an ssh call.
type sshConn struct {
	net.Conn
	call  *sshCall
	stop  func() bool
	read  atomic.Int64
	armed atomic.Bool
}

// Read counts what comes in and refuses what is over the call's limit. The first bytes that come after
// the key signed end the handshake, and the deadline that bounded it goes.
func (c *sshConn) Read(p []byte) (int, error) {
	left := c.call.limit - c.read.Load()
	if left <= 0 {
		c.call.tooLarge.Store(true)
		return 0, registry.ErrResponseTooLarge
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	if n > 0 && c.call.signed.Load() && c.armed.CompareAndSwap(true, false) {
		_ = c.SetDeadline(time.Time{})
	}
	return n, err
}

func (c *sshConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

// markSigner marks the call when the key has signed, which is the last thing the client does of the
// handshake. It is an ssh.AlgorithmSigner so that an RSA key still signs with SHA-2.
type markSigner struct {
	ssh.Signer
	call *sshCall
}

func (s markSigner) Sign(r io.Reader, data []byte) (*ssh.Signature, error) {
	defer s.call.signed.Store(true)
	return s.Signer.Sign(r, data)
}

func (s markSigner) SignWithAlgorithm(r io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	defer s.call.signed.Store(true)
	if as, ok := s.Signer.(ssh.AlgorithmSigner); ok {
		return as.SignWithAlgorithm(r, data, algorithm)
	}
	return s.Signer.Sign(r, data)
}
