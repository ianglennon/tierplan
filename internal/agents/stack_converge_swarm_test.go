// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The real-swarm cases of plans 07-46 and 07-49 for UAT gaps G-07-2 and G-07-5: a deploy whose
// service the swarm will not start again, and one whose network has no free addresses, each ends
// the deploy on the reason instead of on the converge timeout. They deploy through Coaming's API
// and real agents, as the 07-11 and 07-34 stack cases do, and name the image by digest from the
// swarm's own registry, so no lookup runs and no node pulls from Docker Hub.

// endedStackJob is a deploy job as it ended, with the words and result of each item.
type endedStackJob struct {
	State string `json:"state"`
	Items []struct {
		ID      string            `json:"id"`
		State   string            `json:"state"`
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Detail  string            `json:"detail"`
		Result  map[string]string `json:"result"`
	} `json:"items"`
}

// endedItem is one item of an endedStackJob.
type endedItem = struct {
	ID      string            `json:"id"`
	State   string            `json:"state"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Detail  string            `json:"detail"`
	Result  map[string]string `json:"result"`
}

// item is the job's item with the ID, and fails the test, naming the IDs the job has, when there is none.
func (j endedStackJob) item(t *testing.T, id string) endedItem {
	t.Helper()
	ids := make([]string, 0, len(j.Items))
	for _, it := range j.Items {
		if it.ID == id {
			return it
		}
		ids = append(ids, it.ID)
	}
	t.Fatalf("the job has no item %q; it has %s", id, strings.Join(ids, ", "))
	return endedItem{}
}

// deployStackUntilEnded deploys the stack's draft and waits until the job ends, however it ends,
// and returns it. stackDeployWait (5 minutes) is below the default converge timeout (10 minutes),
// so a deploy that waits the timeout out fails here instead of passing late.
func (f *swarmFix) deployStackUntilEnded(t *testing.T, stackID string) endedStackJob {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+stackID+"/deploys", map[string]any{"draftVersion": f.draftVersion(t, stackID)})
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the deploy of stack %s = %d %s", stackID, code, body)
	}
	var job endedStackJob
	testsupport.Poll(t, stackDeployWait, "the deploy job "+started.JobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID+"/deploys/"+started.JobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = endedStackJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	return job
}

// awaitStackUnlocked waits until the stack's operation lock is free. The job frees the lock as its
// function returns, just after the job reads as ended.
func (f *swarmFix) awaitStackUnlocked(t *testing.T, stackID string) {
	t.Helper()
	testsupport.Poll(t, time.Minute, "the operation lock of stack "+stackID+" to be free", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the stack = %d %s", code, body)
		}
		var st struct {
			Lock json.RawMessage `json:"lock"`
		}
		if err := json.Unmarshal(body, &st); err != nil {
			return false, string(body)
		}
		held := len(st.Lock) > 0 && string(st.Lock) != "null"
		return !held, string(body)
	})
}

// convergeFailComposeFmt is the UAT broken service beside one that runs: broken exits 1 and its
// restart policy allows one more try. The one argument is the image.
const convergeFailComposeFmt = `services:
  broken:
    image: %[1]s
    command: ["sh", "-c", "exit 1"]
    deploy:
      restart_policy:
        condition: on-failure
        delay: 1s
        max_attempts: 1
  ok:
    image: %[1]s
    command: ["sleep", "3600"]
`

// sharedStackDeployTasksFailed holds UAT G-07-2 on a real swarm (decisions D-13 and D-14). The
// broken service exits 1, and the swarm stops it after two failed tasks with its UpdateStatus
// null, so no rollout verdict ever comes. The deploy must end on that, as tasks_failed with the
// task's words, and not on the converge timeout. The service beside it runs, so it ends done, and
// the stack's operation lock is free once the job has ended.
func sharedStackDeployTasksFailed(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/converge/app:1", nil)
	image := addr + "/converge/app@" + digest

	const stack = "convfail"
	t.Cleanup(func() { removeStackCLI(t, f, stack) })
	id := f.createStackCoaming(t, stack, fmt.Sprintf(convergeFailComposeFmt, image))
	job := f.deployStackUntilEnded(t, id)

	raw, _ := json.Marshal(job)
	// A job is partial, not failed, when some of its items are done and one failed: the check,
	// lookup and network steps and the service beside the broken one all are.
	if job.State != "partial" {
		t.Fatalf("the deploy ended %s, want partial (some steps done, the broken service failed): %s", job.State, raw)
	}
	broken := job.item(t, "service:"+stack+"_broken")
	if broken.State != "failed" || broken.Code != "tasks_failed" || !strings.Contains(broken.Detail, "non-zero exit (1)") {
		t.Errorf("the broken service = state %q code %q detail %q, want failed, tasks_failed and the task's non-zero exit (1): %s", broken.State, broken.Code, broken.Detail, raw)
	}
	if ok := job.item(t, "service:"+stack+"_ok"); ok.State != "done" {
		t.Errorf("the service beside it = state %q code %q, want done: %s", ok.State, ok.Code, raw)
	}
	if services := job.item(t, "services"); services.Code != "tasks_failed" {
		t.Errorf("the services step = code %q, want tasks_failed: %s", services.Code, raw)
	}
	f.awaitStackUnlocked(t, id)
}

// networkFullComposeFmt is a service of 16 tasks on a stack network of 13 dynamic addresses. The
// one argument is the image.
const networkFullComposeFmt = `services:
  wide:
    image: %s
    command: ["sleep", "3600"]
    deploy:
      replicas: 16
networks:
  default:
    ipam:
      config:
        - subnet: 10.231.20.0/28
`

// sharedStackDeployNetworkFull holds UAT G-07-5 on a real Engine 29 swarm. A /28 has 13 dynamic
// addresses. The service's virtual IP takes one and each node that runs a task takes a
// load-balancer endpoint, 4 at most here, so at least 7 of the 16 tasks stay new. The swarm
// reports no error for them; only a manager's network inspect says the subnet is full, and the
// deploy must end on that, naming the network and the subnet, and not on the converge timeout.
// The case asserts the outcome and never a count of tasks.
//
// It holds the window before any node has a local instance of the network: the tasks sit in
// preparing for about 30 s and Coaming's first read comes sooner, so a full-ID inspect still falls
// through to the swarm. sharedStackDeployNetworkFullOnEveryNode holds the steady state of UAT
// G-07-8, where every manager holds the network locally. Both stay, because between them they
// cover both answers of the engine's lookup.
func sharedStackDeployNetworkFull(t *testing.T, f *swarmFix) {
	if major, err := strconv.Atoi(f.sw.Engine); err == nil && major < 29 {
		t.Skipf("Engine %s reports no address usage on a network inspect (API 1.52 is Engine 29); internal/stackops covers the fallback", f.sw.Engine)
	}
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/netfull/app:1", nil)
	image := addr + "/netfull/app@" + digest

	const stack = "netfull"
	t.Cleanup(func() { removeStackCLI(t, f, stack) })
	id := f.createStackCoaming(t, stack, fmt.Sprintf(networkFullComposeFmt, image))
	job := f.deployStackUntilEnded(t, id)

	raw, _ := json.Marshal(job)
	// partial, not failed: the check, lookup and network steps are done.
	if job.State != "partial" {
		t.Fatalf("the deploy ended %s, want partial (some steps done, the wide service failed): %s", job.State, raw)
	}
	wide := job.item(t, "service:"+stack+"_wide")
	if wide.State != "failed" || wide.Code != "network_full" {
		t.Errorf("the wide service = state %q code %q, want failed and network_full: %s", wide.State, wide.Code, raw)
	}
	if got := wide.Result["network"]; got != stack+"_default" {
		t.Errorf("the wide service names network %q, want %s_default: %s", got, stack, raw)
	}
	if got := wide.Result["subnet"]; got != "10.231.20.0/28" {
		t.Errorf("the wide service names subnet %q, want 10.231.20.0/28: %s", got, raw)
	}
	if services := job.item(t, "services"); services.Code != "network_full" {
		t.Errorf("the services step = code %q, want network_full: %s", services.Code, raw)
	}
	f.awaitStackUnlocked(t, id)
}

// netEveryNodeAnchorFmt is the anchor alone: one task on every node, on a stack network of a /27.
// The one argument is the image.
const netEveryNodeAnchorFmt = `services:
  anchor:
    image: %s
    command: ["sleep", "3600"]
    deploy:
      mode: global
networks:
  default:
    ipam:
      config:
        - subnet: 10.231.21.0/27
`

// netEveryNodeWideFmt is the anchor and the network of netEveryNodeAnchorFmt unchanged, with wide
// added: 32 tasks on a /27. The one argument is the image.
const netEveryNodeWideFmt = `services:
  anchor:
    image: %[1]s
    command: ["sleep", "3600"]
    deploy:
      mode: global
  wide:
    image: %[1]s
    command: ["sleep", "3600"]
    deploy:
      replicas: 32
networks:
  default:
    ipam:
      config:
        - subnet: 10.231.21.0/27
`

// sharedStackDeployNetworkFullOnEveryNode holds UAT G-07-8 on a real Engine 29 swarm in its steady
// state (decisions D-13 and D-14): every manager already holds the stack network locally when a
// redeploy adds a service too wide for it. sharedStackDeployNetworkFull cannot hold that. Its
// tasks sit in preparing for about 30 s, Coaming's first read comes before any node has a local
// instance of the network, and a full-ID inspect still falls through to the swarm. Here a global
// anchor service runs a task, and so holds an endpoint of the network, on all 4 nodes, and the
// case waits until each manager's full-ID inspect lists a local endpoint before it redeploys.
//
// A /27 has 29 dynamic addresses. The anchor takes 4 task addresses, 1 virtual IP and 4
// load-balancer endpoints, which is 9. wide takes 1 virtual IP, which leaves 19 for its tasks, so
// 13 of its 32 tasks stay new. The case asserts the outcome and never a count of tasks: the deploy
// ends network_full, naming the network and the subnet, and not on the converge timeout.
//
// After the deploy the case reads every manager by full ID again. A manager holds only its own
// endpoints and the other three hold at least 2 each, so none can show the subnet at 0 available.
// That proves the read 07-49 made would have missed the full subnet, so the verdict came from the
// swarm's count (07-51) and the case does not pass vacuously.
func sharedStackDeployNetworkFullOnEveryNode(t *testing.T, f *swarmFix) {
	if major, err := strconv.Atoi(f.sw.Engine); err == nil && major < 29 {
		t.Skipf("Engine %s reports no address usage on a network inspect (API 1.52 is Engine 29); internal/stackops covers the fallback", f.sw.Engine)
	}
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/netlocal/app:1", nil)
	image := addr + "/netlocal/app@" + digest

	const stack = "netlocal"
	t.Cleanup(func() { removeStackCLI(t, f, stack) })
	id := f.createStackCoaming(t, stack, fmt.Sprintf(netEveryNodeAnchorFmt, image))
	f.deployStackCoaming(t, id)

	netID := strings.TrimSpace(sh(t, mgr, "docker network inspect --format '{{.ID}}' "+stack+"_default"))
	for _, m := range f.sw.Managers {
		testsupport.Poll(t, time.Minute, "manager "+m.Hostname+" to hold the network "+stack+"_default locally, the steady state of UAT G-07-8 where every manager does", func() (bool, string) {
			out, err := m.Sh(execCtx(t, time.Minute), "docker network inspect --format '{{len .Containers}}' "+netID)
			if err != nil {
				return false, err.Error()
			}
			n, err := strconv.Atoi(strings.TrimSpace(out))
			return err == nil && n >= 1, "local endpoints: " + strings.TrimSpace(out)
		})
	}

	f.saveStackCoaming(t, id, paritySource{Compose: fmt.Sprintf(netEveryNodeWideFmt, image)})
	job := f.deployStackUntilEnded(t, id)

	raw, _ := json.Marshal(job)
	if job.State != "partial" {
		t.Fatalf("the deploy ended %s, want partial (some steps done, the wide service failed): %s", job.State, raw)
	}
	wide := job.item(t, "service:"+stack+"_wide")
	if wide.State != "failed" || wide.Code != "network_full" {
		t.Errorf("the wide service = state %q code %q, want failed and network_full: %s", wide.State, wide.Code, raw)
	}
	if got := wide.Result["network"]; got != stack+"_default" {
		t.Errorf("the wide service names network %q, want %s_default: %s", got, stack, raw)
	}
	if got := wide.Result["subnet"]; got != "10.231.21.0/27" {
		t.Errorf("the wide service names subnet %q, want 10.231.21.0/27: %s", got, raw)
	}
	if services := job.item(t, "services"); services.Code != "network_full" {
		t.Errorf("the services step = code %q, want network_full: %s", services.Code, raw)
	}

	// The condition: a full-ID read on every manager, each of which holds the network, shows the subnet with
	// room. The subnet must be in that Status, or the check would pass on an answer that says nothing about it.
	for _, m := range f.sw.Managers {
		var st struct {
			IPAM struct {
				Subnets map[string]struct {
					IPsInUse            uint64
					DynamicIPsAvailable uint64
				}
			}
		}
		statusOf := func(ref string) string {
			return strings.TrimSpace(sh(t, m, "docker network inspect --format '{{json .Status}}' "+ref))
		}
		byID := statusOf(netID)
		t.Logf("%s: Status by full ID: %s", m.Hostname, byID)
		t.Logf("%s: Status by name: %s", m.Hostname, statusOf(stack+"_default"))
		if err := json.Unmarshal([]byte(byID), &st); err != nil {
			t.Fatalf("%s: decode the Status by full ID %q: %v", m.Hostname, byID, err)
		}
		sub, ok := st.IPAM.Subnets["10.231.21.0/27"]
		if !ok {
			t.Fatalf("%s: the Status by full ID has no 10.231.21.0/27 entry (%s), so the case cannot show that the full-ID read sees room", m.Hostname, byID)
		}
		if sub.DynamicIPsAvailable == 0 {
			t.Errorf("%s: its full-ID read shows 10.231.21.0/27 full (%s), so the case did not reproduce G-07-8: the read 07-49 made would have seen it", m.Hostname, byID)
		}
	}
	f.awaitStackUnlocked(t, id)
}
