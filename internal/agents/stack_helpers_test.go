// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The helpers of the stack suites (plan 07-11). The parity oracle deploys one compose file twice on
// the same swarm, once with the real `docker stack deploy --resolve-image=never` on a manager and
// once through Bayplan's API and a real agent, and compares what the engine holds afterwards. A
// difference is a converter fix, never a wider normaliser: normaliseSpec removes only what two
// correct deploys of one file must differ in, and says so in its list.

const (
	// stackDeployWait is how long a Bayplan deploy job gets to end, pulls and network creation included.
	stackDeployWait = 5 * time.Minute
	// stackGoneWait is how long a removed stack gets to leave services, containers and networks behind.
	stackGoneWait = 3 * time.Minute

	// parityCLIStack and parityBayplanStack are the two stack names a parity case deploys under. They
	// differ, so the two never share an object, and the normaliser maps both onto stackToken.
	parityCLIStack     = "pcli"
	parityBayplanStack = "pbp"
	stackToken         = "STACK"
)

// readCorpus reads a compose file of the parity corpus and puts the values in it. A value goes where
// {{KEY}} stands.
func readCorpus(t testing.TB, name string, values map[string]string) string {
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

// stackFile is a file beside the compose file of a stack: a secret's or a config's content.
type stackFile struct {
	Path    string
	Content string
}

// paritySource is a compose file with the files beside it.
type paritySource struct {
	Compose string
	Files   []stackFile
}

// deployWithCLI deploys the stack with the real docker CLI on a manager, the way an admin would:
// the compose file and the files beside it are written to a directory of the node, and nothing is
// asked of any registry (--resolve-image=never), so the images stay as the file wrote them.
func deployWithCLI(ctx context.Context, node *dockertest.Node, name string, src paritySource) error {
	dir := "/tmp/" + name
	if out, err := node.Exec(ctx, "mkdir", "-p", dir); err != nil {
		return fmt.Errorf("make %s: %w\n%s", dir, err, out)
	}
	for _, f := range src.Files {
		if err := node.WriteFile(ctx, dir+"/"+f.Path, []byte(f.Content)); err != nil {
			return err
		}
	}
	if err := node.WriteFile(ctx, dir+"/compose.yaml", []byte(src.Compose)); err != nil {
		return err
	}
	out, err := node.Sh(ctx, "cd "+dir+" && docker stack deploy -c compose.yaml --resolve-image=never --detach=true "+name+" 2>&1")
	if err != nil {
		return fmt.Errorf("docker stack deploy %s: %w\n%s", name, err, out)
	}
	return nil
}

// stackAPI is the stack routes of one environment.
func (f *swarmFix) stackAPI() string { return "/api/env/" + f.env.ID + "/stacks" }

// stackDraft is the part of a draft these tests read.
type stackDraft struct {
	Version int64 `json:"version"`
}

// draftVersion is the version of a stack's draft now.
func (f *swarmFix) draftVersion(t *testing.T, stackID string) int64 {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID+"/draft", nil)
	var d stackDraft
	if code != http.StatusOK || json.Unmarshal(body, &d) != nil {
		t.Fatalf("GET the draft of stack %s = %d %s", stackID, code, body)
	}
	return d.Version
}

// createStackBayplan creates the stack with its first compose file and returns its ID.
func (f *swarmFix) createStackBayplan(t *testing.T, name, compose string) string {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.stackAPI(), map[string]any{"name": name, "compose": compose})
	var st struct {
		ID string `json:"id"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &st) != nil || st.ID == "" {
		t.Fatalf("POST stacks (%s) = %d %s", name, code, body)
	}
	return st.ID
}

// saveStackBayplan replaces the draft of a stack: its compose file and the files beside it.
func (f *swarmFix) saveStackBayplan(t *testing.T, stackID string, src paritySource) {
	t.Helper()
	files := []map[string]any{{"path": "compose.yaml", "content": src.Compose, "secret": false}}
	for _, sf := range src.Files {
		files = append(files, map[string]any{"path": sf.Path, "content": sf.Content, "secret": false})
	}
	code, body := f.srv.Do(http.MethodPut, f.stackAPI()+"/"+stackID+"/draft", map[string]any{
		"version": f.draftVersion(t, stackID), "files": files, "vars": []map[string]any{},
	})
	if code != http.StatusOK {
		t.Fatalf("PUT the draft of stack %s = %d %s", stackID, code, body)
	}
}

// stackJob is a deploy job as the route answers it.
type stackJob struct {
	State string `json:"state"`
	Items []struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"items"`
}

// deployStackBayplan deploys the stack's draft, waits until the job ends and fails the test unless
// every step is done. It returns the job as it ended.
func (f *swarmFix) deployStackBayplan(t *testing.T, stackID string) stackJob {
	t.Helper()
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+stackID+"/deploys", map[string]any{"draftVersion": f.draftVersion(t, stackID)})
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the deploy of stack %s = %d %s", stackID, code, body)
	}
	var job stackJob
	testsupport.Poll(t, stackDeployWait, "the deploy job "+started.JobID+" to end", func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID+"/deploys/"+started.JobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		job = stackJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the deploy of stack %s ended %s, want done: %s", stackID, job.State, raw)
	}
	return job
}

// deployWithBayplan creates a stack, saves the files beside its compose file, deploys it through the
// API and returns the stack ID once the job is done.
func deployWithBayplan(t *testing.T, f *swarmFix, name string, src paritySource) string {
	t.Helper()
	id := f.createStackBayplan(t, name, src.Compose)
	if len(src.Files) > 0 {
		f.saveStackBayplan(t, id, src)
	}
	f.deployStackBayplan(t, id)
	return id
}

// serviceSpecs is `docker service inspect` of every service of the stack, by service name.
func serviceSpecs(ctx context.Context, node *dockertest.Node, stack string) (map[string]json.RawMessage, error) {
	names, err := node.Sh(ctx, "docker service ls --filter label=com.docker.stack.namespace="+stack+" --format '{{.Name}}'")
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, name := range strings.Fields(names) {
		raw, err := node.Sh(ctx, "docker service inspect --format '{{json .}}' "+name)
		if err != nil {
			return nil, err
		}
		out[name] = json.RawMessage(strings.TrimSpace(raw))
	}
	return out, nil
}

// engineNames maps the ID of every network, secret and config the swarm holds to its name, so a
// spec that names one by ID can be compared with another that names an object of the same name.
func engineNames(ctx context.Context, node *dockertest.Node) (map[string]string, error) {
	ids := map[string]string{}
	for _, what := range []string{
		"docker network ls --no-trunc --format '{{.ID}} {{.Name}}'",
		"docker secret ls --format '{{.ID}} {{.Name}}'",
		"docker config ls --format '{{.ID}} {{.Name}}'",
	} {
		out, err := node.Sh(ctx, what)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if id, name, ok := strings.Cut(line, " "); ok {
				ids[id] = name
			}
		}
	}
	return ids, nil
}

// hashedSuffix is the short hash Bayplan puts at the end of a secret's or a config's name; the
// docker CLI names them <stack>_<name> alone.
var hashedSuffix = regexp.MustCompile(`_[0-9a-f]{8}$`)

// normaliseSpec reads a `docker service inspect` answer and removes what two correct deploys of one
// file differ in, and nothing else. The list is the whole of it:
//   - Version, CreatedAt, UpdatedAt and ID of the service, and everything but Spec: Endpoint,
//     UpdateStatus and the rest are what the engine did, not what was asked of it;
//   - the stack's name, which becomes STACK (in a name, STACK_ and a prefix of one);
//   - the ID of a network, secret or config, which becomes its name;
//   - the hash at the end of a secret's or a config's name;
//   - every label whose key starts with bayplan., which are Bayplan's own;
//   - the order of the networks, which both converters give by ID and which therefore follows the
//     IDs of two different deploys; they are put in order by name instead.
func normaliseSpec(raw json.RawMessage, stack string, ids map[string]string) map[string]any {
	var whole map[string]any
	if err := json.Unmarshal(raw, &whole); err != nil {
		return map[string]any{"unreadable": string(raw)}
	}
	for _, k := range []string{"Version", "CreatedAt", "UpdatedAt", "ID"} {
		delete(whole, k)
	}
	spec, _ := whole["Spec"].(map[string]any)
	if spec == nil {
		return map[string]any{"no Spec": string(raw)}
	}
	n := normaliser{stack: stack, ids: ids}
	out, _ := n.walk("", spec).(map[string]any)
	if tt, ok := out["TaskTemplate"].(map[string]any); ok {
		if nets, ok := tt["Networks"].([]any); ok {
			sort.SliceStable(nets, func(i, j int) bool { return netTarget(nets[i]) < netTarget(nets[j]) })
		}
	}
	return out
}

func netTarget(v any) string {
	m, _ := v.(map[string]any)
	s, _ := m["Target"].(string)
	return s
}

type normaliser struct {
	stack string
	ids   map[string]string
}

// name puts STACK where the stack's name is.
func (n normaliser) name(s string) string {
	switch {
	case s == n.stack:
		return stackToken
	case strings.HasPrefix(s, n.stack+"_"):
		return stackToken + "_" + strings.TrimPrefix(s, n.stack+"_")
	}
	return s
}

func (n normaliser) walk(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if strings.HasPrefix(k, "bayplan.") {
				continue
			}
			out[n.name(k)] = n.walk(k, e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = n.walk(key, e)
		}
		return out
	case string:
		if name, ok := n.ids[x]; ok && (key == "Target" || key == "SecretID" || key == "ConfigID") {
			x = name
		}
		x = n.name(x)
		if key == "SecretName" || key == "ConfigName" {
			x = hashedSuffix.ReplaceAllString(x, "")
		}
		return x
	}
	return v
}

// flatten writes a decoded JSON tree as path = value lines.
func flattenJSON(prefix string, v any, into map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 && prefix != "" {
			into[prefix] = "{}"
		}
		for k, e := range x {
			flattenJSON(prefix+"."+k, e, into)
		}
	case []any:
		if len(x) == 0 {
			into[prefix] = "[]"
		}
		for i, e := range x {
			flattenJSON(fmt.Sprintf("%s[%d]", prefix, i), e, into)
		}
	default:
		raw, _ := json.Marshal(x)
		into[prefix] = string(raw)
	}
}

// stackSpecDiff is a field-level account of how two normalised specs differ, empty when they are equal.
func stackSpecDiff(a, b map[string]any) string {
	fa, fb := map[string]string{}, map[string]string{}
	flattenJSON("", a, fa)
	flattenJSON("", b, fb)
	paths := map[string]bool{}
	for p := range fa {
		paths[p] = true
	}
	for p := range fb {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var lines []string
	for _, p := range sorted {
		va, oka := fa[p]
		vb, okb := fb[p]
		switch {
		case !oka:
			lines = append(lines, fmt.Sprintf("  %s: docker stack deploy has nothing, Bayplan has %s", p, vb))
		case !okb:
			lines = append(lines, fmt.Sprintf("  %s: docker stack deploy has %s, Bayplan has nothing", p, va))
		case va != vb:
			lines = append(lines, fmt.Sprintf("  %s: docker stack deploy has %s, Bayplan has %s", p, va, vb))
		}
	}
	return strings.Join(lines, "\n")
}

// stackSnapshot is what the engine holds for one stack, normalised: the spec of each of its services
// by service name.
type stackSnapshot struct {
	Services map[string]map[string]any
}

// snapshotStack reads and normalises the stack's services from a manager.
func snapshotStack(ctx context.Context, node *dockertest.Node, stack string) (stackSnapshot, error) {
	specs, err := serviceSpecs(ctx, node, stack)
	if err != nil {
		return stackSnapshot{}, err
	}
	ids, err := engineNames(ctx, node)
	if err != nil {
		return stackSnapshot{}, err
	}
	snap := stackSnapshot{Services: map[string]map[string]any{}}
	for name, raw := range specs {
		snap.Services[strings.TrimPrefix(name, stack+"_")] = normaliseSpec(raw, stack, ids)
	}
	return snap, nil
}

// compareSnapshots fails the test with the field-level differences between the docker CLI's deploy
// and Bayplan's.
func compareSnapshots(t *testing.T, cli, bp stackSnapshot) {
	t.Helper()
	var names []string
	seen := map[string]bool{}
	for n := range cli.Services {
		seen[n] = true
	}
	for n := range bp.Services {
		seen[n] = true
	}
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("neither deploy created a service: the comparison would prove nothing")
	}
	for _, n := range names {
		a, okA := cli.Services[n]
		b, okB := bp.Services[n]
		switch {
		case !okA:
			t.Errorf("service %s: only Bayplan created it", n)
		case !okB:
			t.Errorf("service %s: only docker stack deploy created it", n)
		default:
			fields := map[string]string{}
			flattenJSON("", a, fields)
			t.Logf("service %s: %d fields of the spec compared", n, len(fields))
			if d := stackSpecDiff(a, b); d != "" {
				t.Errorf("service %s differs between docker stack deploy and Bayplan:\n%s", n, d)
			}
		}
	}
}

// removeStackCLI removes the stack with the docker CLI and waits until no service, no container and
// no network of it is left on any node, and, when Bayplan's own reads are given, until the
// environment's services list no longer shows it either.
func removeStackCLI(t *testing.T, f *swarmFix, stack string) {
	t.Helper()
	mgr := f.sw.Managers[0]
	if out, err := mgr.Sh(execCtx(t, 3*time.Minute), "docker stack rm "+stack+" 2>&1"); err != nil {
		t.Fatalf("docker stack rm %s: %v\n%s", stack, err, out)
	}
	testsupport.Poll(t, stackGoneWait, "the stack "+stack+" to leave no service, container or network", func() (bool, string) {
		ctx := execCtx(t, time.Minute)
		var left []string
		for _, what := range []string{
			"docker service ls -q --filter label=com.docker.stack.namespace=" + stack,
			"docker network ls -q --filter label=com.docker.stack.namespace=" + stack,
		} {
			out, err := mgr.Sh(ctx, what)
			if err != nil {
				return false, err.Error()
			}
			if s := strings.TrimSpace(out); s != "" {
				left = append(left, what+": "+strings.ReplaceAll(s, "\n", " "))
			}
		}
		for _, n := range f.sw.Nodes() {
			out, err := n.Sh(ctx, "docker ps -aq --filter label=com.docker.stack.namespace="+stack)
			if err != nil {
				return false, err.Error()
			}
			if s := strings.TrimSpace(out); s != "" {
				left = append(left, n.Hostname+" containers "+strings.ReplaceAll(s, "\n", " "))
			}
		}
		return len(left) == 0, strings.Join(left, "; ")
	})
	testsupport.Poll(t, stackGoneWait, "Bayplan's services list to show no service of "+stack, func() (bool, string) {
		f.srv.RefreshCluster(f.env.ID)
		var left []string
		for _, it := range f.services(t) {
			if strings.HasPrefix(it.Name, stack+"_") {
				left = append(left, it.Name)
			}
		}
		return len(left) == 0, strings.Join(left, ", ")
	})
}
