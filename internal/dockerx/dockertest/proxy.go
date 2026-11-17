// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// UnixProxy listens on a unix socket of its own and forwards every connection to the engine at host (a
// DockerHost value such as tcp://localhost:32771), and returns the socket's path. The product only ever
// builds a local-socket transport, so a test that drives product code (the environment service, the
// aggregator) against a dind container reaches it through this, as the server reaches its own host. The
// listener and the connections end with the test.
func UnixProxy(t testing.TB, host string) string {
	t.Helper()
	return SwitchableUnixProxy(t, host).Path()
}

// SwitchProxy is a unix socket whose target engine can be changed while it listens.
type SwitchProxy struct {
	path string

	mu     sync.Mutex
	target string
	conns  map[net.Conn]struct{}
}

// SwitchableUnixProxy is UnixProxy for a test whose server reads DOCKER_HOST once at start and must later
// reach another engine: it listens on a unix socket of its own, forwards each new connection to the
// engine at host (a DockerHost value such as tcp://localhost:32771), and PointAt changes where later
// connections go. The listener and the connections end with the test.
func SwitchableUnixProxy(t testing.TB, host string) *SwitchProxy {
	t.Helper()
	// A short path: a unix socket path may not be longer than about 100 bytes, and a test's own
	// temporary directory is named after the test.
	dir, err := os.MkdirTemp("", "tpx")
	if err != nil {
		t.Fatalf("make the proxy directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	p := &SwitchProxy{path: path, target: strings.TrimPrefix(host, "tcp://"), conns: map[net.Conn]struct{}{}}

	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = l.Close()
		p.closeAll()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			addr := p.target
			p.mu.Unlock()
			out, err := net.Dial("tcp", addr)
			if err != nil {
				_ = in.Close()
				continue
			}
			p.mu.Lock()
			// PointAt ran between the read of the target and now: this pair would reach the old engine
			// after the switch, so it is dropped and the client dials again.
			switched := p.target != addr
			if !switched {
				p.conns[in], p.conns[out] = struct{}{}, struct{}{}
			}
			p.mu.Unlock()
			if switched {
				_ = in.Close()
				_ = out.Close()
				continue
			}
			wg.Go(func() { pipe(out, in) })
			wg.Go(func() { pipe(in, out) })
		}
	})
	return p
}

// Path is the unix socket's path.
func (p *SwitchProxy) Path() string { return p.path }

// PointAt makes every later connection go to the engine at host, and closes the connections forwarded
// so far, so nothing a client pooled before the switch reaches the old engine afterwards.
func (p *SwitchProxy) PointAt(host string) {
	p.mu.Lock()
	p.target = strings.TrimPrefix(host, "tcp://")
	p.mu.Unlock()
	p.closeAll()
}

func (p *SwitchProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
	clear(p.conns)
}

// pipe copies src to dst until either ends, then closes both ends of the pair.
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}
