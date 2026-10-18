// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// sharedRestartLoop runs a service whose task exits 1 a second after it starts, for ever, on a real swarm
// (plan 06-37, RES-13, D-14, D-15, D-16): with the threshold set through the settings route to 3 failures in one
// minute, the service is flagged within two cluster reads of the third failed task being there, the services list
// and the bell both say so, a mute takes the bell item away and leaves the flag, an unmute brings it back, and
// once the service is scaled to 0 and a whole quiet minute has passed the flag clears. Nothing is slept for: every
// wait is a poll of what the swarm and the server say.
func sharedRestartLoop(t *testing.T, f *swarmFix) {
	mgr := f.sw.Managers[0]
	dockertest.LoadImage(t, dockertest.ServiceTestImage, f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	base := "/api/env/" + f.env.ID

	// The threshold, through the route, for speed: 3 failed tasks within one minute instead of ten.
	code, body := f.srv.Do(http.MethodPut, base+"/restart-loop-settings", map[string]int{"failures": 3, "windowMinutes": 1})
	var settings struct {
		Failures      int `json:"failures"`
		WindowMinutes int `json:"windowMinutes"`
		HistoryLimit  int `json:"historyLimit"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &settings) != nil || settings.Failures != 3 || settings.WindowMinutes != 1 {
		t.Fatalf("PUT restart-loop-settings = %d %s, want 3 in 1 minute", code, body)
	}

	id := dockertest.CreateCrashLoopService(t, mgr, "loop-crash")

	// Failed tasks pile up in the swarm; the third is the crossing.
	failed := func() int {
		out, err := mgr.Sh(execCtx(t, time.Minute), "docker service ps --no-trunc --format '{{.CurrentState}}' "+id)
		if err != nil {
			return 0
		}
		n := 0
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.HasPrefix(l, "Failed") || strings.HasPrefix(l, "Rejected") {
				n++
			}
		}
		return n
	}
	dockertest.Wait(t, 3*time.Minute, "the swarm to hold three failed tasks of the crashing service", func() (bool, string) {
		n := failed()
		return n >= 3, "failed tasks: " + strconv.Itoa(n)
	})
	crossed := time.Now()

	// Flagged within two cluster reads of that.
	var item svcItem
	reads := 0
	for ; reads < 2; reads++ {
		f.srv.RefreshCluster(f.env.ID)
		var ok bool
		if item, ok = f.service(t, "loop-crash"); ok && item.RestartLoop != nil {
			break
		}
	}
	if item.RestartLoop == nil {
		t.Fatalf("after %d cluster reads past the third failure the service is %+v, want it flagged", reads, item)
	}
	t.Logf("flagged %s after the third failed task, on read %d", time.Since(crossed).Round(time.Second), reads+1)
	if item.RestartLoop.State != "flagged" || item.RestartLoop.Count < 3 {
		t.Errorf("restart loop = %+v, want flagged with at least the 3 failures of the threshold", item.RestartLoop)
	}
	if items := f.servicesWithStatus(t, "loop"); len(items) != 1 || items[0].ID != id {
		t.Errorf("GET services?status=loop = %+v, want the crashing service", items)
	}
	if w, ok := f.loopWarning(t, id); !ok || w.Service != "loop-crash" || w.Count < 3 || w.WindowMinutes != 1 {
		t.Errorf("bell item = %+v (%v), want a restart_loop item naming loop-crash with its count and the 1-minute window", w, ok)
	}

	// Mute: the flag stays and the bell item goes (D-16).
	code, body = f.srv.Do(http.MethodPut, base+"/services/"+id+"/restart-loop-mute", map[string]any{})
	var muted struct {
		Muted   bool `json:"muted"`
		Changed bool `json:"changed"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &muted) != nil || !muted.Muted || !muted.Changed {
		t.Fatalf("PUT restart-loop-mute = %d %s, want muted and changed", code, body)
	}
	f.waitService(t, "loop-crash", "to show its restart loop as muted", time.Minute, func(it svcItem) bool { return it.RestartLoop != nil && it.RestartLoop.State == "muted" })
	if w, ok := f.loopWarning(t, id); ok {
		t.Errorf("bell item %+v after the mute, want none", w)
	}
	// Muting again changes nothing.
	if code, body = f.srv.Do(http.MethodPut, base+"/services/"+id+"/restart-loop-mute", map[string]any{}); code != http.StatusOK || !strings.Contains(string(body), `"changed":false`) {
		t.Errorf("a second mute = %d %s, want changed false", code, body)
	}

	// Unmute: flagged again, the bell item back.
	if code, body = f.srv.Do(http.MethodDelete, base+"/services/"+id+"/restart-loop-mute", nil); code != http.StatusOK {
		t.Fatalf("DELETE restart-loop-mute = %d %s", code, body)
	}
	f.waitService(t, "loop-crash", "to show its restart loop as flagged again", time.Minute, func(it svcItem) bool { return it.RestartLoop != nil && it.RestartLoop.State == "flagged" })
	if _, ok := f.loopWarning(t, id); !ok {
		t.Error("no bell item after the unmute")
	}

	// Stop the failing: scale to 0 through the route. The flag holds until a whole window passes with no new failure.
	code, body = f.srv.Do(http.MethodPost, base+"/services/scale", map[string]any{"targets": []map[string]any{{"serviceId": id, "replicas": 0}}})
	if code != http.StatusOK || !strings.Contains(string(body), `"result":"done"`) {
		t.Fatalf("POST scale to 0 = %d %s", code, body)
	}
	stopped := time.Now()
	f.waitService(t, "loop-crash", "to be listed with no task wanted", time.Minute, func(it svcItem) bool { return it.Desired == 0 })
	f.waitService(t, "loop-crash", "to clear its restart loop after a quiet minute", 4*time.Minute, func(it svcItem) bool { return it.RestartLoop == nil })
	if quiet := time.Since(stopped); quiet < 30*time.Second {
		t.Errorf("the flag cleared %s after the last failure, before the window of one minute could have passed", quiet.Round(time.Second))
	}
	if w, ok := f.loopWarning(t, id); ok {
		t.Errorf("bell item %+v after the loop cleared, want none", w)
	}
	if items := f.servicesWithStatus(t, "loop"); len(items) != 0 {
		t.Errorf("GET services?status=loop = %+v after it cleared, want none", items)
	}
}

// servicesWithStatus is the services list narrowed to a status chip.
func (f *swarmFix) servicesWithStatus(t *testing.T, status string) []svcItem {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/services?status="+status, nil)
	var list struct {
		Items []svcItem `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET services?status=%s = %d %s", status, code, body)
	}
	return list.Items
}

// loopWarning is the restart_loop item of the header bell for a service, when it has one.
type loopWarning struct {
	ID            string `json:"id"`
	EnvID         string `json:"envId"`
	Service       string `json:"service"`
	ServiceID     string `json:"serviceId"`
	Count         int    `json:"count"`
	WindowMinutes int    `json:"windowMinutes"`
	LatestError   string `json:"latestError"`
}

func (f *swarmFix) loopWarning(t *testing.T, serviceID string) (loopWarning, bool) {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/environments/warnings", nil)
	var list struct {
		Items []loopWarning `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET environments/warnings = %d %s", code, body)
	}
	for _, w := range list.Items {
		if w.ID == "restart_loop" && w.ServiceID == serviceID && w.EnvID == f.env.ID {
			return w, true
		}
	}
	return loopWarning{}, false
}
