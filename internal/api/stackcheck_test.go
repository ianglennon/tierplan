// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/stackstest"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// Checking a stack's working copy (plan 07-06, STK-01): the editor sends what it holds, the server
// answers every problem with its file and line, and the check writes and sends nothing.

type checkProblem struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	EndLine  int    `json:"endLine"`
	EndCol   int    `json:"endCol"`
}

type checkVariable struct {
	SecretLike bool   `json:"secretLike"`
	Name       string `json:"name"`
	Value      string `json:"value"`
	Secret     bool   `json:"secret"`
	Source     string `json:"source"`
	Default    string `json:"default"`
	HasDefault bool   `json:"hasDefault"`
	Required   bool   `json:"required"`
	Unused     bool   `json:"unused"`
	Places     []struct {
		File string `json:"file"`
		Line int    `json:"line"`
	} `json:"places"`
}

type checkAnswer struct {
	Problems    []checkProblem  `json:"problems"`
	UnusedFiles []string        `json:"unusedFiles"`
	Variables   []checkVariable `json:"variables"`
}

func (c *roleCaller) check(stackID, body string) (checkAnswer, int, string) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "stacks/"+stackID+"/check", body)
	var a checkAnswer
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
			c.t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return a, rec.Code, rec.Body.String()
}

func checkBody(compose string, extra ...draftFile) string {
	files := append([]draftFile{{Path: "compose.yaml", Content: compose}}, extra...)
	b, _ := json.Marshal(struct {
		Files []draftFile `json:"files"`
		Vars  []struct{}  `json:"vars"`
	}{files, []struct{}{}})
	return string(b)
}

// engineStateReads counts the requests that read the resources a stack touches. The monitor probes
// /_ping and /info; nothing but a request that reads these goes to the lists.
func engineStateReads(requests []string) int {
	n := 0
	for _, p := range requests {
		for _, tail := range []string{"/services", "/networks", "/secrets", "/configs", "/volumes", "/containers/json", "/nodes", "/tasks"} {
			if strings.HasSuffix(p, tail) {
				n++
			}
		}
	}
	return n
}

func TestCheckIsPureAndPositioned(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "lint")

	draftBefore := readDraft(t, viewer, st.ID)
	auditBefore := len(f.stackAudits(t))
	// The first check fills the environment's cached network listing and pool read, which the explicit-subnet
	// hints compare with (plan 10-08); the checks that follow in the cache's lifetime send the engine nothing.
	viewer.check(st.ID, checkBody("services:\n  web:\n    image: nginx:1.27\n"))
	readsBefore := engineStateReads(f.fake.Requests())

	// A tab as the indentation of line 2: the problem is the file, the line and the sentence.
	answer, code, raw := viewer.check(st.ID, checkBody("services:\n\tweb:\n    image: nginx\n"))
	if code != http.StatusOK {
		t.Fatalf("check by a viewer: %d %s, want 200 (stacks.read is enough)", code, raw)
	}
	if len(answer.Problems) != 1 {
		t.Fatalf("problems = %+v, want one", answer.Problems)
	}
	p := answer.Problems[0]
	if p.Severity != "error" || p.Code != "yaml_invalid" || p.File != "compose.yaml" || p.Line != 2 ||
		p.Message != "Line 2: found character that cannot start any token" {
		t.Errorf("problem = %+v", p)
	}

	// A schema error is at the line of its key.
	answer, code, raw = viewer.check(st.ID, checkBody("services:\n  web:\n    image: nginx\n    imag: x\n"))
	if code != http.StatusOK || len(answer.Problems) != 1 || answer.Problems[0].Line != 4 || answer.Problems[0].Code != "schema_invalid" {
		t.Errorf("schema check: %d %s, want one schema_invalid at line 4", code, raw)
	}

	// A good file has no problems, and the answer always carries both lists.
	answer, code, raw = viewer.check(st.ID, checkBody("services:\n  web:\n    image: nginx:1.27\n"))
	if code != http.StatusOK || len(answer.Problems) != 0 || answer.UnusedFiles == nil {
		t.Errorf("clean check: %d %s", code, raw)
	}
	if !strings.Contains(raw, `"problems":[]`) || !strings.Contains(raw, `"unusedFiles":[]`) {
		t.Errorf("a clean answer must carry empty lists, not null: %s", raw)
	}

	// Pure: nothing written, nothing sent to an engine, no audit row, the draft where it was.
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("engine writes = %v, a check must change nothing", w)
	}
	if got := engineStateReads(f.fake.Requests()); got != readsBefore {
		t.Errorf("the check read the engine: %d state reads, was %d", got, readsBefore)
	}
	if after := len(f.stackAudits(t)); after != auditBefore {
		t.Errorf("audit rows %d -> %d: a check must write none", auditBefore, after)
	}
	if d := readDraft(t, viewer, st.ID); d.Version != draftBefore.Version {
		t.Errorf("draft version %d -> %d: a check must not save", draftBefore.Version, d.Version)
	}
}

func TestCheckRefusesWhatSaveRefuses(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "rules")

	_, code, raw := admin.check(st.ID, checkBody("services: {}\n", draftFile{Path: "../x"}))
	if code != http.StatusUnprocessableEntity || !strings.Contains(raw, "file_path_invalid") {
		t.Errorf("a bad path: %d %s, want 422 file_path_invalid", code, raw)
	}
	_, code, _ = admin.check("no-such-stack", checkBody("services: {}\n"))
	if code != http.StatusNotFound {
		t.Errorf("a stack that is not there: %d, want 404", code)
	}
}

func TestCheckRefusesWhatNamesAFileOfTheServerAndListsUnusedFiles(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "files")

	// include, an env_file outside the stack and a secret file of the server's disk are each a
	// blocking problem at their line, and nothing is read.
	const hostile = "include:\n  - /etc/hostname\nservices:\n  web:\n    image: nginx\n    env_file: /proc/self/environ\nsecrets:\n  s:\n    file: /etc/shadow\n"
	answer, code, raw := admin.check(st.ID, checkBody(hostile))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	type at struct {
		code string
		line int
	}
	var got []at
	for _, p := range answer.Problems {
		got = append(got, at{p.Code, p.Line})
		if p.Severity != "error" || p.File != "compose.yaml" {
			t.Errorf("problem = %+v, want an error in compose.yaml", p)
		}
	}
	if want := []at{{"include_unsupported", 1}, {"path_illegal", 6}, {"path_illegal", 9}}; !slices.Equal(got, want) {
		t.Errorf("problems = %v, want %v", got, want)
	}

	// A file the compose file refers to is used; one it never mentions is listed.
	answer, code, raw = admin.check(st.ID, checkBody("services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n",
		draftFile{Path: "app.env", Content: "A=1\n"}, draftFile{Path: "notes.txt"}))
	if code != http.StatusOK || len(answer.Problems) != 0 {
		t.Fatalf("check: %d %s", code, raw)
	}
	if len(answer.UnusedFiles) != 1 || answer.UnusedFiles[0] != "notes.txt" {
		t.Errorf("unusedFiles = %v, want [notes.txt]", answer.UnusedFiles)
	}
}

func TestCheckAnswersWhatTheTargetIgnoresAsWarningsAtTheirLines(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "target-lint")

	// build with an image is a warning; a service with no tag is another; both are 200 answers
	// that block nothing, and the error for a build with no image is the only one that blocks.
	answer, code, raw := admin.check(st.ID, checkBody("services:\n  web:\n    image: nginx\n    build: .\n  job:\n    build: ./job\n"))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	type at struct {
		severity, code string
		line           int
	}
	var got []at
	for _, p := range answer.Problems {
		got = append(got, at{p.Severity, p.Code, p.Line})
	}
	want := []at{{"warning", "moving_tag", 3}, {"warning", "ignored_on_target", 4}, {"error", "build_without_image", 6}}
	if !slices.Equal(got, want) {
		t.Errorf("problems = %v, want %v", got, want)
	}
}

// checkEnvTuned is actionsEnv with the stack check tuned before the routes are built.
func checkEnvTuned(t *testing.T, tune func(*envFix)) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, swarmManagerEngine())
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	tune(f)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

func TestCheckIsLimitedPerUser(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	f, e := checkEnvTuned(t, func(f *envFix) {
		// The first two checks are held in flight; the others pass through.
		f.stackOnCheck = func(context.Context) {
			if calls.Add(1) <= 2 {
				entered <- struct{}{}
				<-release
			}
		}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	holder := f.caller(t, e, "holder-1", "operator")
	other := f.caller(t, e, "other-1", "operator")
	st := newStack(t, admin, "limited")

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, code, _ := holder.check(st.ID, checkBody(stacksStarter))
			codes <- code
		}()
	}
	testsupport.Await(t, "the first check to be in flight", entered)
	testsupport.Await(t, "the second check to be in flight", entered)

	// Two are in flight for this user: the third is refused, and says why.
	_, code, raw := holder.check(st.ID, checkBody(stacksStarter))
	if code != http.StatusTooManyRequests || !strings.Contains(raw, "check_busy") {
		t.Errorf("a third check: %d %s, want 429 check_busy", code, raw)
	}
	// Another user is unaffected.
	if _, code, raw := other.check(st.ID, checkBody(stacksStarter)); code != http.StatusOK {
		t.Errorf("another user's check: %d %s, want 200", code, raw)
	}

	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("a held check ended with %d, want 200", code)
		}
	}
	// The slots are given back.
	if _, code, raw := holder.check(st.ID, checkBody(stacksStarter)); code != http.StatusOK {
		t.Errorf("a check after the others finished: %d %s, want 200", code, raw)
	}
}

func TestCheckPastItsDeadlineAnswersAProblemNotAnError(t *testing.T) {
	t.Parallel()
	f, e := checkEnvTuned(t, func(f *envFix) { f.stackCheckDeadline = 1 }) // 1 ns: over before it starts
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "slow")

	answer, code, raw := admin.check(st.ID, checkBody(stacksStarter))
	if code != http.StatusOK || len(answer.Problems) != 1 {
		t.Fatalf("check: %d %s, want 200 with one problem", code, raw)
	}
	if p := answer.Problems[0]; p.Code != "check_timeout" || p.Severity != "error" || p.File != "compose.yaml" {
		t.Errorf("problem = %+v, want an error check_timeout in compose.yaml", p)
	}
}

const stacksStarter = "services:\n  web:\n    image: nginx:1.27-alpine\n"

// checkBodyWithVars is checkBody with the variables of the table; each is {name, value, secret}.
func checkBodyWithVars(compose, vars string) string {
	b, _ := json.Marshal(compose)
	return `{"files":[{"path":"compose.yaml","content":` + string(b) + `,"secret":false}],"vars":` + vars + `}`
}

func TestCheckIsGivenTheVariablesOfTheTableAndAnswersThemResolved(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "vars")

	answer, code, raw := viewer.check(st.ID, checkBodyWithVars(stacks.StarterCompose, `[]`))
	if code != http.StatusOK || len(answer.Variables) != 1 {
		t.Fatalf("check: %d %s, want one variable", code, raw)
	}
	v := answer.Variables[0]
	if v.Name != "WEB_PORT" || v.Value != "8080" || v.Source != "default" || !v.HasDefault || v.Default != "8080" || v.Required || v.Unused {
		t.Errorf("variable = %+v, want WEB_PORT 8080 from the default", v)
	}
	if len(v.Places) != 1 || v.Places[0].File != "compose.yaml" || v.Places[0].Line != 12 {
		t.Errorf("places = %+v, want compose.yaml line 12", v.Places)
	}

	answer, code, raw = viewer.check(st.ID, checkBodyWithVars(stacks.StarterCompose, `[{"name":"WEB_PORT","value":"9090","secret":false}]`))
	if code != http.StatusOK || len(answer.Variables) != 1 || answer.Variables[0].Value != "9090" || answer.Variables[0].Source != "table" {
		t.Errorf("with WEB_PORT=9090: %d %s, want 9090 from the table", code, raw)
	}

	// The list is never null.
	_, code, raw = viewer.check(st.ID, checkBodyWithVars("services: {}\n", `[]`))
	if code != http.StatusOK || !strings.Contains(raw, `"variables":[]`) {
		t.Errorf("a file with no variables: %d %s, want an empty list", code, raw)
	}
}

func TestCheckRouteLocatesThePortBudgetProblem(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "port-budget")

	// 40 distinct 1,000-port ranges: compose-go would expand them to 40,000 ports. The check
	// refuses the file at the entry that takes the count over 20,000 and expands nothing.
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n")
	for i := range 40 {
		fmt.Fprintf(&b, "      - %d-%d:20000-20999\n", 10000+1000*i, 10999+1000*i)
	}
	answer, code, raw := admin.check(st.ID, checkBody(b.String()))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	if len(answer.Problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", answer.Problems)
	}
	if p := answer.Problems[0]; p.Code != "ports_too_many" || p.Severity != "error" || p.Line != 25 || p.File != "compose.yaml" {
		t.Errorf("problem = %+v, want an error ports_too_many at compose.yaml line 25", p)
	}
}

// The check route builds its problems from the loader's and the converter's words, and a variable value in the
// request is used as given. For a field fed by a secret variable the answer holds the fixed sentence and no
// run of the value (07-44, D-02, T-07-187).
func TestStackCheckAnswerNeverHoldsARunOfASecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct {
		body, secret string
		// want is the Message a problem of the answer must equal.
		want string
	}{
		"mem_limit":       {"    mem_limit: ${PW}\n", "Password2024!", stacks.SecretValueMessage("services.web.mem_limit")},
		"deploy.replicas": {"    deploy:\n      replicas: ${PW}\n", "Password2024!", stacks.SecretValueMessage("services.web.deploy.replicas")},
		"a port spec":     {"    ports: [\"${PW}:80\"]\n", "abcd:efgh9", stacks.SecretValueMessage("")},
		"two documents, a tmpfs size in the second": {
			"    volumes:\n      - {type: tmpfs, target: /a, tmpfs: {size: 1m}}\n---\nservices:\n  web:\n    volumes:\n      - {type: tmpfs, target: /b, tmpfs: {size: \"${PW}\"}}\n      - {type: tmpfs, target: /c, tmpfs: {size: 1m}}\n",
			"Password2024!", stacks.SecretFileMessage("services.web.volumes.1.tmpfs.size"),
		},
		"two documents, a relative bind in the second": {
			"    volumes: [\"./p1:/a1\"]\n---\nservices:\n  web:\n    volumes: [\"./${PW}:/s\", \"./p2:/a2\", \"./p3:/a3\", \"./p4:/a4\"]\n",
			"Zq9xkwmpr:Vb7nm", stacks.SecretFileMessage("services.web.volumes.1"),
		},
		"one document, a repeated target before the secret item": {
			"    volumes:\n      - {type: tmpfs, target: /a, tmpfs: {size: 1m}}\n      - {type: tmpfs, target: /a, tmpfs: {size: 2m}}\n      - {type: tmpfs, target: /b, tmpfs: {size: \"${PW}\"}}\n      - {type: tmpfs, target: /c, tmpfs: {size: 1m}}\n",
			"Password2024!", stacks.SecretFileMessage("services.web.volumes.1.tmpfs.size"),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := stackEnv(t, nil)
			admin := f.caller(t, e, "envadmin-1", "admin")
			operator := f.caller(t, e, "operator-1", "operator")
			st := newStack(t, admin, "checkleak")
			answer, code, raw := operator.check(st.ID, checkBodyWithVars(head+tc.body, partSecretVars(t, tc.secret)))
			if code != http.StatusOK {
				t.Fatalf("check: %d %.300s, want 200", code, raw)
			}
			var messages []string
			found := false
			for _, p := range answer.Problems {
				messages = append(messages, p.Message)
				found = found || p.Message == tc.want
			}
			if !found {
				t.Errorf("problems = %+v, want one with %q", answer.Problems, tc.want)
			}
			stackstest.NoSecretRun(t, tc.secret, messages...)
		})
	}
}

// netCheckEnv is a swarm manager with an address pool and the given networks, with the address reader a real
// server hands the editor's check (plan 10-08, D-19).
func netCheckEnv(t *testing.T, pool string, nets ...dockertest.FakeNetwork) (*envFix, envBody) {
	t.Helper()
	return stackEnv(t, func(c *dockertest.FakeConfig) {
		c.SwarmClusterID, c.SwarmSubnetSize = "cluster-1", 24
		if pool != "" {
			c.SwarmDefaultAddrPool = []string{pool}
		}
		c.Networks = nets
	})
}

// subnetCompose is a one-service compose file with the network backend at the given explicit subnet.
func subnetCompose(subnet string) string {
	return "services:\n  web:\n    image: nginx:1.27\n    networks: [backend]\nnetworks:\n  backend:\n    ipam:\n      config:\n        - subnet: " + subnet + "\n"
}

func TestStackCheckWarnsSubnetOverlap(t *testing.T) {
	t.Parallel()
	f, e := netCheckEnv(t, "10.0.0.0/16",
		dockertest.FakeNetwork{ID: "o1", Name: "other_net", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}})
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "shop")

	answer, code, raw := admin.check(st.ID, checkBody(subnetCompose("10.0.1.0/24")))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	if len(answer.Problems) != 1 {
		t.Fatalf("problems = %+v, want the one overlap warning", answer.Problems)
	}
	p := answer.Problems[0]
	if p.Severity != "warning" || p.Code != "network_subnet_overlap" || p.Line != 9 ||
		p.Message != "network “backend” uses subnet 10.0.1.0/24, which overlaps “other_net” (10.0.1.0/24) in this environment." {
		t.Errorf("problem = %+v", p)
	}
}

// createEngineNetwork makes a network on the fake engine the way a daemon would take a deploy's: through its API,
// after the stack exists.
func createEngineNetwork(t *testing.T, f *envFix, name, driver, subnet string, labels map[string]string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"Name": name, "Driver": driver, "Labels": labels,
		"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": subnet}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sock := f.fake.SocketPath()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	t.Cleanup(client.CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://docker/networks/create", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create network %s: %v", name, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create network %s: %d", name, resp.StatusCode)
	}
}

func TestStackCheckOwnNetworkNotReported(t *testing.T) {
	t.Parallel()
	// The stack's own networks already exist, with the subnets the file asks for: a redeploy must not collide
	// with itself. A network of another stack on the same subnet does.
	f, e := netCheckEnv(t, "10.0.0.0/16")
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "shop")
	createEngineNetwork(t, f, "shop_backend", "overlay", "10.0.1.0/24", map[string]string{"com.docker.stack.namespace": "shop"})
	createEngineNetwork(t, f, "shop_default", "overlay", "10.0.2.0/24", map[string]string{"com.docker.compose.project": "shop"})
	createEngineNetwork(t, f, "blog_backend", "overlay", "10.0.3.0/24", map[string]string{"com.docker.stack.namespace": "blog"})
	f.clk.t = f.clk.t.Add(time.Minute) // past the cached listing, so the new networks are read

	for _, own := range []string{"10.0.1.0/24", "10.0.2.0/24"} {
		answer, code, raw := admin.check(st.ID, checkBody(subnetCompose(own)))
		if code != http.StatusOK || len(answer.Problems) != 0 {
			t.Errorf("check of the stack's own subnet %s: %d %s, want no problem", own, code, raw)
		}
	}
	answer, code, raw := admin.check(st.ID, checkBody(subnetCompose("10.0.3.0/24")))
	if code != http.StatusOK || len(answer.Problems) != 1 || answer.Problems[0].Code != "network_subnet_overlap" ||
		!strings.Contains(answer.Problems[0].Message, "“blog_backend”") {
		t.Errorf("check of another stack's subnet: %d %s, want the overlap with blog_backend", code, raw)
	}
}

func TestStackCheckWarnsSubnetOutsidePool(t *testing.T) {
	t.Parallel()
	f, e := netCheckEnv(t, "10.0.0.0/16")
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "shop")

	answer, code, raw := admin.check(st.ID, checkBody(subnetCompose("172.30.0.0/24")))
	if code != http.StatusOK || len(answer.Problems) != 1 {
		t.Fatalf("check: %d %s, want the one outside-pool warning", code, raw)
	}
	if p := answer.Problems[0]; p.Severity != "warning" || p.Code != "network_subnet_outside_pool" || p.Line != 9 ||
		p.Message != "network “backend” uses subnet 172.30.0.0/24, which is outside the address pool 10.0.0.0/16." {
		t.Errorf("problem = %+v", p)
	}
	if answer, _, raw = admin.check(st.ID, checkBody(subnetCompose("10.0.7.0/24"))); len(answer.Problems) != 0 {
		t.Errorf("a subnet inside the pool: %s, want no problem", raw)
	}
}

func TestStackCheckEnvUnreadable(t *testing.T) {
	t.Parallel()
	f, e := netCheckEnv(t, "10.0.0.0/16",
		dockertest.FakeNetwork{ID: "o1", Name: "other_net", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}})
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "shop")
	if answer, _, raw := admin.check(st.ID, checkBody(subnetCompose("10.0.1.0/24"))); len(answer.Problems) != 1 {
		t.Fatalf("before the engine goes: %s, want the overlap warning", raw)
	}

	// The engine goes away and the cached listing expires: the environment cannot be read.
	f.fake.Stop()
	f.clk.t = f.clk.t.Add(time.Hour)
	answer, code, raw := admin.check(st.ID, checkBody(subnetCompose("10.0.1.0/24")))
	if code != http.StatusOK {
		t.Fatalf("check with the environment unreadable: %d %s, want 200", code, raw)
	}
	if len(answer.Problems) != 0 || !strings.Contains(raw, `"problems":[]`) {
		t.Errorf("problems = %+v, want no network finding when the environment cannot be read", answer.Problems)
	}
}
