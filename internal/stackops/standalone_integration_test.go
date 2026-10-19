// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package stackops_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// TestStandaloneDeployOnARealEngine reviews and deploys a stack on a real engine through the Compose SDK, the
// way the server does for its own host (plan 07-20, STK-02, STK-06, STK-09): the environment is a local
// socket (a proxy to a docker:<N>-dind container), the image is in a registry of the node's own, and the SDK
// is the real one over Bayplan's client behind the guard. It shows that the first review lists the service as
// added with a new pin, that the deploy runs one container labelled with the project and the stack, that the
// config hash the review computes is the one the SDK stamps, that a redeploy nobody edited keeps the same
// container (even after the tag moved), and that an edit recreates it.
func TestStandaloneDeployOnARealEngine(t *testing.T) {
	// The SDK's process-wide state: its private configuration directory and its logging.
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/bp/web:1"
	pushed := dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)
	// Bayplan's own client to the node, for what the test reads back: the project's containers.
	cli, err := dockerx.New(dockertest.TCPTransportFor(node.Host()))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	rig := newRigOver(t, dockertest.UnixProxy(t, node.Host()))
	rig.deps.ConvergeTimeout = 3 * time.Minute
	pins := &memPins{}
	const name = "itest"
	compose := func(port string) string {
		return "services:\n  web:\n    image: " + image + "\n    command: [\"httpd\", \"-f\", \"-p\", \"80\"]\n    ports:\n      - \"" + port + ":80\"\n"
	}
	ctx := testsupport.Context(t)
	containers := func() []dockerx.ContainerRow {
		t.Helper()
		rows, err := cli.ContainersWithLabel(ctx, stacks.LabelComposeProject, name)
		if err != nil {
			t.Fatalf("list the project's containers: %v", err)
		}
		return rows
	}

	// 1. The review of a stack nobody deployed: the service is added with a pin the registry gave.
	rv, err := rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose("18080"), pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if got := changesOf(rv); !slices.Equal(got, []string{"added web"}) || rv.Problems.Errors() != 0 {
		t.Fatalf("first review: services %v problems %v, want web added and nothing blocking", got, rv.Problems)
	}
	wantPin := image + "@" + pushed
	if rv.Pins["web"] != wantPin || len(rv.Images) != 1 || rv.Images[0].State != stackops.ImageNew {
		t.Fatalf("first review: pins %v images %+v, want a new pin %s", rv.Pins, rv.Images, wantPin)
	}

	// 2. The deploy, with the review's pins: one container, labelled, running; the hash is the review's.
	revision := 0
	deploy := func(compose string, reviewed map[string]string) stackops.DeployResult {
		t.Helper()
		revision++
		in := standaloneInput(name, compose)
		in.Pins, in.RevisionID, in.Reviewed = pins, "rev-"+strconv.Itoa(revision), reviewed
		res, job := rig.deployStandalone(t, in)
		if !res.OK() {
			var words []string
			for _, it := range job.Items {
				if it.Detail != "" {
					words = append(words, it.ID+": "+it.Detail)
				}
			}
			t.Fatalf("deploy: %+v items %v details %q", res, itemStates(job), words)
		}
		return res
	}
	res := deploy(compose("18080"), rv.Pins)
	if res.Services != 1 || res.Updated != 0 || res.Networks != 1 {
		t.Fatalf("first deploy = %+v, want one service and one network created", res)
	}
	first := containers()
	if len(first) != 1 || first[0].State != "running" {
		t.Fatalf("containers after the deploy = %+v, want one running", first)
	}
	labels := first[0].Labels
	if labels[stacks.LabelComposeProject] != name || labels[stacks.LabelStackID] != "stack-"+name ||
		labels[stacks.LabelComposeService] != "web" || labels[stacks.LabelStackImage] != image {
		t.Errorf("labels = %v, want the project, the stack's ID, the service and the image as written", labels)
	}
	if want := preparedHash(t, name, compose("18080"), map[string]string{"web": wantPin})["web"]; labels[stacks.LabelComposeConfigHash] != want {
		t.Errorf("config hash label = %q, want the hash the review computes %q", labels[stacks.LabelComposeConfigHash], want)
	}
	if first[0].Image != wantPin {
		t.Errorf("the container runs %q, want the pinned reference %q", first[0].Image, wantPin)
	}

	// 3. Right after the deploy the review has nothing to do, and the digest is kept.
	rv, err = rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose("18080"), pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Services) != 0 || len(rv.Images) != 1 || rv.Images[0].State != stackops.ImagePinned {
		t.Fatalf("review after the deploy: services %v images %+v, want nothing to do and web pinned", changesOf(rv), rv.Images)
	}

	// 4. A redeploy nobody edited keeps the container, even after the tag moved to another image.
	otherDigest := dockertest.PushImage(t, node, "alpine:3", image, nil)
	if otherDigest == pushed {
		t.Fatalf("the tag did not move: %s", otherDigest)
	}
	res = deploy(compose("18080"), nil)
	if res.Services != 0 || res.Updated != 0 {
		t.Errorf("redeploy = %+v, want nothing created or updated", res)
	}
	again := containers()
	if len(again) != 1 || again[0].ID != first[0].ID || again[0].Image != wantPin {
		t.Fatalf("containers after a redeploy = %+v, want the same container %s on the same pin", again, first[0].ID)
	}

	// 5. An edit recreates it: the review says changed, the deploy replaces the container.
	rv, err = rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose("18081"), pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if got := changesOf(rv); !slices.Equal(got, []string{"changed web"}) {
		t.Fatalf("review of an edit: %v, want web changed", got)
	}
	res = deploy(compose("18081"), rv.Pins)
	if res.Services != 0 || res.Updated != 1 {
		t.Errorf("deploy of an edit = %+v, want one service updated", res)
	}
	edited := containers()
	if len(edited) != 1 || edited[0].ID == first[0].ID || edited[0].State != "running" {
		t.Fatalf("containers after an edit = %+v, want one new running container (was %s)", edited, first[0].ID)
	}
	if !strings.HasSuffix(edited[0].Image, pushed) {
		t.Errorf("the recreated container runs %q, want the pin %s kept through the edit", edited[0].Image, pushed)
	}
}

// composeRequest is one request the Compose SDK sent to the engine, as the corpus of the agent's replay test
// holds it (agent/internal/proxy/testdata/compose-replay.json): the method, the path without its API
// version, the query, and the body (JSON as a document, a tar as its bytes).
type composeRequest struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Query   string          `json:"query,omitempty"`
	HasBody bool            `json:"hasBody"`
	Upgrade bool            `json:"upgrade,omitempty"`
	JSON    json.RawMessage `json:"json,omitempty"`
	Tar     []byte          `json:"tar,omitempty"`
}

var (
	// longID is a container, network or volume ID, an image digest or a config hash: 64 hex digits.
	longID = regexp.MustCompile(`[0-9a-f]{64}`)
	// tempName is the 12 hex digits and the underscore the SDK puts before a container's name while it
	// recreates it.
	tempName = regexp.MustCompile(`\b[0-9a-f]{12}_`)
	// apiVersion is the version segment at the start of a path the client negotiated.
	apiVersion = regexp.MustCompile(`^/v[0-9]+\.[0-9]+/`)
)

const (
	placeholderID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	placeholderTemp = "aaaaaaaaaaaa_"
)

// normalised makes the text of a recorded request the same on every run: the IDs, digests and hashes the
// engine and the content gave are one placeholder, and so is the temporary name of a recreate.
func normalised(s string) string {
	return tempName.ReplaceAllString(longID.ReplaceAllString(s, placeholderID), placeholderTemp)
}

// recorder is a reverse proxy in front of an engine that remembers the requests it forwards.
type recorder struct {
	mu   sync.Mutex
	reqs []composeRequest
	// failures are the requests that could not be read as a corpus entry.
	failures []string
}

func (r *recorder) record(req *http.Request, body []byte) {
	rec := composeRequest{Method: req.Method, Path: normalised(apiVersion.ReplaceAllString(req.URL.Path, "/")), HasBody: len(body) > 0 || req.ContentLength > 0}
	if q, err := url.ParseQuery(req.URL.RawQuery); err == nil && len(q) > 0 {
		rec.Query = normalised(q.Encode())
	}
	rec.Upgrade = req.Header.Get("Upgrade") != ""
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(body) == 0:
	case req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/archive"):
		fixed, err := fixedTar(body)
		if err != nil {
			r.failures = append(r.failures, req.Method+" "+req.URL.Path+": "+err.Error())
			return
		}
		rec.Tar = fixed
	default:
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			r.failures = append(r.failures, req.Method+" "+req.URL.Path+": the body is neither JSON nor a tar")
			return
		}
		canonical, _ := json.Marshal(doc) // object keys in order
		rec.JSON = json.RawMessage(normalised(string(canonical)))
	}
	r.reqs = append(r.reqs, rec)
}

// fixedTar rewrites a tar with no time or owner in its headers, so that the same content is the same bytes on
// every run. Name, mode, type and content are kept.
func fixedTar(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		h.ModTime, h.AccessTime, h.ChangeTime = time.Unix(0, 0), time.Time{}, time.Time{}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		h.Format = tar.FormatUSTAR
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // G110: a test reads the tar the SDK just made
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// startRecorder serves a recording proxy to the engine at host (a DockerHost value such as tcp://localhost:32771)
// and returns the recorder and a dockerx client that reaches the engine through it. Only the Compose SDK uses
// that client, so what is recorded is what the SDK sent.
func startRecorder(t *testing.T, host string) (*recorder, *dockerx.Client) {
	t.Helper()
	rec := &recorder{}
	target, err := url.Parse("http://" + strings.TrimPrefix(host, "tcp://"))
	if err != nil {
		t.Fatal(err)
	}
	rp := &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
		FlushInterval: -1,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		rec.record(r, body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cli, err := dockerx.New(dockertest.TCPTransportFor("tcp://" + srv.Listener.Addr().String()))
	if err != nil {
		t.Fatalf("dockerx.New over the recorder: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return rec, cli
}

// corpus is what the recorder holds, without duplicates, in an order that does not depend on the run.
func (r *recorder) corpus() []composeRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[[sha256.Size]byte]bool{}
	var out []composeRequest
	for _, q := range r.reqs {
		raw, _ := json.Marshal(q)
		if k := sha256.Sum256(raw); !seen[k] {
			seen[k] = true
			out = append(out, q)
		}
	}
	key := func(q composeRequest) string {
		return q.Path + "\x00" + q.Method + "\x00" + q.Query + "\x00" + string(q.JSON) + "\x00" + string(q.Tar)
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

// replayCorpus is the file the agent's replay test reads, relative to this package.
const replayCorpus = "../../agent/internal/proxy/testdata/compose-replay.json"

// TestRecordComposeRequests runs a stack through every request the Compose SDK sends for a standalone deploy
// (create with a secret and a config, recreate, orphan removal, stop, start, down) against a real engine, with
// a recording proxy between the SDK and the engine, and holds the result as the corpus the agent's allowlist
// is replayed against (plan 07-21, STK-02, RESEARCH Wave 0). BAYPLAN_UPDATE_REPLAY=1 writes the corpus; without
// it the test only fails when a request has appeared that the corpus has no method and path for.
func TestRecordComposeRequests(t *testing.T) {
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/bp/web:1"
	dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)
	rec, sdk := startRecorder(t, node.Host())
	ctx := testsupport.Context(t)

	rig := newRigOver(t, dockertest.UnixProxy(t, node.Host()))
	rig.deps.ConvergeTimeout = 3 * time.Minute
	// The SDK's up goes over the recording client. The first one finds the image gone from the host (a deploy
	// has pulled it by then, so the SDK itself normally finds it), so the SDK's own pull is recorded too.
	var project *types.Project
	pulledOnce := false
	rig.deps.Up = func(ctx context.Context, _ *dockerx.Client, p *types.Project, o dockerx.ComposeUpOptions, on func(dockerx.ComposeEvent)) error {
		if !pulledOnce {
			pulledOnce = true
			// Every image of the repository, by ID: the pull by digest made one beside the one the push left,
			// and a tag alone would leave the digest.
			if out, err := node.Sh(ctx, "docker image rm -f $(docker image ls -aq --no-trunc "+strings.TrimSuffix(image, ":1")+" | sort -u)"); err != nil {
				t.Fatalf("remove the image so the SDK has to pull it: %v\n%s", err, out)
			}
		}
		project = p
		return sdk.ComposeUp(ctx, p, o, on)
	}

	const name = "replay"
	raw, err := os.ReadFile("../agents/testdata/stacks/standalone-full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compose := func(mode string, withWeb bool) string {
		text := strings.NewReplacer("{{IMAGE}}", image, "{{MODE}}", mode).Replace(string(raw))
		if !withWeb {
			// The orphan: web is gone from the file, db stays.
			start := strings.Index(text, "  web:\n")
			end := strings.Index(text, "networks:\n  back: {}")
			text = text[:start] + "\n" + text[end:]
		}
		return text
	}
	files := func(compose string) stacks.WorkingCopy {
		return stacks.WorkingCopy{Files: []stacks.File{
			{Path: stacks.ComposeFile, Content: []byte(compose)},
			{Path: "tok.txt", Content: []byte("replay-secret-value")},
			{Path: "nginx.conf", Content: []byte("worker_processes 1;\n")},
		}}
	}
	pins := &memPins{}
	revision := 0
	deploy := func(compose string) {
		t.Helper()
		revision++
		in := standaloneInput(name, compose)
		in.Copy = files(compose)
		in.Pins, in.RevisionID = pins, "rev-"+strconv.Itoa(revision)
		res, job := rig.deployStandalone(t, in)
		if !res.OK() {
			var words []string
			for _, it := range job.Items {
				if it.Detail != "" {
					words = append(words, it.ID+": "+it.Detail)
				}
			}
			t.Fatalf("deploy %d: %+v items %v details %q", revision, res, itemStates(job), words)
		}
	}

	deploy(compose("one", true)) // create, secrets and configs by the archive route, start
	deploy(compose("two", true)) // recreate web
	if err := sdk.ComposeStop(ctx, project); err != nil {
		t.Fatalf("ComposeStop: %v", err)
	}
	if err := sdk.ComposeStart(ctx, project); err != nil {
		t.Fatalf("ComposeStart: %v", err)
	}
	deploy(compose("two", false)) // orphan removal of web
	if err := sdk.ComposeDown(ctx, name); err != nil {
		t.Fatalf("ComposeDown: %v", err)
	}

	if len(rec.failures) > 0 {
		t.Fatalf("requests that could not be recorded: %v", rec.failures)
	}
	got := rec.corpus()
	assertNoFileBinds(t, got)
	assertKindsRecorded(t, got)

	if os.Getenv("BAYPLAN_UPDATE_REPLAY") == "1" {
		out, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Clean(replayCorpus)
		if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil { //nolint:gosec // a test corpus, read by a test
			t.Fatal(err)
		}
		t.Logf("wrote %d requests to %s", len(got), path)
		return
	}
	held, err := os.ReadFile(replayCorpus)
	if err != nil {
		t.Fatalf("read the corpus (BAYPLAN_UPDATE_REPLAY=1 writes it): %v", err)
	}
	var corpus []composeRequest
	if err := json.Unmarshal(held, &corpus); err != nil {
		t.Fatalf("the corpus is not readable: %v", err)
	}
	have := map[string]bool{}
	for _, q := range corpus {
		have[q.Method+" "+q.Path] = true
	}
	var missing []string
	for _, q := range got {
		if k := q.Method + " " + q.Path; !have[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the SDK sent requests the corpus has no method and path for, run with BAYPLAN_UPDATE_REPLAY=1: %v", slices.Compact(missing))
	}
}

// assertNoFileBinds fails when a container create binds a file or a directory of the machine the SDK ran on:
// a stack secret or config arrives through the archive route, never as a bind (07-04's carry-over).
func assertNoFileBinds(t *testing.T, reqs []composeRequest) {
	t.Helper()
	creates := 0
	for _, q := range reqs {
		if q.Method != http.MethodPost || q.Path != "/containers/create" {
			continue
		}
		creates++
		var body struct {
			HostConfig struct {
				Binds  []string
				Mounts []struct{ Type, Source string }
			}
		}
		if err := json.Unmarshal(q.JSON, &body); err != nil {
			t.Fatalf("a container create body is not JSON: %v", err)
		}
		for _, b := range body.HostConfig.Binds {
			// A named volume is written as a bind too (name:path); a path of the machine starts with a slash.
			if src, _, _ := strings.Cut(b, ":"); strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") {
				t.Errorf("a container create binds the path %q, want none: a secret or config is written with the archive route", b)
			}
		}
		for _, m := range body.HostConfig.Mounts {
			if m.Type == "bind" {
				t.Errorf("a container create carries a bind mount of %q, want none", m.Source)
			}
		}
	}
	if creates == 0 {
		t.Error("no container create was recorded")
	}
}

// assertKindsRecorded fails when the corpus lacks a kind of request the plan names: a corpus that does not
// hold them proves nothing about the agent's allowlist.
func assertKindsRecorded(t *testing.T, reqs []composeRequest) {
	t.Helper()
	want := map[string]bool{
		"POST /containers/create": false, "PUT /containers/" + placeholderID + "/archive": false,
		"POST /containers/" + placeholderID + "/rename": false, "POST /containers/" + placeholderID + "/start": false,
		"POST /containers/" + placeholderID + "/stop": false, "DELETE /containers/" + placeholderID: false,
		"POST /networks/create": false, "POST /volumes/create": false, "POST /images/create": false,
	}
	for _, q := range reqs {
		for k := range want {
			if q.Method+" "+q.Path == k {
				want[k] = true
			}
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("the recording holds no %s", k)
		}
	}
}

// TestStandaloneReviewEmptyAfterDeployLocal is the completeness test of the review's normaliser (plan 07-22,
// STK-06, Pitfall 5) on Bayplan's own host: a stack that sets many of the settings the engine keeps (a
// restart policy, a healthcheck, a user, a working directory, a hostname, capabilities, ulimits, a log
// driver with options, limits, a tmpfs, a read-only root, init, extra hosts, DNS, a sysctl, labels and an
// environment, user networks with an alias and a named volume) and services that share another's network
// namespace, PID namespace and volumes is deployed through the real Compose SDK, and the review right
// after lists nothing: no service, no network and no volume. Nothing the image or the engine filled in may
// read as a difference, and a reference to another service must hash as the SDK hashed it.
func TestStandaloneReviewEmptyAfterDeployLocal(t *testing.T) {
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/bp/web:1"
	dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)

	rig := newRigOver(t, dockertest.UnixProxy(t, node.Host()))
	rig.deps.ConvergeTimeout = 3 * time.Minute
	pins := &memPins{}
	const name = "ireview"
	compose := strings.ReplaceAll(`services:
  db:
    image: IMAGE
    command: ["sleep", "3600"]
    stop_grace_period: 1s
    stop_signal: SIGTERM
    restart: unless-stopped
    volumes:
      - data:/data
    networks:
      - back
    environment:
      MODE: prod
      EMPTY: ""
    labels:
      team: platform
    healthcheck:
      test: ["CMD", "true"]
      interval: 2s
      timeout: 2s
      retries: 3
    init: true
    read_only: true
    tmpfs:
      - /run
    user: "1000:1000"
    working_dir: /tmp
    hostname: dbhost
    cap_drop: [NET_RAW]
    ulimits:
      nofile:
        soft: 1024
        hard: 2048
    logging:
      driver: json-file
      options:
        max-size: 1m
    mem_limit: 64m
    cpus: 0.5
    sysctls:
      net.ipv4.ip_unprivileged_port_start: "0"
    extra_hosts:
      - "x.local:10.1.2.3"
    dns:
      - 1.1.1.1
  web:
    image: IMAGE
    command: ["httpd", "-f", "-p", "80"]
    stop_grace_period: 1s
    ports:
      - "18080:80"
    depends_on:
      db:
        condition: service_healthy
    networks:
      back:
        aliases: [www]
      front: {}
  sidecar:
    image: IMAGE
    command: ["sleep", "3600"]
    stop_grace_period: 1s
    network_mode: service:web
    volumes_from: [web]
  watcher:
    image: IMAGE
    command: ["sleep", "3600"]
    stop_grace_period: 1s
    pid: service:web
  extras:
    image: IMAGE
    container_name: ireview-extras
    command: ["sleep 3600"]
    entrypoint: ["/bin/sh", "-c"]
    stop_grace_period: 1s
    cap_add: [NET_BIND_SERVICE]
    security_opt: ["no-new-privileges:true"]
    dns_search: [example.test]
    dns_opt: ["ndots:1"]
    domainname: example.test
    tty: true
    stdin_open: true
    shm_size: 128m
    pids_limit: 100
    cpu_shares: 512
    mem_reservation: 32m
    devices:
      - "/dev/null:/dev/nulltest"
    ports:
      - "5353:53/udp"
      - "127.0.0.1:18443:443/tcp"
    expose: ["9000", "9100-9101"]
    deploy:
      restart_policy:
        condition: on-failure
        max_attempts: 3
    networks:
      front:
        ipv4_address: 172.28.7.10
networks:
  back: {}
  front:
    ipam:
      config:
        - subnet: 172.28.7.0/24
volumes:
  data: {}
`, "IMAGE", image)
	ctx := testsupport.Context(t)

	rv, err := rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose, pins))
	if err != nil || rv.Problems.Errors() != 0 {
		t.Fatalf("first review: %v problems %v", err, rv.Problems)
	}
	in := standaloneInput(name, compose)
	in.Pins, in.RevisionID, in.Reviewed = pins, "rev-1", rv.Pins
	res, job := rig.deployStandalone(t, in)
	if !res.OK() {
		var words []string
		for _, it := range job.Items {
			if it.Detail != "" {
				words = append(words, it.ID+": "+it.Detail)
			}
		}
		t.Fatalf("deploy: %+v items %v details %q", res, itemStates(job), words)
	}

	rv, err = rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose, pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Services) != 0 || len(rv.Objects) != 0 {
		var detail []string
		for _, c := range rv.Services {
			detail = append(detail, c.Change+" "+c.Short+"\n  current: "+string(c.Current)+"\n  next:    "+string(c.Next))
		}
		t.Fatalf("the review right after a deploy lists services %v objects %v, want nothing:\n%s", changesOf(rv), objectsOf(rv), strings.Join(detail, "\n"))
	}
}
