// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// TestSwarmServiceLogsMerged follows the log of a service of three tasks on a real swarm through the agents
// (plan 06-37, OBS-02, D-34, D-35, D-37, RESEARCH E1, E4, E5): the lines of all three tasks come on one stream
// in time order, the interleaving being real. A container killed on its node ends its task; the stream marks the
// slot as replaced and, on the same connection and with no reopen, carries the replacement's lines (E4), the
// first of which are later than the last of the task that died. The page of earlier lines and the download are
// in time order too, and the download holds the lines of the replaced task as well as the new one's (E5).
func TestSwarmServiceLogsMerged(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	mgr := f.sw.Managers[0]
	dockertest.LoadImage(t, dockertest.ServiceTestImage, f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	base := "/api/env/" + f.env.ID

	id := dockertest.CreateLoggingService(t, mgr, "logs-merged", 3)
	dockertest.WaitRunningTasks(t, mgr, id, 3, taskWait)
	f.waitService(t, "logs-merged", "to be listed with its three tasks running", time.Minute, func(it svcItem) bool { return it.Running == 3 })

	type logLine struct {
		T    string `json:"t"`
		M    string `json:"m"`
		Task string `json:"task"`
		Slot int    `json:"slot"`
		Node string `json:"node"`
	}
	type marker struct {
		Kind     string `json:"kind"`
		Slot     int    `json:"slot"`
		Old      string `json:"old"`
		OldState string `json:"oldState"`
		OldNode  string `json:"oldNode"`
		New      string `json:"new"`
		Node     string `json:"node"`
	}
	type taskRef struct {
		ID   string `json:"id"`
		Slot int    `json:"slot"`
		Node string `json:"node"`
	}

	var (
		lines    []logLine
		tasks    []taskRef
		killed   taskRef
		replaced marker
		phase    int
		reopens  int
	)
	perTask := func() map[string]int {
		out := map[string]int{}
		for _, l := range lines {
			out[l.Task]++
		}
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	f.readSSE(t, ctx, base+"/services/"+id+"/logs/stream?sid=swarm-logs-merged-0001&tail=5", func(ev sseEvent) bool {
		switch ev.Name {
		case "ready":
			reopens++
			var r struct {
				Tasks []taskRef `json:"tasks"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &r); err != nil {
				t.Fatalf("ready event %q: %v", ev.Data, err)
			}
			tasks = r.Tasks
		case "log":
			var b struct {
				Lines []logLine `json:"lines"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &b); err != nil {
				t.Fatalf("log event %q: %v", ev.Data, err)
			}
			lines = append(lines, b.Lines...)
		case "marker":
			var m marker
			if err := json.Unmarshal([]byte(ev.Data), &m); err != nil {
				t.Fatalf("marker event %q: %v", ev.Data, err)
			}
			if phase == 1 && m.Kind == "replaced" && m.Old == killed.ID {
				replaced, phase = m, 2
			}
		}
		switch phase {
		case 0:
			// Enough lines from all three tasks to say they interleave: now stop one of them from its node.
			counts := perTask()
			if len(tasks) != 3 || len(counts) != 3 || len(lines) < 30 {
				return false
			}
			for _, c := range counts {
				if c < 5 {
					return false
				}
			}
			killed = tasks[0]
			node := f.nodeNamed(t, killed.Node)
			t.Logf("killing the container of task %s (slot %d) on %s", killed.ID, killed.Slot, node.Hostname)
			sh(t, node, "docker kill $(docker ps -q --filter name="+killed.ID+")")
			phase = 1
		case 2:
			// The replacement's first line, on the stream that is still open.
			for _, l := range lines {
				if l.Task == replaced.New {
					return true
				}
			}
		}
		return false
	})
	if reopens != 1 {
		t.Errorf("the stream sent %d ready events, want the one it opened with: the replacement's lines must join without a reopen (RESEARCH E4)", reopens)
	}

	// The whole stream is in time order, and it is no concatenation: the tasks alternate.
	stamps := make([]string, len(lines))
	for i, l := range lines {
		stamps[i] = l.T
	}
	if !slices.IsSorted(stamps) {
		var at int
		for i := 1; i < len(stamps); i++ {
			if stamps[i] < stamps[i-1] {
				at = i
				break
			}
		}
		t.Errorf("the merged stream is not in time order: line %d (%s %s) is earlier than the line before it (%s %s)", at, lines[at].Task[:8], stamps[at], lines[at-1].Task[:8], stamps[at-1])
	}
	switches := 0
	for i := 1; i < len(lines); i++ {
		if lines[i].Task != lines[i-1].Task {
			switches++
		}
	}
	if switches < len(lines)/4 {
		t.Errorf("the stream switches task %d times in %d lines: the tasks are not interleaved in time order", switches, len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l.M, "slot="+strconv.Itoa(l.Slot)+" n=") {
			t.Errorf("a line of slot %d says %q: the task and slot of a line must be the ones that wrote it", l.Slot, l.M)
			break
		}
	}

	// The marker names the dead task and its successor; the successor writes after the dead one stopped.
	if replaced.Kind != "replaced" || replaced.Old != killed.ID || replaced.Slot != killed.Slot || replaced.New == "" || replaced.New == killed.ID || replaced.OldNode != killed.Node {
		t.Fatalf("marker = %+v, want a replaced marker for task %s of slot %d on %s with a new task", replaced, killed.ID, killed.Slot, killed.Node)
	}
	var lastOld, firstNew string
	for _, l := range lines {
		if l.Task == killed.ID {
			lastOld = l.T
		}
		if l.Task == replaced.New && firstNew == "" {
			firstNew = l.T
		}
	}
	if lastOld == "" || firstNew == "" || firstNew <= lastOld {
		t.Errorf("the dead task's last line is at %q and its replacement's first at %q: the replacement must write after it", lastOld, firstNew)
	}

	// A page of earlier lines, oldest first.
	code, body := f.srv.Do(http.MethodGet, base+"/services/"+id+"/logs?sid=swarm-logs-merged-0002&tail=200&stopped=true", nil)
	var page struct {
		Lines []logLine `json:"lines"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &page) != nil || len(page.Lines) < 30 {
		t.Fatalf("GET logs = %d, %d lines: %.300s", code, len(page.Lines), body)
	}
	pageStamps := make([]string, len(page.Lines))
	for i, l := range page.Lines {
		pageStamps[i] = l.T
	}
	if !slices.IsSorted(pageStamps) {
		t.Error("the page of earlier lines is not oldest first")
	}

	// The download is in time order and holds both the task that died and the one that took over.
	code, body = f.srv.Do(http.MethodGet, base+"/services/"+id+"/logs/download", nil)
	if code != http.StatusOK {
		t.Fatalf("GET logs/download = %d %.300s", code, body)
	}
	var times []string
	slots := map[string]bool{}
	for _, row := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		name, rest, _ := strings.Cut(row, " ")
		stamp, msg, _ := strings.Cut(rest, " ")
		if !strings.HasPrefix(name, "logs-merged.") || !strings.Contains(name, "@") || !strings.HasPrefix(msg, "slot=") {
			t.Fatalf("a download line is %q, want the task's name, the time and the text as written", row)
		}
		slots[name] = true
		times = append(times, stamp)
	}
	if len(times) < len(lines)/2 {
		t.Errorf("the download holds %d lines, the stream showed %d", len(times), len(lines))
	}
	if !slices.IsSorted(times) {
		t.Error("the download is not in time order")
	}
	if len(slots) < 3 {
		t.Errorf("the download names only %v: want every task, the replaced one's slot included", slots)
	}
	if first, last := times[0], times[len(times)-1]; !(first <= lastOld && last >= firstNew) {
		t.Errorf("the download runs from %s to %s: want it to hold the dead task's lines (to %s) and the replacement's (from %s)", first, last, lastOld, firstNew)
	}
}
