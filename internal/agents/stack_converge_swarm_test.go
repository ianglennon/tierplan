// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
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

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The real-swarm cases of plans 07-46 and 07-49 for UAT gaps G-07-2 and G-07-5: a deploy whose
// service the swarm will not start again, and one whose network has no free addresses, each ends
// the deploy on the reason instead of on the converge timeout. They deploy through Bayplan's API
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
	id := f.createStackBayplan(t, stack, fmt.Sprintf(convergeFailComposeFmt, image))
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
	id := f.createStackBayplan(t, stack, fmt.Sprintf(networkFullComposeFmt, image))
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
