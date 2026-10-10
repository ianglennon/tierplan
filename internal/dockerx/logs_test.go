// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func logsFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{
			{ID: "web-id", Names: []string{"/web"}, Image: "nginx", State: "running", Status: "Up 3 hours"},
			{ID: "tty-id", Names: []string{"/shell"}, Image: "alpine", State: "running", Status: "Up 1 hour",
				Inspect: dockertest.FakeInspect{Tty: true}},
		},
	})
	return newClient(t, fake), fake
}

func TestContainerLogsOfAContainerWithoutATTYIsMultiplexedAndStampedWithTheEnginesTime(t *testing.T) {
	c, fake := logsFake(t)
	at := time.Date(2026, 10, 9, 12, 58, 0, 123456789, time.UTC)
	fake.AppendLogs("web", dockertest.FakeLogLine{Text: "a", Time: at}, dockertest.FakeLogLine{Stream: "stderr", Text: "b"})

	rc, tty, err := c.ContainerLogs(context.Background(), "web", dockerx.LogOptions{Tail: "all"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if tty {
		t.Fatal("a container without a TTY was reported as having one")
	}
	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errOut, rc); err != nil {
		t.Fatalf("the stream is not stdcopy frames: %v", err)
	}
	if got := out.String(); got != "2026-10-09T12:58:00.123456789Z a\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := errOut.String(); !strings.HasSuffix(got, "Z b\n") {
		t.Errorf("stderr = %q, want the line b with its time", got)
	}
}

func TestContainerLogsOfATTYContainerAreRaw(t *testing.T) {
	c, fake := logsFake(t)
	fake.AppendLogs("shell", dockertest.FakeLogLine{Text: "prompt"})
	rc, tty, err := c.ContainerLogs(context.Background(), "shell", dockerx.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if !tty {
		t.Fatal("a container with a TTY was reported as having none")
	}
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "2026-01-01T00:00:00.000000000Z prompt\r\n" {
		t.Errorf("raw stream = %q", got)
	}
}

func TestContainerLogsOfAnUnknownContainerIsNotFound(t *testing.T) {
	c, _ := logsFake(t)
	_, _, err := c.ContainerLogs(context.Background(), "nope", dockerx.LogOptions{})
	if !dockerx.IsNotFound(err) {
		t.Fatalf("err = %v, want a not found", err)
	}
}

func TestContainerLogsPassTailAndSinceToTheEngine(t *testing.T) {
	c, fake := logsFake(t)
	for _, text := range []string{"one", "two", "three"} {
		fake.AppendLogs("web", dockertest.FakeLogLine{Text: text})
	}
	rc, _, err := c.ContainerLogs(context.Background(), "web", dockerx.LogOptions{Tail: "2", Since: "2026-01-01T00:00:00.001Z"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, io.Discard, rc); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 2 || !strings.Contains(out.String(), "two") || !strings.Contains(out.String(), "three") {
		t.Errorf("lines = %q, want two and three", out.String())
	}
	q := fake.LogQueries()
	if len(q) != 1 || q[0].Get("tail") != "2" || q[0].Get("timestamps") != "1" || q[0].Get("stdout") != "1" || q[0].Get("stderr") != "1" || q[0].Get("since") == "" {
		t.Errorf("the engine was asked %v", q)
	}
}

func TestContainerLogsFollowDeliversNewLinesAndCloseEndsTheFollow(t *testing.T) {
	c, fake := logsFake(t)
	fake.AppendLogs("web", dockertest.FakeLogLine{Text: "before"})
	rc, _, err := c.ContainerLogs(context.Background(), "web", dockerx.LogOptions{Follow: true, Tail: "all"})
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, io.Discard, rc)
		_ = pw.CloseWithError(err)
	}()
	sc := bufio.NewScanner(pr)
	next := func() string {
		t.Helper()
		done := make(chan string, 1)
		go func() {
			if sc.Scan() {
				done <- sc.Text()
			} else {
				done <- "EOF"
			}
		}()
		select {
		case s := <-done:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("no line arrived")
			return ""
		}
	}
	if got := next(); !strings.HasSuffix(got, " before") {
		t.Fatalf("first line = %q", got)
	}
	fake.AppendLogs("web", dockertest.FakeLogLine{Text: "after"})
	if got := next(); !strings.HasSuffix(got, " after") {
		t.Fatalf("followed line = %q", got)
	}
	if n := fake.LogFollows(); n != 1 {
		t.Fatalf("%d follows open, want 1", n)
	}
	_ = rc.Close()
	deadline := time.Now().Add(5 * time.Second)
	for fake.LogFollows() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the engine's follow stayed open after Close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
