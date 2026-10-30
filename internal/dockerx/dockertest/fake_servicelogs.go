// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

var _ = registerFakeHandler((*Fake).serveServiceLogs)

var (
	serviceLogsRoute = regexp.MustCompile(`^/services/([^/]+)/logs$`)
	taskLogsRoute    = regexp.MustCompile(`^/tasks/([^/]+)/logs$`)
)

// FakeServiceLogLine is one line a task of a service wrote.
type FakeServiceLogLine struct {
	// TaskID and NodeID are the task that wrote the line and the node it ran on; they are
	// what a manager puts in the details in front of the message.
	TaskID, NodeID string
	// Stream is "stdout" (the default) or "stderr".
	Stream string
	// Time is when the line was written. Zero means a millisecond after the line pushed
	// before it, from 2026-01-01T00:00:00Z.
	Time time.Time
	// Text is the line without its line ending.
	Text string
}

// fakeServiceLogs is the Fake's service log state. The zero value is ready to use.
type fakeServiceLogs struct {
	mu sync.Mutex
	// pushed are the lines PushServiceLog added, by service ID, after the configured ones.
	pushed  map[string][]FakeServiceLogLine
	wake    chan struct{}
	queries []url.Values
	follows int
}

// PushServiceLog adds a line to the log of a service, named by ID or name, and wakes every
// follow of it. A line without a time follows the one pushed before it by a millisecond.
func (f *Fake) PushServiceLog(service string, l FakeServiceLogLine) {
	id := service
	if fs, ok := f.config().resolveService(service); ok {
		id = fs.ID
	}
	s := &f.svcLogs
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pushed == nil {
		s.pushed = map[string][]FakeServiceLogLine{}
	}
	if s.wake == nil {
		s.wake = make(chan struct{})
	}
	if l.Time.IsZero() {
		l.Time = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if n := len(s.pushed[id]); n > 0 {
			l.Time = s.pushed[id][n-1].Time.Add(time.Millisecond)
		}
	}
	s.pushed[id] = append(s.pushed[id], l)
	close(s.wake)
	s.wake = make(chan struct{})
}

// ServiceLogQueries are the query strings of every service and task log request served,
// oldest first.
func (f *Fake) ServiceLogQueries() []url.Values {
	f.svcLogs.mu.Lock()
	defer f.svcLogs.mu.Unlock()
	return append([]url.Values(nil), f.svcLogs.queries...)
}

// ServiceLogFollows is how many follow requests for the log of a service or a task are
// open now.
func (f *Fake) ServiceLogFollows() int {
	f.svcLogs.mu.Lock()
	defer f.svcLogs.mu.Unlock()
	return f.svcLogs.follows
}

// serveServiceLogs answers GET /services/{id}/logs and GET /tasks/{id}/logs the way a
// swarm manager does: the lines of every task of the service (or of the one task), grouped
// task by task in the order the tasks first appear, each task cut to its last tail lines,
// not in time order (RESEARCH E1); until is ignored (E2); each record is the timestamp when
// timestamps=1, then the details when details=1, then the text (E3). With follow=1 the
// lines pushed after follow, until the client leaves. A service with a TTY in its spec is
// answered raw, any other multiplexed as stdcopy frames, one per line. A service or task
// the swarm does not know is the 404 a manager gives, a node that is not a manager the
// refusal it gives.
func (f *Fake) serveServiceLogs(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	var serviceRef, taskRef string
	if m := serviceLogsRoute.FindStringSubmatch(p); m != nil {
		serviceRef = m[1]
	} else if m := taskLogsRoute.FindStringSubmatch(p); m != nil {
		taskRef = m[1]
	} else {
		return false
	}
	if clusterRefused(w, cfg) {
		return true
	}
	serviceID := ""
	if taskRef != "" {
		for _, t := range cfg.Tasks {
			if t.ID == taskRef {
				serviceID = t.ServiceID
			}
		}
		if serviceID == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "task " + taskRef + " not found"})
			return true
		}
	} else {
		fs, ok := cfg.resolveService(serviceRef)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "service " + serviceRef + " not found"})
			return true
		}
		serviceID = fs.ID
	}
	tty := false
	for _, s := range cfg.serviceList(false) {
		if s.ID == serviceID && s.Spec.TaskTemplate.ContainerSpec != nil {
			tty = s.Spec.TaskTemplate.ContainerSpec.TTY
		}
	}

	q := r.URL.Query()
	l := &f.svcLogs
	l.mu.Lock()
	l.queries = append(l.queries, q)
	all := append([]FakeServiceLogLine(nil), cfg.ServiceLogs[serviceID]...)
	all = append(all, l.pushed[serviceID]...)
	seenPushed := len(l.pushed[serviceID])
	l.mu.Unlock()

	want := func(line FakeServiceLogLine) bool {
		if taskRef != "" && line.TaskID != taskRef {
			return false
		}
		if line.Stream == "stderr" {
			return q.Get("stderr") == "1"
		}
		return q.Get("stdout") == "1"
	}
	since, hasSince := parseLogTime(q.Get("since"))
	timestamps, details, follow := q.Get("timestamps") == "1", q.Get("details") == "1", q.Get("follow") == "1"

	// Group by task in first-seen order, take the tail of each, then cut by since.
	var order []string
	byTask := map[string][]FakeServiceLogLine{}
	for _, line := range all {
		if !want(line) {
			continue
		}
		if _, seen := byTask[line.TaskID]; !seen {
			order = append(order, line.TaskID)
		}
		byTask[line.TaskID] = append(byTask[line.TaskID], line)
	}
	var first []FakeServiceLogLine
	for _, id := range order {
		lines := byTask[id]
		if t := q.Get("tail"); t != "" && t != "all" {
			if n, err := strconv.Atoi(t); err == nil && n >= 0 && n < len(lines) {
				lines = lines[len(lines)-n:]
			}
		}
		for _, line := range lines {
			if hasSince && line.Time.Before(since) {
				continue
			}
			first = append(first, line)
		}
	}

	if tty {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	} else {
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	}
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	flush()

	write := func(line FakeServiceLogLine) bool {
		text := ""
		if timestamps {
			text = line.Time.UTC().Format(logTimeLayout) + " "
		}
		if details {
			text += fmt.Sprintf("com.docker.swarm.node.id=%s,com.docker.swarm.service.id=%s,com.docker.swarm.task.id=%s ",
				url.QueryEscape(line.NodeID), url.QueryEscape(serviceID), url.QueryEscape(line.TaskID))
		}
		text += line.Text
		if tty {
			_, err := w.Write([]byte(text + "\r\n"))
			return err == nil
		}
		text += "\n"
		kind := byte(1)
		if line.Stream == "stderr" {
			kind = 2
		}
		frame := make([]byte, 8, 8+len(text))
		frame[0] = kind
		binary.BigEndian.PutUint32(frame[4:], uint32(len(text))) //nolint:gosec // a test line is far below 4 GiB
		_, err := w.Write(append(frame, text...))
		return err == nil
	}
	for _, line := range first {
		if !write(line) {
			return true
		}
	}
	flush()
	if follow {
		l.mu.Lock()
		l.follows++
		l.mu.Unlock()
		f.await.notify()
		defer func() {
			l.mu.Lock()
			l.follows--
			l.mu.Unlock()
			f.await.notify()
		}()
	}
	for follow {
		l.mu.Lock()
		next := append([]FakeServiceLogLine(nil), l.pushed[serviceID][seenPushed:]...)
		seenPushed = len(l.pushed[serviceID])
		if l.wake == nil {
			l.wake = make(chan struct{})
		}
		wake := l.wake
		l.mu.Unlock()
		for _, line := range next {
			if want(line) && !write(line) {
				return true
			}
		}
		flush()
		select {
		case <-wake:
		case <-r.Context().Done():
			return true
		}
	}
	return true
}
