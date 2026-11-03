// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// janitorCompose writes a compose file into the host and runs the real docker compose on it for the
// project, with the arguments given after the file (up -d, stop, run ...). The test fails when the
// CLI does, and the Compose version is logged.
func janitorCompose(t *testing.T, host *dockertest.Node, project, compose, args string) string {
	t.Helper()
	dir := "/tmp/janitor-" + project
	sh(t, host, "mkdir -p "+dir)
	if err := host.WriteFile(execCtx(t, time.Minute), dir+"/compose.yaml", []byte(compose)); err != nil {
		t.Fatal(err)
	}
	return sh(t, host, "docker compose -p "+project+" -f "+dir+"/compose.yaml "+args)
}

// TestJanitorStandaloneIntegration judges and removes containers of a standalone host on a real engine
// (JAN-02, JAN-06, D-05, D-11, D-18, RESEARCH F16). The host holds, beside the agent:
//
//   - lonely: a compose project of one service, brought up and then stopped with docker compose stop
//     (the whole project is one stopped container: reported, someone may start it again)
//   - plainbox: a docker run container that exited (reported until the opt-in)
//   - live: a compose project of two services, one stopped while the other runs (the project exists:
//     never listed, not even as reported)
//   - runner: three one-off containers of docker compose run, of which keep-N leaves the newest two
//   - gonebox: a container labelled with the ID of a TierPlan stack that does not exist and a project
//     with no other container (a leftover of a removed stack)
//
// With the janitor's clock 48 hours on, the preview lists the one-off beyond keep-N and the leftover as
// removable and the lone and plain containers as reported; Clean now removes the first two and nothing else.
func TestJanitorStandaloneIntegration(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dockertest.LoadImage(t, dockertest.ServiceTestImage, host)
	dumpOnFailure(t, host)
	clock := &shiftedClock{}
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway, JanitorClock: clock})

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)
	t.Logf("docker compose %s", strings.TrimSpace(requireComposeCLI(t, host)))

	idOf := func(ref string) string {
		return strings.TrimSpace(sh(t, host, "docker inspect -f '{{.Id}}' "+ref))
	}
	base := dockertest.ServiceTestImage
	longRunning := func(services ...string) string {
		var b strings.Builder
		b.WriteString("services:\n")
		for _, s := range services {
			b.WriteString("  " + s + ":\n    image: " + base + "\n    command: [\"sleep\", \"600\"]\n")
		}
		return b.String()
	}

	janitorCompose(t, host, "lonely", longRunning("app"), "up -d")
	janitorCompose(t, host, "lonely", longRunning("app"), "stop")
	lonely := strings.TrimSpace(sh(t, host, "docker ps -aq --no-trunc --filter label=com.docker.compose.project=lonely"))

	sh(t, host, "docker run --name plainbox "+base+" true")
	plain := idOf("plainbox")

	janitorCompose(t, host, "live", longRunning("a", "b"), "up -d")
	janitorCompose(t, host, "live", longRunning("a", "b"), "stop b")
	live := strings.TrimSpace(sh(t, host, "docker ps -aq --no-trunc --filter label=com.docker.compose.project=live --filter status=exited"))

	oneoffFile := "services:\n  job:\n    image: " + base + "\n    command: [\"true\"]\n"
	for range 3 {
		janitorCompose(t, host, "runner", oneoffFile, "run --no-deps -T job")
	}
	oneoffs := strings.Fields(sh(t, host, "docker ps -aq --no-trunc --filter label=com.docker.compose.project=runner --filter label=com.docker.compose.oneoff=True"))

	sh(t, host, "docker run --name gonebox --label tierplan.stack.id=stk-gone --label com.docker.compose.project=gone --label com.docker.compose.service=web "+base+" true")
	gone := idOf("gonebox")

	for name, id := range map[string]string{"lonely": lonely, "live": live} {
		if id == "" || strings.ContainsAny(id, " \n") {
			t.Fatalf("%s: expected exactly one container, found %q", name, id)
		}
	}
	if len(oneoffs) != 3 {
		t.Fatalf("one-offs of runner = %v, want three", oneoffs)
	}

	preview := func() janitorPreviewRead {
		t.Helper()
		code, body := srv.Do(http.MethodPost, "/api/env/"+env.ID+"/janitor/preview", nil)
		if code != http.StatusOK {
			t.Fatalf("preview: %d %s", code, body)
		}
		var p janitorPreviewRead
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return p
	}
	find := func(items []janitorItemRead, ref string) (janitorItemRead, bool) {
		for _, it := range items {
			if it.Ref == ref {
				return it, true
			}
		}
		return janitorItemRead{}, false
	}

	// Inside the grace period nothing of this is removable, and what qualifies is held by grace.
	before := preview()
	for name, id := range map[string]string{"lonely": lonely, "plain": plain, "gone": gone} {
		if _, ok := find(before.Items, id); ok {
			t.Errorf("%s (%s) is removable inside the grace period", name, id)
		}
		if it, ok := find(before.NotRemoved, id); !ok || it.Class != "grace" || it.Reason.Code != "younger_than_grace" {
			t.Errorf("%s (%s) = %+v in %+v, want it held by grace", name, id, it, before.NotRemoved)
		}
	}
	for _, id := range oneoffs {
		if _, ok := find(before.Items, id); ok {
			t.Errorf("the one-off %s is removable inside the grace period", id)
		}
	}

	clock.by.Store(int64(48 * time.Hour))
	after := preview()
	// The project that still has a running container is nobody's leftover: not listed in any way.
	if _, ok := find(append(after.Items, after.NotRemoved...), live); ok {
		t.Errorf("the stopped container of a project that is still running (%s) was listed", live)
	}
	if it, ok := find(after.NotRemoved, lonely); !ok || it.Class != "reported" || it.Reason.Code != "lone_compose_container" || it.Type != "container" {
		t.Errorf("lonely = %+v, want reported lone_compose_container under notRemoved %+v", it, after.NotRemoved)
	}
	if it, ok := find(after.NotRemoved, plain); !ok || it.Class != "reported" || it.Reason.Code != "opt_in_off_containers" {
		t.Errorf("plainbox = %+v, want reported opt_in_off_containers under notRemoved %+v", it, after.NotRemoved)
	}
	for _, id := range []string{lonely, plain} {
		if _, ok := find(after.Items, id); ok {
			t.Errorf("%s is removable, but a stopped container someone may restart is only reported", id)
		}
	}
	if it, ok := find(after.Items, gone); !ok || it.Class != "removable" || it.Reason.Code != "stack_gone" {
		t.Errorf("gonebox = %+v, want removable stack_gone in %+v", it, after.Items)
	}
	var removableOneoffs []janitorItemRead
	for _, id := range oneoffs {
		if it, ok := find(after.Items, id); ok {
			removableOneoffs = append(removableOneoffs, it)
		}
		if it, ok := find(after.NotRemoved, id); ok {
			t.Errorf("the one-off %s = %+v is under notRemoved: keep-N leaves the newest ones out of the plan", id, it)
		}
	}
	if len(removableOneoffs) != 1 || removableOneoffs[0].Class != "removable" || removableOneoffs[0].Reason.Code != "compose_oneoff" {
		t.Fatalf("removable one-offs = %+v, want exactly one (the oldest of three, keep 2)", removableOneoffs)
	}

	// Clean now removes the one-off and the leftover, ticked, and nothing else.
	code, body := srv.Do(http.MethodPost, "/api/env/"+env.ID+"/janitor/runs", map[string]any{
		"items": []map[string]string{
			{"type": "container", "nodeId": removableOneoffs[0].NodeID, "ref": removableOneoffs[0].Ref},
			{"type": "container", "nodeId": removableOneoffs[0].NodeID, "ref": gone},
		},
	})
	if code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", code, body)
	}
	var started struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	var run struct {
		State  string `json:"state"`
		Counts struct {
			RemovedContainers int `json:"removedContainers"`
			Failed            int `json:"failed"`
		} `json:"counts"`
	}
	srv.WaitFor("the janitor run to end", 2*time.Minute, func() (bool, string) {
		code, body := srv.Do(http.MethodGet, "/api/env/"+env.ID+"/janitor/runs/"+started.RunID, nil)
		if code != http.StatusOK || json.Unmarshal(body, &run) != nil {
			return false, string(body)
		}
		return run.State != "running" && run.State != "waiting", string(body)
	})
	if run.State != "completed" || run.Counts.RemovedContainers != 2 || run.Counts.Failed != 0 {
		t.Fatalf("run = %+v, want completed with two containers removed", run)
	}

	exists := func(id string) bool {
		_, err := host.Sh(execCtx(t, time.Minute), "docker container inspect "+id)
		return err == nil
	}
	for name, id := range map[string]string{"the removable one-off": removableOneoffs[0].Ref, "the leftover of the removed stack": gone} {
		if exists(id) {
			t.Errorf("%s (%s) is still on the host", name, id)
		}
	}
	survivors := map[string]string{"lonely": lonely, "plainbox": plain, "the stopped container of the running project": live}
	for _, id := range oneoffs {
		if id != removableOneoffs[0].Ref {
			survivors["the one-off "+id[:12]] = id
		}
	}
	for name, id := range survivors {
		if !exists(id) {
			t.Errorf("%s (%s) was removed", name, id)
		}
	}
}
