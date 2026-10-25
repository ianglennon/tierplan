// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// driftWait is how long the background drift worker gets to see an outside change and store it: its debounce is a
// few seconds and a check reads the swarm, so a minute is far past what it needs. It is a deadline, never a pause.
const driftWait = 2 * time.Minute

// driftCause is a cause as the drift route answers it.
type driftCause struct {
	Action  string `json:"action"`
	Service string `json:"service"`
	By      string `json:"by"`
	At      string `json:"at"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// driftAnswer is the part of the drift route's answer these tests read.
type driftAnswer struct {
	State    string      `json:"state"`
	Revision int64       `json:"revision"`
	Count    int         `json:"count"`
	Causes   int         `json:"causes"`
	Cause    *driftCause `json:"cause"`
	Services []struct {
		Name          string         `json:"name"`
		Short         string         `json:"short"`
		Change        string         `json:"change"`
		Current       map[string]any `json:"current"`
		Next          map[string]any `json:"next"`
		HiddenChanges []string       `json:"hiddenChanges"`
		Settings      int            `json:"settings"`
		Cause         *driftCause    `json:"cause"`
	} `json:"services"`
}

// service is the drifted service of the answer by its compose-file name, nil when it is not listed.
func (a driftAnswer) service(short string) *struct {
	Name          string         `json:"name"`
	Short         string         `json:"short"`
	Change        string         `json:"change"`
	Current       map[string]any `json:"current"`
	Next          map[string]any `json:"next"`
	HiddenChanges []string       `json:"hiddenChanges"`
	Settings      int            `json:"settings"`
	Cause         *driftCause    `json:"cause"`
} {
	for i := range a.Services {
		if a.Services[i].Short == short {
			return &a.Services[i]
		}
	}
	return nil
}

// names are the compose-file names of the drifted services, sorted.
func (a driftAnswer) names() []string {
	out := make([]string, 0, len(a.Services))
	for _, s := range a.Services {
		out = append(out, s.Short)
	}
	slices.Sort(out)
	return out
}

// driftOf reads the stack's drift through the API. With fresh the swarm is read again; without it the newest check
// the background worker or a person stored is answered.
func driftOf(t *testing.T, f *swarmFix, stackID string, fresh bool) driftAnswer {
	t.Helper()
	path := f.stackAPI() + "/" + stackID + "/drift"
	if fresh {
		path += "?fresh=true"
	}
	code, body := f.srv.Do(http.MethodGet, path, nil)
	var a driftAnswer
	if code != http.StatusOK || json.Unmarshal(body, &a) != nil {
		t.Fatalf("GET the drift of stack %s (fresh %v) = %d %s", stackID, fresh, code, body)
	}
	return a
}

// listedDrift is the drift state the environment's stack list shows for the stack, "" when the row carries none.
// The list answers the newest check in the drift store and reads no engine for it, so only the background worker (or
// an earlier check) can have put it there.
func listedDrift(f *swarmFix, stackID string) (state string, seen string) {
	code, body := f.srv.Do(http.MethodGet, f.stackAPI(), nil)
	var list struct {
		Items []struct {
			ID    string `json:"id"`
			Drift *struct {
				State string `json:"state"`
				Count int    `json:"count"`
			} `json:"drift"`
		} `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		return "", fmt.Sprintf("GET the stack list = %d %s", code, body)
	}
	for _, it := range list.Items {
		if it.ID != stackID {
			continue
		}
		if it.Drift == nil {
			return "", "the row has no drift summary yet"
		}
		return it.Drift.State, fmt.Sprintf("the row reads %s with %d settings", it.Drift.State, it.Drift.Count)
	}
	return "", "the stack is not in the list: " + string(body)
}

// waitListedDrift waits, without asking for a fresh check, until the stack list shows the drift state.
func waitListedDrift(t *testing.T, f *swarmFix, stackID, want string) {
	t.Helper()
	testsupport.Poll(t, driftWait, "the background worker to store drift "+want+" for the list", func() (bool, string) {
		state, seen := listedDrift(f, stackID)
		return state == want, seen
	})
}

// replicasOf reads the replica count out of a spec tree of the drift route.
func replicasOf(spec map[string]any) any {
	mode, _ := spec["Mode"].(map[string]any)
	rep, _ := mode["Replicated"].(map[string]any)
	return rep["Replicas"]
}

// envOf flattens the environment of a spec tree to "NAME=value" words, whatever form the tree keeps them in.
func envOf(spec map[string]any) []string {
	tt, _ := spec["TaskTemplate"].(map[string]any)
	cs, _ := tt["ContainerSpec"].(map[string]any)
	var out []string
	switch env := cs["Env"].(type) {
	case []any:
		for _, e := range env {
			out = append(out, fmt.Sprint(e))
		}
	case map[string]any:
		for k, v := range env {
			out = append(out, k+"="+fmt.Sprint(v))
		}
	}
	return out
}

// sharedStackDrift is the real-engine oracle of drift (STK-14, D-21 to D-24, ROADMAP SC4, RESEARCH assumption A5): a
// stack deployed through Bayplan is changed on a manager with the docker CLI, and
//
//   - a force update, which only bumps the counter that makes new tasks, is not drift;
//   - a scale and an added environment variable are, and the background worker stores them for the list without
//     anyone opening the stack, off the service events of the engine;
//   - the drift lists the services with the replicas and the environment as what the revision deployed against what
//     runs, with no cause, since Bayplan did none of it;
//   - detecting drift, reviewing it and accepting it change no service: only a person's restore does (the
//     prohibition of STK-14);
//   - a restore puts the deployed revision back and the drift clears;
//   - a scale from Bayplan's own service route is drift whose cause names the user, with the replica counts;
//   - accepting what runs writes the new replica count into the draft, and deploying that draft records a revision
//     with no drift left.
//
// The service events of a real engine carry only the service's name, not the stack's namespace (RESEARCH assumption A5
// does not hold), so this also proves the worker's lookup of the stack by service name.
func sharedStackDrift(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	ref := addr + "/drift/app:1"
	dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, ref, nil)
	const stack, web, api = "drift1", "drift1_web", "drift1_api"

	service := func(name string) string {
		return "  " + name + ":\n    image: " + ref + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n    deploy:\n      replicas: 1\n"
	}
	id := f.createStackBayplan(t, stack, "services:\n"+service("web")+service("api"))
	// Cleanups run last in, first out: Bayplan removes the stack; the docker CLI only sweeps up what a failure left.
	t.Cleanup(func() { removeStackCLI(t, f, stack) })
	t.Cleanup(func() { removeStackBayplan(t, f, id, stack, engineObjects{}) })
	f.deployStackBayplan(t, id)
	waitStackConverged(t, f, stack)

	if a := driftOf(t, f, id, true); a.State != "none" || a.Revision != 1 || len(a.Services) != 0 {
		t.Fatalf("drift right after the deploy = %+v, want none at revision 1", a)
	}

	// A force update bumps the counter that makes new tasks and writes a new version of the spec. It is not drift.
	sh(t, mgr, "docker service update --force --quiet "+web)
	waitStackConverged(t, f, stack)
	if a := driftOf(t, f, id, true); a.State != "none" || len(a.Services) != 0 {
		t.Fatalf("drift after docker service update --force = %+v, want none: a new task counter is not drift", a)
	}

	// An outside scale. Nobody asks for a fresh check from here until the list has its answer: the worker, woken by
	// the service event, has to find it.
	sh(t, mgr, "docker service scale --detach=false "+web+"=3")
	waitListedDrift(t, f, id, "drifted")

	a := driftOf(t, f, id, false)
	if a.State != "drifted" || a.Revision != 1 {
		t.Fatalf("drift after docker service scale = %+v, want drifted against revision 1", a)
	}
	if got := a.names(); !slices.Equal(got, []string{"web"}) {
		t.Fatalf("the drift lists %v, want only web", got)
	}
	w := a.service("web")
	if w.Name != web || w.Change != "changed" || w.Settings < 1 || w.Current == nil || w.Next == nil {
		t.Errorf("web in the drift = %+v, want drift1_web changed with both sides", w)
	}
	// The drift reads from what the revision deployed (current) to what runs now (next).
	if replicasOf(w.Current) != float64(1) || replicasOf(w.Next) != float64(3) {
		t.Errorf("web replicas = %v then %v, want the deployed 1 as current and the running 3 as next", replicasOf(w.Current), replicasOf(w.Next))
	}
	if w.Cause != nil || a.Cause != nil || a.Causes != 0 {
		t.Errorf("an outside scale has the cause %+v / %+v (%d), want none: Bayplan did not do it", w.Cause, a.Cause, a.Causes)
	}

	// An outside environment variable on the other service, seen by the worker the same way.
	sh(t, mgr, "docker service update --env-add X=1 --quiet "+api)
	waitStackConverged(t, f, stack)
	testsupport.Poll(t, driftWait, "the background worker to store the environment change of api", func() (bool, string) {
		got := driftOf(t, f, id, false)
		return got.service("api") != nil, fmt.Sprintf("%s %v", got.State, got.names())
	})
	a = driftOf(t, f, id, false)
	if got := a.names(); !slices.Equal(got, []string{"api", "web"}) {
		t.Fatalf("the drift lists %v, want api and web", got)
	}
	if ap := a.service("api"); ap.Name != api || ap.Change != "changed" || ap.Cause != nil {
		t.Errorf("api in the drift = %+v, want drift1_api changed with no cause", ap)
	} else if !slices.Contains(envOf(ap.Next), "X=1") && !slices.Contains(ap.HiddenChanges, "TaskTemplate.ContainerSpec.Env[X]") {
		t.Errorf("api in the drift has the environment %v and hidden changes %v, want the added X=1 or its path", envOf(ap.Next), ap.HiddenChanges)
	} else if slices.Contains(envOf(ap.Current), "X=1") {
		t.Errorf("api in the drift deployed with the environment %v, want it without X", envOf(ap.Current))
	}
	if w := a.service("web"); w == nil || replicasOf(w.Next) != float64(3) {
		t.Errorf("web in the drift = %+v, want it still scaled to 3", w)
	}

	// The prohibition: finding drift changed nothing. The versions are read now, with both outside changes in, and
	// must be the same after the reviews and the accepting below, up to the person's restore.
	versionsOutside, err := serviceVersions(execCtx(t, time.Minute), mgr, stack)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(when string) {
		t.Helper()
		now, err := serviceVersions(execCtx(t, time.Minute), mgr, stack)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(now, versionsOutside) {
			t.Errorf("a service was written %s: versions were %v, are %v", when, versionsOutside, now)
		}
	}
	unchanged("after the worker found the drift")

	// The review of a restore lists the drift as what would change.
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/review", map[string]any{"mode": "restore"})
	var rv struct {
		Revision    int64 `json:"revision"`
		DeployedSeq int64 `json:"deployedSeq"`
		Services    []struct {
			Name, Short, Change string
			Current, Next       map[string]any
		} `json:"services"`
		Problems []struct {
			Severity, Code, Message string
		} `json:"problems"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil {
		t.Fatalf("POST the review of a restore = %d %s", code, body)
	}
	for _, p := range rv.Problems {
		if p.Severity == "error" {
			t.Errorf("the restore review found an error: %s (%s)", p.Message, p.Code)
		}
	}
	if rv.Revision != 1 || rv.DeployedSeq != 1 {
		t.Errorf("the restore review is of revision %d against deployed %d, want 1 against 1", rv.Revision, rv.DeployedSeq)
	}
	changed := map[string]bool{}
	for _, s := range rv.Services {
		if s.Change == "changed" {
			changed[s.Short] = true
		}
		if s.Short == "web" && (replicasOf(s.Current) != float64(3) || replicasOf(s.Next) != float64(1)) {
			t.Errorf("the restore review has web from %v replicas to %v, want from the running 3 to the deployed 1", replicasOf(s.Current), replicasOf(s.Next))
		}
	}
	if !changed["web"] || !changed["api"] || len(changed) != 2 {
		t.Errorf("the restore review changes %v, want web and api: the drift", changed)
	}
	unchanged("by the review")

	// The restore, as the review sends it, puts the deployed revision back.
	code, body = f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/rollback", map[string]any{"mode": "restore", "revision": rv.Revision, "expectedDeployed": rv.DeployedSeq})
	var started struct {
		JobID    string `json:"jobId"`
		Revision int64  `json:"revision"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the restore = %d %s", code, body)
	}
	if started.Revision != 2 {
		t.Errorf("the restore took revision %d, want 2", started.Revision)
	}
	waitJob(t, f, id, started.JobID, "restore")
	waitStackConverged(t, f, stack)
	testsupport.Poll(t, driftWait, "a fresh drift check to read none after the restore", func() (bool, string) {
		got := driftOf(t, f, id, true)
		return got.State == "none" && len(got.Services) == 0, fmt.Sprintf("%s %v", got.State, got.names())
	})
	if got := serviceField(t, f, web, "{{.Spec.Mode.Replicated.Replicas}}"); got != "1" {
		t.Errorf("web has %s replicas after the restore, want the deployed 1", got)
	}
	if env := serviceField(t, f, api, "{{json .Spec.TaskTemplate.ContainerSpec.Env}}"); strings.Contains(env, "X=1") {
		t.Errorf("api has the environment %s after the restore, want X=1 gone", env)
	}
	waitListedDrift(t, f, id, "none")

	// A scale from Bayplan's own service route is drift too, and the audit log says who did it.
	f.srv.RefreshCluster(f.env.ID)
	svc, ok := f.service(t, web)
	if !ok {
		t.Fatalf("Bayplan lists no service %s", web)
	}
	code, body = f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/services/scale", map[string]any{"targets": []map[string]any{{"serviceId": svc.ID, "replicas": 2}}})
	if code != http.StatusOK {
		t.Fatalf("POST the scale of %s to 2 = %d %s", web, code, body)
	}
	waitStackConverged(t, f, stack)
	a = driftOf(t, f, id, true)
	if a.State != "drifted" || !slices.Equal(a.names(), []string{"web"}) {
		t.Fatalf("drift after a scale from Bayplan = %+v, want drifted with web alone", a)
	}
	c := a.service("web").Cause
	if c == nil || c.Action != "scaled" || c.Service != "web" || c.From != "1" || c.To != "2" || c.By == "" || c.At == "" {
		t.Errorf("the cause of web = %+v, want it scaled from 1 to 2 by the test user, with the time", c)
	}
	if a.Cause == nil || a.Cause.Action != "scaled" || a.Causes != 1 {
		t.Errorf("the cause of the stack = %+v (%d), want the scale, once", a.Cause, a.Causes)
	}
	if rows := f.srv.AuditRows("service.scaled"); len(rows) != 1 || rows[0].TargetID != svc.ID {
		t.Errorf("service.scaled audit rows = %+v, want one about %s", rows, svc.ID)
	}
	versionsOutside, err = serviceVersions(execCtx(t, time.Minute), mgr, stack)
	if err != nil {
		t.Fatal(err)
	}

	// Accept what runs: the draft is rebuilt from the engine on top of the deployed revision, and nothing is deployed.
	code, body = f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/drift/accept", map[string]any{"draftVersion": f.draftVersion(t, id)})
	var accepted struct {
		Version  int64 `json:"version"`
		Revision int64 `json:"revision"`
		Services int   `json:"services"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &accepted) != nil {
		t.Fatalf("POST drift/accept = %d %s", code, body)
	}
	if accepted.Revision != 2 || accepted.Services != 2 {
		t.Errorf("accept-live = %+v, want a draft from revision 2 rebuilt from 2 services", accepted)
	}
	unchanged("by accepting what runs")
	d := readDraft(t, f, id)
	if !d.IsDraft || d.Version != accepted.Version {
		t.Fatalf("the draft after accept-live = %+v, want a saved draft at version %d", d, accepted.Version)
	}
	var files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(d.Files, &files); err != nil {
		t.Fatalf("read the files of the draft: %v: %s", err, d.Files)
	}
	var compose struct {
		Services map[string]struct {
			Deploy struct {
				Replicas *int `yaml:"replicas"`
			} `yaml:"deploy"`
		} `yaml:"services"`
	}
	found := false
	for _, file := range files {
		if file.Path != "compose.yaml" {
			continue
		}
		found = true
		if err := yaml.Unmarshal([]byte(file.Content), &compose); err != nil {
			t.Fatalf("parse the draft's compose.yaml: %v\n%s", err, file.Content)
		}
		if r := compose.Services["web"].Deploy.Replicas; r == nil || *r != 2 {
			t.Errorf("the draft's web has replicas %v, want 2 (what runs now):\n%s", r, file.Content)
		}
		// One replica is the default, which the rebuilt file leaves out.
		if r := compose.Services["api"].Deploy.Replicas; r != nil && *r != 1 {
			t.Errorf("the draft's api has replicas %v, want 1 or none:\n%s", *r, file.Content)
		}
	}
	if !found {
		t.Fatalf("the draft after accept-live has no compose.yaml: %s", d.Files)
	}

	// Deploying that draft records a revision, and nothing is left to drift from.
	f.deployStackBayplan(t, id)
	waitStackConverged(t, f, stack)
	testsupport.Poll(t, driftWait, "a fresh drift check to read none against the new revision", func() (bool, string) {
		got := driftOf(t, f, id, true)
		return got.State == "none" && got.Revision == 3 && len(got.Services) == 0, fmt.Sprintf("%s at revision %d %v", got.State, got.Revision, got.names())
	})
	if got := serviceField(t, f, web, "{{.Spec.Mode.Replicated.Replicas}}"); got != "2" {
		t.Errorf("web has %s replicas after deploying the accepted draft, want 2", got)
	}
	waitListedDrift(t, f, id, "none")

	code, body = f.srv.Do(http.MethodGet, f.stackAPI()+"/"+id+"/revisions", nil)
	var hist struct {
		Items []struct {
			Seq          int64  `json:"seq"`
			Origin       string `json:"origin"`
			RestoredFrom int64  `json:"restoredFrom"`
			Outcome      string `json:"outcome"`
			Deployed     bool   `json:"deployed"`
		} `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &hist) != nil {
		t.Fatalf("GET the revisions = %d %s", code, body)
	}
	if len(hist.Items) != 3 || hist.Items[0].Seq != 3 {
		t.Fatalf("History lists %+v, want revisions 3, 2 and 1 newest first", hist.Items)
	}
	if r := hist.Items[0]; r.Origin != "deploy" || r.Outcome != "good" || !r.Deployed {
		t.Errorf("revision #3 = %+v, want a good deploy, deployed", r)
	}
	if r := hist.Items[1]; r.Origin != "rollback" || r.RestoredFrom != 1 || r.Outcome != "good" || r.Deployed {
		t.Errorf("revision #2 = %+v, want the good restore of #1, no longer the deployed one", r)
	}
}
