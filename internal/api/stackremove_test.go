// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// Remove of a stack (plan 07-27, STK-11, D-20): a job on the operation lock, audited, that deletes what the
// stack owns in a safe order and the definition with it unless the person keeps it.

// countPrefix counts the writes the fake took that start with prefix.
func countPrefix(writes []string, prefix string) int {
	n := 0
	for _, w := range writes {
		if strings.HasPrefix(w, prefix) {
			n++
		}
	}
	return n
}

func removalPreviewPath(stackID string) string { return "stacks/" + stackID + "/removal-preview" }
func removePath(stackID string) string         { return "stacks/" + stackID + "/remove" }

type removalPreviewAnswer struct {
	Services   []string `json:"services"`
	Containers []string `json:"containers"`
	Networks   []string `json:"networks"`
	Secrets    []string `json:"secrets"`
	Configs    []string `json:"configs"`
	Volumes    []struct {
		Name     string `json:"name"`
		NodeID   string `json:"nodeId"`
		NodeName string `json:"nodeName"`
	} `json:"volumes"`
	Externals []string `json:"externals"`
	HasDraft  bool     `json:"hasDraft"`
}

// removeBody is the body of a removal that ticks no volume.
func removeBody(name string, keep bool) string {
	if keep {
		return `{"confirmName":"` + name + `","volumes":[],"keepDefinition":true}`
	}
	return `{"confirmName":"` + name + `","volumes":[],"keepDefinition":false}`
}

// remove posts a removal, expects 202 and waits for the job.
func (f *envFix) removeStack(t *testing.T, c *roleCaller, stackID, body string) (string, jobAnswer) {
	t.Helper()
	rec := c.do(http.MethodPost, removePath(stackID), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST remove: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	return started.JobID, f.awaitDeploy(t, started.JobID)
}

func TestRemovalPreviewListsWhatTheStackOwnsAndNeedsAnAdmin(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "preview")
	f.deployGood(t, operator, st.ID, composeWebAndApi)

	rec := admin.do(http.MethodGet, removalPreviewPath(st.ID), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[removalPreviewAnswer](t, rec)
	if want := []string{"preview_api", "preview_web"}; !slices.Equal(got.Services, want) {
		t.Errorf("services = %v, want %v", got.Services, want)
	}
	if want := []string{"preview_default"}; !slices.Equal(got.Networks, want) {
		t.Errorf("networks = %v, want %v", got.Networks, want)
	}
	if got.HasDraft {
		t.Errorf("preview says a draft is pending, but the deployed file is the draft")
	}
	if rec := operator.do(http.MethodGet, removalPreviewPath(st.ID), ""); rec.Code != http.StatusForbidden {
		t.Errorf("an operator's preview: %d %s, want 403", rec.Code, rec.Body)
	}
}

func TestRemoveStackDeletesItsServicesAndNetworksAndItsDefinitionAndIsAudited(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "gone")
	f.deployGood(t, operator, st.ID, composeWebAndApi)

	jobID, job := f.removeStack(t, admin, st.ID, removeBody("gone", false))

	if job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	writes := f.fake.Writes()
	if n := countPrefix(writes, "DELETE /services/"); n != 2 {
		t.Errorf("writes = %v, want both services removed", writes)
	}
	if n := countPrefix(writes, "DELETE /networks/"); n != 1 {
		t.Errorf("writes = %v, want the stack's default network removed", writes)
	}
	if rec := admin.do(http.MethodGet, "stacks/"+st.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET of the removed stack: %d %s, want 404", rec.Code, rec.Body)
	}
	for _, table := range []string{"stacks", "stack_snapshots", "stack_deploys"} {
		var n int
		if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s has %d rows (%v), want the definition and its history gone", table, n, err)
		}
	}
	// The console of the removal is still readable after its stack is gone: the page follows it to its end.
	rec := admin.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+jobID, "")
	if rec.Code != http.StatusOK {
		t.Errorf("GET of the removal job: %d %s, want 200", rec.Code, rec.Body)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.removed" {
			row = &r
		}
	}
	if row == nil || row.Result != "success" || row.TargetID != st.ID || row.Details["stack"] != "gone" || row.Details["result"] != "done" ||
		row.Details["volumes"] != "0" || row.Details["kept"] != "0" || row.Details["definition"] != "deleted" {
		t.Errorf("stack.removed = %+v, want a successful row for the stack with no volumes and the definition deleted", row)
	}
}

func TestRemoveStackKeepingTheDefinitionLeavesItNotDeployedAndAbleToDeployAgain(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "keeper")
	f.deployGood(t, operator, st.ID, composeWeb127)

	_, job := f.removeStack(t, admin, st.ID, removeBody("keeper", true))

	if job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	got := readStackAnswer(t, operator, st.ID)
	if got.State != "not_deployed" {
		t.Errorf("stack after a removal that kept the definition = %+v, want Not deployed", got)
	}
	if n := countPrefix(f.fake.Writes(), "DELETE /services/"); n != 1 {
		t.Errorf("writes = %v, want the service removed", f.fake.Writes())
	}
	f.deployGood(t, operator, st.ID, composeWeb128)
	if again := readStackAnswer(t, operator, st.ID); again.State != "running" {
		t.Errorf("stack after a deploy = %+v, want Running", again)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.removed" {
			row = &r
		}
	}
	if row == nil || row.Details["definition"] != "kept" {
		t.Errorf("stack.removed = %+v, want the definition recorded as kept", row)
	}
}

func TestRemoveStackNeedsTheNameTypedExactlyAnAdminAndChangesNothingOtherwise(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "careful")
	f.deployGood(t, operator, st.ID, composeWeb127)
	writes := len(f.fake.Writes())

	for name, body := range map[string]string{
		"a different name": removeBody("careless", false), "an empty name": removeBody("", false),
		"the name in another case": removeBody("Careful", false), "the name with a space": removeBody("careful ", false),
	} {
		rec := admin.do(http.MethodPost, removePath(st.ID), body)
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "confirm_mismatch" {
			t.Errorf("%s: %d %s, want 422 confirm_mismatch", name, rec.Code, rec.Body)
		}
	}
	if rec := operator.do(http.MethodPost, removePath(st.ID), removeBody("careful", false)); rec.Code != http.StatusForbidden {
		t.Errorf("an operator's removal: %d %s, want 403", rec.Code, rec.Body)
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("writes went from %d to %d, a refused removal writes nothing", writes, got)
	}
	if got := readStackAnswer(t, operator, st.ID); got.State != "running" {
		t.Errorf("stack = %+v, want it untouched", got)
	}
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.removed" {
			t.Errorf("audit row %+v for a refused removal", r)
		}
	}
}

func TestRemoveStackThatIsAlreadyGoneIs404(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "twice")
	f.removeStack(t, admin, st.ID, removeBody("twice", false))

	rec := admin.do(http.MethodPost, removePath(st.ID), removeBody("twice", false))

	if rec.Code != http.StatusNotFound {
		t.Errorf("second removal: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := admin.do(http.MethodGet, removalPreviewPath(st.ID), ""); rec.Code != http.StatusNotFound {
		t.Errorf("preview of a removed stack: %d %s, want 404", rec.Code, rec.Body)
	}
}

func TestRemoveStackTakesTheOperationLockAndIsRefusedWhileADeployRuns(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	alpha, beta := newStack(t, admin, "alpha"), newStack(t, admin, "beta")
	f.deployGood(t, operator, beta.ID, composeWeb127)
	jobID := startStuckDeploy(t, f, operator, alpha)
	writes := len(f.fake.Writes())

	rec := admin.do(http.MethodPost, removePath(beta.ID), removeBody("beta", false))

	if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Fatalf("removal during a deploy: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "alpha") {
		t.Errorf("answer %s does not name the stack that holds the lock", rec.Body)
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("a refused removal wrote %d times", got-writes)
	}
	// Let the stuck deploy settle so the test ends clean.
	f.fake.SetTaskState("alpha_web", "", "")
	f.awaitDeploy(t, jobID)
}

// putVolumes puts volumes on the fake engine beside what the deploys made: the stack's own (they carry its labels),
// one of another stack and an external one, as a swarm would hold them once tasks had used them.
func (f *envFix) putVolumes(stack, stackID string) {
	cfg := swarmManagerEngine()
	cfg.TasksFollowWrites = true
	owned := map[string]string{"com.docker.stack.namespace": stack, "bayplan.stack.id": stackID}
	cfg.Volumes = []dockertest.FakeVolume{
		{Name: stack + "_data", Driver: "local", Labels: owned},
		{Name: stack + "_cache", Driver: "local", Labels: owned},
		{Name: "outside_data", Driver: "local", Labels: map[string]string{"com.docker.stack.namespace": "outside", "bayplan.stack.id": "someone-else"}},
		{Name: "shared_vol", Driver: "local"},
	}
	f.fake.Reconfigure(cfg)
}

func volumesBody(name string, keep bool, vols ...[2]string) string {
	var parts []string
	for _, v := range vols {
		parts = append(parts, `{"name":"`+v[0]+`","nodeId":"`+v[1]+`"}`)
	}
	k := "false"
	if keep {
		k = "true"
	}
	return `{"confirmName":"` + name + `","volumes":[` + strings.Join(parts, ",") + `],"keepDefinition":` + k + `}`
}

func TestRemoveStackDeletesTheTickedVolumeAndKeepsTheOthersAndAuditsTheCounts(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "vols")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.putVolumes("vols", st.ID)
	preview := answerOf[removalPreviewAnswer](t, admin.do(http.MethodGet, removalPreviewPath(st.ID), ""))
	if len(preview.Volumes) != 2 || preview.Volumes[0].Name != "vols_cache" || preview.Volumes[1].Name != "vols_data" {
		t.Fatalf("preview volumes = %+v, want the stack's own two and neither another stack's nor the external one", preview.Volumes)
	}
	node := preview.Volumes[1].NodeID

	_, job := f.removeStack(t, admin, st.ID, volumesBody("vols", false, [2]string{"vols_data", node}))

	if job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	if got := writesWith(f.fake.Writes(), "DELETE /volumes/"); !slices.Equal(got, []string{"DELETE /volumes/vols_data"}) {
		t.Errorf("volume writes = %v, want only the ticked vols_data", got)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.removed" {
			row = &r
		}
	}
	if row == nil || row.Details["volumes"] != "1" || row.Details["kept"] != "1" || row.Details["definition"] != "deleted" || row.Details["result"] != "done" {
		t.Errorf("stack.removed = %+v, want 1 volume removed and 1 kept", row)
	}
}

func TestRemoveStackRefusesAVolumeThatIsNotTheStacksAndRemovesNothing(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "guard")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.putVolumes("guard", st.ID)
	preview := answerOf[removalPreviewAnswer](t, admin.do(http.MethodGet, removalPreviewPath(st.ID), ""))
	node := preview.Volumes[0].NodeID
	writes := len(f.fake.Writes())

	for name, vol := range map[string]string{"another stack's volume": "outside_data", "an external volume": "shared_vol", "one that is not there": "nowhere"} {
		rec := admin.do(http.MethodPost, removePath(st.ID), volumesBody("guard", false, [2]string{"guard_data", node}, [2]string{vol, node}))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "volume_not_owned" {
			t.Errorf("%s: %d %s, want 422 volume_not_owned", name, rec.Code, rec.Body)
		}
	}
	rec := admin.do(http.MethodPost, removePath(st.ID), volumesBody("guard", false, [2]string{"guard_data", "node-of-nowhere"}))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "volume_not_owned" {
		t.Errorf("a volume on a node that holds none: %d %s, want 422 volume_not_owned", rec.Code, rec.Body)
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("writes went from %d to %d, a refused removal writes nothing", writes, got)
	}
	if got := readStackAnswer(t, operator, st.ID); got.State != "running" {
		t.Errorf("stack = %+v, want it untouched", got)
	}
}

// downCalls records the project names the Compose SDK's down stand-in was asked to bring down.
type downCalls struct {
	mu    sync.Mutex
	calls []string
	err   error
	after func()
}

func (d *downCalls) run(_ context.Context, _ *dockerx.Client, project string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, project)
	if d.err == nil && d.after != nil {
		d.after()
	}
	return d.err
}

func (d *downCalls) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func TestRemoveStackOfAStandaloneStackRunsTheSDKsDownThenRemovesTheTickedVolume(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	calls := &downCalls{}
	f.stackDown = calls.run
	f.h = f.build(t)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy = %+v", job)
	}
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	owned := map[string]string{"com.docker.compose.project": "solo", "bayplan.stack.id": st.ID}
	cfg.Volumes = []dockertest.FakeVolume{
		{Name: "solo_data", Driver: "local", Labels: owned}, {Name: "solo_keep", Driver: "local", Labels: owned}, {Name: "external_vol", Driver: "local"},
	}
	cfg.Containers = []dockertest.FakeContainer{{
		ID: "c1", Names: []string{"/solo-web-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "solo", "com.docker.compose.service": "web", "bayplan.stack.id": st.ID},
	}}
	f.fake.Reconfigure(cfg)
	calls.after = func() {
		// The SDK removes the containers of the project.
		down := cfg
		down.Containers = nil
		f.fake.Reconfigure(down)
	}
	preview := answerOf[removalPreviewAnswer](t, admin.do(http.MethodGet, removalPreviewPath(st.ID), ""))
	if !slices.Equal(preview.Containers, []string{"solo-web-1"}) || len(preview.Volumes) != 2 {
		t.Fatalf("preview = %+v, want the container and the stack's two volumes", preview)
	}
	node := preview.Volumes[0].NodeID

	_, job := f.removeStack(t, admin, st.ID, volumesBody("solo", false, [2]string{"solo_data", node}))

	if job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	if got := calls.seen(); !slices.Equal(got, []string{"solo"}) {
		t.Errorf("SDK down calls = %v, want one for the project solo", got)
	}
	if got := writesWith(f.fake.Writes(), "DELETE /volumes/"); !slices.Equal(got, []string{"DELETE /volumes/solo_data"}) {
		t.Errorf("volume writes = %v, want only the ticked solo_data", got)
	}
	if rec := admin.do(http.MethodGet, "stacks/"+st.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET of the removed stack: %d, want 404", rec.Code)
	}
}

func writesWith(writes []string, prefix string) []string {
	var out []string
	for _, w := range writes {
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	return out
}
