// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

// What counts as failing (plan 02-20, ENV-06, D-34, RESEARCH Pitfall 21).

func service(mode string, running, desired uint64, state string, updatedAgo time.Duration) dockerx.ServiceRow {
	return dockerx.ServiceRow{
		ID: fmt.Sprintf("%s-%d-%d", mode, running, desired), Name: mode, Mode: mode, Running: running, Desired: desired,
		UpdateState: state, UpdatedAt: t0.Add(-updatedAgo),
	}
}

func TestFailingServices(t *testing.T) {
	grace := 2 * time.Minute
	for _, tc := range []struct {
		name              string
		svc               dockerx.ServiceRow
		failing, inTotals int
	}{
		{"a replicated service with 2 of 3 running", service("replicated", 2, 3, "", time.Hour), 1, 1},
		{"a replicated service with all running", service("replicated", 3, 3, "", time.Hour), 0, 1},
		{"a replicated service with none running", service("replicated", 0, 3, "", time.Hour), 1, 1},
		{"a global service missing on a node", service("global", 3, 4, "", time.Hour), 1, 1},
		{"a global service everywhere", service("global", 4, 4, "", time.Hour), 0, 1},
		{"a replicated job is skipped", service("replicated-job", 0, 1, "", time.Hour), 0, 0},
		{"a global job is skipped", service("global-job", 0, 4, "", time.Hour), 0, 0},
		{"a service that is updating", service("replicated", 1, 3, "updating", time.Hour), 0, 1},
		{"a service that is rolling back", service("replicated", 1, 3, "rollback_started", time.Hour), 0, 1},
		{"a finished update that left tasks missing", service("replicated", 2, 3, "completed", time.Hour), 1, 1},
		{"a failed update with tasks missing", service("replicated", 2, 3, "paused", time.Hour), 1, 1},
		{"updated 90 s ago", service("replicated", 2, 3, "", 90*time.Second), 0, 1},
		{"updated 119.999 s ago", service("replicated", 2, 3, "", 2*time.Minute-time.Millisecond), 0, 1},
		{"updated exactly the grace ago", service("replicated", 2, 3, "", 2*time.Minute), 1, 1},
		{"updated 3 min ago", service("replicated", 2, 3, "", 3*time.Minute), 1, 1},
		{"a service that never had an update time", dockerx.ServiceRow{Name: "x", Mode: "replicated", Running: 0, Desired: 1}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failing, total := FailingServices([]dockerx.ServiceRow{tc.svc}, t0, grace)
			if failing != tc.failing || total != tc.inTotals {
				t.Fatalf("FailingServices = %d of %d, want %d of %d", failing, total, tc.failing, tc.inTotals)
			}
		})
	}
}

func TestFailingServicesCountsAMixOfServices(t *testing.T) {
	failing, total := FailingServices([]dockerx.ServiceRow{
		service("replicated", 2, 3, "", time.Hour),
		service("replicated", 3, 3, "", time.Hour),
		service("global", 1, 4, "", time.Hour),
		service("replicated-job", 0, 1, "", time.Hour),
		service("replicated", 0, 2, "updating", time.Hour),
	}, t0, 2*time.Minute)
	if failing != 2 || total != 4 {
		t.Fatalf("FailingServices = %d of %d, want 2 of 4 (the job does not count at all)", failing, total)
	}
	if f, n := FailingServices(nil, t0, time.Minute); f != 0 || n != 0 {
		t.Errorf("no services = %d of %d", f, n)
	}
}

func ctr(id, state, status string) dockerx.ContainerRow {
	return dockerx.ContainerRow{ID: id, Name: id, State: state, Status: status}
}

// policies is an InspectFunc over a fixed table that counts what it was asked.
type policies struct {
	mu    sync.Mutex
	table map[string]dockerx.RestartPolicy
	calls [][]string
}

func (p *policies) inspect(_ context.Context, ids []string) (map[string]dockerx.RestartPolicy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, append([]string(nil), ids...))
	out := map[string]dockerx.RestartPolicy{}
	for _, id := range ids {
		pol, ok := p.table[id]
		if !ok {
			return nil, errors.New("No such container: " + id)
		}
		out[id] = pol
	}
	return out, nil
}

func (p *policies) asked() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		n += len(c)
	}
	return n
}

func TestFailingContainers(t *testing.T) {
	for _, tc := range []struct {
		name            string
		row             dockerx.ContainerRow
		policy          string
		failing, inRate int
	}{
		{"restarting with always", ctr("c", "restarting", "Restarting (1) 5 seconds ago"), "always", 1, 1},
		{"exited 1 with on-failure", ctr("c", "exited", "Exited (1) 2 hours ago"), "on-failure", 1, 1},
		{"exited 137 with unless-stopped", ctr("c", "exited", "Exited (137) 3 minutes ago"), "unless-stopped", 1, 1},
		{"exited 0 with always", ctr("c", "exited", "Exited (0) 2 hours ago"), "always", 0, 1},
		{"exited 1 with no policy", ctr("c", "exited", "Exited (1) 2 hours ago"), "no", 0, 0},
		{"running and unhealthy with unless-stopped", ctr("c", "running", "Up 2 hours (unhealthy)"), "unless-stopped", 1, 1},
		{"running and healthy with always", ctr("c", "running", "Up 2 hours (healthy)"), "always", 0, 1},
		{"running with no health check", ctr("c", "running", "Up 2 hours"), "always", 0, 1},
		{"running and unhealthy with no policy", ctr("c", "running", "Up 2 hours (unhealthy)"), "no", 0, 0},
		{"restarting with no policy", ctr("c", "restarting", "Restarting (1) 5 seconds ago"), "no", 0, 0},
		{"created and never started", ctr("c", "created", "Created"), "always", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &policies{table: map[string]dockerx.RestartPolicy{"c": {Name: tc.policy}}}
			failing, total := FailingContainers(context.Background(), []dockerx.ContainerRow{tc.row}, NewPolicyCache(), p.inspect)
			if failing != tc.failing || total != tc.inRate {
				t.Fatalf("FailingContainers = %d of %d, want %d of %d", failing, total, tc.failing, tc.inRate)
			}
		})
	}
}

func TestFailingContainersCachesPoliciesByContainerID(t *testing.T) {
	rows := []dockerx.ContainerRow{
		ctr("a", "restarting", "Restarting (1) 1 second ago"),
		ctr("b", "running", "Up 1 hour"),
	}
	p := &policies{table: map[string]dockerx.RestartPolicy{"a": {Name: "always"}, "b": {Name: "no"}}}
	cache := NewPolicyCache()
	for range 3 {
		failing, total := FailingContainers(context.Background(), rows, cache, p.inspect)
		if failing != 1 || total != 1 {
			t.Fatalf("FailingContainers = %d of %d, want 1 of 1", failing, total)
		}
	}
	if got := p.asked(); got != 2 {
		t.Errorf("%d inspects over three cycles, want 2: each container once, then the cache", got)
	}
	if cache.Len() != 2 {
		t.Errorf("the cache holds %d containers, want 2", cache.Len())
	}

	// A container that is no longer listed leaves the cache, so it cannot grow for ever.
	FailingContainers(context.Background(), rows[:1], cache, p.inspect)
	if cache.Len() != 1 {
		t.Errorf("the cache holds %d containers after one was removed, want 1", cache.Len())
	}
}

func TestFailingContainersInspectsAtMostFiftyPerCycleProblemsFirst(t *testing.T) {
	var rows []dockerx.ContainerRow
	table := map[string]dockerx.RestartPolicy{}
	for i := range 120 {
		id := fmt.Sprintf("c%03d", i)
		rows = append(rows, ctr(id, "running", "Up 1 hour"))
		table[id] = dockerx.RestartPolicy{Name: "always"}
	}
	// One failing container, last in the list: it must not wait behind the rest.
	rows = append(rows, ctr("zzz", "restarting", "Restarting (1) 1 second ago"))
	table["zzz"] = dockerx.RestartPolicy{Name: "always"}

	p := &policies{table: table}
	cache := NewPolicyCache()
	failing, total := FailingContainers(context.Background(), rows, cache, p.inspect)
	if got := p.asked(); got != MaxPolicyInspects {
		t.Fatalf("first cycle inspected %d containers, want %d", got, MaxPolicyInspects)
	}
	if failing != 1 {
		t.Errorf("the failing container was not found in the first cycle: %d failing", failing)
	}
	if total != MaxPolicyInspects {
		t.Errorf("total = %d, want the %d whose policy is known", total, MaxPolicyInspects)
	}
	// The next cycles finish the job, 50 at a time.
	for cycle := 2; cycle <= 3; cycle++ {
		before := p.asked()
		FailingContainers(context.Background(), rows, cache, p.inspect)
		if got := p.asked() - before; got > MaxPolicyInspects {
			t.Errorf("cycle %d inspected %d, want at most %d", cycle, got, MaxPolicyInspects)
		}
	}
	failing, total = FailingContainers(context.Background(), rows, cache, p.inspect)
	if failing != 1 || total != 121 {
		t.Errorf("after three cycles = %d of %d, want 1 of 121", failing, total)
	}
	if p.asked() != 121 {
		t.Errorf("%d inspects in all, want 121: nothing is inspected twice", p.asked())
	}
}

func TestFailingContainersSkipsAContainerThatVanishedBeforeItWasInspected(t *testing.T) {
	rows := []dockerx.ContainerRow{
		ctr("gone", "restarting", "Restarting (1) 1 second ago"),
		ctr("here", "restarting", "Restarting (1) 1 second ago"),
	}
	p := &policies{table: map[string]dockerx.RestartPolicy{"here": {Name: "always"}}}
	failing, total := FailingContainers(context.Background(), rows, NewPolicyCache(), p.inspect)
	if failing != 1 || total != 1 {
		t.Fatalf("FailingContainers = %d of %d, want 1 of 1: one missing container must not hide the others", failing, total)
	}
}

func TestFailingContainersWithoutAnInspectCountsNothing(t *testing.T) {
	rows := []dockerx.ContainerRow{ctr("a", "restarting", "Restarting (1) 1 second ago")}
	failing, total := FailingContainers(context.Background(), rows, NewPolicyCache(), func(context.Context, []string) (map[string]dockerx.RestartPolicy, error) {
		return nil, errors.New("tunnel down")
	})
	if failing != 0 || total != 0 {
		t.Fatalf("FailingContainers = %d of %d, want 0 of 0 while no policy is known", failing, total)
	}
}

// The rows say which services and containers the counts count, and why (plan 12-26, ENV-10).

func TestFailingServiceRowsAreTheServicesTheCountCounts(t *testing.T) {
	grace := 2 * time.Minute
	svcs := []dockerx.ServiceRow{
		service("replicated", 2, 3, "", time.Hour),
		service("replicated", 3, 3, "", time.Hour),
		service("global", 3, 4, "", time.Hour),
		service("replicated-job", 0, 1, "", time.Hour),
		service("replicated", 1, 3, "updating", time.Hour),
		service("replicated", 2, 3, "", 90*time.Second),
	}
	rows := FailingServiceRows(svcs, nil, t0, grace)
	failing, _ := FailingServices(svcs, t0, grace)
	if len(rows) != failing || failing != 2 {
		t.Fatalf("FailingServiceRows = %d rows, FailingServices = %d, want both 2", len(rows), failing)
	}
	if rows[0].Running != 2 || rows[0].Desired != 3 || rows[1].Running != 3 || rows[1].Desired != 4 {
		t.Errorf("rows = %+v, want 2 of 3 then 3 of 4", rows)
	}
}

func TestFailingServiceRowsTakeSinceAndErrorFromTheNewestFailedTask(t *testing.T) {
	s := service("replicated", 1, 2, "", time.Hour)
	s.ID, s.Stack = "svc1", "shop"
	other := service("replicated", 1, 2, "", time.Hour)
	other.ID = "svc2"
	failed := []dockerx.FailedTaskRow{
		{ID: "t3", ServiceID: "svc1", At: t0.Add(-time.Minute), Err: "task: non-zero exit (1)\x1b[0m\n"},
		{ID: "t2", ServiceID: "svc1", At: t0.Add(-time.Hour), Err: "an older error"},
		{ID: "t1", ServiceID: "other", At: t0, Err: "someone else's"},
	}
	rows := FailingServiceRows([]dockerx.ServiceRow{s, other}, failed, t0, time.Minute)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Stack != "shop" || !rows[0].Since.Equal(t0.Add(-time.Minute)) || rows[0].LastError != "task: non-zero exit (1)[0m" {
		t.Errorf("row with a failed task = %+v, want its newest task's time, and its error without control characters", rows[0])
	}
	if !rows[1].Since.Equal(other.UpdatedAt) || rows[1].LastError != "" {
		t.Errorf("row without a failed task = %+v, want the update time and no error", rows[1])
	}
}

func TestFailingServiceRowsCutALongError(t *testing.T) {
	s := service("replicated", 0, 1, "", time.Hour)
	s.ID = "x"
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'e'
	}
	rows := FailingServiceRows([]dockerx.ServiceRow{s}, []dockerx.FailedTaskRow{{ServiceID: "x", At: t0, Err: string(long)}}, t0, time.Minute)
	if n := len(rows[0].LastError); n != MaxFailingErrorLen {
		t.Fatalf("error length = %d, want %d", n, MaxFailingErrorLen)
	}
}

func TestFailingContainerRowsAreTheContainersTheCountCounts(t *testing.T) {
	mk := func(id, state, status string) ContainerRow {
		r := ctr(id, state, status)
		r.Labels = map[string]string{"com.docker.compose.project": "shop"}
		return ContainerRow{ContainerRow: r, Node: "node-1", NodeID: "n1"}
	}
	rows := []ContainerRow{
		mk("restart", "restarting", "Restarting (1) 5 seconds ago"),
		mk("exit", "exited", "Exited (137) 3 minutes ago"),
		mk("sick", "running", "Up 2 hours (unhealthy)"),
		mk("clean", "exited", "Exited (0) 2 hours ago"),
		mk("nopolicy", "exited", "Exited (1) 2 hours ago"),
		mk("unknown", "exited", "Exited (1) 2 hours ago"),
	}
	cache := NewPolicyCache()
	pol := map[string]dockerx.RestartPolicy{
		"restart": {Name: "always"}, "exit": {Name: "on-failure"}, "sick": {Name: "unless-stopped"},
		"clean": {Name: "always"}, "nopolicy": {Name: "no"},
	}
	plain := make([]dockerx.ContainerRow, 0, len(rows))
	for _, r := range rows {
		plain = append(plain, r.ContainerRow)
	}
	failing, _ := FailingContainers(context.Background(), plain, cache, (&policies{table: pol}).inspect)

	got := FailingContainerRows(rows, cache, t0)
	if len(got) != failing || failing != 3 {
		t.Fatalf("FailingContainerRows = %d rows, FailingContainers = %d, want both 3: %+v", len(got), failing, got)
	}
	want := []FailingContainerRow{
		{ID: "restart", Name: "restart", Node: "node-1", NodeID: "n1", Stack: "shop", State: FailingRestarting, Since: t0.Add(-5 * time.Second), LastError: "Keeps restarting, last exit code 1"},
		{ID: "exit", Name: "exit", Node: "node-1", NodeID: "n1", Stack: "shop", State: FailingExited, Since: t0.Add(-3 * time.Minute), LastError: "Exited with code 137"},
		{ID: "sick", Name: "sick", Node: "node-1", NodeID: "n1", Stack: "shop", State: FailingUnhealthy, LastError: "Its health check says it is unhealthy"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if rows := FailingContainerRows(rows, nil, t0); len(rows) != 0 {
		t.Errorf("with no cache = %+v, want none", rows)
	}
}

func TestAgoOfReadsTheEnginesRoundedAges(t *testing.T) {
	for status, want := range map[string]time.Duration{
		"Exited (1) 5 seconds ago":          5 * time.Second,
		"Exited (1) About a minute ago":     time.Minute,
		"Exited (1) About an hour ago":      time.Hour,
		"Exited (1) 3 days ago":             72 * time.Hour,
		"Exited (1) Less than a second ago": time.Second,
	} {
		if got := agoOf(status, t0); !got.Equal(t0.Add(-want)) {
			t.Errorf("agoOf(%q) = %v, want %v before", status, got, want)
		}
	}
	for _, status := range []string{"Up 2 hours (unhealthy)", "Created", "", "Exited (1) 99999999999999999999 days ago"} {
		if got := agoOf(status, t0); !got.IsZero() {
			t.Errorf("agoOf(%q) = %v, want zero", status, got)
		}
	}
}
