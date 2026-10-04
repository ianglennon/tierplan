// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package selfmaint

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"

	"github.com/ianglennon/bayplan/proto/wire"
)

// uninstallServiceJSON is the agent's own service with its token secret, and one
// secret that belongs to somebody else and must never be touched.
const uninstallServiceJSON = `{
 "ID":"svc1","Version":{"Index":42},
 "Spec":{"Name":"bayplan-agent","Labels":{"bayplan.agent":"true"},
  "TaskTemplate":{"ContainerSpec":{"Image":"` + ownImage + `",
   "Secrets":[
    {"File":{"Name":"bayplan-agent-token","UID":"0","GID":"0","Mode":256},"SecretID":"sec-token","SecretName":"bayplan-agent-token-g2"},
    {"File":{"Name":"other","UID":"0","GID":"0","Mode":256},"SecretID":"sec-other","SecretName":"somebody-elses-secret"}
   ]}},
  "Mode":{"Global":{}}}}`

// swarmUninstallFixture is a swarm agent with the Docker socket and its state volume
// mounted, as the install leaves it, plus a second volume that is not the agent's.
func swarmUninstallFixture(t *testing.T, manager bool, mut func(*Config)) (*fakeEngine, *Maint) {
	t.Helper()
	f := newFakeEngine(t)
	f.swarmControl = manager
	f.serviceID, f.service = "svc1", uninstallServiceJSON
	f.addContainer(container.InspectResponse{
		ID: selfLong, Name: "/bayplan-agent.1.abc", State: &container.State{Running: true, Status: "running"},
		Config: &container.Config{
			Hostname: selfShort, Image: ownImage,
			Labels: map[string]string{"bayplan.agent": "true", "com.docker.swarm.service.id": "svc1"},
			Env:    []string{"BAYPLAN_AGENT_DOCKER_GID=998", "BAYPLAN_AGENT_URL=wss://x/agent/connect"},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: sockPath, Destination: f.sock, RW: true},
			{Type: mount.TypeVolume, Name: "bayplan-agent", Destination: stateDir, RW: true},
			{Type: mount.TypeVolume, Name: "somebodys-data", Destination: "/data", RW: true},
		},
	})
	return f, newMaint(f, func(c *Config) {
		c.StateDir = stateDir
		if mut != nil {
			mut(c)
		}
	})
}

func TestASwarmManagerToldToRemoveTheServiceRemovesOnlyItsOwnServiceAndTokenSecretAfterStartingItsHelper(t *testing.T) {
	f, m := swarmUninstallFixture(t, true, nil)

	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "u1", RemoveData: true, Service: true})
	if !res.OK || res.Error != "" || res.ID != "u1" || res.T != wire.TypeUpdateResult {
		t.Fatalf("the uninstall was refused: %+v", res)
	}
	if !reflect.DeepEqual(f.serviceDeletes, []string{"svc1"}) {
		t.Fatalf("services removed: %v, want only svc1", f.serviceDeletes)
	}
	if !reflect.DeepEqual(f.secretDeletes, []string{"sec-token"}) {
		t.Fatalf("secrets removed: %v, want only the token secret of this service (not somebody-elses-secret)", f.secretDeletes)
	}
	if len(f.volumeDeletes) != 0 {
		t.Fatalf("the agent removed a volume itself: %v (its helper does that once the container is gone)", f.volumeDeletes)
	}
	// The helper must exist before the service goes: the service removal kills this
	// very process.
	writes := f.writes()
	iStart, iService, iSecret := indexOf(writes, "POST /containers/", "/start"), indexOf(writes, "DELETE /services/svc1", ""), indexOf(writes, "DELETE /secrets/sec-token", "")
	if iStart < 0 || iService < 0 || iSecret < 0 || !(iStart < iService && iService < iSecret) {
		t.Fatalf("want helper start, then service removal, then secret removal; writes: %v", writes)
	}
	if len(f.creates) != 1 {
		t.Fatalf("want one helper, got %d creates", len(f.creates))
	}
	h := f.creates[0].Req
	if want := []string{"helper", "remove-volume", "--wait", selfLong, "--volume", "bayplan-agent"}; !reflect.DeepEqual([]string(h.Cmd), want) {
		t.Errorf("helper command = %v, want %v", h.Cmd, want)
	}
	if h.Image != ownImage {
		t.Errorf("the helper runs %q, want the agent's own image %q (nothing is pulled)", h.Image, ownImage)
	}
	if h.Labels["bayplan.agent"] != "true" || h.Labels["bayplan.agent.helper"] != "true" {
		t.Errorf("helper labels = %v", h.Labels)
	}
	if !slices.Contains(h.Env, "BAYPLAN_AGENT_SOCKET="+f.sock) || !slices.Contains(h.Env, "BAYPLAN_AGENT_DOCKER_GID=998") {
		t.Errorf("helper env = %v", h.Env)
	}
	if len(f.pulled) != 0 {
		t.Errorf("an uninstall pulled %v", f.pulled)
	}
	hc := h.HostConfig
	if !hc.AutoRemove || !hc.ReadonlyRootfs || hc.NetworkMode != "none" {
		t.Errorf("the helper is not hardened: autoremove=%v readonly=%v network=%q", hc.AutoRemove, hc.ReadonlyRootfs, hc.NetworkMode)
	}
	if len(hc.Mounts) != 1 || hc.Mounts[0].Target != f.sock {
		t.Fatalf("helper mounts = %+v, want only the Docker socket: a mounted state volume would be in use and could not be removed", hc.Mounts)
	}
}

func indexOf(list []string, prefix, contains string) int {
	for i, s := range list {
		if strings.HasPrefix(s, prefix) && strings.Contains(s, contains) {
			return i
		}
	}
	return -1
}

func TestASwarmAgentNotToldToRemoveTheServiceOnlyStartsItsVolumeHelper(t *testing.T) {
	for _, manager := range []bool{true, false} {
		f, m := swarmUninstallFixture(t, manager, nil)
		res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "u", RemoveData: true})
		if !res.OK {
			t.Fatalf("manager=%v: refused: %+v", manager, res)
		}
		if len(f.serviceDeletes) != 0 || len(f.secretDeletes) != 0 || len(f.volumeDeletes) != 0 {
			t.Fatalf("manager=%v removed something itself: services %v secrets %v volumes %v", manager, f.serviceDeletes, f.secretDeletes, f.volumeDeletes)
		}
		if len(f.creates) != 1 {
			t.Fatalf("manager=%v: want one helper, got %d", manager, len(f.creates))
		}
		if want := []string{"helper", "remove-volume", "--wait", selfLong, "--volume", "bayplan-agent"}; !reflect.DeepEqual([]string(f.creates[0].Req.Cmd), want) {
			t.Fatalf("helper command = %v", f.creates[0].Req.Cmd)
		}
	}
}

func TestASwarmAgentKeepingItsDataStartsNoHelper(t *testing.T) {
	f, m := swarmUninstallFixture(t, true, nil)
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: false})
	if !res.OK {
		t.Fatalf("refused: %+v", res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("an uninstall that keeps the data and removes no service wrote %v", w)
	}
}

func TestAWorkerAskedToRemoveTheServiceRefusesAsNotAManagerAndChangesNothing(t *testing.T) {
	f, m := swarmUninstallFixture(t, false, nil)
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "w", RemoveData: true, Service: true})
	if res.OK || res.Error != wire.UpdateErrNotAManager {
		t.Fatalf("want %s: %+v", wire.UpdateErrNotAManager, res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a worker wrote to Docker: %v", w)
	}
}

func TestAServiceThatCannotBeRemovedIsAnUpdateFailedAndItsSecretIsKept(t *testing.T) {
	f, m := swarmUninstallFixture(t, true, nil)
	f.service = "" // the engine answers 404 for the service
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: true, Service: true})
	if res.OK || res.Error != wire.UpdateErrFailed {
		t.Fatalf("want %s: %+v", wire.UpdateErrFailed, res)
	}
	if len(f.secretDeletes) != 0 {
		t.Fatalf("a secret was removed although the service was not: %v", f.secretDeletes)
	}
}

func TestAServiceWithNoSecretOfTheAgentsOwnRemovesNoSecret(t *testing.T) {
	f, m := swarmUninstallFixture(t, true, nil)
	f.service = strings.Replace(uninstallServiceJSON, "bayplan-agent-token-g2", "mine-not-bayplans", 1)
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: true, Service: true})
	if !res.OK {
		t.Fatalf("refused: %+v", res)
	}
	if len(f.secretDeletes) != 0 {
		t.Fatalf("secrets that are not the agent's token were removed: %v", f.secretDeletes)
	}
	if !reflect.DeepEqual(f.serviceDeletes, []string{"svc1"}) {
		t.Fatalf("services removed: %v", f.serviceDeletes)
	}
}

func TestAHelperThatCannotStartMeansNothingIsRemovedAndTheHelperIsCleanedUp(t *testing.T) {
	f, m := swarmUninstallFixture(t, true, nil)
	f.failStart[fakeID(1)] = true // the first container created is the helper
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: true, Service: true})
	if res.OK || res.Error != wire.UpdateErrFailed {
		t.Fatalf("want %s: %+v", wire.UpdateErrFailed, res)
	}
	if len(f.serviceDeletes) != 0 || len(f.secretDeletes) != 0 {
		t.Fatalf("the service was removed although the helper could not start: %v %v", f.serviceDeletes, f.secretDeletes)
	}
	if _, left := f.containers[fakeID(1)]; left {
		t.Fatal("the helper that never started was left behind")
	}
}

func fakeID(n int) string { return fmt.Sprintf("%064x", n) }

func TestAnUninstallOfAnAgentThatDoesNotKnowItsOwnContainerRefusesAndWritesNothing(t *testing.T) {
	f := newFakeEngine(t)
	m := newMaint(f, nil)
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "x", RemoveData: true, Service: true})
	if res.OK || res.Error != wire.UpdateErrOwnContainerUnknown {
		t.Fatalf("want %s: %+v", wire.UpdateErrOwnContainerUnknown, res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("wrote to Docker: %v", w)
	}
}

func TestAnUninstallWhoseOwnImageIsInAnotherRepositoryIsRefused(t *testing.T) {
	f, m := swarmUninstallFixture(t, true, func(c *Config) { c.OwnRepository = "ghcr.io/somebody/else" })
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: true, Service: true})
	if res.OK || res.Error != wire.UpdateErrOwnContainerUnknown {
		t.Fatalf("want %s: %+v", wire.UpdateErrOwnContainerUnknown, res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("wrote to Docker: %v", w)
	}
}

func TestTwoUninstallsAtOnceAnswerBusyForTheSecond(t *testing.T) {
	f, m := swarmUninstallFixture(t, true, nil)
	gate := make(chan struct{})
	m.beforeDocker = func() { <-gate }
	done := make(chan wire.UpdateResult, 1)
	go func() {
		done <- m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "a", RemoveData: true, Service: true})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !m.busy() {
		if time.Now().After(deadline) {
			t.Fatal("the first uninstall never started")
		}
		time.Sleep(time.Millisecond)
	}
	if second := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "b", RemoveData: true, Service: true}); second.OK || second.Error != wire.UpdateErrBusy || second.ID != "b" {
		t.Fatalf("the second uninstall must answer %s: %+v", wire.UpdateErrBusy, second)
	}
	close(gate)
	if first := <-done; !first.OK {
		t.Fatalf("the first uninstall failed: %+v", first)
	}
	if len(f.serviceDeletes) != 1 {
		t.Fatalf("want one service removal, got %v", f.serviceDeletes)
	}
}

// ---- standalone ----

func standaloneUninstallFixture(t *testing.T) (*fakeEngine, *Maint) {
	t.Helper()
	f, m := standaloneFixture(t, nil)
	f.images[ownImage] = image.InspectResponse{ID: "sha256:old", Config: oldImageConfig()}
	return f, m
}

func TestAStandaloneAgentStartsAHelperThatRemovesItsContainerAndThenItsVolume(t *testing.T) {
	f, m := standaloneUninstallFixture(t)
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "s1", RemoveData: true})
	if !res.OK || res.ID != "s1" {
		t.Fatalf("refused: %+v", res)
	}
	if len(f.creates) != 1 {
		t.Fatalf("want one helper, got %d", len(f.creates))
	}
	h := f.creates[0].Req
	want := []string{"helper", "remove-volume", "--wait", oldLong, "--volume", "bayplan-agent", "--remove-container"}
	if !reflect.DeepEqual([]string(h.Cmd), want) {
		t.Fatalf("helper command = %v, want %v", h.Cmd, want)
	}
	if h.Image != ownImage {
		t.Errorf("the helper runs %q, want %q", h.Image, ownImage)
	}
	if len(h.HostConfig.Mounts) != 1 {
		t.Errorf("helper mounts = %+v, want only the socket", h.HostConfig.Mounts)
	}
	if len(f.serviceDeletes)+len(f.secretDeletes)+len(f.volumeDeletes) != 0 {
		t.Fatalf("the agent removed things itself: %v %v %v", f.serviceDeletes, f.secretDeletes, f.volumeDeletes)
	}
	if _, running := f.containers[oldLong]; !running {
		t.Fatal("the agent's own container was removed by the agent: that is the helper's job, after the answer has gone out")
	}
}

func TestAStandaloneUninstallThatKeepsTheDataAddsKeepVolume(t *testing.T) {
	f, m := standaloneUninstallFixture(t)
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: false})
	if !res.OK {
		t.Fatalf("refused: %+v", res)
	}
	want := []string{"helper", "remove-volume", "--wait", oldLong, "--volume", "bayplan-agent", "--remove-container", "--keep-volume"}
	if got := []string(f.creates[0].Req.Cmd); !reflect.DeepEqual(got, want) {
		t.Fatalf("helper command = %v, want %v", got, want)
	}
}

func TestAStandaloneAgentIgnoresTheServiceFlag(t *testing.T) {
	f, m := standaloneUninstallFixture(t)
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: true, Service: true})
	if !res.OK {
		t.Fatalf("refused: %+v", res)
	}
	if len(f.serviceDeletes) != 0 || len(f.secretDeletes) != 0 {
		t.Fatalf("a standalone agent removed a service or secret: %v %v", f.serviceDeletes, f.secretDeletes)
	}
}

func TestAStandaloneAgentWithoutAStateVolumeMountStillRemovesItselfButNoVolume(t *testing.T) {
	f, m := standaloneUninstallFixture(t)
	c := f.containers[oldLong]
	c.Mounts[1] = container.MountPoint{Type: mount.TypeBind, Source: "/srv/state", Destination: stateDir, RW: true}
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: true})
	if !res.OK {
		t.Fatalf("refused: %+v", res)
	}
	want := []string{"helper", "remove-volume", "--wait", oldLong, "--remove-container", "--keep-volume"}
	if got := []string(f.creates[0].Req.Cmd); !reflect.DeepEqual(got, want) {
		t.Fatalf("helper command = %v, want %v (a bind-mounted state directory is the operator's and is left)", got, want)
	}
}

func TestAStandaloneUninstallThatCannotStartItsHelperKeepsTheAgent(t *testing.T) {
	f, m := standaloneUninstallFixture(t)
	f.failStart[fakeID(1)] = true
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: true})
	if res.OK || res.Error != wire.UpdateErrFailed {
		t.Fatalf("want %s: %+v", wire.UpdateErrFailed, res)
	}
	if _, ok := f.containers[oldLong]; !ok {
		t.Fatal("the agent was removed")
	}
	if _, left := f.containers[fakeID(1)]; left {
		t.Fatal("the helper that never started was left behind")
	}
}

func TestAStandaloneUninstallWithAnAgentWithoutTheSocketMountFails(t *testing.T) {
	f, m := standaloneUninstallFixture(t)
	f.containers[oldLong].Mounts = f.containers[oldLong].Mounts[1:] // no socket
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, RemoveData: true})
	if res.OK || res.Error != wire.UpdateErrFailed {
		t.Fatalf("want %s: %+v", wire.UpdateErrFailed, res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("wrote to Docker: %v", w)
	}
}
