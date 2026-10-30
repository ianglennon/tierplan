// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
)

// The restart-loop tracker of standalone containers (plan 06-36, RES-13, D-15, D-16,
// RESEARCH Pattern 8 and A12). Like the tracker of services it is pure: the clock is an
// argument, the rows and the restart policies are functions the test supplies, and nothing
// here sleeps.

const (
	clNode  = "node-1"
	clOther = "node-2"
)

// clDie is a die event of the container name on a node that happened ago before rlNow. The
// event ID is unique per call so that two deaths at the same moment are two events.
func clDie(id, name string, ago time.Duration, exit string) dockerx.EventRow {
	at := rlNow.Add(-ago)
	return dockerx.EventRow{
		ID: id, Time: at, TimeNano: at.UnixNano(), Type: "container", Action: "die", ActorID: "cid-" + name, Name: name,
		Attributes: map[string]string{"name": name, "exitCode": exit},
	}
}

// clRows serves the same container rows for every node it knows and none for a node it does
// not.
func clRows(byNode map[string][]dockerx.ContainerRow) func(string) ([]dockerx.ContainerRow, bool) {
	return func(node string) ([]dockerx.ContainerRow, bool) {
		rows, ok := byNode[node]
		return rows, ok
	}
}

func clRow(name string, labels map[string]string) dockerx.ContainerRow {
	return dockerx.ContainerRow{ID: "cid-" + name, Name: name, State: "restarting", Status: "Restarting (3) 2 seconds ago", Labels: labels}
}

// clPolicy answers by container name and counts how often it is asked.
type clPolicy struct {
	byName map[string]string
	asked  []string
}

func (p *clPolicy) get(node string, row dockerx.ContainerRow) (string, bool) {
	p.asked = append(p.asked, node+"/"+row.Name)
	name, ok := p.byName[row.Name]
	return name, ok
}

type clRig struct {
	tr   *ContainerLoopTracker
	rows map[string][]dockerx.ContainerRow
	pol  *clPolicy
}

func newCLRig(rows ...dockerx.ContainerRow) *clRig {
	r := &clRig{tr: NewContainerLoopTracker(), rows: map[string][]dockerx.ContainerRow{clNode: rows}, pol: &clPolicy{byName: map[string]string{}}}
	for _, row := range rows {
		r.pol.byName[row.Name] = "always"
	}
	return r
}

func (r *clRig) observe(at time.Time, events ...dockerx.EventRow) map[string]ContainerLoop {
	return r.tr.Observe(at, []ContainerLoopSource{{NodeID: clNode, Events: events}}, clRows(r.rows), r.pol.get, rlThreshold, rlWindow)
}

func TestContainerLoopFlagsAContainerThatDiesWithANonZeroCodeThresholdTimesInTheWindow(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	two := []dockerx.EventRow{clDie("e1", "crashy", time.Minute, "3"), clDie("e2", "crashy", 2*time.Minute, "3")}
	if got := r.observe(rlNow, two...); len(got) != 0 {
		t.Fatalf("two deaths flagged %+v at a threshold of 3", got)
	}
	got := r.observe(rlNow, append(two, clDie("e3", "crashy", 3*time.Minute, "3"))...)
	loop, ok := got[ContainerLoopKey(clNode, "crashy")]
	if !ok || len(got) != 1 {
		t.Fatalf("loops = %+v, want crashy on node-1 only", got)
	}
	if loop.Count != 3 || loop.NodeID != clNode || loop.Name != "crashy" || loop.ContainerID != "cid-crashy" || loop.Window != rlWindow || !loop.Since.Equal(rlNow) || loop.Muted {
		t.Errorf("loop = %+v, want 3 deaths of cid-crashy on node-1 in a 10 minute window, flagged now and not muted", loop)
	}
}

func TestContainerLoopReportsTheExitCodeOfTheNewestDeath(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	got := r.observe(rlNow, clDie("e1", "crashy", 3*time.Minute, "1"), clDie("e2", "crashy", time.Minute, "137"), clDie("e3", "crashy", 2*time.Minute, "2"))
	loop := got[ContainerLoopKey(clNode, "crashy")]
	if loop.LastExitCode != "137" || !loop.LatestAt.Equal(rlNow.Add(-time.Minute)) {
		t.Fatalf("loop = %+v, want the newest death's exit code 137 and its time", loop)
	}
}

func TestContainerLoopIgnoresCleanExitsAndEverythingThatIsNotAContainerDying(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	start := clDie("s1", "crashy", time.Minute, "3")
	start.Action = "start"
	image := clDie("i1", "crashy", time.Minute, "3")
	image.Type = "image"
	noCode := clDie("n1", "crashy", time.Minute, "3")
	delete(noCode.Attributes, "exitCode")
	got := r.observe(rlNow,
		clDie("c1", "crashy", time.Minute, "0"), clDie("c2", "crashy", 2*time.Minute, "0"), clDie("c3", "crashy", 3*time.Minute, "0"),
		start, image, noCode,
	)
	if len(got) != 0 {
		t.Fatalf("loops = %+v, want none: clean exits, starts, other types and a death with no code are no failures", got)
	}
}

func TestContainerLoopCountsADeathSeenInManyCyclesOnce(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	events := []dockerx.EventRow{clDie("e1", "crashy", time.Minute, "3"), clDie("e2", "crashy", 2*time.Minute, "3")}
	for range 5 {
		if got := r.observe(rlNow, events...); len(got) != 0 {
			t.Fatalf("the same two deaths, read again, flagged %+v", got)
		}
	}
	got := r.observe(rlNow, append(events, clDie("e3", "crashy", 30*time.Second, "3"))...)
	if got[ContainerLoopKey(clNode, "crashy")].Count != 3 {
		t.Fatalf("loops = %+v, want a third distinct death to make 3", got)
	}
}

func TestContainerLoopKeepsADeathThatHasLeftTheFeedUntilItsWindowEnds(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	r.observe(rlNow, clDie("e1", "crashy", time.Minute, "3"), clDie("e2", "crashy", 2*time.Minute, "3"), clDie("e3", "crashy", 3*time.Minute, "3"))
	// The ring forgot the events; the tracker did not.
	got := r.observe(rlNow.Add(30 * time.Second))
	if got[ContainerLoopKey(clNode, "crashy")].Count != 3 {
		t.Fatalf("loops = %+v, want the 3 deaths still counted with the feed empty", got)
	}
}

func TestContainerLoopStaysFlaggedUntilAFullQuietWindowThenClears(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	key := ContainerLoopKey(clNode, "crashy")
	r.observe(rlNow, clDie("e1", "crashy", 9*time.Minute, "3"), clDie("e2", "crashy", 5*time.Minute, "3"), clDie("e3", "crashy", time.Minute, "3"))
	// e1 leaves the window: two deaths are below the threshold, yet the flag holds.
	got := r.observe(rlNow.Add(2 * time.Minute))
	if loop, ok := got[key]; !ok || loop.Count != 2 || !loop.Since.Equal(rlNow) {
		t.Fatalf("loops = %+v, want crashy still flagged with 2 deaths and its first Since", got)
	}
	// A whole window after the newest death, nothing is left.
	if got := r.observe(rlNow.Add(time.Minute + rlWindow)); len(got) != 0 {
		t.Fatalf("a quiet window later = %+v, want cleared", got)
	}
	// And it can be raised again with a new Since.
	later := rlNow.Add(time.Hour)
	events := []dockerx.EventRow{}
	for i := range 3 {
		at := later.Add(-time.Duration(i+1) * time.Minute)
		ev := clDie(fmt.Sprintf("n%d", i), "crashy", 0, "3")
		ev.Time, ev.TimeNano = at, at.UnixNano()
		events = append(events, ev)
	}
	got = r.observe(later, events...)
	if loop := got[key]; loop.Count != 3 || !loop.Since.Equal(later) {
		t.Fatalf("loops = %+v, want a new loop flagged at %v", got, later)
	}
}

func TestContainerLoopKeysAContainerByItsNodeAndName(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	r.rows[clOther] = []dockerx.ContainerRow{clRow("crashy", nil)}
	deaths := func(prefix string) []dockerx.EventRow {
		return []dockerx.EventRow{clDie(prefix+"1", "crashy", time.Minute, "3"), clDie(prefix+"2", "crashy", 2*time.Minute, "3"), clDie(prefix+"3", "crashy", 3*time.Minute, "3")}
	}
	got := r.tr.Observe(rlNow, []ContainerLoopSource{{NodeID: clNode, Events: deaths("a")}, {NodeID: clOther, Events: deaths("a")[:2]}},
		clRows(r.rows), r.pol.get, rlThreshold, rlWindow)
	if len(got) != 1 || got[ContainerLoopKey(clNode, "crashy")].Count != 3 {
		t.Fatalf("loops = %+v, want only node-1's crashy: the same name on another node is another container", got)
	}
}

func TestContainerLoopFollowsAContainerRecreatedUnderTheSameName(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	key := ContainerLoopKey(clNode, "crashy")
	r.observe(rlNow, clDie("e1", "crashy", 2*time.Minute, "3"), clDie("e2", "crashy", 90*time.Second, "3"), clDie("e3", "crashy", time.Minute, "3"))
	// The container is recreated: a new ID, the same name.
	r.rows[clNode] = []dockerx.ContainerRow{{ID: "cid-new", Name: "crashy", State: "restarting"}}
	got := r.observe(rlNow.Add(10 * time.Second))
	if loop, ok := got[key]; !ok || loop.ContainerID != "cid-new" || loop.Count != 3 {
		t.Fatalf("loops = %+v, want the same loop, now on the new container ID", got)
	}
}

func TestContainerLoopForgetsAContainerThatIsGone(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	r.observe(rlNow, clDie("e1", "crashy", 2*time.Minute, "3"), clDie("e2", "crashy", 90*time.Second, "3"), clDie("e3", "crashy", time.Minute, "3"))
	r.rows[clNode] = nil
	if got := r.observe(rlNow.Add(10 * time.Second)); len(got) != 0 {
		t.Fatalf("loops = %+v, want none once the container is not listed", got)
	}
	// It does not come back with its old deaths when a container of that name appears.
	r.rows[clNode] = []dockerx.ContainerRow{clRow("crashy", nil)}
	if got := r.observe(rlNow.Add(20 * time.Second)); len(got) != 0 {
		t.Fatalf("loops = %+v, want a new container to start from nothing", got)
	}
}

func TestContainerLoopNeverFlagsATaskContainerOfASwarmService(t *testing.T) {
	r := newCLRig(clRow("web.1.abcdef", map[string]string{"com.docker.swarm.task.id": "abcdef", "com.docker.swarm.service.name": "web"}))
	got := r.observe(rlNow,
		clDie("e1", "web.1.abcdef", time.Minute, "1"), clDie("e2", "web.1.abcdef", 2*time.Minute, "1"), clDie("e3", "web.1.abcdef", 3*time.Minute, "1"), clDie("e4", "web.1.abcdef", 4*time.Minute, "1"))
	if len(got) != 0 {
		t.Fatalf("loops = %+v, want none: the service of a task container has the restart loop, not the task", got)
	}
	if len(r.pol.asked) != 0 {
		t.Errorf("policy asked for %v, want a task container never inspected", r.pol.asked)
	}
}

func TestContainerLoopOnlyFlagsAContainerWhosePolicyRestartsIt(t *testing.T) {
	r := newCLRig(clRow("oneoff", nil), clRow("manual", nil), clRow("crashy", nil))
	r.pol.byName["oneoff"] = "no"
	r.pol.byName["manual"] = ""
	deaths := func(name string) []dockerx.EventRow {
		return []dockerx.EventRow{clDie(name+"1", name, time.Minute, "1"), clDie(name+"2", name, 2*time.Minute, "1"), clDie(name+"3", name, 3*time.Minute, "1")}
	}
	var events []dockerx.EventRow
	for _, n := range []string{"oneoff", "manual", "crashy"} {
		events = append(events, deaths(n)...)
	}
	got := r.observe(rlNow, events...)
	if len(got) != 1 || got[ContainerLoopKey(clNode, "crashy")].Count != 3 {
		t.Fatalf("loops = %+v, want only crashy: a policy of no, or none, never restarts a container", got)
	}
}

func TestContainerLoopAsksForAPolicyOnlyOfContainersOverTheThreshold(t *testing.T) {
	r := newCLRig(clRow("calm", nil), clRow("twice", nil), clRow("crashy", nil))
	events := []dockerx.EventRow{
		clDie("c1", "calm", time.Minute, "0"),
		clDie("t1", "twice", time.Minute, "1"), clDie("t2", "twice", 2*time.Minute, "1"),
		clDie("x1", "crashy", time.Minute, "1"), clDie("x2", "crashy", 2*time.Minute, "1"), clDie("x3", "crashy", 3*time.Minute, "1"),
	}
	r.observe(rlNow, events...)
	if len(r.pol.asked) != 1 || r.pol.asked[0] != clNode+"/crashy" {
		t.Fatalf("policy asked for %v, want only crashy: the one container over the threshold", r.pol.asked)
	}
	// A flagged container is not asked again.
	r.observe(rlNow.Add(time.Second), events...)
	if len(r.pol.asked) != 1 {
		t.Errorf("policy asked for %v after a second cycle, want the decision kept", r.pol.asked)
	}
}

func TestContainerLoopWaitsForARowOrAPolicyItCannotReadYet(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	key := ContainerLoopKey(clNode, "crashy")
	events := []dockerx.EventRow{clDie("e1", "crashy", time.Minute, "3"), clDie("e2", "crashy", 2*time.Minute, "3"), clDie("e3", "crashy", 3*time.Minute, "3")}

	rows := r.rows
	r.rows = map[string][]dockerx.ContainerRow{} // the node's list cannot be read
	if got := r.observe(rlNow, events...); len(got) != 0 {
		t.Fatalf("loops = %+v, want none while the node's containers cannot be listed", got)
	}
	r.rows = rows
	delete(r.pol.byName, "crashy") // the inspect fails
	if got := r.observe(rlNow, events...); len(got) != 0 {
		t.Fatalf("loops = %+v, want none while the restart policy is unknown", got)
	}
	r.pol.byName["crashy"] = "unless-stopped"
	if got := r.observe(rlNow); got[key].Count != 3 {
		t.Fatalf("loops = %+v, want the loop once the policy is known, from the deaths remembered", got)
	}
	// A flagged loop survives a cycle in which the node cannot be listed.
	r.rows = map[string][]dockerx.ContainerRow{}
	if got := r.observe(rlNow.Add(time.Second)); got[key].Count != 3 {
		t.Fatalf("loops = %+v, want the flag kept while the node cannot be read", got)
	}
}

func TestContainerLoopUsesTheThresholdAndWindowItIsGiven(t *testing.T) {
	r := newCLRig(clRow("crashy", nil))
	events := []dockerx.EventRow{clDie("e1", "crashy", time.Minute, "3"), clDie("e2", "crashy", 2*time.Minute, "3")}
	got := r.tr.Observe(rlNow, []ContainerLoopSource{{NodeID: clNode, Events: events}}, clRows(r.rows), r.pol.get, 2, rlWindow)
	if got[ContainerLoopKey(clNode, "crashy")].Count != 2 {
		t.Fatalf("loops = %+v, want a loop of 2 at a threshold of 2", got)
	}
	short := newCLRig(clRow("crashy", nil))
	if got := short.tr.Observe(rlNow, []ContainerLoopSource{{NodeID: clNode, Events: events}}, clRows(short.rows), short.pol.get, 2, time.Minute+30*time.Second); len(got) != 0 {
		t.Fatalf("loops = %+v, want none: only one death is inside a window of 90 seconds", got)
	}
	def := newCLRig(clRow("crashy", nil))
	three := append(events, clDie("e3", "crashy", 3*time.Minute, "3"))
	if got := def.tr.Observe(rlNow, []ContainerLoopSource{{NodeID: clNode, Events: three}}, clRows(def.rows), def.pol.get, 0, 0); got[ContainerLoopKey(clNode, "crashy")].Count != 3 {
		t.Fatalf("loops = %+v, want the defaults of 3 in 10 minutes for a setting that is not positive", got)
	}
}

// TestContainerLoopTrackerCannotReachDocker is the prohibition of plan 06-36: detection only
// flags. The tracker holds no client and no function, so it has nothing to restart, stop or
// remove a container with (RES-13 safety, T-06-104).
func TestContainerLoopTrackerCannotReachDocker(t *testing.T) {
	var walk func(ty reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(ty reflect.Type, path string, seen map[reflect.Type]bool) {
		if seen[ty] {
			return
		}
		seen[ty] = true
		switch ty.Kind() {
		case reflect.Func, reflect.Interface, reflect.Chan:
			t.Errorf("%s is a %s: the tracker may hold data only", path, ty.Kind())
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(ty.Elem(), path+"[]", seen)
		case reflect.Map:
			walk(ty.Key(), path+"{key}", seen)
			walk(ty.Elem(), path+"{}", seen)
		case reflect.Struct:
			if strings.Contains(ty.PkgPath(), "dockerx") || strings.Contains(ty.PkgPath(), "moby") {
				t.Errorf("%s is %s: the tracker must have no Docker type", path, ty)
			}
			for i := range ty.NumField() {
				f := ty.Field(i)
				if ty == reflect.TypeFor[time.Time]() {
					continue
				}
				walk(f.Type, path+"."+f.Name, seen)
			}
		}
	}
	walk(reflect.TypeFor[ContainerLoopTracker](), "ContainerLoopTracker", map[reflect.Type]bool{})
	// The state it keeps must exist at all, or the walk proves nothing.
	if reflect.TypeFor[ContainerLoopTracker]().NumField() < 2 {
		t.Error("the tracker keeps no state of its own")
	}
}

// Mutes (plan 06-36, D-16, A12): a mute is keyed by node and name.

func TestApplyContainerMutesMarksAFlaggedMutedContainerAndKeepsTheOthersAsTheyWere(t *testing.T) {
	a, b := ContainerLoopKey(clNode, "crashy"), ContainerLoopKey(clNode, "other")
	loops := map[string]ContainerLoop{a: {NodeID: clNode, Name: "crashy", Count: 3}, b: {NodeID: clNode, Name: "other", Count: 4}}
	merged, muted := ApplyContainerMutes(loops, []string{a})
	if !merged[a].Muted || merged[b].Muted || merged[a].Count != 3 {
		t.Fatalf("merged = %+v, want only crashy muted, counts kept", merged)
	}
	if loops[a].Muted {
		t.Error("the tracker's own answer was changed: the merge must return copies")
	}
	if !muted[a] || muted[b] {
		t.Errorf("muted = %v, want crashy", muted)
	}
}

func TestApplyContainerMutesKeepsAMuteOfAContainerThatIsNotLoopingAndNeverCreatesALoop(t *testing.T) {
	key := ContainerLoopKey(clNode, "quiet")
	merged, muted := ApplyContainerMutes(nil, []string{key})
	if len(merged) != 0 || !muted[key] {
		t.Fatalf("merged = %+v, muted = %v, want no loop and the mute listed", merged, muted)
	}
	merged, muted = ApplyContainerMutes(map[string]ContainerLoop{}, nil)
	if merged == nil || muted == nil || len(merged) != 0 || len(muted) != 0 {
		t.Errorf("with nothing muted = %v / %v, want empty sets", merged, muted)
	}
}
