// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// hungTransport dials a unix socket that accepts connections and never
// answers, like an engine that is wedged.
type hungTransport struct{ path string }

func newHungTransport(t *testing.T) *hungTransport {
	t.Helper()
	dir, err := os.MkdirTemp("", "bphung")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	return &hungTransport{path: path}
}

func (h *hungTransport) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", h.path)
}

func (h *hungTransport) Describe() string { return "unix://" + h.path }
