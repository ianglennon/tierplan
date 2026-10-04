// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package selfmaint

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/bayplan/proto/wire"
)

const (
	ownRepo   = "ghcr.io/ianglennon/bayplan-agent"
	ownImage  = ownRepo + ":0.2.0@sha256:" + "0000000000000000000000000000000000000000000000000000000000000001"
	newImage  = ownRepo + ":0.2.1"
	selfLong  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	selfShort = "0123456789ab"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newMaint(f *fakeEngine, mut func(*Config)) *Maint {
	cfg := Config{SocketPath: f.sock, SelfID: selfShort, SelfUpdate: true, OwnRepository: ownRepo, Log: quietLog()}
	if mut != nil {
		mut(&cfg)
	}
	return New(cfg)
}

// swarmSelf registers the agent's own container as a swarm task of service svc1.
func swarmSelf(f *fakeEngine, labels map[string]string) {
	l := map[string]string{"bayplan.agent": "true", "com.docker.swarm.service.id": "svc1"}
	for k, v := range labels {
		if v == "" {
			delete(l, k)
		} else {
			l[k] = v
		}
	}
	f.addContainer(container.InspectResponse{
		ID: selfLong, Name: "/bayplan-agent.1.abc", State: &container.State{Running: true, Status: "running"},
		Config: &container.Config{Hostname: selfShort, Image: ownImage, Labels: l},
	})
}

// serviceJSON is a service as a newer engine would inspect it: it holds fields no
// client of this agent's version knows (FutureSpecField, FutureTaskField,
// FutureContainerField), and a number a float cannot hold exactly.
const serviceJSON = `{
 "ID":"svc1","Version":{"Index":42},"CreatedAt":"2026-10-01T10:00:00Z","UpdatedAt":"2026-10-02T10:00:00Z",
 "Spec":{"Name":"bayplan-agent","Labels":{"bayplan.agent":"true"},
  "TaskTemplate":{
   "ContainerSpec":{"Image":"` + ownImage + `","Env":["A=1"],"Mounts":[{"Type":"bind","Source":"/var/run/docker.sock","Target":"/var/run/docker.sock"}],
    "ReadOnly":true,"CapabilityDrop":["ALL"],"FutureContainerField":{"x":[1,2,3],"big":12345678901234567890123}},
   "RestartPolicy":{"Condition":"any"},"FutureTaskField":"keep me"},
  "Mode":{"Global":{}},
  "UpdateConfig":{"Parallelism":1,"Delay":10000000000,"FailureAction":"pause"},
  "EndpointSpec":{"Mode":"vip"},"FutureSpecField":true},
 "PreviousSpec":{"Name":"bayplan-agent"},"Endpoint":{"Spec":{}}}`

func decodeNumbers(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func swarmFixture(t *testing.T, mut func(*Config)) (*fakeEngine, *Maint) {
	t.Helper()
	f := newFakeEngine(t)
	f.swarmControl = true
	f.serviceID = "svc1"
	f.service = serviceJSON
	swarmSelf(f, nil)
	return f, newMaint(f, mut)
}

func TestSwarmUpdatePatchesOnlyTheImageAndPostsWithTheInspectedVersion(t *testing.T) {
	f, m := swarmFixture(t, nil)

	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, ID: "req-7", Image: newImage})
	if !res.OK || res.Error != "" {
		t.Fatalf("the update was refused: %+v", res)
	}
	if res.T != wire.TypeUpdateResult || res.ID != "req-7" {
		t.Fatalf("the result must be an update_result that echoes the id: %+v", res)
	}

	if len(f.servicePosts) != 1 {
		t.Fatalf("want exactly one service update, got %d (%v)", len(f.servicePosts), f.callList())
	}
	post := f.servicePosts[0]
	if post.ID != "svc1" || post.Version != "42" {
		t.Fatalf("posted to service %q with version %q, want svc1 and the inspected index 42", post.ID, post.Version)
	}

	// Everything in the spec but the image is exactly as inspected, including the
	// fields this client has no type for and the number a float would round.
	want := decodeNumbers(t, []byte(serviceJSON))["Spec"].(map[string]any)
	want["TaskTemplate"].(map[string]any)["ContainerSpec"].(map[string]any)["Image"] = newImage
	got := decodeNumbers(t, post.Body)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the posted spec differs from the inspected one in more than the image:\n got: %s\nwant: %v", post.Body, want)
	}
	if !strings.Contains(string(post.Body), `"FutureContainerField":{"x":[1,2,3],"big":12345678901234567890123}`) {
		t.Fatalf("the raw bytes of an unknown field were not kept: %s", post.Body)
	}

	// The only writes are that one update.
	if w := f.writes(); len(w) != 1 || !strings.HasPrefix(w[0], "POST /services/svc1/update?version=42") {
		t.Fatalf("self-update wrote more than the one service update: %v", w)
	}
}

func TestSwarmUpdateReadsItsOwnServiceFromItsContainerLabel(t *testing.T) {
	f, m := swarmFixture(t, nil)
	_ = m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	calls := strings.Join(f.callList(), "\n")
	if !strings.Contains(calls, "GET /containers/"+selfShort+"/json") || !strings.Contains(calls, "GET /services/svc1") {
		t.Fatalf("the agent must inspect its own container and then that service: %v", f.callList())
	}
}

func TestAnUpdateOutsideTheOwnRepositoryIsRefusedWithoutADockerCall(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/evil/agent:1",
		"evil.example/ianglennon/bayplan-agent:0.2.1",
		"ghcr.io/ianglennon/bayplan-agent-extra:0.2.1",
		"ghcr.io/ianglennon/bayplan:0.2.1",
		"ianglennon/bayplan-agent:0.2.1",
		"ghcr.io/ianglennon/bayplan-agent/sub:0.2.1",
		"ghcr.io:5000/ianglennon/bayplan-agent:0.2.1",
		"sha256:" + strings.Repeat("a", 64), // the reference grammar reads this as docker.io/library/sha256
	} {
		t.Run(image, func(t *testing.T) {
			f, m := swarmFixture(t, nil)
			res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, ID: "x", Image: image})
			if res.OK || res.Error != wire.UpdateErrImageNotOwnRepository {
				t.Fatalf("a foreign repository must answer %s: %+v", wire.UpdateErrImageNotOwnRepository, res)
			}
			if res.ID != "x" {
				t.Fatalf("the id must be echoed even on a refusal: %+v", res)
			}
			if c := f.callList(); len(c) != 0 {
				t.Fatalf("a refused update made Docker calls: %v", c)
			}
		})
	}
}

func TestAnUpdateOfTheOwnRepositoryInAnyOfItsFormsIsAccepted(t *testing.T) {
	digest := ownRepo + "@sha256:" + strings.Repeat("ab", 32)
	for _, image := range []string{newImage, digest, ownRepo + ":0.2.1@sha256:" + strings.Repeat("cd", 32), ownRepo + ":edge"} {
		t.Run(image, func(t *testing.T) {
			f, m := swarmFixture(t, nil)
			res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: image})
			if !res.OK {
				t.Fatalf("refused: %+v", res)
			}
			if got := decodeNumbers(t, f.servicePosts[0].Body)["TaskTemplate"].(map[string]any)["ContainerSpec"].(map[string]any)["Image"]; got != image {
				t.Fatalf("the service got image %v, want %s", got, image)
			}
		})
	}
}

func TestAnImageThatIsNotAValidReferenceIsRefusedWithoutADockerCall(t *testing.T) {
	long := ownRepo + ":" + strings.Repeat("a", wire.MaxImageLen)
	for name, image := range map[string]string{
		"empty": "", "spaces": "ghcr.io/ianglennon/bayplan-agent:0.2 .1", "uppercase": "GHCR.io/x/Y:1",
		"newline": ownRepo + ":1\n", "too long": long, "scheme": "https://" + ownRepo + ":1",
		"no tag or digest": ownRepo, "own repository with a trailing colon": ownRepo + ":",
	} {
		t.Run(name, func(t *testing.T) {
			f, m := swarmFixture(t, nil)
			res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: image})
			if res.OK || res.Error != wire.UpdateErrImageInvalid {
				t.Fatalf("want %s, got %+v", wire.UpdateErrImageInvalid, res)
			}
			if c := f.callList(); len(c) != 0 {
				t.Fatalf("Docker was called: %v", c)
			}
		})
	}
}

func TestTheOwnRepositoryComesFromTheOwnContainerWhenNoneIsConfigured(t *testing.T) {
	f, m := swarmFixture(t, func(c *Config) { c.OwnRepository = "" })

	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: "ghcr.io/evil/agent:1"})
	if res.OK || res.Error != wire.UpdateErrImageNotOwnRepository {
		t.Fatalf("a foreign repository must be refused: %+v", res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a refusal wrote to Docker: %v", w)
	}

	res = m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	if !res.OK {
		t.Fatalf("an image of the container's own repository must be accepted: %+v", res)
	}
}

func TestAWorkerAgentRefusesUpdateAsNotAManager(t *testing.T) {
	f, m := swarmFixture(t, nil)
	f.swarmControl = false
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, ID: "w", Image: newImage})
	if res.OK || res.Error != wire.UpdateErrNotAManager {
		t.Fatalf("a worker must answer %s: %+v", wire.UpdateErrNotAManager, res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a worker wrote to Docker: %v", w)
	}
}

func TestADisabledAgentRefusesUpdateAndUninstallWithoutADockerCall(t *testing.T) {
	f, m := swarmFixture(t, func(c *Config) { c.SelfUpdate = false })

	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, ID: "u1", Image: newImage})
	if res.OK || res.Error != wire.UpdateErrSelfUpdateDisabled || res.T != wire.TypeUpdateResult || res.ID != "u1" {
		t.Fatalf("update: want self_update_disabled echoing the id, got %+v", res)
	}
	res = m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "u2", RemoveData: true})
	if res.OK || res.Error != wire.UpdateErrSelfUpdateDisabled || res.T != wire.TypeUpdateResult || res.ID != "u2" {
		t.Fatalf("uninstall: want self_update_disabled echoing the id, got %+v", res)
	}
	// Even a foreign image answers disabled: the host owner's switch comes first.
	res = m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: "ghcr.io/evil/agent:1"})
	if res.Error != wire.UpdateErrSelfUpdateDisabled {
		t.Fatalf("the switch must win over the repository check: %+v", res)
	}
	if c := f.callList(); len(c) != 0 {
		t.Fatalf("a disabled agent made Docker calls: %v", c)
	}
}

func TestAnAgentWhoseOwnContainerIsNotLabelledRefusesAndWritesNothing(t *testing.T) {
	f := newFakeEngine(t)
	f.swarmControl = true
	f.serviceID, f.service = "svc1", serviceJSON
	swarmSelf(f, map[string]string{"bayplan.agent": "false"})
	m := newMaint(f, nil)

	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	if res.OK || res.Error != wire.UpdateErrOwnContainerUnknown {
		t.Fatalf("want %s: %+v", wire.UpdateErrOwnContainerUnknown, res)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("wrote to Docker: %v", w)
	}
}

func TestAnAgentThatCannotFindItsOwnContainerRefuses(t *testing.T) {
	f := newFakeEngine(t)
	m := newMaint(f, nil)
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	if res.OK || res.Error != wire.UpdateErrOwnContainerUnknown {
		t.Fatalf("want %s: %+v", wire.UpdateErrOwnContainerUnknown, res)
	}
}

func TestADockerFailureOfTheServiceUpdateAnswersUpdateFailed(t *testing.T) {
	f, m := swarmFixture(t, nil)
	f.serviceErr = 500
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	if res.OK || res.Error != wire.UpdateErrFailed {
		t.Fatalf("want %s: %+v", wire.UpdateErrFailed, res)
	}
}

func TestAServiceThatHasNoImageToPatchAnswersUpdateFailedAndPostsNothing(t *testing.T) {
	f, m := swarmFixture(t, nil)
	f.service = `{"ID":"svc1","Version":{"Index":3},"Spec":{"Name":"x","TaskTemplate":{}}}`
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, Image: newImage})
	if res.OK || res.Error != wire.UpdateErrFailed {
		t.Fatalf("want %s: %+v", wire.UpdateErrFailed, res)
	}
	if len(f.servicePosts) != 0 {
		t.Fatal("a spec without a container image was posted")
	}
}

func TestTwoUpdatesAtOnceAnswerBusyForTheSecond(t *testing.T) {
	f, m := swarmFixture(t, nil)
	gate := make(chan struct{})
	m.beforeDocker = func() { <-gate }

	var wg sync.WaitGroup
	wg.Add(1)
	var first wire.UpdateResult
	go func() {
		defer wg.Done()
		first = m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, ID: "a", Image: newImage})
	}()
	// Wait until the first holds the lock.
	deadline := time.Now().Add(5 * time.Second)
	for !m.busy() {
		if time.Now().After(deadline) {
			t.Fatal("the first update never started")
		}
		time.Sleep(time.Millisecond)
	}
	second := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, ID: "b", Image: newImage})
	if second.OK || second.Error != wire.UpdateErrBusy || second.ID != "b" {
		t.Fatalf("the second update must answer %s: %+v", wire.UpdateErrBusy, second)
	}
	close(gate)
	wg.Wait()
	if !first.OK {
		t.Fatalf("the first update failed: %+v", first)
	}
	if len(f.servicePosts) != 1 {
		t.Fatalf("want one service update, got %d", len(f.servicePosts))
	}
}

func TestTheEchoedIDIsCapped(t *testing.T) {
	_, m := swarmFixture(t, func(c *Config) { c.SelfUpdate = false })
	res := m.Update(context.Background(), wire.Update{T: wire.TypeUpdate, ID: strings.Repeat("z", 500), Image: newImage})
	if len(res.ID) != wire.MaxMessageIDLen {
		t.Fatalf("the id was echoed at %d bytes, want the cap %d", len(res.ID), wire.MaxMessageIDLen)
	}
}

func TestUninstallIsNotYetImplementedWhenEnabled(t *testing.T) {
	f := newFakeEngine(t)
	m := newMaint(f, nil)
	res := m.Uninstall(context.Background(), wire.Uninstall{T: wire.TypeUninstall, ID: "q"})
	if res.OK || res.Error != wire.UpdateErrNotImplemented || res.ID != "q" {
		t.Fatalf("want %s: %+v", wire.UpdateErrNotImplemented, res)
	}
	if c := f.callList(); len(c) != 0 {
		t.Fatalf("Docker was called: %v", c)
	}
}

func TestOwnContainerIDFromMountinfo(t *testing.T) {
	const id = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"
	info := strings.Join([]string{
		"1020 1019 0:48 / / rw,relatime - overlay overlay rw",
		"1031 1020 254:1 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/vda1 rw",
		"1032 1020 254:1 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw,relatime - ext4 /dev/vda1 rw",
	}, "\n")
	if got := containerIDFromMountinfo(strings.NewReader(info)); got != id {
		t.Fatalf("got %q, want %s", got, id)
	}
	if got := containerIDFromMountinfo(strings.NewReader("1020 1019 0:48 / / rw - overlay overlay rw\n")); got != "" {
		t.Fatalf("no container mount must give an empty id, got %q", got)
	}
	// A path that merely looks like one, with a short id, is not taken.
	if got := containerIDFromMountinfo(strings.NewReader("1 2 3:4 /var/lib/docker/containers/abc/hostname /etc/hostname rw - ext4 x rw\n")); got != "" {
		t.Fatalf("a short id was taken: %q", got)
	}
}
