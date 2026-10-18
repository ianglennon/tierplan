// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The per-environment events hub (plan 21, ENV-07, D-57, Pattern 8, Pitfall 7).
// Docker is a FakeDaemon whose /events stream can be scripted, dropped and
// resumed, behind the real monitor, so the real client and probe run. Nothing
// here touches a database: events are never stored (D-57).

const hubEnv = "env1"

type hubRig struct {
	mon   *Monitor
	fake  *dockertest.Fake
	hub   *Hub
	snaps *Snapshots
}

// fastSleep lets a monitor wait 10ms whatever it asked for, so a stopped daemon
// is noticed and an online one probed again within the test's patience.
func fastSleep(ctx context.Context, _ time.Duration) error {
	return realSleep(ctx, 10*time.Millisecond)
}

// newHubRig starts a monitor over one fake engine and a hub observing it. The
// hub is created first, so it learns of the environment from the monitor and
// nobody calls Hub.Start by hand.
func newHubRig(t *testing.T, cfg dockertest.FakeConfig) *hubRig {
	t.Helper()
	fake := dockertest.NewFake(t, cfg)
	mon := NewMonitor(slog.New(slog.DiscardHandler), nil, fastSleep)
	mon.interval = 20 * time.Millisecond
	t.Cleanup(mon.StopAll)
	snaps := NewSnapshots(mon, time.Hour, nil)
	hub := NewHub(mon, snaps, slog.New(slog.DiscardHandler), time.Now)
	hub.poll = 10 * time.Millisecond
	hub.retryMin, hub.retryMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(hub.Close)
	if err := mon.Start(context.Background(), hubEnv, fake); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitState(t, mon, hubEnv, "the environment to be online", stateIs(StateOnline))
	return &hubRig{mon: mon, fake: fake, hub: hub, snaps: snaps}
}

func (r *hubRig) eventRequests() int {
	n := 0
	for _, p := range r.fake.Requests() {
		if strings.HasSuffix(p, "/events") {
			n++
		}
	}
	return n
}

// awaitFake waits for a condition on the fake daemon, which wakes the waiter on every request
// it serves and every stream it opens or ends.
func (r *hubRig) awaitFake(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if err := r.fake.Await(testsupport.Context(t), cond); err != nil {
		t.Fatalf("timed out waiting for %s: %v", what, err)
	}
}

// awaitHub waits for a condition on the hub, which wakes the waiter whenever a feed opens or
// closes and whenever an event is buffered.
func (r *hubRig) awaitHub(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if err := r.hub.Await(testsupport.Context(t), cond); err != nil {
		t.Fatalf("timed out waiting for %s: %v", what, err)
	}
}

func (r *hubRig) waitStreamOpen(t *testing.T) {
	t.Helper()
	r.awaitFake(t, "the daemon stream to open", func() bool { return r.fake.OpenEventStreams() == 1 })
}

func (r *hubRig) waitRecent(t *testing.T, n int) {
	t.Helper()
	r.awaitHub(t, fmt.Sprintf("%d buffered events", n), func() bool { return len(r.hub.Recent(hubEnv, 1000)) >= n })
}

// waitReopened waits for the hub to have asked the daemon for a second stream and to hold it.
func (r *hubRig) waitReopened(t *testing.T) {
	t.Helper()
	r.awaitFake(t, "the stream to reopen", func() bool { return len(r.fake.EventQueries()) == 2 && r.fake.OpenEventStreams() == 1 })
}

func (r *hubRig) subscribe(t *testing.T) ([]dockerx.EventRow, <-chan dockerx.EventRow, func()) {
	t.Helper()
	backlog, ch, cancel := r.hub.Subscribe(context.Background(), hubEnv)
	t.Cleanup(cancel)
	return backlog, ch, cancel
}

func recvEvent(t *testing.T, ch <-chan dockerx.EventRow) dockerx.EventRow {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the subscriber channel closed while an event was expected")
		}
		return ev
	case <-testsupport.Context(t).Done():
		t.Fatal("timed out waiting for an event")
	}
	return dockerx.EventRow{}
}

// expectNext proves a row did not arrive twice, or at all, without waiting for it not to: it
// takes the sentinel row the caller published after the one under test, and requires it to be
// the next row the subscriber receives. The hub hands every subscriber its rows in the order
// they were ingested (feed.fanOutLocked, under the feed's lock, into one channel), so a
// duplicate or a stray row would be delivered before the sentinel and fail this check every
// time, on a fast machine and a slow one.
func expectNext(t *testing.T, ch <-chan dockerx.EventRow, sentinelID string) {
	t.Helper()
	if ev := recvEvent(t, ch); ev.ActorID != sentinelID && ev.ID != sentinelID {
		t.Fatalf("an unexpected event arrived before the sentinel %q: %+v", sentinelID, ev)
	}
}

func actors(rows []dockerx.EventRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ActorID)
	}
	return out
}

func TestHubFirstSubscriberGetsTheBackfillAsBacklogThenLiveEvents(t *testing.T) {
	now := time.Now()
	r := newHubRig(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "container", Action: "die", ActorID: "too-old", TimeNano: now.Add(-30 * time.Hour).UnixNano()},
		{Type: "container", Action: "start", ActorID: "c-a", TimeNano: now.Add(-20 * time.Hour).UnixNano()},
		{Type: "container", Action: "stop", ActorID: "c-b", TimeNano: now.Add(-20 * time.Minute).UnixNano()},
	}})
	r.waitRecent(t, 2)

	backlog, ch, _ := r.subscribe(t)
	if got := actors(backlog); !slices.Equal(got, []string{"c-a", "c-b"}) {
		t.Fatalf("backlog = %v, want the last 24 hours' events oldest first, without the 30 hour old one", got)
	}
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-live"})
	if ev := recvEvent(t, ch); ev.ActorID != "c-live" {
		t.Errorf("live event = %+v", ev)
	}

	qs := r.fake.EventQueries()
	if len(qs) != 1 {
		t.Fatalf("%d /events requests, want 1", len(qs))
	}
	if since, want := qs[0].Get("since"), fmt.Sprint(now.Add(-24*time.Hour).Unix()); since < want || since > fmt.Sprint(time.Now().Add(-24*time.Hour).Unix()) {
		t.Errorf("first since = %q, want about now minus 24 hours (%s)", since, want)
	}
}

func TestHubSecondSubscriberGetsTheSameBacklogWithoutASecondDaemonRequest(t *testing.T) {
	now := time.Now()
	r := newHubRig(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "container", Action: "start", ActorID: "c-a", TimeNano: now.Add(-time.Minute).UnixNano()},
	}})
	r.waitRecent(t, 1)

	first, _, _ := r.subscribe(t)
	second, _, _ := r.subscribe(t)
	if !slices.Equal(actors(first), []string{"c-a"}) || !slices.Equal(actors(second), actors(first)) {
		t.Errorf("backlogs = %v and %v", actors(first), actors(second))
	}
	// Starting it again, as the monitor does when an environment is restarted,
	// must not open a second stream: exactly one per environment.
	for range 3 {
		r.hub.Start(hubEnv)
	}
	// An event published after the Starts and seen in the buffer proves the hub has
	// carried on past them; a second stream they had opened would have asked the daemon
	// by now and been counted below.
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-after-starts"})
	r.waitRecent(t, 2)
	if got := r.eventRequests(); got != 1 {
		t.Errorf("%d /events requests, want 1 whatever the number of viewers", got)
	}
	if got := r.fake.OpenEventStreams(); got != 1 {
		t.Errorf("%d daemon streams open, want 1", got)
	}
}

func TestHubRingKeepsTheNewest1000EventsAndRemembersMoreIDsThanThat(t *testing.T) {
	if ring, seen := ringSize, seenSize; ring != 1000 || seen != 4096 || seen <= ring {
		t.Fatalf("ringSize = %d and seenSize = %d, want 1000 and 4096: the IDs remembered must outnumber the ring, or a replayed event that just left it would come back", ring, seen)
	}
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	base := time.Now().UnixNano()
	// In batches the fake's per-stream buffer can hold; a stream nobody reads fast
	// enough is dropped, which is not what this test is about.
	const total = 1100
	for start := 0; start < total; start += 100 {
		for i := start; i < start+100; i++ {
			r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: fmt.Sprintf("c%04d", i), TimeNano: base + int64(i)})
		}
		last := fmt.Sprintf("c%04d", start+99)
		r.awaitHub(t, "a batch to arrive", func() bool {
			recent := r.hub.Recent(hubEnv, 1)
			return len(recent) == 1 && recent[0].ActorID == last
		})
	}

	all := r.hub.Recent(hubEnv, 5000)
	if len(all) != 1000 || all[0].ActorID != "c1099" || all[999].ActorID != "c0100" {
		t.Fatalf("Recent holds %d events from %s to %s, want the newest 1000, c1099 down to c0100", len(all), all[0].ActorID, all[len(all)-1].ActorID)
	}
	backlog, _, _ := r.subscribe(t)
	if len(backlog) != 1000 || backlog[0].ActorID != "c0100" || backlog[999].ActorID != "c1099" {
		t.Errorf("backlog holds %d events, oldest %s and newest %s, want c0100 up to c1099", len(backlog), backlog[0].ActorID, backlog[len(backlog)-1].ActorID)
	}
}

func TestHubRecentIsNewestFirstAndLimited(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	for i := range 5 {
		r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: fmt.Sprintf("c%d", i)})
	}
	r.waitRecent(t, 5)
	if got := actors(r.hub.Recent(hubEnv, 3)); !slices.Equal(got, []string{"c4", "c3", "c2"}) {
		t.Errorf("Recent(3) = %v, want the newest three, newest first", got)
	}
	if got := r.hub.Recent(hubEnv, 0); len(got) != 0 {
		t.Errorf("Recent(0) = %v, want none", got)
	}
	if got := r.hub.Recent("no-such-env", 5); len(got) != 0 {
		t.Errorf("Recent of an unknown environment = %v, want none", got)
	}
}

func TestHubAfterADropResumesFromTheLastEventAndShowsItOnce(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)

	n := time.Now().UnixNano()
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-n", TimeNano: n})
	if ev := recvEvent(t, ch); ev.ActorID != "c-n" {
		t.Fatalf("first event = %+v", ev)
	}

	r.fake.DropEventStreams()
	r.waitReopened(t)
	// The fake replays everything at or after since, so the event the hub already
	// has is sent again. A subscriber must not see it twice.
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "c-n1", TimeNano: n + 1})
	if ev := recvEvent(t, ch); ev.ActorID != "c-n1" {
		t.Errorf("after the drop the next event is %+v, want c-n1 (c-n must appear once)", ev)
	}
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "sentinel", TimeNano: n + 2})
	expectNext(t, ch, "sentinel")

	want := fmt.Sprintf("%d.%09d", n/int64(time.Second), n%int64(time.Second))
	if got := r.fake.EventQueries()[1].Get("since"); got != want {
		t.Errorf("the reconnect asked since=%q, want the last event's time %q", got, want)
	}
	if got := actors(r.hub.Recent(hubEnv, 10)); !slices.Equal(got, []string{"sentinel", "c-n1", "c-n"}) {
		t.Errorf("the buffer holds %v, want each event once", got)
	}
}

func TestHubDedupesAnEventTheBackfillAndTheLiveStreamBothDeliver(t *testing.T) {
	// Pitfall 7: the daemon re-sends an event at the since boundary. Here the
	// same event arrives twice on one stream, once as history and once live.
	n := time.Now().Add(-time.Minute).UnixNano()
	r := newHubRig(t, dockertest.FakeConfig{Events: []dockertest.FakeEvent{
		{Type: "container", Action: "start", ActorID: "c-dup", TimeNano: n},
	}})
	r.waitRecent(t, 1)
	_, ch, _ := r.subscribe(t)

	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-dup", TimeNano: n})
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-next", TimeNano: n + 1})
	if ev := recvEvent(t, ch); ev.ActorID != "c-next" {
		t.Errorf("first live event = %+v, want c-next (the duplicate must be ignored)", ev)
	}
	if got := len(r.hub.Recent(hubEnv, 10)); got != 2 {
		t.Errorf("the buffer holds %d events, want 2", got)
	}
}

func TestHubKeepsArrivalOrderForEventsWithTheSameTimestamp(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)
	ts := time.Now().UnixNano()
	for _, id := range []string{"zeta", "alpha", "mid"} {
		r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: id, TimeNano: ts})
	}
	var live []string
	for range 3 {
		live = append(live, recvEvent(t, ch).ActorID)
	}
	if !slices.Equal(live, []string{"zeta", "alpha", "mid"}) {
		t.Errorf("live order = %v, want arrival order", live)
	}
	backlog, _, _ := r.subscribe(t)
	if !slices.Equal(actors(backlog), []string{"zeta", "alpha", "mid"}) {
		t.Errorf("backlog order = %v, want arrival order", actors(backlog))
	}
	if got := actors(r.hub.Recent(hubEnv, 3)); !slices.Equal(got, []string{"mid", "alpha", "zeta"}) {
		t.Errorf("Recent = %v, want the reverse of arrival", got)
	}
}

func TestHubDropsExecNoiseBeforeTheBuffer(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "exec_start: sh", ActorID: "noise"})
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "exec_die", ActorID: "noise"})
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "kept"})
	if ev := recvEvent(t, ch); ev.ActorID != "kept" {
		t.Errorf("first event = %+v, want the kept one", ev)
	}
	if got := actors(r.hub.Recent(hubEnv, 10)); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("the buffer holds %v, want only the kept event", got)
	}
}

func TestHubInvalidateOnlyRowDropsTheSnapshotAndReachesSubscribersButNeverTheBuffer(t *testing.T) {
	// RES-14: a health change must refresh the open lists, and must not enter the
	// feed people read.
	r := newHubRig(t, oneContainer())
	r.waitStreamOpen(t)
	listRequests := func() int {
		n := 0
		for _, p := range r.fake.Requests() {
			if strings.HasSuffix(p, "/containers/json") {
				n++
			}
		}
		return n
	}
	ctx := context.Background()
	if _, _, err := r.snaps.Get(ctx, hubEnv); err != nil {
		t.Fatal(err)
	}
	before := listRequests()
	_, ch, _ := r.subscribe(t)

	n := time.Now().UnixNano()
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "health_status: unhealthy", ActorID: "c1", TimeNano: n,
		Attributes: map[string]string{"name": "web", "check": "curl -u a:secret"}})
	ev := recvEvent(t, ch)
	if !ev.InvalidateOnly || ev.ActorID != "c1" || len(ev.Attributes) != 0 {
		t.Fatalf("subscriber got %+v, want an invalidate-only row for c1 with no attributes", ev)
	}
	if _, _, err := r.snaps.Get(ctx, hubEnv); err != nil {
		t.Fatal(err)
	}
	if got := listRequests(); got != before+1 {
		t.Errorf("%d list fetches after a health change, want %d (the row must drop the cached snapshot)", got, before+1)
	}
	if got := r.hub.Recent(hubEnv, 10); len(got) != 0 {
		t.Errorf("Recent = %v, want the invalidate-only row kept out of the buffer", actors(got))
	}
	if backlog, _, _ := r.subscribe(t); len(backlog) != 0 {
		t.Errorf("a new subscriber's backlog = %v, want none", actors(backlog))
	}

	// After a drop the daemon replays the row; it is remembered, so it is not
	// delivered, and does not invalidate, a second time.
	r.fake.DropEventStreams()
	r.waitReopened(t)
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "c-next", TimeNano: n + 1})
	if ev := recvEvent(t, ch); ev.ActorID != "c-next" {
		t.Errorf("after the drop the next event is %+v, want c-next (the health row must not repeat)", ev)
	}
}

func TestHubEveryKeptEventInvalidatesTheSnapshotCache(t *testing.T) {
	r := newHubRig(t, oneContainer())
	r.waitStreamOpen(t)
	listRequests := func() int {
		n := 0
		for _, p := range r.fake.Requests() {
			if strings.HasSuffix(p, "/containers/json") {
				n++
			}
		}
		return n
	}
	ctx := context.Background()
	if _, _, err := r.snaps.Get(ctx, hubEnv); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.snaps.Get(ctx, hubEnv); err != nil {
		t.Fatal(err)
	}
	if got := listRequests(); got != 1 {
		t.Fatalf("%d list fetches for two reads inside the TTL, want 1", got)
	}

	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-new"})
	r.waitRecent(t, 1)
	if _, _, err := r.snaps.Get(ctx, hubEnv); err != nil {
		t.Fatal(err)
	}
	if got := listRequests(); got != 2 {
		t.Errorf("%d list fetches after an event, want 2 (the event must drop the cached snapshot)", got)
	}

	r.fake.PublishEvent(dockertest.FakeEvent{Type: "image", Action: "pull", ActorID: "nginx"})
	r.waitRecent(t, 2)
	if _, _, err := r.snaps.Get(ctx, hubEnv); err != nil {
		t.Fatal(err)
	}
	if got := listRequests(); got != 3 {
		t.Errorf("%d list fetches after an image event, want 3", got)
	}
}

func TestHubStopsItsStreamWhileTheEnvironmentIsNotOnlineAndBackfillsWhenItReturns(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)
	n := time.Now().UnixNano()
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-before", TimeNano: n})
	recvEvent(t, ch)

	// The engine still answers /events but its info does not, so the monitor
	// says the environment is no longer online: the stream must stop.
	r.fake.Reconfigure(dockertest.FakeConfig{FailInfo: true})
	left := awaitState(t, r.mon, hubEnv, "the monitor to leave online", func(s Status) bool { return s.State != StateOnline })
	r.awaitFake(t, "the daemon stream to be closed", func() bool { return r.fake.OpenEventStreams() == 0 })
	before := len(r.fake.EventQueries())
	// A later failed probe is a causally later event: the monitor has looked again and still
	// does not offer the hub an engine, so a stream the hub meant to reopen would have been
	// asked for by now.
	awaitState(t, r.mon, hubEnv, "another failed probe", func(s Status) bool {
		return s.State != StateOnline && s.LastAttempt.After(left.LastAttempt)
	})
	if got := len(r.fake.EventQueries()); got != before {
		t.Errorf("%d more /events requests while the environment is not online, want none", got-before)
	}

	r.fake.Reconfigure(dockertest.FakeConfig{})
	awaitState(t, r.mon, hubEnv, "the environment to be online again", stateIs(StateOnline))
	r.awaitFake(t, "the stream to reopen", func() bool { return r.fake.OpenEventStreams() == 1 })
	qs := r.fake.EventQueries()
	want := fmt.Sprintf("%d.%09d", n/int64(time.Second), n%int64(time.Second))
	if got := qs[len(qs)-1].Get("since"); got != want {
		t.Errorf("the new stream asked since=%q, want the last seen event %q", got, want)
	}
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "c-after", TimeNano: n + 5})
	if ev := recvEvent(t, ch); ev.ActorID != "c-after" {
		t.Errorf("after the return the next event is %+v, want c-after", ev)
	}
}

func TestHubSurvivesTheDaemonGoingDownAndComingBack(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)
	n := time.Now().UnixNano()
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-before", TimeNano: n})
	recvEvent(t, ch)

	r.fake.Stop()
	awaitState(t, r.mon, hubEnv, "the monitor to see it offline", stateIs(StateOffline))
	// A restarted daemon has an empty buffer (Pitfall 7).
	r.fake.ClearEvents()
	r.fake.Restart(t)
	awaitState(t, r.mon, hubEnv, "the environment to be online again", stateIs(StateOnline))
	r.awaitFake(t, "the stream to reopen", func() bool { return r.fake.OpenEventStreams() == 1 })

	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "c-after", TimeNano: n + 5})
	if ev := recvEvent(t, ch); ev.ActorID != "c-after" {
		t.Errorf("after the restart the next event is %+v, want c-after", ev)
	}
	if got := actors(r.hub.Recent(hubEnv, 10)); !slices.Equal(got, []string{"c-after", "c-before"}) {
		t.Errorf("the buffer holds %v, want both events", got)
	}
}

func TestHubDropsASlowSubscriberInsteadOfBlockingTheOthers(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, slow, _ := r.subscribe(t) // never read
	_, fast, _ := r.subscribe(t)

	base := time.Now().UnixNano()
	const total = 150
	// Lock-step: the fast subscriber takes each event before the next one is
	// published, so it can never fall 64 events behind, however busy the machine
	// is. Publishing the burst while a goroutine raced to read it let a loaded
	// runner starve that goroutine until the hub dropped the fast subscriber too.
	// The slow one never reads, so it fills up and is dropped by the 65th event.
	for i := range total {
		r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: fmt.Sprintf("c%03d", i), TimeNano: base + int64(i)})
		ev := recvEvent(t, fast)
		if want := fmt.Sprintf("c%03d", i); ev.ActorID != want {
			t.Fatalf("fast subscriber event %d = %s, want %s", i, ev.ActorID, want)
		}
	}

	// The slow channel holds its buffer of 64 and is then closed.
	got := 0
	timeout := testsupport.Context(t).Done()
	for open := true; open; {
		select {
		case _, ok := <-slow:
			if ok {
				got++
			}
			open = ok
		case <-timeout:
			t.Fatal("the slow subscriber's channel was never closed")
		}
	}
	if got == 0 || got > 64 {
		t.Errorf("the slow subscriber received %d events before it was closed, want between 1 and 64", got)
	}
}

func TestHubCancelClosesTheChannelAndStopsDelivery(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, cancel := r.subscribe(t)
	cancel()
	cancel() // safe to call twice
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("an event arrived after cancel")
		}
	case <-testsupport.Context(t).Done():
		t.Error("the channel was not closed by cancel")
	}
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "later"})
	r.waitRecent(t, 1) // the hub carries on for others
}

func TestHubSubscribeEndsWhenItsContextEnds(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, ch, stop := r.hub.Subscribe(ctx, hubEnv)
	defer stop()
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("an event arrived after the context ended")
		}
	case <-testsupport.Context(t).Done():
		t.Error("the channel was not closed when the context ended")
	}
}

func TestHubStopEndsTheStreamAndClosesSubscribers(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)
	r.mon.Stop(hubEnv) // removal goes through the monitor, which tells the hub

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("an event arrived after the environment stopped")
		}
	case <-testsupport.Context(t).Done():
		t.Fatal("the subscriber was not closed when the environment was removed")
	}
	r.awaitFake(t, "the daemon stream to end", func() bool { return r.fake.OpenEventStreams() == 0 })
	if got := r.hub.Recent(hubEnv, 10); len(got) != 0 {
		t.Errorf("Recent after removal = %v, want nothing kept", got)
	}
	// Starting the monitor again starts a feed again.
	if err := r.mon.Start(context.Background(), hubEnv, r.fake); err != nil {
		t.Fatal(err)
	}
	r.waitStreamOpen(t)
}

func TestHubCloseEndsEverythingAndLaterSubscribersGetAClosedChannel(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)

	r.hub.Close()
	r.hub.Close() // safe to call twice
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("an event arrived after Close")
		}
	case <-testsupport.Context(t).Done():
		t.Fatal("Close did not end the subscriber")
	}
	r.awaitFake(t, "the daemon stream to end", func() bool { return r.fake.OpenEventStreams() == 0 })

	_, late, cancel := r.hub.Subscribe(context.Background(), hubEnv)
	defer cancel()
	if _, ok := <-late; ok {
		t.Error("a subscriber after Close received an event instead of a closed channel")
	}
	r.hub.Start(hubEnv)
	// Start creates a feed under the hub's lock or not at all, so once it has returned the
	// feed is there or it never will be; a stream follows only from a feed.
	r.hub.mu.Lock()
	feeds := len(r.hub.feeds)
	r.hub.mu.Unlock()
	if feeds != 0 {
		t.Errorf("Start after Close created %d feeds", feeds)
	}
	if r.fake.OpenEventStreams() != 0 {
		t.Error("Start after Close opened a stream")
	}
}

func TestHubSubscribeToAnUnknownEnvironmentGivesAnEmptyBacklogAndAClosedChannel(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	backlog, ch, cancel := r.hub.Subscribe(context.Background(), "no-such-env")
	defer cancel()
	if len(backlog) != 0 {
		t.Errorf("backlog = %v", backlog)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("an unknown environment delivered an event")
		}
	case <-testsupport.Context(t).Done():
		t.Error("the channel of an unknown environment is open and would block a stream forever")
	}
}

// A row of Bayplan's own, such as a manager failover (plan 02-20, D-43).

func TestInjectedRowsAppearInTheBacklogAndToSubscribersAndRepeatsShowOnce(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)

	row := dockerx.EventRow{
		ID: "bayplan-1", Time: time.Now().UTC(), TimeNano: time.Now().UnixNano(),
		Type: "bayplan", Action: "failover", ActorID: hubEnv, Name: "Manager a unreachable, using b",
	}
	if !r.hub.Inject(hubEnv, row) {
		t.Fatal("Inject reported no feed for a monitored target")
	}
	if got := recvEvent(t, ch); got.ID != "bayplan-1" || got.Name != row.Name {
		t.Fatalf("subscriber got %+v, want the injected row", got)
	}
	if !r.hub.Inject(hubEnv, row) {
		t.Fatal("a repeated Inject reported no feed")
	}
	sentinel := dockerx.EventRow{ID: "bayplan-2", Time: time.Now().UTC(), TimeNano: time.Now().UnixNano(), Type: "bayplan", Action: "failover", ActorID: hubEnv, Name: "sentinel"}
	if !r.hub.Inject(hubEnv, sentinel) {
		t.Fatal("Inject of the sentinel reported no feed")
	}
	expectNext(t, ch, "bayplan-2")
	if rows := r.hub.Recent(hubEnv, 10); len(rows) != 2 || rows[0].ID != "bayplan-2" || rows[1].Type != "bayplan" || rows[1].ID != "bayplan-1" {
		t.Fatalf("Recent = %+v, want the sentinel, then the injected row once (newest first)", rows)
	}
	backlog, _, _ := r.hub.Subscribe(context.Background(), hubEnv)
	if len(backlog) != 2 || backlog[0].ID != "bayplan-1" {
		t.Fatalf("backlog of a later subscriber = %+v, want the injected row once, then the sentinel", backlog)
	}
}

func TestInjectingIntoATargetWithNoFeedIsRefusedAndAnInjectedRowDoesNotMoveTheResumePoint(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	if r.hub.Inject("nobody", dockerx.EventRow{ID: "x"}) {
		t.Error("Inject reported a feed for a target that has none")
	}
	// The resume point is the time of the newest event the daemon sent; a row of
	// Bayplan's own, however new, must not move it past events the daemon has yet to send.
	future := time.Now().Add(time.Hour).UnixNano()
	r.hub.Inject(hubEnv, dockerx.EventRow{ID: "bp", Time: time.Now().Add(time.Hour).UTC(), TimeNano: future, Type: "bayplan", Action: "failover"})
	r.hub.mu.Lock()
	f := r.hub.feeds[hubEnv]
	r.hub.mu.Unlock()
	if n, ok := f.lastSeen(); ok && n >= future {
		t.Errorf("the resume point moved to %d because of an injected row", n)
	}
}

// The merged feed of an environment (plan 02-21, ENV-06, D-46, D-43, Pitfall 18):
// every present target's events with their node, a swarm-scoped event from the
// manager in use only, and Bayplan's own rows. The agents are real sessions over
// real tunnels with a fake daemon behind each, as in cluster_test.go.

// nodeSink reads a feed in the background and keeps what it saw.
type nodeSink struct {
	mu   sync.Mutex
	rows []NodeEvent
	// changed is made by the first waiter and closed when the next row arrives.
	changed chan struct{}
}

func collect(backlog []NodeEvent, ch <-chan NodeEvent) *nodeSink {
	s := &nodeSink{rows: append([]NodeEvent(nil), backlog...)}
	go func() {
		for r := range ch {
			s.mu.Lock()
			s.rows = append(s.rows, r)
			if s.changed != nil {
				close(s.changed)
				s.changed = nil
			}
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *nodeSink) where(pred func(NodeEvent) bool) []NodeEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []NodeEvent
	for _, r := range s.rows {
		if pred(r) {
			out = append(out, r)
		}
	}
	return out
}

// await waits until n rows satisfy pred, waking on each row the sink receives.
func (s *nodeSink) await(t *testing.T, what string, pred func(NodeEvent) bool, n int) {
	t.Helper()
	ctx := testsupport.Context(t)
	for {
		s.mu.Lock()
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		ch := s.changed
		s.mu.Unlock()
		if len(s.where(pred)) >= n {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func actor(id string) func(NodeEvent) bool { return func(e NodeEvent) bool { return e.ActorID == id } }

// mergedRig is a three-manager swarm with the leader known and a quick feed poll.
func newMergedRig(t *testing.T) *swarmRig {
	t.Helper()
	s := newSwarmRig(t, managerNodes(), nil)
	s.agg.WithEventPoll(10 * time.Millisecond)
	t.Cleanup(s.agg.Close)
	// The first read does not know the leader yet; the second goes to it.
	for range 2 {
		if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *swarmRig) subscribeMerged(t *testing.T) *nodeSink {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	backlog, ch, stop := s.agg.Events(ctx, s.env.ID)
	t.Cleanup(stop)
	return collect(backlog, ch)
}

// sentinels publishes a node-local event on each daemon and waits for all of them.
// A daemon's events are handled in order, so once its sentinel is in the feed
// everything it published before has been decided too.
func sentinels(t *testing.T, sink *nodeSink, nodes map[string]*agentConn) {
	t.Helper()
	for node, a := range nodes {
		a.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "sentinel-" + node, Attributes: map[string]string{"name": "sentinel"}})
	}
	for node := range nodes {
		sink.await(t, "the sentinel of "+node, actor("sentinel-"+node), 1)
	}
}

func TestMergedFeedTagsEachEventWithTheNodeItHappenedOn(t *testing.T) {
	s := newMergedRig(t)
	sink := s.subscribeMerged(t)
	s.m[2].fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "abc", Attributes: map[string]string{"name": "web"}})
	sink.await(t, "the event", actor("abc"), 1)
	if got := sink.where(actor("abc")); len(got) != 1 || got[0].Node != "mgr-3" || got[0].Name != "web" {
		t.Fatalf("event = %+v, want one event with node mgr-3", got)
	}
}

func TestSwarmScopeEventReportedByEveryManagerReachesTheFeedOnceFromTheManagerInUse(t *testing.T) {
	s := newMergedRig(t)
	sink := s.subscribeMerged(t)
	base := time.Now().UnixNano()
	// The same cluster event, each manager stamping it with its own clock.
	for i, a := range s.m {
		a.fake.PublishEvent(dockertest.FakeEvent{
			Type: "network", Action: "create", ActorID: "ovl-1", Scope: "swarm", TimeNano: base + int64(i)*int64(time.Millisecond),
			Attributes: map[string]string{"name": "ovl"},
		})
	}
	sentinels(t, sink, map[string]*agentConn{"mgr-1": s.m[0], "mgr-2": s.m[1], "mgr-3": s.m[2]})
	got := sink.where(actor("ovl-1"))
	if len(got) != 1 || got[0].Node != "mgr-2" {
		t.Fatalf("the cluster event reached the feed as %+v, want once, from mgr-2 (the leader, in use)", got)
	}
}

func TestSwarmScopeEventsFollowTheManagerInUseAcrossAFailoverWithoutLossOrRepeat(t *testing.T) {
	s := newMergedRig(t)
	sink := s.subscribeMerged(t)
	base := time.Now().UnixNano()
	swarmEvent := func(a *agentConn, id string, at int64) {
		a.fake.PublishEvent(dockertest.FakeEvent{Type: "network", Action: "create", ActorID: id, Scope: "swarm", TimeNano: at})
	}
	ms := int64(time.Millisecond)

	// Before: all three see net-1; the leader's is the one shown.
	swarmEvent(s.m[0], "net-1", base+ms)
	swarmEvent(s.m[1], "net-1", base)
	swarmEvent(s.m[2], "net-1", base-ms)
	sink.await(t, "net-1", actor("net-1"), 1)

	// The leader dies. net-2 happens after that, so only the two managers left see it.
	s.kick(t, s.m[1])
	swarmEvent(s.m[0], "net-2", base+10*ms)
	swarmEvent(s.m[2], "net-2", base+11*ms)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	fo := s.failovers(t)
	if len(fo) != 1 {
		t.Fatalf("failovers = %+v, want one", fo)
	}
	to := fo[0].ToNode

	sink.await(t, "net-2", actor("net-2"), 1)
	// And once more after the switch.
	swarmEvent(s.m[0], "net-3", base+20*ms)
	swarmEvent(s.m[2], "net-3", base+21*ms)
	sink.await(t, "net-3", actor("net-3"), 1)
	sentinels(t, sink, map[string]*agentConn{"mgr-1": s.m[0], "mgr-3": s.m[2]})

	one := func(id, node string) {
		t.Helper()
		got := sink.where(actor(id))
		if len(got) != 1 || (node != "" && got[0].Node != node) {
			t.Errorf("%s reached the feed as %+v, want once from %q", id, got, node)
		}
	}
	one("net-1", "mgr-2")
	one("net-2", to) // seen by the new manager while it was not yet the one in use, and not lost
	one("net-3", to)
}

func TestMergedFeedInvalidateOnlyRowReachesSubscribersWithItsNodeButNeverTheRing(t *testing.T) {
	s := newMergedRig(t)
	sink := s.subscribeMerged(t)
	s.m[2].fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "health_status: unhealthy", ActorID: "sick", Attributes: map[string]string{"name": "web", "cmd": "secret"}})
	sink.await(t, "the invalidate row", actor("sick"), 1)
	got := sink.where(actor("sick"))
	if len(got) != 1 || !got[0].InvalidateOnly || got[0].Node != "mgr-3" || len(got[0].Attributes) != 0 {
		t.Fatalf("subscriber rows = %+v, want one invalidate-only row from mgr-3 with no attributes", got)
	}
	sentinels(t, sink, map[string]*agentConn{"mgr-3": s.m[2]})
	for _, ev := range s.agg.Recent(context.Background(), s.env.ID, 100) {
		if ev.ActorID == "sick" {
			t.Errorf("Recent holds the invalidate-only row %+v", ev)
		}
	}
	late := s.subscribeMerged(t)
	if len(late.where(actor("sick"))) != 0 {
		t.Error("a later subscriber's backlog holds the invalidate-only row")
	}
}

func TestMergedFeedRecentSinceKeepsRowsAtOrAfterTheTimeNewestFirstAndAtMostN(t *testing.T) {
	s := newMergedRig(t)
	sink := s.subscribeMerged(t)
	now := time.Now()
	times := map[string]time.Duration{"old": -10 * time.Hour, "mid": -5 * time.Hour, "new": -time.Hour, "newest": -time.Minute}
	for _, id := range []string{"old", "mid", "new", "newest"} {
		s.m[2].fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: id, TimeNano: now.Add(times[id]).UnixNano()})
	}
	sentinels(t, sink, map[string]*agentConn{"mgr-3": s.m[2]})
	ids := func(rows []NodeEvent) []string {
		var out []string
		for _, r := range rows {
			if r.ActorID != "sentinel-mgr-3" {
				out = append(out, r.ActorID)
			}
		}
		return out
	}
	ctx := context.Background()
	if got := ids(s.agg.RecentSince(ctx, s.env.ID, now.Add(-6*time.Hour), 100)); !slices.Equal(got, []string{"newest", "new", "mid"}) {
		t.Errorf("since 6h ago = %v, want newest, new, mid", got)
	}
	if got := ids(s.agg.RecentSince(ctx, s.env.ID, time.Time{}, 100)); !slices.Equal(got, []string{"newest", "new", "mid", "old"}) {
		t.Errorf("no bound = %v, want all four newest first", got)
	}
	if got := s.agg.RecentSince(ctx, s.env.ID, time.Time{}, 2); len(got) != 2 {
		t.Errorf("n = 2 returned %d rows", len(got))
	}
	if got := s.agg.RecentSince(ctx, s.env.ID, now.Add(time.Hour), 100); len(got) != 0 {
		t.Errorf("since the future = %v, want none", ids(got))
	}
	if got := s.agg.RecentSince(ctx, s.env.ID, time.Time{}, 0); len(got) != 0 {
		t.Errorf("n = 0 returned %d rows", len(got))
	}
}

func TestMergedFeedCarriesTheFailoverRowAsBayplanWithTheNewManagerAsNode(t *testing.T) {
	s := newMergedRig(t)
	sink := s.subscribeMerged(t)
	s.kick(t, s.m[1])
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	to := s.failovers(t)[0].ToNode
	isFailover := func(e NodeEvent) bool { return e.Type == "bayplan" && e.Action == "failover" }
	sink.await(t, "the failover row", isFailover, 1)
	if got := sink.where(isFailover); len(got) != 1 || got[0].Node != to {
		t.Fatalf("failover rows = %+v, want one with node %s, the manager that took over", got, to)
	}
}

func TestMergedFeedRecentMergesTheNodesNewestFirstAndBacklogIsOldestFirst(t *testing.T) {
	s := newMergedRig(t)
	base := time.Now().UnixNano()
	ms := int64(time.Millisecond)
	// Interleaved in time across the three daemons.
	for i, a := range s.m {
		a.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: fmt.Sprintf("e%d", i+1), TimeNano: base + int64(i+1)*ms})
		a.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "stop", ActorID: fmt.Sprintf("e%d", i+4), TimeNano: base + int64(i+4)*ms})
	}
	for _, a := range s.m {
		if err := s.evs.Await(testsupport.Context(t), func() bool { return len(s.evs.Recent(AgentTarget(a.id), 10)) >= 2 }); err != nil {
			t.Fatalf("the hub never held both events of a node: %v", err)
		}
	}
	ids := func(rows []NodeEvent) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ActorID)
		}
		return strings.Join(out, " ")
	}
	if got := ids(s.agg.Recent(context.Background(), s.env.ID, 10)); got != "e6 e5 e4 e3 e2 e1" {
		t.Errorf("Recent = %s, want every node's events newest first", got)
	}
	if got := ids(s.agg.Recent(context.Background(), s.env.ID, 2)); got != "e6 e5" {
		t.Errorf("Recent(2) = %s, want the newest two", got)
	}
	backlog, _, stop := s.agg.Events(context.Background(), s.env.ID)
	defer stop()
	if got := ids(backlog); got != "e1 e2 e3 e4 e5 e6" {
		t.Errorf("backlog = %s, want oldest first", got)
	}
}

func TestMergedFeedOfAnEnvironmentWithNoAgentIsEmptyAndAlreadyClosed(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Waiting")
	backlog, ch, stop := r.agg.Events(context.Background(), env.ID)
	defer stop()
	if len(backlog) != 0 {
		t.Errorf("backlog = %+v, want none", backlog)
	}
	select {
	case _, open := <-ch:
		if open {
			t.Error("the channel delivered an event")
		}
	case <-testsupport.Context(t).Done():
		t.Error("the channel of an environment with nothing to read is not closed")
	}
	if rows := r.agg.Recent(context.Background(), env.ID, 5); len(rows) != 0 {
		t.Errorf("Recent = %+v, want none", rows)
	}
}

func TestMergedFeedClosesItsSubscribersWhenTheAggregatorIsClosed(t *testing.T) {
	s := newMergedRig(t)
	_, ch, stop := s.agg.Events(context.Background(), s.env.ID)
	defer stop()
	s.agg.Close()
	select {
	case _, open := <-ch:
		if open {
			t.Error("an event arrived instead of the end of the feed")
		}
	case <-testsupport.Context(t).Done():
		t.Fatal("the subscription is still open after Close")
	}
	if _, ch, stop := s.agg.Events(context.Background(), s.env.ID); true {
		defer stop()
		if _, open := <-ch; open {
			t.Error("a subscriber after Close got an open channel")
		}
	}
}
