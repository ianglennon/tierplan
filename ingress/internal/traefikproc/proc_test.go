// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package traefikproc

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// helperEnv marks the re-executed test binary that stands in for Traefik.
const helperEnv = "TRAEFIKPROC_HELPER"

// helperReport is one JSON line the stand-in writes: first its environment and arguments, then, once
// SIGTERM arrives, the word "terminated".
type helperReport struct {
	Env    []string `json:"env,omitempty"`
	Args   []string `json:"args,omitempty"`
	Signal string   `json:"signal,omitempty"`
}

// TestHelperProcess is the stand-in for Traefik. Without the marker it does nothing, so a normal run passes
// it. It registers for SIGTERM before it reports anything, so a SIGTERM the test sends after reading the
// first line is never lost.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(helperReport{Env: os.Environ(), Args: os.Args})
	<-term
	_ = enc.Encode(helperReport{Signal: "terminated"})
	os.Exit(0)
}

// helperArgs runs the helper case of this test binary. "--" ends the test binary's own flags, so what
// follows (Traefik's real arguments) reaches the helper untouched.
func helperArgs(rest ...string) []string {
	return append([]string{"-test.run=^TestHelperProcess$", "--"}, rest...)
}

// reports returns a file to use as the child's stdout and a channel of the lines the child writes to it.
// The parent's copy of the write end must be closed once the child holds its own, or the reader never ends.
func reports(t *testing.T) (*os.File, <-chan helperReport) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	out := make(chan helperReport, 8)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			var h helperReport
			if json.Unmarshal(sc.Bytes(), &h) == nil {
				out <- h
			}
		}
	}()
	return w, out
}

// countingStart is the real starter that counts how many children it started.
func countingStart(n *atomic.Int32) starter {
	return func(binary string, args, env []string, stdout, stderr io.Writer) (process, error) {
		n.Add(1)
		return realStart(binary, args, env, stdout, stderr)
	}
}

func TestEnvIsOnlyPathAndArgsAreTheStaticConfig(t *testing.T) {
	if got, want := Env(), []string{"PATH=/usr/local/bin:/usr/bin:/bin"}; !slices.Equal(got, want) {
		t.Fatalf("Env() = %q, want %q", got, want)
	}
	if got, want := DefaultArgs(), []string{"--configFile=/var/lib/tierplan-ingress/static/traefik.yml"}; !slices.Equal(got, want) {
		t.Fatalf("DefaultArgs() = %q, want %q", got, want)
	}
	if ingressbundle.NodeRoot != "/var/lib/tierplan-ingress" {
		t.Fatalf("NodeRoot = %q: the argument above no longer names the static file", ingressbundle.NodeRoot)
	}
	p := New(Options{})
	if !slices.Equal(p.o.Env, Env()) || !slices.Equal(p.o.Args, DefaultArgs()) || p.o.Binary != Binary {
		t.Fatalf("New did not default the binary, arguments and environment: %+v", p.o)
	}
}

// The child starts with exactly the minimal environment (the helper's own marker aside) and Traefik's
// arguments, and reports both from its own side.
func TestStartMinimalEnvAndArgs(t *testing.T) {
	out, lines := reports(t)
	var starts atomic.Int32
	p := New(Options{
		Binary: os.Args[0],
		Args:   helperArgs(DefaultArgs()...),
		Env:    append(Env(), helperEnv+"=1"),
		Stdout: out,
		start:  countingStart(&starts),
	})
	t.Cleanup(func() { _ = p.Stop() })
	if err := p.Start(testsupport.Context(t)); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	got := testsupport.Await(t, "the child's report", lines)
	if want := []string{"PATH=/usr/local/bin:/usr/bin:/bin", helperEnv + "=1"}; !slices.Equal(got.Env, want) {
		t.Fatalf("child environment = %q, want exactly %q", got.Env, want)
	}
	if last := got.Args[len(got.Args)-1]; last != "--configFile=/var/lib/tierplan-ingress/static/traefik.yml" {
		t.Fatalf("child's last argument = %q, want the static config", last)
	}
	if !p.Running() {
		t.Fatal("Running() is false after Start")
	}
	if p.PID() <= 0 || p.PID() == os.Getpid() {
		t.Fatalf("PID() = %d, want the child's pid", p.PID())
	}
}

// Stop forwards SIGTERM, the child exits on it, Stop returns, and nothing restarts the child.
func TestStopForwardsSIGTERM(t *testing.T) {
	out, lines := reports(t)
	var starts atomic.Int32
	p := New(Options{
		Binary: os.Args[0],
		Args:   helperArgs(DefaultArgs()...),
		Env:    append(Env(), helperEnv+"=1"),
		Stdout: out,
		start:  countingStart(&starts),
	})
	t.Cleanup(func() { _ = p.Stop() })
	if err := p.Start(testsupport.Context(t)); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the child to be ready", lines)

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := testsupport.Await(t, "the child's SIGTERM report", lines); got.Signal != "terminated" {
		t.Fatalf("child report after Stop = %+v, want it to have seen SIGTERM", got)
	}
	if p.Running() {
		t.Fatal("Running() is true after Stop")
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("the child was started %d times, want once: a stop must not restart it", n)
	}
}
