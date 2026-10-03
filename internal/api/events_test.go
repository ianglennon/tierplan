// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
)

// The events routes (plan 21, ENV-07, D-47, D-57, Pitfall 8, T-01-87 to
// T-01-90). Docker is a FakeDaemon with a scripted /events stream behind the
// real monitor and hub; the stream tests drive the real router with a writer
// that is read as a stream, so no network or sleeps stand between the test and
// the handler.

type eventDTO struct {
	ID         string            `json:"id"`
	Time       string            `json:"time"`
	Type       string            `json:"type"`
	Action     string            `json:"action"`
	ActorID    string            `json:"actorId"`
	Name       string            `json:"name"`
	Attributes map[string]string `json:"attributes"`
}

type recentBody struct {
	Items []eventDTO `json:"items"`
}

// eventsFix is an environment fixture with the events hub wired in.
type eventsFix struct {
	*envFix
	env envBody
}

func newEventsFix(t *testing.T, cfg dockertest.FakeConfig, heartbeat time.Duration, maxStreams int) *eventsFix {
	t.Helper()
	f := newAuthFixture(t, config.Config{})
	fake := dockertest.NewFake(t, cfg)
	mon := envs.NewMonitor(slog.New(slog.DiscardHandler), nil, nil)
	t.Cleanup(mon.StopAll)
	svc := envs.NewService(f.st, f.aud, mon, "unix://"+fake.SocketPath(), "0123456789ab", f.clk.now)
	f.envSvc = svc
	f.snaps = envs.NewSnapshots(mon, 5*time.Second, f.clk.now)
	f.hub = envs.NewHub(mon, f.snaps, slog.New(slog.DiscardHandler), time.Now)
	t.Cleanup(f.hub.Close)
	f.agg = envs.NewAggregator(svc, f.snaps, f.hub, f.clk.now)
	f.eventsHeartbeat, f.eventsMaxStreams = heartbeat, maxStreams
	f.localHost.DockerHost = "unix://" + fake.SocketPath()
	f.localHost.ProbeTimeout = 500 * time.Millisecond
	f.localHost.Envs = svc
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)
	ef := &envFix{authFixture: f, fake: fake, svc: svc, mon: mon, cookie: cookie, csrf: csrf}
	return &eventsFix{envFix: ef, env: ef.add(t)}
}

func (f *eventsFix) waitBuffered(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(f.hub.Recent(f.env.ID, 1000)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d buffered events", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *eventsFix) waitStream(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.fake.OpenEventStreams() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the daemon stream")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *eventsFix) streamPath() string { return "/api/env/" + f.env.ID + "/events/stream" }

// frame is one server-sent event, or a comment.
type frame struct {
	id, event, data, comment string
}

// sseConn is one open request to the stream route. The handler writes into a
// pipe that the test reads frame by frame.
type sseConn struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan struct{} // closed when the handler returned

	mu        sync.Mutex
	header    http.Header
	status    int
	deadlines []time.Time
	flushes   int
	raw       strings.Builder // everything written, for a response that is not a stream

	started chan struct{}
	frames  chan frame
	body    chan string // chunks on their way to the frame parser
}

// Header, Write, WriteHeader, Flush and SetWriteDeadline make sseConn an
// http.ResponseWriter that streams, and that records the deadline the handler
// asked for.
func (c *sseConn) Header() http.Header { return c.header }

func (c *sseConn) WriteHeader(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == 0 {
		c.status = code
		close(c.started)
	}
}

func (c *sseConn) Write(b []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	c.mu.Lock()
	c.raw.Write(b)
	c.mu.Unlock()
	select {
	case c.body <- string(b):
		return len(b), nil
	case <-c.done:
		return 0, io.ErrClosedPipe
	}
}

func (c *sseConn) Flush() {
	c.mu.Lock()
	c.flushes++
	c.mu.Unlock()
}

func (c *sseConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, t)
	c.mu.Unlock()
	return nil
}

var _ http.Flusher = (*sseConn)(nil)

// bodyString is everything the handler wrote so far.
func (c *sseConn) bodyString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw.String()
}

// openStream sends the request and waits for the response to start. cookie is
// the raw session ID; lastEventID may be empty.
func (f *eventsFix) openStream(t *testing.T, cookie, lastEventID string) *sseConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, f.streamPath(), nil).WithContext(ctx)
	req.Header.Set("User-Agent", authUA)
	req.RemoteAddr = authIP + ":40000"
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: cookie}) //nolint:gosec // a request cookie as a browser sends it
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	c := &sseConn{
		t: t, cancel: cancel, done: make(chan struct{}), header: http.Header{},
		started: make(chan struct{}), frames: make(chan frame, 1024), body: make(chan string),
	}
	go func() {
		defer close(c.done)
		f.h.ServeHTTP(c, req)
	}()
	go c.parse()
	t.Cleanup(func() { c.close() })
	select {
	case <-c.started:
	case <-c.done:
		// a refusal that wrote nothing
	case <-time.After(5 * time.Second):
		t.Fatal("the stream route never answered")
	}
	return c
}

// parse reads the body chunks the handler wrote and splits them into frames.
func (c *sseConn) parse() {
	defer close(c.frames)
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		for {
			select {
			case s := <-c.body:
				if _, err := pw.Write([]byte(s)); err != nil {
					return
				}
			case <-c.done:
				return
			}
		}
	}()
	sc := bufio.NewScanner(pr)
	var cur frame
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur != (frame{}) {
				c.frames <- cur
			}
			cur = frame{}
		case strings.HasPrefix(line, ": "):
			cur.comment = strings.TrimPrefix(line, ": ")
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func (c *sseConn) close() {
	c.cancel()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		c.t.Error("the stream handler did not return after its request ended")
	}
}

// next returns the next data frame, skipping heartbeat comments.
func (c *sseConn) next(timeout time.Duration) (frame, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				return frame{}, false
			}
			if fr.comment != "" {
				continue
			}
			return fr, true
		case <-deadline:
			return frame{}, false
		}
	}
}

func (c *sseConn) mustNext(t *testing.T) (frame, eventDTO) {
	t.Helper()
	fr, ok := c.next(5 * time.Second)
	if !ok {
		t.Fatal("no frame arrived")
	}
	var dto eventDTO
	if err := json.Unmarshal([]byte(fr.data), &dto); err != nil {
		t.Fatalf("frame data %q: %v", fr.data, err)
	}
	return fr, dto
}

func (c *sseConn) status0() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func publish(f *eventsFix, ids ...string) {
	base := time.Now().UnixNano()
	for i, id := range ids {
		f.fake.PublishEvent(dockertest.FakeEvent{
			Type: "container", Action: "start", ActorID: id, TimeNano: base + int64(i),
			Attributes: map[string]string{"name": "n-" + id, "image": "img", "secret": "x"},
		})
	}
}

func TestEventStreamSendsTheHeadersTheBacklogAndThenLiveFrames(t *testing.T) {
	now := time.Now()
	f := newEventsFix(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "container", Action: "create", ActorID: "c-a", TimeNano: now.Add(-2 * time.Minute).UnixNano(), Attributes: map[string]string{"name": "web", "image": "nginx"}},
		{Type: "network", Action: "connect", ActorID: "n-b", TimeNano: now.Add(-time.Minute).UnixNano(), Attributes: map[string]string{"name": "app"}},
	}}, time.Hour, 0)
	f.waitBuffered(t, 2)

	c := f.openStream(t, f.cookie, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d", c.status0())
	}
	h := c.header
	if got := h.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := h.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no so nginx does not buffer the stream", got)
	}
	if got := h.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none: a compressed stream is held back by the compressor (Pitfall 8)", got)
	}
	fr1, e1 := c.mustNext(t)
	fr2, e2 := c.mustNext(t)
	// Read after the first frames arrive: the handler flushes the open response
	// just after WriteHeader, which is when openStream returns, so reading
	// earlier races with it.
	c.mu.Lock()
	deadlines, flushes := append([]time.Time(nil), c.deadlines...), c.flushes
	c.mu.Unlock()
	if len(deadlines) == 0 || !deadlines[0].IsZero() {
		t.Errorf("write deadlines asked for = %v, want the zero time first: the stream has no write deadline", deadlines)
	}
	if flushes == 0 {
		t.Error("the response was never flushed, so the browser would not see the stream open")
	}
	if e1.ActorID != "c-a" || e2.ActorID != "n-b" {
		t.Fatalf("backlog = %s then %s, want c-a then n-b (oldest first)", e1.ActorID, e2.ActorID)
	}
	if fr1.event != "docker" || fr1.id == "" || fr1.id != e1.ID || fr2.id != e2.ID {
		t.Errorf("frames = %+v / %+v: want event docker and the id of the event", fr1, fr2)
	}
	if e1.Type != "container" || e1.Action != "create" || e1.Name != "web" || e1.Attributes["image"] != "nginx" {
		t.Errorf("first event = %+v", e1)
	}
	if ts, err := time.Parse(time.RFC3339Nano, e1.Time); err != nil || ts.Location() != time.UTC {
		t.Errorf("time %q is not RFC 3339 UTC: %v", e1.Time, err)
	}

	publish(f, "c-live")
	fr3, e3 := c.mustNext(t)
	if e3.ActorID != "c-live" || fr3.id != e3.ID {
		t.Errorf("live frame = %+v / %+v", fr3, e3)
	}
	if _, ok := e3.Attributes["secret"]; ok {
		t.Errorf("attributes %v: only name, image and exitCode may reach the browser (T-01-88)", e3.Attributes)
	}
}

func TestEventStreamResumesAfterLastEventID(t *testing.T) {
	now := time.Now()
	f := newEventsFix(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "container", Action: "start", ActorID: "c-1", TimeNano: now.Add(-3 * time.Minute).UnixNano()},
		{Type: "container", Action: "start", ActorID: "c-2", TimeNano: now.Add(-2 * time.Minute).UnixNano()},
		{Type: "container", Action: "start", ActorID: "c-3", TimeNano: now.Add(-time.Minute).UnixNano()},
	}}, time.Hour, 0)
	f.waitBuffered(t, 3)
	recent := f.hub.Recent(f.env.ID, 3) // newest first
	second := recent[1].ID

	c := f.openStream(t, f.cookie, second)
	if _, e := c.mustNext(t); e.ActorID != "c-3" {
		t.Errorf("first frame after Last-Event-ID = %s, want c-3 only", e.ActorID)
	}
	if fr, ok := c.next(150 * time.Millisecond); ok {
		t.Errorf("an extra frame arrived: %+v", fr)
	}

	// An id the buffer no longer holds (the event fell out of the ring, or the
	// server restarted) gets the whole backlog: a repeat is harmless, a gap is not.
	c2 := f.openStream(t, f.cookie, "ffffffffffffffffffffffff")
	for _, want := range []string{"c-1", "c-2", "c-3"} {
		if _, e := c2.mustNext(t); e.ActorID != want {
			t.Errorf("unknown Last-Event-ID backlog frame = %s, want %s", e.ActorID, want)
		}
	}
}

func TestEventStreamSendsAHeartbeatCommentAtTheConfiguredInterval(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, 30*time.Millisecond, 0)
	c := f.openStream(t, f.cookie, "")

	beats := 0
	deadline := time.After(5 * time.Second)
	for beats < 3 {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				t.Fatal("the stream ended")
			}
			if fr.comment == "heartbeat" {
				beats++
			} else {
				t.Errorf("unexpected frame %+v", fr)
			}
		case <-deadline:
			t.Fatalf("only %d heartbeats arrived", beats)
		}
	}
}

func TestEventStreamAllowsEightPerUserAndRefusesTheNinthWith429(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, time.Hour, 0)
	conns := make([]*sseConn, 0, 8)
	for range 8 {
		c := f.openStream(t, f.cookie, "")
		if c.status0() != http.StatusOK {
			t.Fatalf("stream %d status = %d", len(conns)+1, c.status0())
		}
		conns = append(conns, c)
	}

	ninth := f.openStream(t, f.cookie, "")
	if ninth.status0() != http.StatusTooManyRequests {
		t.Fatalf("the 9th stream = %d, want 429", ninth.status0())
	}
	select {
	case <-ninth.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a refused stream stayed open")
	}
	if got := ninth.header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("429 Content-Type = %q", got)
	}
	if body := ninth.bodyString(); !strings.Contains(body, `"type":"rate_limited"`) {
		t.Errorf("429 body = %s, want the rate_limited problem type", body)
	}

	// Another user has their own 8.
	f.addUser(t, "other", true)
	otherCookie, _ := f.signIn(t, "other", false)
	if c := f.openStream(t, otherCookie, ""); c.status0() != http.StatusOK {
		t.Errorf("another user's first stream = %d, want 200: the limit is per user", c.status0())
	}

	// Closing one frees its slot.
	conns[0].close()
	if c := f.openStream(t, f.cookie, ""); c.status0() != http.StatusOK {
		t.Errorf("a stream after one closed = %d, want 200", c.status0())
	}
}

func TestEventStreamWithoutASessionIs401AndAuditedAsADeniedRead(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, time.Hour, 0)
	c := f.openStream(t, "", "")
	if c.status0() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", c.status0())
	}
	entries := f.audits(t, audit.ActionPermissionDenied)
	found := false
	for _, e := range entries {
		found = found || (e.Result == "denied" && strings.Contains(e.TargetID, "/events/stream"))
	}
	if !found {
		t.Errorf("no permission.denied entry for the stream route: %+v", entries)
	}
}

func TestEventRoutesNeedEnvironmentsReadAndAKnownEnvironment(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, time.Hour, 0)
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)

	c := f.openStream(t, cookie, "")
	if c.status0() != http.StatusForbidden {
		t.Errorf("the stream for a user without environments.read = %d, want 403", c.status0())
	}
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + f.env.ID + "/events", cookie: cookie})
	if rec.Code != http.StatusForbidden {
		t.Errorf("recent events for a user without environments.read = %d, want 403", rec.Code)
	}

	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/no-such-env/events", cookie: f.cookie})
	if rec.Code != http.StatusNotFound {
		t.Errorf("recent events of an unknown environment = %d, want 404", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/env/no-such-env/events/stream", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: f.cookie}) //nolint:gosec // a request cookie as a browser sends it
	rec2 := httptest.NewRecorder()
	f.h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("the stream of an unknown environment = %d, want 404", rec2.Code)
	}
}

func TestEventStreamEndsWhenTheHubClosesAndWhenTheClientLeaves(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, time.Hour, 0)
	f.waitStream(t)

	gone := f.openStream(t, f.cookie, "")
	gone.cancel()
	select {
	case <-gone.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler kept running after the client left")
	}

	open := f.openStream(t, f.cookie, "")
	f.hub.Close() // server shutdown: open streams must not hold it up
	select {
	case <-open.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler kept running after the hub closed")
	}
}

func TestEventRecentReturnsTheNewest50NewestFirstWithTheStreamFields(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, time.Hour, 0)
	f.waitStream(t)

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + f.env.ID + "/events", cookie: f.cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("an environment with no events must answer an empty list, got %s", rec.Body)
	}

	ids := make([]string, 0, 60)
	for i := range 60 {
		ids = append(ids, fmt.Sprintf("c%02d", i))
	}
	publish(f, ids...)
	f.waitBuffered(t, 60)

	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + f.env.ID + "/events", cookie: f.cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var body recentBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 50 || body.Items[0].ActorID != "c59" || body.Items[49].ActorID != "c10" {
		t.Fatalf("got %d items from %s to %s, want 50 from c59 down to c10", len(body.Items), body.Items[0].ActorID, body.Items[len(body.Items)-1].ActorID)
	}
	first := body.Items[0]
	if first.ID == "" || first.Type != "container" || first.Action != "start" || first.Name != "n-c59" || first.Attributes["image"] != "img" || first.Time == "" {
		t.Errorf("first item = %+v", first)
	}
	if _, ok := first.Attributes["secret"]; ok {
		t.Errorf("attributes %v leak a label", first.Attributes)
	}
}

func TestEventOpenAPIDocumentsTheRecentEndpointButNotTheStream(t *testing.T) {
	r := httpx.New(httpx.Options{})
	api.RegisterEvents(r, api.EventsDeps{})
	doc, err := json.Marshal(r.API().OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if !strings.Contains(s, `"/api/env/{envId}/events"`) || !strings.Contains(s, `"listRecentEvents"`) {
		t.Errorf("the recent events operation is missing from the contract")
	}
	if !strings.Contains(s, `"EventDTO"`) {
		t.Error("the event schema is missing from the contract, so the frontend would have no type for it")
	}
	if strings.Contains(s, "events/stream") {
		t.Error("the SSE route must not appear in the OpenAPI document: huma cannot model it")
	}
	if err := httpx.VerifyAllGuarded(r); err != nil {
		t.Errorf("the events routes are not all guarded: %v", err)
	}
}

// --- The stream ends when what it was authorised by goes away (WR-02) -------

// waitEnded fails unless the stream's handler returns within d.
func (c *sseConn) waitEnded(t *testing.T, d time.Duration, why string) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(d):
		t.Fatalf("the stream was still open %v after %s", d, why)
	}
}

func TestEventStreamEndsWithinAHeartbeatOfSignOut(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, 20*time.Millisecond, 0)
	c := f.openStream(t, f.cookie, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d", c.status0())
	}
	if _, err := f.st.Write.Exec(`DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	c.waitEnded(t, time.Second, "the session was deleted")

	// A reconnect goes through the whole policy and is a denied read, which the
	// SPA already treats as a signed-out session.
	rec := f.call(t, authReq{method: http.MethodGet, path: f.streamPath(), cookie: f.cookie})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reconnect after sign-out: %d, want 401", rec.Code)
	}
	var denied int
	for _, e := range f.audits(t, audit.ActionPermissionDenied) {
		if e.Result == "denied" && e.Reason == "unauthenticated" {
			denied++
		}
	}
	if denied == 0 {
		t.Fatal("the refused reconnect was not audited as a denied read")
	}
}

func TestEventStreamEndsWhenTheUserIsDisabledOrTheEnvironmentRemoved(t *testing.T) {
	t.Run("user disabled", func(t *testing.T) {
		f := newEventsFix(t, dockertest.FakeConfig{}, 20*time.Millisecond, 0)
		c := f.openStream(t, f.cookie, "")
		if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1`); err != nil {
			t.Fatal(err)
		}
		c.waitEnded(t, time.Second, "the user was disabled")
	})
	t.Run("environment removed", func(t *testing.T) {
		f := newEventsFix(t, dockertest.FakeConfig{}, 20*time.Millisecond, 0)
		c := f.openStream(t, f.cookie, "")
		actor := audit.Actor{Type: audit.ActorUser, ID: "id-admin", Name: "admin"}
		if err := f.svc.Remove(context.Background(), actor, f.env.ID, authIP, authUA); err != nil {
			t.Fatal(err)
		}
		c.waitEnded(t, time.Second, "the environment was removed")
	})
	t.Run("environment disabled", func(t *testing.T) {
		f := newEventsFix(t, dockertest.FakeConfig{}, 20*time.Millisecond, 0)
		c := f.openStream(t, f.cookie, "")
		if _, err := f.st.Write.Exec(`UPDATE environments SET enabled = 0`); err != nil {
			t.Fatal(err)
		}
		c.waitEnded(t, time.Second, "the environment was disabled")
	})
	t.Run("permission withdrawn", func(t *testing.T) {
		f := newEventsFix(t, dockertest.FakeConfig{}, 20*time.Millisecond, 0)
		c := f.openStream(t, f.cookie, "")
		if _, err := f.st.Write.Exec(`DELETE FROM role_assignments`); err != nil {
			t.Fatal(err)
		}
		c.waitEnded(t, time.Second, "the admin role was withdrawn")
	})
}

func TestEventStreamEndsWhenTheSessionIdlesOutWithoutBeingTouched(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, 20*time.Millisecond, 0)
	c := f.openStream(t, f.cookie, "")
	var seen0 int64
	if err := f.st.Read.QueryRow(`SELECT last_seen_at FROM sessions`).Scan(&seen0); err != nil {
		t.Fatal(err)
	}
	// Age the row rather than the shared fake clock, which the stream's
	// goroutine reads: the session has been idle for 12 hours and a second.
	if _, err := f.st.Write.Exec(`UPDATE sessions SET last_seen_at = last_seen_at - ?`, int64(authIdle+time.Second)); err != nil {
		t.Fatal(err)
	}
	seen0 -= int64(authIdle + time.Second)
	c.waitEnded(t, time.Second, "the session idled out")
	var seen1 int64
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*), COALESCE(MAX(last_seen_at), 0) FROM sessions`).Scan(&n, &seen1); err != nil {
		t.Fatal(err)
	}
	if n != 1 || seen1 != seen0 {
		t.Fatalf("re-checking changed the session: rows %d, last_seen_at %d -> %d (D-31)", n, seen0, seen1)
	}
}

func TestEventStreamEndsAtItsMaximumLifetime(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, time.Hour, 0)
	f.eventsMaxLifetime = 100 * time.Millisecond
	f.h = f.build(t)
	c := f.openStream(t, f.cookie, "")
	c.waitEnded(t, time.Second, "its maximum lifetime passed")
}

func TestEventStreamKeepsRunningWhileTheSessionStaysValid(t *testing.T) {
	f := newEventsFix(t, dockertest.FakeConfig{}, 20*time.Millisecond, 0)
	c := f.openStream(t, f.cookie, "")
	beats := 0
	deadline := time.After(5 * time.Second)
	for beats < 6 {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				t.Fatalf("the stream ended after %d heartbeats although nothing was revoked", beats)
			}
			if fr.comment == "heartbeat" {
				beats++
			}
		case <-deadline:
			t.Fatalf("only %d heartbeats arrived", beats)
		}
	}
	publish(f, "c-after-beats")
	if _, e := c.mustNext(t); e.ActorID != "c-after-beats" {
		t.Fatalf("event after several re-checks = %+v", e)
	}
	select {
	case <-c.done:
		t.Fatal("the stream ended with a valid session")
	default:
	}
}
