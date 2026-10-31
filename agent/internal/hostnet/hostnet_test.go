// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package hostnet

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/proto/hostroute"
	"github.com/ianglennon/coaming/proto/wire"
)

const fixture = "../../../proto/hostroute/testdata/route-host.txt"

func TestProbeAndRead(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the fixture is a little-endian host's /proc/net/route")
	}
	dir := t.TempDir()
	if got := Probe(filepath.Join(dir, "missing")); got != wire.HostNetUnmounted {
		t.Errorf("Probe(missing) = %q", got)
	}
	if got := Probe(dir); got != wire.HostNetUnmounted {
		t.Errorf("Probe(directory) = %q", got)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil { //nolint:gosec // a temp path
		t.Fatal(err)
	}
	if got := Probe(empty); got != wire.HostNetUnmounted {
		t.Errorf("Probe(empty file) = %q", got)
	}
	if got := Probe(fixture); got != wire.HostNetOK {
		t.Errorf("Probe(fixture) = %q", got)
	}

	doc := Read(fixture)
	if doc.V != 1 || doc.Error != "" || doc.Truncated || len(doc.Routes) != 3 {
		t.Fatalf("Read(fixture) = %+v", doc)
	}
	if doc.Routes[0] != (wire.HostRoute{Iface: "eth0", Dst: "192.168.2.0/24"}) {
		t.Errorf("first route = %+v", doc.Routes[0])
	}

	if doc := Read(filepath.Join(dir, "missing")); doc.Error != wire.HostNetUnreadable || len(doc.Routes) != 0 || doc.Routes == nil {
		t.Errorf("Read(missing) = %+v, want unreadable with an empty, non-nil list", doc)
	}
	if doc := Read(dir); doc.Error != wire.HostNetUnreadable || len(doc.Routes) != 0 {
		t.Errorf("Read(directory) = %+v", doc)
	}
}

func TestReadAnswersTooLargeForAHugeLineAndAHugeFile(t *testing.T) {
	dir := t.TempDir()
	header := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\n"
	long := filepath.Join(dir, "long")
	if err := os.WriteFile(long, []byte(header+"eth0 "+strings.Repeat("0", 600)+"\n"), 0o600); err != nil { //nolint:gosec // a temp path
		t.Fatal(err)
	}
	huge := filepath.Join(dir, "huge")
	if err := os.WriteFile(huge, []byte(header+strings.Repeat("eth0\t0000080A\t00000000\t0001\t0\t0\t0\t00FFFFFF\n", 300<<10/40)), 0o600); err != nil { //nolint:gosec // a temp path
		t.Fatal(err)
	}
	for name, p := range map[string]string{"a 600 byte line": long, "a 300 KiB file": huge} {
		doc := Read(p)
		if doc.Error != wire.HostNetTooLarge || len(doc.Routes) != 0 {
			t.Errorf("%s: %+v", name, doc)
		}
	}
	if hostroute.MaxFile != 256<<10 {
		t.Fatalf("MaxFile = %d", hostroute.MaxFile)
	}
}

// The handler reads one file and runs nothing (T-10-06). This is the same grep
// the plan's acceptance criterion uses, kept as a test so it cannot be dropped.
func TestHostnetNeverTouchesTheDockerSocketOrRunsACommand(t *testing.T) {
	src, err := os.ReadFile("hostnet.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") {
			continue
		}
		if strings.Contains(code, "docker.sock") || strings.Contains(code, "exec.Command") || strings.Contains(code, "os/exec") {
			t.Errorf("hostnet.go must not touch the Docker socket or run a command: %q", line)
		}
	}
}
