// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package agents_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/server/servertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The standalone stack suite of plan 07-21 (STK-02, D-17, D-27): TierPlan's API deploys a compose file to a
// standalone host through a real agent, so the Compose SDK's requests cross the tunnel and the agent's route
// table and body rules, and the host's engine does what they ask.

const (
	// standaloneDeployWait is how long a TierPlan deploy job gets to end, pulls and health waits included.
	standaloneDeployWait = 5 * time.Minute
	// standaloneToken is the content of the stack's secret file; standaloneConfText is its config's.
	standaloneToken    = "s3cret-token-0123456789"
	standaloneConfText = "worker_processes 1;\n"
)

// saFix is a standalone host with a real agent, a TierPlan server it is enrolled in, and a registry of the
// host's own that the images of the stacks come from.
type saFix struct {
	host *dockertest.Node
	srv  *servertest.Server
	env  servertest.Created
	// registry is how the host's engine reaches its registry.
	registry string
}

// startSAFix starts a dind host, loads the agent image into it, enrols the agent, and starts a registry on the
// host. The registry requires a sign-in when o has an htpasswd file.
func startSAFix(t *testing.T, o dockertest.RegistryOptions) *saFix {
	t.Helper()
	return startSAFixWith(t, o, false)
}

// startSAFixWith is startSAFix with the one setting a host owner adds to let containers that share another
// container's namespaces or volumes through (TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES, D-36), put in the
// command the server generated and nothing else changed. A stack whose service has network_mode, ipc or pid
// service:X, or volumes_from X, creates a container with a container:<id> mode or VolumesFrom, which the
// agent refuses as host access without it (docs/agent.md, "Container creates and host access").
func startSAFixWith(t *testing.T, o dockertest.RegistryOptions, shareNamespaces bool) *saFix {
	t.Helper()
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})
	f := &saFix{host: host, srv: srv, registry: dockertest.StartRegistry(t, host, o)}
	f.env = srv.CreateAgentEnvironment("")
	cmd := f.env.Install.Standalone.Command
	if shareNamespaces {
		cmd = strings.Replace(cmd, " "+f.env.Install.Image, " -e TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true "+f.env.Install.Image, 1)
		if cmd == f.env.Install.Standalone.Command {
			t.Fatalf("the generated standalone command has no image to put the setting before:\n%s", cmd)
		}
	}
	sh(t, host, cmd)
	srv.WaitOnline(f.env.ID, 60*time.Second)
	if agents := srv.Agents(f.env.ID); len(agents) != 1 {
		t.Fatalf("agents = %+v, want the one that enrolled", agents)
	}
	return f
}

// pushBusybox puts busybox in the host's registry as ref, signing in for the push when login is given, and
// removes every copy the host's engine holds, so the deploy has to fetch the image.
func (f *saFix) pushBusybox(t *testing.T, ref string, login *dockertest.Login) string {
	t.Helper()
	digest := dockertest.PushImage(t, f.host, "busybox:1.36.1", ref, login)
	if login != nil {
		dockertest.Logout(t, f.host)
	}
	sh(t, f.host, "docker image rm "+ref+" busybox:1.36.1")
	return digest
}

// saCorpus reads a compose file of testdata/stacks and puts the values where {{KEY}} stands.
func saCorpus(t *testing.T, name string, values map[string]string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "stacks", name)) //nolint:gosec // a test reads its own corpus file, named by the test
	if err != nil {
		t.Fatalf("read the corpus file %s: %v", name, err)
	}
	text := string(raw)
	for k, v := range values {
		text = strings.ReplaceAll(text, "{{"+k+"}}", v)
	}
	if i := strings.Index(text, "{{"); i >= 0 {
		t.Fatalf("the corpus file %s has a value no one gave: %s", name, text[i:min(len(text), i+30)])
	}
	return text
}

// saFile is a file beside the compose file of a stack.
type saFile struct{ Path, Content string }

func (f *saFix) api() string { return "/api/env/" + f.env.ID + "/stacks" }

// saJob is a deploy job as the route answers it.
type saJob struct {
	State  string      `json:"state"`
	Items  []saJobItem `json:"items"`
	Output []struct {
		Text string `json:"text"`
	} `json:"output"`
}

// outputText is the job's output log, a line each.
func (j saJob) outputText() string {
	var b strings.Builder
	for _, l := range j.Output {
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

type saJobItem struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Parent  string `json:"parent"`
	State   string `json:"state"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// create makes the stack with its compose file and saves the files beside it. It returns the stack's ID.
func (f *saFix) create(t *testing.T, name, compose string, files []saFile) string {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.api(), map[string]any{"name": name, "compose": compose})
	var st struct {
		ID string `json:"id"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &st) != nil || st.ID == "" {
		t.Fatalf("POST stacks (%s) = %d %s", name, code, body)
	}
	if len(files) > 0 {
		f.save(t, st.ID, compose, files)
	}
	return st.ID
}

// save replaces the draft of a stack: its compose file and the files beside it.
func (f *saFix) save(t *testing.T, stackID, compose string, files []saFile) {
	t.Helper()
	all := []map[string]any{{"path": "compose.yaml", "content": compose, "secret": false}}
	for _, sf := range files {
		all = append(all, map[string]any{"path": sf.Path, "content": sf.Content, "secret": false})
	}
	code, body := f.srv.Do(http.MethodPut, f.api()+"/"+stackID+"/draft", map[string]any{
		"version": f.draftVersion(t, stackID), "files": all, "vars": []map[string]any{},
	})
	if code != http.StatusOK {
		t.Fatalf("PUT the draft of stack %s = %d %s", stackID, code, body)
	}
}

func (f *saFix) draftVersion(t *testing.T, stackID string) int64 {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, f.api()+"/"+stackID+"/draft", nil)
	var d struct {
		Version int64 `json:"version"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &d) != nil {
		t.Fatalf("GET the draft of stack %s = %d %s", stackID, code, body)
	}
	return d.Version
}

// review reviews the stack's draft and returns the images it chose, by service.
func (f *saFix) review(t *testing.T, stackID string) map[string]string {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.api()+"/"+stackID+"/review", map[string]any{})
	var rv struct {
		Pins map[string]string `json:"pins"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil || len(rv.Pins) == 0 {
		t.Fatalf("POST the review of stack %s = %d %s", stackID, code, body)
	}
	return rv.Pins
}

// deploy deploys the stack's draft and waits until the job ends. It returns the job as it ended.
func (f *saFix) deploy(t *testing.T, stackID string) saJob { return f.deployPins(t, stackID, nil) }

// deployPins is deploy with the images a review chose, which the deploy then uses without a lookup.
func (f *saFix) deployPins(t *testing.T, stackID string, pins map[string]string) saJob {
	t.Helper()
	body0 := map[string]any{"draftVersion": f.draftVersion(t, stackID)}
	if pins != nil {
		body0["pins"] = pins
	}
	code, body := f.srv.Do(http.MethodPost, f.api()+"/"+stackID+"/deploys", body0)
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the deploy of stack %s = %d %s", stackID, code, body)
	}
	var job saJob
	testsupport.Poll(t, standaloneDeployWait, "the deploy job "+started.JobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.api()+"/"+stackID+"/deploys/"+started.JobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = saJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	return job
}

// deployOK deploys and fails the test unless the job is done.
func (f *saFix) deployOK(t *testing.T, stackID string) saJob {
	t.Helper()
	job := f.deploy(t, stackID)
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the deploy of stack %s ended %s, want done: %s", stackID, job.State, raw)
	}
	return job
}

// containers are the containers of a Compose project on the host, by service, as "<id> <state>" (every one the
// host holds, stopped ones too).
func (f *saFix) containers(t *testing.T, project string) map[string][]string {
	t.Helper()
	out := sh(t, f.host, `docker ps -a --filter label=com.docker.compose.project=`+project+
		` --format '{{.Label "com.docker.compose.service"}} {{.ID}} {{.State}}'`)
	got := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 {
			got[fields[0]] = append(got[fields[0]], fields[1]+" "+fields[2])
		}
	}
	return got
}

// noDenials fails the test when the agent refused a request during the deploy: the allowlist has to suffice.
func (f *saFix) noDenials(t *testing.T) {
	t.Helper()
	if rows := f.srv.AuditRows("agent.policy_denied"); len(rows) != 0 {
		t.Errorf("the agent refused %d requests during the deploy, want none: %+v", len(rows), rows)
	}
}

// TestStandaloneStackThroughAnAgent deploys a compose stack to a standalone host through a real agent
// (STK-02, A10): two services, a user network, a named volume, a healthcheck, depends_on with
// service_healthy, a stack-file secret and a config. The secret and the config reach the container through
// the agent's archive route, not as a bind of a file, the agent records no refusal, an edit of one service
// recreates only its container, and a redeploy nobody edited changes no container.
func TestStandaloneStackThroughAnAgent(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	image := f.registry + "/tp/app:1"
	f.pushBusybox(t, image, nil)

	const name = "sfull"
	files := []saFile{{Path: "tok.txt", Content: standaloneToken}, {Path: "nginx.conf", Content: standaloneConfText}}
	compose := func(mode string) string {
		return saCorpus(t, "standalone-full.yaml", map[string]string{"IMAGE": image, "MODE": mode})
	}
	id := f.create(t, name, compose("one"), files)

	// 1. The first deploy brings both services up, db healthy before web starts.
	job := f.deployOK(t, id)
	first := f.containers(t, name)
	for _, svc := range []string{"db", "web"} {
		if len(first[svc]) != 1 || !strings.HasSuffix(first[svc][0], " running") {
			t.Fatalf("containers of %s after the deploy = %v, want one running (all: %v, job: %+v)", svc, first[svc], first, job.Items)
		}
	}
	if got := strings.TrimSpace(sh(t, f.host, "docker inspect -f '{{.State.Health.Status}}' "+name+"-db-1")); got != "healthy" {
		t.Errorf("db health = %q, want healthy: web waited for it", got)
	}
	// The secret and the config are files inside the container, written through the archive route.
	if got := sh(t, f.host, "docker exec "+name+"-db-1 cat /run/secrets/tok"); got != standaloneToken {
		t.Errorf("/run/secrets/tok = %q, want the stack file's content %q", got, standaloneToken)
	}
	if got := sh(t, f.host, "docker exec "+name+"-db-1 cat /etc/nginx.conf"); got != standaloneConfText {
		t.Errorf("/etc/nginx.conf = %q, want the stack file's content %q", got, standaloneConfText)
	}
	// Nothing of the server's disk is mounted: no bind carries a secret or a config.
	if mounts := strings.TrimSpace(sh(t, f.host, "docker inspect -f '{{range .Mounts}}{{.Type}}:{{.Destination}} {{end}}' "+name+"-db-1")); strings.Contains(mounts, "bind:") {
		t.Errorf("db mounts = %q, want no bind", mounts)
	}
	// The one entry of HostConfig.Binds is the named volume; a secret or a config would be a host path.
	if got := strings.TrimSpace(sh(t, f.host, "docker inspect -f '{{range .HostConfig.Binds}}{{println .}}{{end}}' "+name+"-db-1")); got != name+"_data:/data:rw" {
		t.Errorf("db binds = %q, want the named volume alone: a secret or config arrives by the archive route", got)
	}
	// The user network, the default one and the volume exist, labelled with the project.
	for _, want := range []string{name + "_back", name + "_front"} {
		if got := sh(t, f.host, "docker network ls --filter label=com.docker.compose.project="+name+" --format '{{.Name}}'"); !strings.Contains(got, want) {
			t.Errorf("networks of the project = %q, want %s", got, want)
		}
	}
	if got := sh(t, f.host, "docker volume ls --filter label=com.docker.compose.project="+name+" --format '{{.Name}}'"); !strings.Contains(got, name+"_data") {
		t.Errorf("volumes of the project = %q, want %s_data", got, name)
	}
	f.noDenials(t)

	// 2. A redeploy nobody edited changes no container.
	f.deployOK(t, id)
	if again := f.containers(t, name); fmt.Sprint(again) != fmt.Sprint(first) {
		t.Errorf("containers after an unedited redeploy = %v, want %v", again, first)
	}

	// 3. An edit of web's environment recreates web alone.
	f.save(t, id, compose("two"), files)
	f.deployOK(t, id)
	edited := f.containers(t, name)
	if fmt.Sprint(edited["db"]) != fmt.Sprint(first["db"]) {
		t.Errorf("db after an edit of web = %v, want the same container %v", edited["db"], first["db"])
	}
	if len(edited["web"]) != 1 || edited["web"][0] == first["web"][0] || !strings.HasSuffix(edited["web"][0], " running") {
		t.Errorf("web after an edit = %v, want one new running container (was %v)", edited["web"], first["web"])
	}
	if got := strings.TrimSpace(sh(t, f.host, "docker exec "+name+"-web-1 printenv MODE")); got != "two" {
		t.Errorf("MODE in web = %q, want two", got)
	}
	// Nothing of the recreate is left behind: the SDK's temporary container is gone.
	if all := sh(t, f.host, "docker ps -aq --filter label=com.docker.compose.project="+name); len(strings.Fields(all)) != 2 {
		t.Errorf("containers of the project after the edit = %q, want two", all)
	}
	f.noDenials(t)
}

// TestStandalonePrivateImageThroughAnAgent deploys a stack whose image is in a registry that requires a sign-in,
// to a standalone host through a real agent (plan 07-21, IMG-02 for stacks, D-27): TierPlan pulls the image with
// the credential it stores, as a step of the deploy, and the Compose SDK finds it on the host and needs no
// credential. The host is never signed in and holds no Docker client configuration. A wrong password stops the
// deploy at the pull step with pull_unauthorized, records the failure on the credential and creates nothing; a
// corrected one deploys and clears it.
func TestStandalonePrivateImageThroughAnAgent(t *testing.T) {
	const user, password = "bot", "registry-pass-0123456789"
	f := startSAFix(t, dockertest.RegistryOptions{Htpasswd: dockertest.HtpasswdFor(user, password)})
	image := f.registry + "/private/app:1"
	// Signed in for the push only. From here on the host knows no credential, and the deploy has to fetch.
	pushed := f.pushBusybox(t, image, &dockertest.Login{User: user, Password: password})

	creds := registry.NewStore(f.srv.Core().Store(), f.srv.Core().Vault(), nil)
	ctx := context.Background()
	cred, err := creds.Create(ctx, registry.CreateInput{
		Name: "private-bot", Type: registry.TypeGeneric, Host: f.registry, Prefix: "private", Username: user, Secret: password,
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

	const name = "spriv"
	id := f.create(t, name, saCorpus(t, "standalone-private.yaml", map[string]string{"IMAGE": image}), nil)
	setPassword := func(secret string) {
		t.Helper()
		if _, _, err := creds.Update(ctx, cred.ID, registry.UpdateInput{
			Name: cred.Name, Type: cred.Type, Host: cred.Host, Prefix: cred.Prefix, Username: user, Secret: secret,
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	pullStep := func(job saJob) saJobItem {
		for _, it := range job.Items {
			if strings.HasPrefix(it.ID, "pull:") {
				return it
			}
		}
		return saJobItem{}
	}

	// The review pins the image with the right password; the password is then changed, so the deploy has to
	// pull with a wrong one: the pull step fails with the code, the failure is recorded on the credential,
	// and nothing is created.
	pins := f.review(t, id)
	setPassword("wrong-" + password)
	job := f.deployPins(t, id, pins)
	if job.State == "done" {
		t.Fatalf("the deploy with a wrong password ended done: %+v", job)
	}
	if pull := pullStep(job); pull.State != "failed" || pull.Code != "pull_unauthorized" || pull.Parent != "lookup" {
		t.Fatalf("pull step = %+v, want failed with pull_unauthorized under the lookup (items: %+v)", pull, job.Items)
	}
	if fl := failureOf(); fl == nil || fl.Code != "pull_unauthorized" {
		t.Fatalf("credential failure = %+v, want pull_unauthorized", fl)
	}
	if got := f.containers(t, name); len(got) != 0 {
		t.Errorf("containers after a failed pull = %v, want none", got)
	}
	if raw, _ := json.Marshal(job); strings.Contains(string(raw), password) {
		t.Errorf("the job carries the password: %s", raw)
	}

	// The right password: the pull works, the service runs the pinned image, the failure is cleared.
	setPassword(password)
	job = f.deployOK(t, id)
	if pull := pullStep(job); pull.State != "done" || pull.Label != "Pull "+image {
		t.Errorf("pull step = %+v, want done, 'Pull %s'", pull, image)
	}
	got := f.containers(t, name)
	if len(got["app"]) != 1 || !strings.HasSuffix(got["app"][0], " running") {
		t.Fatalf("containers after the deploy = %v, want app running", got)
	}
	if ran := strings.TrimSpace(sh(t, f.host, "docker inspect -f '{{.Config.Image}}' "+name+"-app-1")); ran != image+"@"+pushed {
		t.Errorf("the container runs %q, want the pin %q", ran, image+"@"+pushed)
	}
	if fl := failureOf(); fl != nil {
		t.Errorf("credential failure = %+v, want it cleared by a pull that worked", fl)
	}
	if dockertest.HasClientConfig(t, f.host) {
		t.Errorf("the host has a Docker client configuration after the deploy: TierPlan must not log in")
	}
	f.noDenials(t)
}

// saReviewRow is one service row of a review as the route answers it.
type saReviewRow struct {
	Short    string `json:"short"`
	Change   string `json:"change"`
	Because  string `json:"because"`
	Settings int    `json:"settings"`
	Current  any    `json:"current"`
	Next     any    `json:"next"`
}

// reviewRows reviews the stack's draft and returns its service rows and how many other resources it lists.
func (f *saFix) reviewRows(t *testing.T, stackID string) ([]saReviewRow, int) {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.api()+"/"+stackID+"/review", map[string]any{})
	var rv struct {
		Services []saReviewRow     `json:"services"`
		Objects  []json.RawMessage `json:"objects"`
		Problems []struct {
			Severity, Code, Message string
		} `json:"problems"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil {
		t.Fatalf("POST the review of stack %s = %d %s", stackID, code, body)
	}
	for _, p := range rv.Problems {
		if p.Severity == "error" {
			t.Fatalf("the review of stack %s has a blocking problem: %s", stackID, body)
		}
	}
	return rv.Services, len(rv.Objects)
}

// words are the rows of a review as "change service", with "because parent" for a carried one.
func (rows saRows) words() []string {
	var out []string
	for _, r := range rows {
		w := r.Change + " " + r.Short
		if r.Because != "" {
			w += " because " + r.Because
		}
		out = append(out, w)
	}
	return out
}

type saRows []saReviewRow

// saState is what a test reads of a container to tell whether it is the one it was.
type saState struct{ ID, Started, ImageID, ImageRef string }

// states reads every container of a project: its ID, when it last started, the image it was created from
// and the reference it was created with.
func (f *saFix) states(t *testing.T, project string) map[string]saState {
	t.Helper()
	out := sh(t, f.host, `docker ps -a --filter label=com.docker.compose.project=`+project+
		` --format '{{.Label "com.docker.compose.service"}} {{.Names}}'`)
	got := map[string]saState{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		raw := strings.TrimSpace(sh(t, f.host, `docker inspect -f '{{.Id}} {{.State.StartedAt}} {{.Image}} {{.Config.Image}}' `+fields[1]))
		parts := strings.Fields(raw)
		if len(parts) != 4 {
			t.Fatalf("inspect of %s = %q", fields[1], raw)
		}
		got[fields[0]] = saState{ID: parts[0], Started: parts[1], ImageID: parts[2], ImageRef: parts[3]}
	}
	return got
}

// TestStandaloneReviewEmptyAfterDeployThroughAnAgent is the completeness test of the review's normaliser
// (plan 07-22, STK-06, Pitfall 5) on a host behind a real agent: right after a deploy of the full stack of
// 07-21 (networks, a volume, a healthcheck, depends_on, a secret and a config) the review lists nothing.
func TestStandaloneReviewEmptyAfterDeployThroughAnAgent(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	image := f.registry + "/tp/app:1"
	f.pushBusybox(t, image, nil)

	const name = "srev"
	files := []saFile{{Path: "tok.txt", Content: standaloneToken}, {Path: "nginx.conf", Content: standaloneConfText}}
	id := f.create(t, name, saCorpus(t, "standalone-full.yaml", map[string]string{"IMAGE": image, "MODE": "one"}), files)

	if rows, objects := f.reviewRows(t, id); len(rows) != 2 || objects == 0 {
		t.Fatalf("the review before the deploy lists %v and %d other resources, want the services added", saRows(rows).words(), objects)
	}
	f.deployOK(t, id)

	rows, objects := f.reviewRows(t, id)
	if len(rows) != 0 || objects != 0 {
		raw, _ := json.Marshal(rows)
		t.Errorf("the review right after a deploy lists %v and %d other resources, want nothing: %s", saRows(rows).words(), objects, raw)
	}
	f.noDenials(t)
}

// TestStandaloneHandChangedContainerIsReviewedAndRedeployed is the proof that the review predicts what the
// Compose SDK does (plan 07-22, STK-06): web is changed by hand on the host (docker update keeps every
// compose label and the config hash), the review lists web changed at its restart policy, the sidecar that
// shares web's network as recreated because of web and the restarter that depends on web with restart: true
// as restarted, and nothing else; a deploy with no edit then does exactly that, and the next review is empty.
func TestStandaloneHandChangedContainerIsReviewedAndRedeployed(t *testing.T) {
	// The sidecar shares web's network namespace, which the agent refuses without the host owner's opt-in.
	f := startSAFixWith(t, dockertest.RegistryOptions{}, true)
	image := f.registry + "/tp/app:1"
	f.pushBusybox(t, image, nil)

	const name = "sdep"
	id := f.create(t, name, saCorpus(t, "standalone-dependents.yaml", map[string]string{"IMAGE": image}), nil)
	f.deployOK(t, id)
	if rows, objects := f.reviewRows(t, id); len(rows) != 0 || objects != 0 {
		t.Fatalf("the review right after the deploy lists %v and %d other resources, want nothing", saRows(rows).words(), objects)
	}
	first := f.states(t, name)
	if len(first) != 5 {
		t.Fatalf("containers after the deploy = %v, want five", first)
	}

	sh(t, f.host, "docker update --restart always "+name+"-web-1")
	rows, _ := f.reviewRows(t, id)
	want := []string{"changed web", "recreated sidecar because web", "restarted restarter because web"}
	if got := saRows(rows).words(); !slices.Equal(got, want) {
		t.Fatalf("the review of a hand-changed web lists %v, want %v", got, want)
	}
	if rows[0].Settings != 1 {
		t.Errorf("web is changed at %d settings, want its restart policy alone", rows[0].Settings)
	}
	cur, _ := json.Marshal(rows[0].Current)
	next, _ := json.Marshal(rows[0].Next)
	if !strings.Contains(string(cur), `"restart":"always"`) || !strings.Contains(string(next), `"restart":"unless-stopped"`) {
		t.Errorf("web: current %s next %s, want the restart policy always against unless-stopped", cur, next)
	}

	// A deploy with no edit puts web right, recreates what shares its network and restarts what depends on it.
	job := f.deployOK(t, id)
	after := f.states(t, name)
	if len(after) != 5 {
		t.Fatalf("containers after the redeploy = %v, want five and none left over", after)
	}
	for _, svc := range []string{"web", "sidecar"} {
		if after[svc].ID == first[svc].ID {
			t.Errorf("%s keeps its container %s, want it recreated", svc, first[svc].ID)
		}
	}
	if after["sidecar"].ImageID != first["sidecar"].ImageID || after["sidecar"].ImageRef != first["sidecar"].ImageRef {
		t.Errorf("the recreated sidecar runs %s (%s), want the image it was pinned to, %s (%s)",
			after["sidecar"].ImageRef, after["sidecar"].ImageID, first["sidecar"].ImageRef, first["sidecar"].ImageID)
	}
	if after["restarter"].ID != first["restarter"].ID || !after["restarter"].startedAfter(first["restarter"]) {
		t.Errorf("restarter: %+v after, %+v before, want the same container started again", after["restarter"], first["restarter"])
	}
	for _, svc := range []string{"worker", "other"} {
		if after[svc] != first[svc] {
			t.Errorf("%s: %+v after, %+v before, want the container untouched", svc, after[svc], first[svc])
		}
	}
	if got := strings.TrimSpace(sh(t, f.host, "docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "+name+"-web-1")); got != "unless-stopped" {
		t.Errorf("web restart policy = %q after the deploy, want the file's unless-stopped", got)
	}
	// The SDK's own events say what the review said: the sidecar recreated, the restarter stopped and started.
	lines := job.outputText()
	for _, want := range []string{"Recreated container " + name + "-sidecar-1", "Stopping container " + name + "-restarter-1", "Started container " + name + "-restarter-1"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the deploy's output lacks %q:\n%s", want, lines)
		}
	}
	// The two it left alone are waited for and found running, never stopped, started, created or recreated.
	for _, line := range strings.Split(lines, "\n") {
		if !strings.Contains(line, name+"-worker-1") && !strings.Contains(line, name+"-other-1") {
			continue
		}
		for _, did := range []string{"Stopping", "Stopped", "Starting", "Started", "Creating", "Created", "Recreate", "Recreated", "Removing", "Removed"} {
			if strings.HasPrefix(line, did+" container ") {
				t.Errorf("the deploy's output says %q of a container it left alone:\n%s", line, lines)
			}
		}
	}

	if rows, objects := f.reviewRows(t, id); len(rows) != 0 || objects != 0 {
		t.Errorf("the review after the redeploy lists %v and %d other resources, want nothing", saRows(rows).words(), objects)
	}
	f.noDenials(t)
}

// startedAfter reports whether s started later than before did (their StartedAt are RFC 3339 with
// nanoseconds, which compare as text only when they are written the same way, so they are parsed).
func (s saState) startedAfter(before saState) bool {
	a, err1 := time.Parse(time.RFC3339Nano, s.Started)
	b, err2 := time.Parse(time.RFC3339Nano, before.Started)
	return err1 == nil && err2 == nil && a.After(b)
}

// TestStandaloneRemoveKeepsUntickedVolumes removes a stack from a standalone host through a real agent (plan
// 07-27, STK-11, D-20): the Compose SDK's down removes the container and the stack's own network, the one
// volume the person ticked is deleted, the one they did not tick stays, and the external network and the
// external volume the file names are not touched. The agent records no refusal: the requests of a down and of a
// volume removal are on its allowlist.
func TestStandaloneRemoveKeepsUntickedVolumes(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	image := f.registry + "/tp/app:1"
	f.pushBusybox(t, image, nil)

	const name, outer, outerVolume = "srm", "srmouter", "srmextvol"
	sh(t, f.host, "docker network create "+outer)
	sh(t, f.host, "docker volume create "+outerVolume)
	compose := "services:\n  app:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n" +
		"    networks: [back, outer]\n    volumes:\n      - keepme:/keep\n      - dropme:/drop\n      - extvol:/ext\n" +
		"networks:\n  back: {}\n  outer:\n    external: true\n    name: " + outer + "\n" +
		"volumes:\n  keepme: {}\n  dropme: {}\n  extvol:\n    external: true\n    name: " + outerVolume + "\n"
	id := f.create(t, name, compose, nil)
	f.deployOK(t, id)
	if got := f.containers(t, name); len(got["app"]) != 1 || !strings.HasSuffix(got["app"][0], " running") {
		t.Fatalf("containers after the deploy = %v, want one running", got)
	}

	// The preview lists what the stack owns; the external volume and network are not offered.
	code, body := f.srv.Do(http.MethodGet, f.api()+"/"+id+"/removal-preview", nil)
	var preview struct {
		Containers []string `json:"containers"`
		Networks   []string `json:"networks"`
		Volumes    []struct {
			Name   string `json:"name"`
			NodeID string `json:"nodeId"`
		} `json:"volumes"`
		Externals []string `json:"externals"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &preview) != nil {
		t.Fatalf("GET the removal preview = %d %s", code, body)
	}
	if !slices.Equal(preview.Containers, []string{name + "-app-1"}) || !slices.Equal(preview.Networks, []string{name + "_back"}) {
		t.Errorf("preview = %+v, want the container and the stack's own network", preview)
	}
	var drop string
	var names []string
	for _, v := range preview.Volumes {
		names = append(names, v.Name)
		if v.Name == name+"_dropme" {
			drop = v.NodeID
		}
	}
	if !slices.Equal(names, []string{name + "_dropme", name + "_keepme"}) || drop == "" {
		t.Fatalf("preview volumes = %v, want only the stack's own dropme and keepme", names)
	}
	if !slices.Contains(preview.Externals, outer) || !slices.Contains(preview.Externals, outerVolume) {
		t.Errorf("preview externals = %v, want the external network %s and volume %s named", preview.Externals, outer, outerVolume)
	}

	code, body = f.srv.Do(http.MethodPost, f.api()+"/"+id+"/remove", map[string]any{
		"confirmName": name, "keepDefinition": false,
		"volumes": []map[string]string{{"name": name + "_dropme", "nodeId": drop}},
	})
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the removal = %d %s", code, body)
	}
	var job saJob
	testsupport.Poll(t, standaloneDeployWait, "the removal job "+started.JobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.api()+"/"+id+"/deploys/"+started.JobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = saJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the removal ended %s, want done: %s", job.State, raw)
	}

	if got := f.containers(t, name); len(got) != 0 {
		t.Errorf("containers of the removed stack = %v, want none", got)
	}
	if out := strings.TrimSpace(sh(t, f.host, "docker network ls -q --filter label=com.docker.compose.project="+name)); out != "" {
		t.Errorf("networks of the removed stack = %q, want none", out)
	}
	if out := strings.TrimSpace(sh(t, f.host, "docker volume ls -q --filter name=^"+name+"_dropme$")); out != "" {
		t.Errorf("the ticked volume is still there: %q", out)
	}
	if out := strings.TrimSpace(sh(t, f.host, "docker volume ls -q --filter name=^"+name+"_keepme$")); out == "" {
		t.Errorf("the unticked volume %s_keepme was removed", name)
	}
	if out := strings.TrimSpace(sh(t, f.host, "docker volume ls -q --filter name=^"+outerVolume+"$")); out == "" {
		t.Errorf("the external volume %s was removed", outerVolume)
	}
	if ok, err := networkNamed(execCtx(t, time.Minute), f.host, outer); err != nil || !ok {
		t.Errorf("the external network %s was removed (%v)", outer, err)
	}
	if code, body := f.srv.Do(http.MethodGet, f.api()+"/"+id, nil); code != http.StatusNotFound {
		t.Errorf("GET the removed stack = %d %s, want 404", code, body)
	}
	f.noDenials(t)
}
