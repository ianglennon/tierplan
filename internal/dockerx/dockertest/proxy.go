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
	addr := strings.TrimPrefix(host, "tcp://")

	var mu sync.Mutex
	conns := map[net.Conn]struct{}{}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", addr)
			if err != nil {
				_ = in.Close()
				continue
			}
			mu.Lock()
			conns[in], conns[out] = struct{}{}, struct{}{}
			mu.Unlock()
			wg.Go(func() { pipe(out, in) })
			wg.Go(func() { pipe(in, out) })
		}
	})
	return path
}

// pipe copies src to dst until either ends, then closes both ends of the pair.
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}
