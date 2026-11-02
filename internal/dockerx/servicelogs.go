// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// ErrTaskNotFound is returned when a task the caller named is not in the swarm.
var ErrTaskNotFound = errors.New("dockerx: no such task")

// maxLogFrame bounds one frame, and one line of a TTY stream, of a service log. The
// engine splits what a task writes into messages of a few KiB, so a bigger one is not an
// engine's frame, and nothing is allocated for the size it claims.
const maxLogFrame = 1 << 20

// Detail keys a swarm manager puts in front of the message of every service log record
// when it is asked for details (RESEARCH E3).
const (
	detailTaskID    = "com.docker.swarm.task.id"
	detailNodeID    = "com.docker.swarm.node.id"
	detailServiceID = "com.docker.swarm.service.id"
)

// ServiceLogOptions say which part of a service's or a task's log to read. The engine
// ignores until on both routes (RESEARCH E2), so there is none: the caller cuts by time
// after reading.
type ServiceLogOptions struct {
	// Follow keeps the stream open for lines written from now on, and for the lines of
	// tasks that are replaced while it runs (RESEARCH E4).
	Follow bool
	// Since is a time the engine reads (RFC 3339 or Unix seconds); empty means no bound.
	Since string
	// Tail is how many of the last lines to read, as a number, or "all" or empty for
	// every line. For a service it is applied per task (RESEARCH E1).
	Tail string
}

// ServiceLogRecord is one record of a service log, split into the parts the manager
// writes: the time, the ids of the task, node and service in the details and the message.
type ServiceLogRecord struct {
	// T is the engine's timestamp; Raw is the same as the engine wrote it, nine digits and
	// all, which sorts as time does.
	T   time.Time
	Raw string
	// TaskID, NodeID and ServiceID come from the details; empty when the record had none.
	TaskID, NodeID, ServiceID string
	// Message is the line without its line ending.
	Message string
	// Stream is "stdout" or "stderr"; ParseServiceLogRecord leaves it empty, because the
	// frame carries it, and ServiceLogFrames' caller sets it.
	Stream string
}

// ServiceLogs opens the log of a service, every task of it together, with the engine's
// timestamp and the ids of the task and node in front of every message. It reports
// whether the service's tasks have a TTY: the stream of a TTY service is raw bytes and
// any other is multiplexed (stdcopy). The caller reads the stream with ServiceLogFrames
// and closes it; closing ends a follow.
//
// A swarm manager answers task by task, not in time order, and applies tail per task
// (RESEARCH E1), so the caller merges. The stream also holds the logs of shut-down tasks
// still in the task history (E5). A service that does not exist is ErrServiceNotFound;
// a service whose log driver keeps nothing answers an empty stream, never an error (E6).
func (c *Client) ServiceLogs(ctx context.Context, serviceID string, o ServiceLogOptions) (rc io.ReadCloser, tty bool, err error) {
	cli := c.cur()
	res, err := cli.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, false, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), ErrServiceNotFound)
		}
		return nil, false, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), err)
	}
	tty = serviceHasTTY(res.Service.Spec.TaskTemplate.ContainerSpec)
	out, err := cli.ServiceLogs(ctx, res.Service.ID, client.ServiceLogsOptions{
		ShowStdout: true, ShowStderr: true, Timestamps: true, Details: true,
		Follow: o.Follow, Since: o.Since, Tail: o.Tail,
	})
	if err != nil {
		return nil, tty, fmt.Errorf("read logs of a service on %s: %w", c.t.Describe(), err)
	}
	return out, tty, nil
}

// TaskLogs opens the log of one task, in the same format as ServiceLogs. The task is
// looked up first, for its service's TTY flag: a task that is not in the swarm is
// ErrTaskNotFound. The engine's client sends tail on a task read whatever it is given, so
// an empty one is sent as "all".
func (c *Client) TaskLogs(ctx context.Context, taskID string, o ServiceLogOptions) (rc io.ReadCloser, tty bool, err error) {
	rows, err := c.Tasks(ctx, TaskFilter{IDs: []string{taskID}})
	if err != nil {
		return nil, false, err
	}
	var task *TaskRow
	for i := range rows {
		if rows[i].ID == taskID {
			task = &rows[i]
			break
		}
	}
	if task == nil {
		return nil, false, fmt.Errorf("find task on %s: %w", c.t.Describe(), ErrTaskNotFound)
	}
	cli := c.cur()
	svc, err := cli.ServiceInspect(ctx, task.ServiceID, client.ServiceInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, false, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), ErrServiceNotFound)
		}
		return nil, false, fmt.Errorf("inspect service on %s: %w", c.t.Describe(), err)
	}
	tty = serviceHasTTY(svc.Service.Spec.TaskTemplate.ContainerSpec)
	tail := o.Tail
	if tail == "" {
		tail = "all"
	}
	out, err := cli.TaskLogs(ctx, task.ID, client.TaskLogsOptions{
		ShowStdout: true, ShowStderr: true, Timestamps: true, Details: true,
		Follow: o.Follow, Since: o.Since, Tail: tail,
	})
	if err != nil {
		return nil, tty, fmt.Errorf("read logs of a task on %s: %w", c.t.Describe(), err)
	}
	return out, tty, nil
}

func serviceHasTTY(cs *swarm.ContainerSpec) bool { return cs != nil && cs.TTY }

// ParseServiceLogRecord splits one record of a service log read with timestamps and
// details: the engine's time, then the details token (key=value pairs, comma separated,
// values URL-escaped), then the message, each cut at the first space. The details token
// is recognised by the swarm's own keys, so a record without one (details not asked for)
// keeps its whole text as the message; keys it does not know are ignored. A time that
// does not parse is an error.
func ParseServiceLogRecord(s string) (ServiceLogRecord, error) {
	ts, rest, _ := strings.Cut(s, " ")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ServiceLogRecord{}, fmt.Errorf("dockerx: a service log record does not begin with a time: %w", err)
	}
	rec := ServiceLogRecord{T: t.UTC(), Raw: ts, Message: rest}
	tok, msg, _ := strings.Cut(rest, " ")
	if ids, ok := parseDetails(tok); ok {
		rec.TaskID, rec.NodeID, rec.ServiceID = ids[detailTaskID], ids[detailNodeID], ids[detailServiceID]
		rec.Message = msg
	}
	return rec, nil
}

// parseDetails reads a details token. It is one only when it is made of key=value pairs
// and names at least one of the swarm's three ids.
func parseDetails(tok string) (map[string]string, bool) {
	if tok == "" || !strings.Contains(tok, "com.docker.swarm.") {
		return nil, false
	}
	out := map[string]string{}
	for _, pair := range strings.Split(tok, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, false
		}
		if u, err := url.QueryUnescape(v); err == nil {
			v = u
		}
		out[k] = v
	}
	_, hasTask := out[detailTaskID]
	_, hasNode := out[detailNodeID]
	_, hasService := out[detailServiceID]
	return out, hasTask || hasNode || hasService
}

// ServiceLogFrames reads a service log stream and calls emit once per record: once per
// stdcopy frame for a multiplexed stream, because the manager writes one whole message
// per frame, and once per line for a TTY stream. The record is the text without its line
// ending; stream is "stdout" or "stderr". It never goes through a line splitter, which
// would cut a message that holds a newline. It returns nil at a clean end of the stream,
// and an error for a frame that is cut short or claims more than maxLogFrame bytes.
func ServiceLogFrames(r io.Reader, tty bool, emit func(stream string, rec []byte)) error {
	if tty {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), maxLogFrame)
		for sc.Scan() {
			emit("stdout", bytes.TrimSuffix(sc.Bytes(), []byte("\r")))
		}
		return sc.Err()
	}
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("dockerx: read a log frame header: %w", err)
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		if size > maxLogFrame {
			return fmt.Errorf("dockerx: a log frame of %d bytes is larger than the %d allowed", size, maxLogFrame)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return fmt.Errorf("dockerx: read a log frame: %w", err)
		}
		stream := "stdout"
		if hdr[0] == 2 || hdr[0] == 3 {
			stream = "stderr"
		}
		payload = bytes.TrimSuffix(payload, []byte("\n"))
		emit(stream, payload)
	}
}
