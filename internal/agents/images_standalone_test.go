// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/server/servertest"
)

// TestStandaloneAgentImagePull pulls an image on a standalone host through a real
// agent built from this branch (plan 05-03, RES-09): the pull crosses the tunnel and
// the agent's route table, the host's engine fetches the image from a registry inside
// the test network, and the job ends Done with the digest that registry serves. A
// second pull of the same reference finds the image there already.
func TestStandaloneAgentImagePull(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})

	registry := dockertest.StartRegistry(t, host, dockertest.RegistryOptions{})
	ref := registry + "/public/busybox:1"
	pushed := dockertest.PushImage(t, host, "busybox:1.36.1", ref, nil)
	// The pull must fetch the image, not find it: drop the copy the push left.
	sh(t, host, "docker image rm "+ref+" busybox:1.36.1")

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)
	if agents := srv.Agents(env.ID); len(agents) != 1 {
		t.Fatalf("agents = %+v, want the one that enrolled", agents)
	}

	job := pullAndWait(t, srv, env.ID, ref, nil)
	if job.State != "done" || len(job.Items) != 1 {
		t.Fatalf("job = %+v, want done with one item", job)
	}
	it := job.Items[0]
	if it.State != "done" || it.UpToDate || !strings.HasPrefix(it.Digest, "sha256:") {
		t.Fatalf("item = %+v, want done, not up to date, with a sha256 digest", it)
	}
	if it.Digest != pushed {
		t.Errorf("digest = %s, want the one the registry served, %s", it.Digest, pushed)
	}
	if got := strings.TrimSpace(sh(t, host, "docker image inspect -f '{{.Id}}' "+ref)); !strings.HasPrefix(got, "sha256:") {
		t.Errorf("the host does not have %s after the pull: %q", ref, got)
	}
	rows := srv.AuditRows("image.pulled")
	if len(rows) != 1 || rows[0].Result != "success" || rows[0].Details["digest"] != pushed || rows[0].Details["bulk"] != job.ID {
		t.Fatalf("audit rows = %+v, want one success carrying the digest and the job ID", rows)
	}

	again := pullAndWait(t, srv, env.ID, ref, nil)
	if again.State != "done" || len(again.Items) != 1 || !again.Items[0].UpToDate {
		t.Fatalf("second pull = %+v, want done and up to date", again)
	}
	if rows := srv.AuditRows("image.pulled"); len(rows) != 2 {
		t.Errorf("audit rows after the second pull = %d, want 2", len(rows))
	}
}

// TestStandaloneAgentPrivatePull pulls an image from a registry that requires a sign-in, on a
// standalone host through a real agent (plan 05-09, IMG-02, D-14, D-25): the stored instance
// credential is sent with the pull, the host is never logged in and holds no Docker client
// configuration afterwards, and a wrong password ends the item pull_unauthorized with the
// credential's failure recorded, then a corrected one pulls again and clears it. It runs on the
// engine BAYPLAN_TEST_ENGINE names, so the same test covers Engine 24 where a refusal can
// arrive inside the progress stream instead of as an HTTP status.
func TestStandaloneAgentPrivatePull(t *testing.T) {
	const user, password = "bot", "registry-pass-0123456789"
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})

	addr := dockertest.StartRegistry(t, host, dockertest.RegistryOptions{Htpasswd: dockertest.HtpasswdFor(user, password)})
	ref := addr + "/private/busybox:1"
	pushed := dockertest.PushImage(t, host, "busybox:1.36.1", ref, &dockertest.Login{User: user, Password: password})
	// Signed in for the push only. From here on the host knows no credential, and the pull must
	// fetch the image rather than find it.
	dockertest.Logout(t, host)
	sh(t, host, "docker image rm "+ref+" busybox:1.36.1")

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)

	creds := registry.NewStore(srv.Core().Store(), srv.Core().Vault(), nil)
	ctx := context.Background()
	cred, err := creds.Create(ctx, registry.CreateInput{
		Name: "private-bot", Type: registry.TypeGeneric, Host: addr, Prefix: "private", Username: user, Secret: "wrong-" + password,
	}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	failureOf := func() *registry.LastFailure {
		c, err := creds.Get(ctx, cred.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.LastFailure
	}

	// A wrong password: refused, recorded, and not retried.
	job := pullAndWait(t, srv, env.ID, ref, nil)
	if job.State != "failed" || len(job.Items) != 1 || job.Items[0].Code != "pull_unauthorized" {
		t.Fatalf("job with a wrong password = %+v, want failed with pull_unauthorized", job)
	}
	if f := failureOf(); f == nil || f.Code != "pull_unauthorized" {
		t.Fatalf("credential failure = %+v, want pull_unauthorized", f)
	}
	rows := srv.AuditRows("image.pulled")
	if len(rows) != 1 || rows[0].Result != "failure" || rows[0].Details["credential"] != "private-bot" {
		t.Fatalf("audit rows = %+v, want one failure naming the credential", rows)
	}

	// The right password: pulls, clears the failure, and the host was never logged in.
	if _, _, err := creds.Update(ctx, cred.ID, registry.UpdateInput{
		Name: cred.Name, Type: cred.Type, Host: cred.Host, Prefix: cred.Prefix, Username: user, Secret: password,
	}, nil); err != nil {
		t.Fatal(err)
	}
	job = pullAndWait(t, srv, env.ID, ref, nil)
	if job.State != "done" || len(job.Items) != 1 || job.Items[0].State != "done" || job.Items[0].Digest != pushed {
		t.Fatalf("job = %+v, want done with the digest %s", job, pushed)
	}
	if f := failureOf(); f != nil {
		t.Errorf("credential failure = %+v, want it cleared by a pull that worked", f)
	}
	if got := strings.TrimSpace(sh(t, host, "docker image inspect -f '{{.Id}}' "+ref)); !strings.HasPrefix(got, "sha256:") {
		t.Errorf("the host does not have %s after the pull: %q", ref, got)
	}
	if dockertest.HasClientConfig(t, host) {
		t.Errorf("the host has a Docker client configuration after the pull: Bayplan must not log in")
	}
	rows = srv.AuditRows("image.pulled")
	if len(rows) != 2 || rows[1].Result != "success" || rows[1].Details["credential"] != "private-bot" {
		t.Fatalf("audit rows = %+v, want the success naming the credential", rows)
	}
}

// TestStandaloneAgentTestFromEnvironment checks a credential from a standalone host through a real
// agent against a registry that requires a sign-in (plan 05-13, IMG-01, D-13): the host's engine
// reads the manifest with the credential over the tunnel, the answer names the host, a wrong
// password is pull_unauthorized and a repository the registry does not have is image_not_found,
// and the host holds no Docker client configuration afterwards.
func TestStandaloneAgentTestFromEnvironment(t *testing.T) {
	const user, password = "bot", "registry-pass-0123456789"
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})

	addr := dockertest.StartRegistry(t, host, dockertest.RegistryOptions{Htpasswd: dockertest.HtpasswdFor(user, password)})
	ref := addr + "/private/busybox:1"
	pushed := dockertest.PushImage(t, host, "busybox:1.36.1", ref, &dockertest.Login{User: user, Password: password})
	dockertest.Logout(t, host)

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)
	agents := srv.Agents(env.ID)
	if len(agents) != 1 {
		t.Fatalf("agents = %+v, want the one that enrolled", agents)
	}

	cred, err := registry.NewStore(srv.Core().Store(), srv.Core().Vault(), nil).Create(context.Background(), registry.CreateInput{
		Name: "private-bot", Type: registry.TypeGeneric, Host: addr, Prefix: "private", Username: user, Secret: password,
	}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		Node       string `json:"node"`
		NodeID     string `json:"nodeId"`
		State      string `json:"state"`
		Code       string `json:"code"`
		Digest     string `json:"digest"`
		Repository string `json:"repository"`
	}
	check := func(body map[string]any) answer {
		t.Helper()
		body["envId"] = env.ID
		body["credentialId"] = cred.ID
		status, raw := srv.Do(http.MethodPost, "/api/registries/test-from-env", body)
		if status != http.StatusOK {
			t.Fatalf("POST test-from-env %v = %d %s", body, status, raw)
		}
		if strings.Contains(string(raw), password) {
			t.Fatalf("the answer carries the secret: %s", raw)
		}
		var a answer
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("answer %s: %v", raw, err)
		}
		return a
	}

	// The stored password: the host's engine reads the manifest and the answer names the host.
	got := check(map[string]any{"repository": "private/busybox", "tag": "1"})
	if got.State != "passed" || got.Code != "" || got.Digest != pushed || got.Node != agents[0].Node || got.NodeID != agents[0].ID ||
		got.Repository != addr+"/private/busybox" {
		t.Fatalf("with the right password: %+v, want passed on %s with the digest %s", got, agents[0].Node, pushed)
	}
	// A wrong password typed into the test is refused by the registry, and nothing is stored.
	got = check(map[string]any{"repository": "private/busybox", "tag": "1", "secret": "wrong-" + password})
	if got.State != "failed" || got.Code != "pull_unauthorized" || got.Node != agents[0].Node || got.Digest != "" {
		t.Fatalf("with a wrong password: %+v, want failed with pull_unauthorized on %s", got, agents[0].Node)
	}
	// A repository the registry does not have.
	got = check(map[string]any{"repository": "private/missing", "tag": "1"})
	if got.State != "failed" || got.Code != "image_not_found" {
		t.Fatalf("with a missing repository: %+v, want failed with image_not_found", got)
	}

	rows := srv.AuditRows("registry.credential_tested")
	if len(rows) != 3 || rows[0].Result != "success" || rows[0].Details["via"] != "env" || rows[0].Details["node"] != agents[0].ID ||
		rows[1].Result != "failure" || rows[1].Reason != "pull_unauthorized" || rows[2].Reason != "image_not_found" {
		t.Fatalf("audit rows = %+v, want three rows via env on the node: success, pull_unauthorized, image_not_found", rows)
	}
	if dockertest.HasClientConfig(t, host) {
		t.Errorf("the host has a Docker client configuration after the checks: Bayplan must not log in")
	}
}
