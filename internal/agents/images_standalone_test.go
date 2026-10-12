// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package agents_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
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
