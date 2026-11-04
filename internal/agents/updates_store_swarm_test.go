//go:build swarm

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// TestImageStoresLocalBuild shows that "not from a registry" is honest on both of Docker's image stores (plan 12-35, D-20,
// RESEARCH Pitfall 1). On each of two standalone hosts with a TierPlan agent, one on the containerd image store (the default
// of Engine 29; switched on with a flag on older engines) and one on the classic store (started with the containerd snapshotter
// off), a container runs an image that was never pushed or pulled, and another runs an image that was pulled from the fixture
// registry. The registry is asked by the server, as in production. The local image reads not_from_registry on both stores (on
// the containerd store because its only digest is its own ID and Docker Hub, asked anonymously, has no such repository or wants
// a sign-in; on the classic store because it has no repo digest, so nobody is asked) and the pulled image reads up_to_date on
// both.
func TestImageStoresLocalBuild(t *testing.T) {
	older := func() bool {
		n, err := strconv.Atoi(strings.SplitN(dockertest.EngineFromEnv(), ".", 2)[0])
		return err != nil || n < 29
	}()
	containerdArgs := []string(nil)
	if older {
		containerdArgs = []string{"--feature", "containerd-snapshotter=true"}
	}
	for _, store := range []struct {
		name   string
		args   []string
		driver string // what docker info reports as the storage driver type
	}{
		{"containerd", containerdArgs, "io.containerd.snapshotter.v1"},
		{"classic", []string{"--storage-driver=overlay2", "--feature", "containerd-snapshotter=false"}, "overlay2"},
	} {
		t.Run(store.name, func(t *testing.T) {
			host := dockertest.StartHostWith(t, "", store.args...)
			dockertest.LoadAgentImage(t, host)
			dumpOnFailure(t, host)
			info := strings.TrimSpace(sh(t, host, "docker info -f '{{.Driver}}|{{.DriverStatus}}'"))
			t.Logf("the %s host reports %s", store.name, info)
			if !strings.Contains(info, store.driver) {
				t.Fatalf("the %s host reports %q, want a storage driver of %s", store.name, info, store.driver)
			}

			// The registry is the host's own, forwarded so the server can ask it; the pulled image comes from it. The local image is
			// named as an image built here is, without a registry of its own, so it is Docker Hub's name and the server asks Docker
			// Hub about it anonymously, which is what answers "no such repository" or "sign in" for a name nobody pushed (D-20 reads
			// that as a local build only for a check made without a credential, so the fixture's credential must not cover it, and a
			// plain-HTTP fixture cannot be asked anonymously). One HEAD of a manifest, which Hub does not count against a pull limit.
			addr := "127.0.0.1:5000"
			dockertest.StartRegistry(t, host, dockertest.RegistryOptions{})
			dockertest.ForwardRegistry(t, host)
			pulled, local := addr+"/store/pulled:1", "tierplan-nobody/never-pushed:1"
			digest := dockertest.PushVariantCmd(t, host, dockertest.ServiceTestImage, pulled, "P", `["sleep","3600"]`, nil)
			sh(t, host, "docker image rm "+pulled+" && docker pull "+pulled)
			sh(t, host, "docker run --name seed "+dockertest.ServiceTestImage+" sh -c 'echo local > /tp-local' && docker commit --change 'CMD [\"sleep\",\"3600\"]' seed "+local+" && docker rm seed")
			sh(t, host, "docker run -d --name pulledapp "+pulled)
			sh(t, host, "docker run -d --name localapp "+local)

			srv := servertest.Start(t, servertest.Options{Host: host.Gateway})
			addFixtureCredential(t, srv, addr, "store")
			env := srv.CreateAgentEnvironment("")
			sh(t, host, env.Install.Standalone.Command)
			srv.WaitOnline(env.ID, 90*time.Second)

			srv.WaitFor("the containers to be listed with an update object", 2*time.Minute, func() (bool, string) {
				_, a := containerUpdate(t, srv, env.ID, "pulledapp")
				_, b := containerUpdate(t, srv, env.ID, "localapp")
				return a && b, "pulledapp listed " + strconv.FormatBool(a) + ", localapp listed " + strconv.FormatBool(b)
			})
			code, body := srv.Do(http.MethodPost, "/api/env/"+env.ID+"/updates/check", nil)
			if code != http.StatusOK {
				t.Fatalf("POST updates/check = %d %s", code, body)
			}
			srv.WaitFor("the pulled image to read up to date and the local one not from a registry", time.Minute, func() (bool, string) {
				p, _ := containerUpdate(t, srv, env.ID, "pulledapp")
				l, _ := containerUpdate(t, srv, env.ID, "localapp")
				return p.State == "up_to_date" && l.State == "not_from_registry", "pulled " + p.State + " (" + p.Reason + "), local " + l.State + " (" + l.Reason + ")"
			})
			if p, _ := containerUpdate(t, srv, env.ID, "pulledapp"); p.RunningDigest != digest {
				t.Errorf("the pulled container runs %s, want %s", p.RunningDigest, digest)
			}
		})
	}
}
