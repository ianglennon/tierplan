// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The service log read (plan 06-29, OBS-02, D-37): the engine's ServiceLogs stream with
// timestamps and details, parsed one record per stdcopy frame.

var svcLogT0 = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

func svcLine(task, node string, offsetMs int, text string) dockertest.FakeServiceLogLine {
	return dockertest.FakeServiceLogLine{
		TaskID: task, NodeID: node, Time: svcLogT0.Add(time.Duration(offsetMs) * time.Millisecond), Text: text,
	}
}

func serviceLogFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	tty := swarm.ServiceSpec{}
	tty.Name = "shell"
	tty.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "alpine", TTY: true}
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1",
		NodeRows: []dockertest.FakeNode{{ID: "node-a", Hostname: "alpha", Role: "manager"}, {ID: "node-b", Hostname: "beta"}},
		ServiceRows: []dockertest.FakeService{
			{ID: "web-id", Name: "web", Mode: "replicated"},
			{ID: "shell-id", Name: "shell", Mode: "replicated", Spec: &tty},
		},
		Tasks: []swarm.Task{
			fakeTask("t1", "web-id", "node-a", 1, "running", "running", time.Minute),
			fakeTask("t2", "web-id", "node-b", 2, "running", "running", time.Minute),
		},
		ServiceLogs: map[string][]dockertest.FakeServiceLogLine{
			"web-id": {
				svcLine("t1", "node-a", 10, "t1 first"),
				svcLine("t1", "node-a", 30, "t1 second"),
				svcLine("t1", "node-a", 50, "t1 third"),
				svcLine("t2", "node-b", 20, "t2 first"),
				svcLine("t2", "node-b", 40, "t2 second"),
			},
			"shell-id": {svcLine("t3", "node-a", 5, "a tty line")},
		},
	})
	return newClient(t, fake), fake
}

// readRecords reads a service log stream to its end and returns the records.
func readRecords(t *testing.T, rc io.ReadCloser, tty bool) []dockerx.ServiceLogRecord {
	t.Helper()
	defer func() { _ = rc.Close() }()
	var out []dockerx.ServiceLogRecord
	err := dockerx.ServiceLogFrames(rc, tty, func(stream string, rec []byte) {
		r, err := dockerx.ParseServiceLogRecord(string(rec))
		if err != nil {
			t.Errorf("a frame did not parse: %v (%q)", err, rec)
			return
		}
		r.Stream = stream
		out = append(out, r)
	})
	if err != nil {
		t.Fatalf("ServiceLogFrames: %v", err)
	}
	return out
}

func TestParseServiceLogRecord(t *testing.T) {
	const details = "com.docker.swarm.node.id=n1,com.docker.swarm.service.id=s1,com.docker.swarm.task.id=t1"
	at := time.Date(2026, 10, 14, 10, 0, 0, 123456789, time.UTC)
	for _, tc := range []struct {
		name, in string
		want     dockerx.ServiceLogRecord
		wantErr  bool
	}{
		{"timestamp, details and message", "2026-10-14T10:00:00.123456789Z " + details + " hello world",
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z", NodeID: "n1", ServiceID: "s1", TaskID: "t1", Message: "hello world"}, false},
		{"an empty message", "2026-10-14T10:00:00.123456789Z " + details,
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z", NodeID: "n1", ServiceID: "s1", TaskID: "t1"}, false},
		{"a message that starts with spaces keeps them", "2026-10-14T10:00:00.123456789Z " + details + "   indented",
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z", NodeID: "n1", ServiceID: "s1", TaskID: "t1", Message: "  indented"}, false},
		{"no details token keeps the message whole", "2026-10-14T10:00:00.123456789Z hello world",
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z", Message: "hello world"}, false},
		{"a message that looks like a pair is not a details token", "2026-10-14T10:00:00.123456789Z a=b c",
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z", Message: "a=b c"}, false},
		{"extra details are ignored", "2026-10-14T10:00:00.123456789Z " + details + ",com.example.env=prod msg",
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z", NodeID: "n1", ServiceID: "s1", TaskID: "t1", Message: "msg"}, false},
		{"escaped values are unescaped", "2026-10-14T10:00:00.123456789Z com.docker.swarm.task.id=t%2F1 msg",
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z", TaskID: "t/1", Message: "msg"}, false},
		{"a bad escape is kept as written", "2026-10-14T10:00:00.123456789Z com.docker.swarm.task.id=t%zz msg",
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z", TaskID: "t%zz", Message: "msg"}, false},
		{"a time and nothing else", "2026-10-14T10:00:00.123456789Z",
			dockerx.ServiceLogRecord{T: at, Raw: "2026-10-14T10:00:00.123456789Z"}, false},
		{"an unparsable time", "yesterday " + details + " x", dockerx.ServiceLogRecord{}, true},
		{"an empty record", "", dockerx.ServiceLogRecord{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dockerx.ParseServiceLogRecord(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !got.T.Equal(tc.want.T) || got.Raw != tc.want.Raw || got.NodeID != tc.want.NodeID || got.ServiceID != tc.want.ServiceID ||
				got.TaskID != tc.want.TaskID || got.Message != tc.want.Message {
				t.Errorf("record = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestServiceLogsComeGroupedPerTaskWithTheTaskAndNodeInTheirDetails(t *testing.T) {
	c, _ := serviceLogFake(t)
	rc, tty, err := c.ServiceLogs(context.Background(), "web", dockerx.ServiceLogOptions{Tail: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if tty {
		t.Fatal("a service without a TTY was reported as having one")
	}
	recs := readRecords(t, rc, tty)
	var got []string
	for _, r := range recs {
		got = append(got, r.TaskID+"/"+r.NodeID+"/"+r.ServiceID+": "+r.Message)
	}
	want := []string{
		"t1/node-a/web-id: t1 first", "t1/node-a/web-id: t1 second", "t1/node-a/web-id: t1 third",
		"t2/node-b/web-id: t2 first", "t2/node-b/web-id: t2 second",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("records = %v\nwant     %v (the engine answers task by task, not in time order: RESEARCH E1)", got, want)
	}
	if recs[0].Stream != "stdout" || !recs[0].T.Equal(svcLogT0.Add(10*time.Millisecond)) {
		t.Errorf("first record = %+v, want stdout at +10ms", recs[0])
	}
}

func TestServiceLogsTailIsPerTaskAndSinceCutsByTime(t *testing.T) {
	c, fake := serviceLogFake(t)
	rc, _, err := c.ServiceLogs(context.Background(), "web-id", dockerx.ServiceLogOptions{Tail: "2"})
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, r := range readRecords(t, rc, false) {
		msgs = append(msgs, r.Message)
	}
	if want := []string{"t1 second", "t1 third", "t2 first", "t2 second"}; !slices.Equal(msgs, want) {
		t.Errorf("tail=2 gave %v, want 2 per task: %v", msgs, want)
	}

	since := svcLogT0.Add(35 * time.Millisecond).Format(time.RFC3339Nano)
	rc, _, err = c.ServiceLogs(context.Background(), "web-id", dockerx.ServiceLogOptions{Tail: "all", Since: since})
	if err != nil {
		t.Fatal(err)
	}
	msgs = nil
	for _, r := range readRecords(t, rc, false) {
		msgs = append(msgs, r.Message)
	}
	if want := []string{"t1 third", "t2 second"}; !slices.Equal(msgs, want) {
		t.Errorf("since gave %v, want %v", msgs, want)
	}

	// Every request says stdout, stderr, timestamps and details, sends the tail it was given
	// and never an until, which the engine ignores for a service anyway (E2).
	qs := fake.ServiceLogQueries()
	if len(qs) != 2 {
		t.Fatalf("queries = %v, want 2", qs)
	}
	for _, q := range qs {
		for _, k := range []string{"stdout", "stderr", "timestamps", "details"} {
			if q.Get(k) != "1" {
				t.Errorf("query %v: %s is not 1", q, k)
			}
		}
		if q.Has("until") {
			t.Errorf("query %v sends until", q)
		}
	}
	if qs[0].Get("tail") != "2" || qs[1].Get("tail") != "" && qs[1].Get("tail") != "all" {
		t.Errorf("tails = %q, %q, want 2 and the default", qs[0].Get("tail"), qs[1].Get("tail"))
	}
}

func TestAFollowedServiceLogShowsLinesPushedAfterItOpened(t *testing.T) {
	c, fake := serviceLogFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, _, err := c.ServiceLogs(ctx, "web", dockerx.ServiceLogOptions{Tail: "0", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got := make(chan dockerx.ServiceLogRecord, 4)
	go func() {
		_ = dockerx.ServiceLogFrames(rc, false, func(stream string, rec []byte) {
			r, err := dockerx.ParseServiceLogRecord(string(rec))
			if err == nil {
				r.Stream = stream
				got <- r
			}
		})
	}()
	fake.PushServiceLog("web", svcLine("t2", "node-b", 100, "live line"))
	select {
	case r := <-got:
		if r.Message != "live line" || r.TaskID != "t2" {
			t.Fatalf("record = %+v, want the pushed line of t2", r)
		}
	case <-time.After(testsupport.Deadline):
		t.Fatal("a followed service log did not show a pushed line")
	}
}

func TestServiceLogsOfATTYServiceAreRawAndSplitPerLine(t *testing.T) {
	c, _ := serviceLogFake(t)
	rc, tty, err := c.ServiceLogs(context.Background(), "shell", dockerx.ServiceLogOptions{Tail: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if !tty {
		t.Fatal("a service with a TTY was reported as having none")
	}
	recs := readRecords(t, rc, tty)
	if len(recs) != 1 || recs[0].Message != "a tty line" || recs[0].TaskID != "t3" {
		t.Fatalf("records = %+v, want the one tty line", recs)
	}
}

func TestServiceLogsOfAnUnknownServiceIsNotFound(t *testing.T) {
	c, _ := serviceLogFake(t)
	_, _, err := c.ServiceLogs(context.Background(), "nope", dockerx.ServiceLogOptions{Tail: "all"})
	if !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Fatalf("err = %v, want ErrServiceNotFound", err)
	}
}

func TestTaskLogsAreThoseOfOneTaskAndAlwaysSendATail(t *testing.T) {
	c, fake := serviceLogFake(t)
	rc, tty, err := c.TaskLogs(context.Background(), "t2", dockerx.ServiceLogOptions{Tail: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if tty {
		t.Fatal("a task of a service without a TTY was reported as having one")
	}
	var msgs []string
	for _, r := range readRecords(t, rc, tty) {
		msgs = append(msgs, r.Message)
	}
	if want := []string{"t2 first", "t2 second"}; !slices.Equal(msgs, want) {
		t.Errorf("task logs = %v, want only t2's: %v", msgs, want)
	}
	// The client sends tail on a task read whatever it is given; an empty one would be
	// refused by an engine, so the wrapper always names one.
	rc, _, err = c.TaskLogs(context.Background(), "t1", dockerx.ServiceLogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = readRecords(t, rc, false)
	qs := fake.ServiceLogQueries()
	if len(qs) != 2 || qs[0].Get("tail") != "all" || qs[1].Get("tail") != "all" {
		t.Errorf("queries = %v, want tail=all on both", qs)
	}
	if qs[0].Get("details") != "1" || qs[0].Get("timestamps") != "1" {
		t.Errorf("query %v does not ask for details and timestamps", qs[0])
	}
}

func TestTaskLogsOfAnUnknownTaskIsAnError(t *testing.T) {
	c, _ := serviceLogFake(t)
	if _, _, err := c.TaskLogs(context.Background(), "nope", dockerx.ServiceLogOptions{Tail: "all"}); err == nil {
		t.Fatal("the logs of a task that does not exist came back without an error")
	}
}

func frame(kind byte, text string) []byte {
	h := make([]byte, 8, 8+len(text))
	h[0] = kind
	binary.BigEndian.PutUint32(h[4:], uint32(len(text))) //nolint:gosec // a test line is far below 4 GiB
	return append(h, text...)
}

func TestServiceLogFramesEmitsOneRecordPerFrameWithItsStream(t *testing.T) {
	in := append(frame(1, "a b c\n"), frame(2, "err line\n")...)
	in = append(in, frame(1, "no newline")...)
	var got []string
	err := dockerx.ServiceLogFrames(bytes.NewReader(in), false, func(stream string, rec []byte) {
		got = append(got, stream+":"+string(rec))
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"stdout:a b c", "stderr:err line", "stdout:no newline"}; !slices.Equal(got, want) {
		t.Errorf("frames = %v, want %v", got, want)
	}
}

func TestServiceLogFramesRefusesATruncatedOrOversizedFrame(t *testing.T) {
	short := frame(1, "hello world\n")[:12]
	if err := dockerx.ServiceLogFrames(bytes.NewReader(short), false, func(string, []byte) {}); err == nil {
		t.Error("a frame cut short did not fail")
	}
	huge := make([]byte, 8)
	huge[0] = 1
	binary.BigEndian.PutUint32(huge[4:], 1<<30)
	if err := dockerx.ServiceLogFrames(bytes.NewReader(huge), false, func(string, []byte) {}); err == nil {
		t.Error("a frame of a gibibyte did not fail")
	}
	if err := dockerx.ServiceLogFrames(strings.NewReader(""), false, func(string, []byte) {}); err != nil {
		t.Errorf("an empty stream = %v, want a clean end", err)
	}
}

func TestServiceLogFramesSplitsATTYStreamPerLine(t *testing.T) {
	var got []string
	err := dockerx.ServiceLogFrames(strings.NewReader("one\r\ntwo\nthree"), true, func(stream string, rec []byte) {
		got = append(got, stream+":"+string(rec))
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"stdout:one", "stdout:two", "stdout:three"}; !slices.Equal(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
}
