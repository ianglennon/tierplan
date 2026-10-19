// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package agents_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/server/servertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The standalone stack suite of plan 07-21 (STK-02, D-17, D-27): Bayplan's API deploys a compose file to a
// standalone host through a real agent, so the Compose SDK's requests cross the tunnel and the agent's route
// table and body rules, and the host's engine does what they ask.

const (
	// standaloneDeployWait is how long a Bayplan deploy job gets to end, pulls and health waits included.
	standaloneDeployWait = 5 * time.Minute
	// standaloneToken is the content of the stack's secret file; standaloneConfText is its config's.
	standaloneToken    = "s3cret-token-0123456789"
	standaloneConfText = "worker_processes 1;\n"
)

// saFix is a standalone host with a real agent, a Bayplan server it is enrolled in, and a registry of the
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
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})
	f := &saFix{host: host, srv: srv, registry: dockertest.StartRegistry(t, host, o)}
	f.env = srv.CreateAgentEnvironment("")
	sh(t, host, f.env.Install.Standalone.Command)
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
	State string      `json:"state"`
	Items []saJobItem `json:"items"`
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
	image := f.registry + "/bp/app:1"
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
// to a standalone host through a real agent (plan 07-21, IMG-02 for stacks, D-27): Bayplan pulls the image with
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
		t.Errorf("the host has a Docker client configuration after the deploy: Bayplan must not log in")
	}
	f.noDenials(t)
}
