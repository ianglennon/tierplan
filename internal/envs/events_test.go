// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
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
	eventually(t, "the environment to be online", func() bool { return mon.Status(hubEnv).State == StateOnline })
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

func (r *hubRig) waitStreamOpen(t *testing.T) {
	t.Helper()
	eventually(t, "the daemon stream to open", func() bool { return r.fake.OpenEventStreams() == 1 })
}

func (r *hubRig) waitRecent(t *testing.T, n int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d buffered events", n), func() bool { return len(r.hub.Recent(hubEnv, 1000)) >= n })
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
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return dockerx.EventRow{}
}

func expectQuiet(t *testing.T, ch <-chan dockerx.EventRow) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("an unexpected event arrived: %+v", ev)
		}
		t.Fatal("the subscriber channel closed unexpectedly")
	case <-time.After(150 * time.Millisecond):
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
		{Type: "container", Action: "die", ActorID: "too-old", TimeNano: now.Add(-2 * time.Hour).UnixNano()},
		{Type: "container", Action: "start", ActorID: "c-a", TimeNano: now.Add(-30 * time.Minute).UnixNano()},
		{Type: "container", Action: "stop", ActorID: "c-b", TimeNano: now.Add(-20 * time.Minute).UnixNano()},
	}})
	r.waitRecent(t, 2)

	backlog, ch, _ := r.subscribe(t)
	if got := actors(backlog); !slices.Equal(got, []string{"c-a", "c-b"}) {
		t.Fatalf("backlog = %v, want the last hour's events oldest first, without the 2 hour old one", got)
	}
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-live"})
	if ev := recvEvent(t, ch); ev.ActorID != "c-live" {
		t.Errorf("live event = %+v", ev)
	}

	qs := r.fake.EventQueries()
	if len(qs) != 1 {
		t.Fatalf("%d /events requests, want 1", len(qs))
	}
	if since, want := qs[0].Get("since"), fmt.Sprint(now.Add(-time.Hour).Unix()); since < want || since > fmt.Sprint(time.Now().Add(-time.Hour).Unix()) {
		t.Errorf("first since = %q, want about now minus one hour (%s)", since, want)
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
	time.Sleep(100 * time.Millisecond)
	if got := r.eventRequests(); got != 1 {
		t.Errorf("%d /events requests, want 1 whatever the number of viewers", got)
	}
	if got := r.fake.OpenEventStreams(); got != 1 {
		t.Errorf("%d daemon streams open, want 1", got)
	}
}

func TestHubRingKeepsTheNewest200Events(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	base := time.Now().UnixNano()
	for i := range 250 {
		r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: fmt.Sprintf("c%03d", i), TimeNano: base + int64(i)})
	}
	eventually(t, "the newest event", func() bool {
		recent := r.hub.Recent(hubEnv, 1)
		return len(recent) == 1 && recent[0].ActorID == "c249"
	})

	all := r.hub.Recent(hubEnv, 1000)
	if len(all) != 200 || all[0].ActorID != "c249" || all[199].ActorID != "c050" {
		t.Fatalf("Recent holds %d events from %s to %s, want the newest 200, c249 down to c050", len(all), all[0].ActorID, all[len(all)-1].ActorID)
	}
	backlog, _, _ := r.subscribe(t)
	if len(backlog) != 200 || backlog[0].ActorID != "c050" || backlog[199].ActorID != "c249" {
		t.Errorf("backlog holds %d events, oldest %s and newest %s, want c050 up to c249", len(backlog), backlog[0].ActorID, backlog[len(backlog)-1].ActorID)
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
	eventually(t, "the stream to reopen", func() bool { return len(r.fake.EventQueries()) == 2 && r.fake.OpenEventStreams() == 1 })
	// The fake replays everything at or after since, so the event the hub already
	// has is sent again. A subscriber must not see it twice.
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "c-n1", TimeNano: n + 1})
	if ev := recvEvent(t, ch); ev.ActorID != "c-n1" {
		t.Errorf("after the drop the next event is %+v, want c-n1 (c-n must appear once)", ev)
	}
	expectQuiet(t, ch)

	want := fmt.Sprintf("%d.%09d", n/int64(time.Second), n%int64(time.Second))
	if got := r.fake.EventQueries()[1].Get("since"); got != want {
		t.Errorf("the reconnect asked since=%q, want the last event's time %q", got, want)
	}
	if got := actors(r.hub.Recent(hubEnv, 10)); !slices.Equal(got, []string{"c-n1", "c-n"}) {
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

func TestHubDropsExecAndHealthNoiseBeforeTheBuffer(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	r.waitStreamOpen(t)
	_, ch, _ := r.subscribe(t)
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "exec_start: sh", ActorID: "noise"})
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "health_status: healthy", ActorID: "noise"})
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: "kept"})
	if ev := recvEvent(t, ch); ev.ActorID != "kept" {
		t.Errorf("first event = %+v, want the kept one", ev)
	}
	if got := actors(r.hub.Recent(hubEnv, 10)); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("the buffer holds %v, want only the kept event", got)
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
	eventually(t, "the second event", func() bool { return len(r.hub.Recent(hubEnv, 10)) == 2 })
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
	eventually(t, "the monitor to leave online", func() bool { return r.mon.Status(hubEnv).State != StateOnline })
	eventually(t, "the daemon stream to be closed", func() bool { return r.fake.OpenEventStreams() == 0 })
	before := len(r.fake.EventQueries())
	time.Sleep(100 * time.Millisecond)
	if got := len(r.fake.EventQueries()); got != before {
		t.Errorf("%d more /events requests while the environment is not online, want none", got-before)
	}

	r.fake.Reconfigure(dockertest.FakeConfig{})
	eventually(t, "the environment to be online again", func() bool { return r.mon.Status(hubEnv).State == StateOnline })
	eventually(t, "the stream to reopen", func() bool { return r.fake.OpenEventStreams() == 1 })
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
	eventually(t, "the monitor to see it offline", func() bool { return r.mon.Status(hubEnv).State == StateOffline })
	// A restarted daemon has an empty buffer (Pitfall 7).
	r.fake.ClearEvents()
	r.fake.Restart(t)
	eventually(t, "the environment to be online again", func() bool { return r.mon.Status(hubEnv).State == StateOnline })
	eventually(t, "the stream to reopen", func() bool { return r.fake.OpenEventStreams() == 1 })

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
	timeout := time.After(5 * time.Second)
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
	case <-time.After(time.Second):
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
	case <-time.After(time.Second):
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
	case <-time.After(5 * time.Second):
		t.Fatal("the subscriber was not closed when the environment was removed")
	}
	eventually(t, "the daemon stream to end", func() bool { return r.fake.OpenEventStreams() == 0 })
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
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the subscriber")
	}
	eventually(t, "the daemon stream to end", func() bool { return r.fake.OpenEventStreams() == 0 })

	_, late, cancel := r.hub.Subscribe(context.Background(), hubEnv)
	defer cancel()
	if _, ok := <-late; ok {
		t.Error("a subscriber after Close received an event instead of a closed channel")
	}
	r.hub.Start(hubEnv)
	time.Sleep(50 * time.Millisecond)
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
	case <-time.After(time.Second):
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
	expectQuiet(t, ch)
	if rows := r.hub.Recent(hubEnv, 10); len(rows) != 1 || rows[0].Type != "bayplan" {
		t.Fatalf("Recent = %+v, want the one injected row", rows)
	}
	backlog, _, _ := r.hub.Subscribe(context.Background(), hubEnv)
	if len(backlog) != 1 || backlog[0].ID != "bayplan-1" {
		t.Fatalf("backlog of a later subscriber = %+v, want the injected row", backlog)
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
