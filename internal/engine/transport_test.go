// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package engine_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/engine"
)

func TestParseDockerHostAcceptsOnlyUnixSockets(t *testing.T) {
	good := []struct{ in, want string }{
		{"", "/var/run/docker.sock"},
		{"unix:///var/run/docker.sock", "/var/run/docker.sock"},
		{"unix:///run/user/1000/docker.sock", "/run/user/1000/docker.sock"},
		{"unix:///opt/dock er/d.sock", "/opt/dock er/d.sock"},
	}
	for _, c := range good {
		got, err := engine.ParseDockerHost(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseDockerHost(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}

	bad := []string{
		"tcp://1.2.3.4:2375",
		"ssh://h",
		"npipe:////./pipe/docker_engine",
		"fd://",
		"http://localhost:2375",
		"/var/run/docker.sock", // a bare path is not a URL: say unix://
		"unix://relative.sock",
		"unix://",
	}
	for _, in := range bad {
		got, err := engine.ParseDockerHost(in)
		if !errors.Is(err, engine.ErrUnsupportedDockerHost) {
			t.Errorf("ParseDockerHost(%q) = %q, %v; want ErrUnsupportedDockerHost", in, got, err)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "unix") || !strings.Contains(msg, "D-21") {
			t.Errorf("ParseDockerHost(%q) error %q should say unix only and cite D-21", in, msg)
		}
	}
}

func TestLocalSocketTransportDialsItsPathAndIgnoresTheAddress(t *testing.T) {
	dir, err := os.MkdirTemp("", "tpt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()

	tr := engine.LocalSocketTransport{Path: path}
	conn, err := tr.DialContext(context.Background(), "tcp", "10.9.9.9:1")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()
	<-accepted
	if got := tr.Describe(); got != "unix://"+path {
		t.Errorf("Describe() = %q, want unix://%s", got, path)
	}
}

func TestLocalSocketTransportDialErrorNamesThePath(t *testing.T) {
	tr := engine.LocalSocketTransport{Path: "/nonexistent/docker.sock"}
	_, err := tr.DialContext(context.Background(), "unix", "")
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/docker.sock") {
		t.Fatalf("DialContext error = %v, want one naming the socket path", err)
	}
}

func TestSocketExistsIsTrueOnlyForASocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "tpt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !engine.SocketExists(sock) {
		t.Error("SocketExists(socket) = false, want true")
	}
	if engine.SocketExists(file) {
		t.Error("SocketExists(regular file) = true, want false")
	}
	if engine.SocketExists(dir) {
		t.Error("SocketExists(directory) = true, want false")
	}
	if engine.SocketExists(filepath.Join(dir, "missing")) {
		t.Error("SocketExists(missing) = true, want false")
	}
}
