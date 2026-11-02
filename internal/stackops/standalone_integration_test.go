// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
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
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
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

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestStandaloneDeployOnARealEngine reviews and deploys a stack on a real engine through the Compose SDK, the
// way the server does for its own host (plan 07-20, STK-02, STK-06, STK-09): the environment is a local
// socket (a proxy to a docker:<N>-dind container), the image is in a registry of the node's own, and the SDK
// is the real one over TierPlan's client behind the guard. It shows that the first review lists the service as
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
	image := registry + "/cm/web:1"
	pushed := dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)
	// TierPlan's own client to the node, for what the test reads back: the project's containers.
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
// is replayed against (plan 07-21, STK-02, RESEARCH Wave 0). TIERPLAN_UPDATE_REPLAY=1 writes the corpus; without
// it the test only fails when a request has appeared that the corpus has no method and path for.
func TestRecordComposeRequests(t *testing.T) {
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/cm/web:1"
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

	if os.Getenv("TIERPLAN_UPDATE_REPLAY") == "1" {
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
		t.Fatalf("read the corpus (TIERPLAN_UPDATE_REPLAY=1 writes it): %v", err)
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
		t.Errorf("the SDK sent requests the corpus has no method and path for, run with TIERPLAN_UPDATE_REPLAY=1: %v", slices.Compact(missing))
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
// STK-06, Pitfall 5) on TierPlan's own host: a stack that sets many of the settings the engine keeps (a
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
	image := registry + "/cm/web:1"
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

// TestLocalOnlyImagePinsToItsID reviews and deploys a stack whose image exists only on the host, through the
// real Compose SDK (plan 07-22, D-18): the registry on the host answers 404 for the repository, the host holds
// the image under that name with no repository digest, so the review pins it to the image ID with the warning
// and the SDK runs the container from the ID. The engine is 28 on purpose: the containerd image store, the
// default of Engine 29, lists a repository digest for every image, built on the host or not, so this rule
// cannot tell a local image from one a registry lost there (see deferred-items.md).
func TestLocalOnlyImagePinsToItsID(t *testing.T) {
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "28")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	// busybox under a name of its own, on the host only: the registry does not have the repository.
	local := registry + "/local/only:1"
	dockertest.LoadImage(t, "busybox:1.36.1", node)
	if out, err := node.Sh(testsupport.Context(t), "docker tag busybox:1.36.1 "+local); err != nil {
		t.Fatalf("tag the local image: %v\n%s", err, out)
	}
	out, err := node.Sh(testsupport.Context(t), "docker image inspect --format '{{.Id}} {{.RepoDigests}}' "+local)
	if err != nil {
		t.Fatalf("inspect the local image: %v\n%s", err, out)
	}
	imageID, digests, _ := strings.Cut(strings.TrimSpace(out), " ")
	if digests != "[]" {
		t.Skipf("the engine lists repository digests %s for an image no registry ever gave it, so TierPlan cannot take it for a local image on this engine's image store", digests)
	}

	rig := newRigOver(t, dockertest.UnixProxy(t, node.Host()))
	rig.deps.ConvergeTimeout = 3 * time.Minute
	pins := &memPins{}
	const name = "ilocal"
	compose := "services:\n  app:\n    image: " + local + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n"
	ctx := testsupport.Context(t)

	rv, err := rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose, pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if rv.Problems.Errors() != 0 || rv.Pins["app"] != imageID || len(rv.Images) != 1 || rv.Images[0].State != stackops.ImageLocal {
		t.Fatalf("review: pins %v images %+v problems %v, want app pinned to the image ID %s as a local image and nothing blocking", rv.Pins, rv.Images, rv.Problems, imageID)
	}
	warned := false
	for _, p := range rv.Problems {
		warned = warned || (p.Severity == stacks.SeverityWarning && p.Code == stackops.CodeLocalImage)
	}
	if !warned {
		t.Errorf("review: problems %v, want the local-image warning", rv.Problems)
	}

	in := standaloneInput(name, compose)
	in.Pins, in.RevisionID, in.Reviewed = pins, "rev-1", rv.Pins
	res, job := rig.deployStandalone(t, in)
	if !res.OK() {
		t.Fatalf("deploy: %+v items %v", res, itemStates(job))
	}
	ran, err := node.Sh(ctx, "docker inspect --format '{{.Image}} {{index .Config.Labels \"tierplan.stack.image\"}}' "+name+"-app-1")
	if err != nil {
		t.Fatalf("inspect the container: %v\n%s", err, ran)
	}
	if got := strings.TrimSpace(ran); got != imageID+" "+local {
		t.Errorf("the container runs %q, want the image ID and the text as written %q", got, imageID+" "+local)
	}

	// Right after the deploy the review has nothing to do, and the image stays pinned to its ID.
	rv, err = rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose, pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Services) != 0 || rv.Pins["app"] != imageID {
		t.Errorf("review after the deploy: services %v pins %v, want nothing to do and the image ID kept", changesOf(rv), rv.Pins)
	}
}

// TestStandaloneStopStartKeepsSecrets stops and starts a stack on a real engine through the Compose SDK (plan
// 07-26, STK-11, D-19): the container is stopped and not removed, the network and the named volume stay, and
// after the start the container runs again with the stack file's secret written into /run/secrets by the
// SDK before the container started. The secret is read from inside the container, so only what the engine
// really holds counts.
func TestStandaloneStopStartKeepsSecrets(t *testing.T) {
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/cm/keep:1"
	pushed := dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)
	cli, err := dockerx.New(dockertest.TCPTransportFor(node.Host()))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	rig := newRigOver(t, dockertest.UnixProxy(t, node.Host()))
	rig.deps.ConvergeTimeout = 3 * time.Minute
	const name = "ikeep"
	compose := "services:\n  web:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n" +
		"    secrets: [tok]\n    volumes:\n      - data:/data\nvolumes:\n  data: {}\nsecrets:\n  tok:\n    file: tok.txt\n"
	const secret = "s3cret-token-0123456789"
	copy := stacks.WorkingCopy{Files: []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte(compose)}, {Path: "tok.txt", Content: []byte(secret)},
	}}
	ctx := testsupport.Context(t)

	in := standaloneInput(name, compose)
	in.Copy = copy
	in.Pins, in.RevisionID = &memPins{}, "rev-1"
	res, job := rig.deployStandalone(t, in)
	if !res.OK() {
		t.Fatalf("deploy: %+v items %v", res, itemStates(job))
	}
	pins := map[string]string{}
	for svc, p := range res.Pins {
		pins[svc] = p.Ref
	}
	if want := image + "@" + pushed; pins["web"] != want {
		t.Fatalf("the deploy pinned %q, want %q", pins["web"], want)
	}
	st := standaloneStackOf(name)
	store := &lifecycleStore{
		rev:  stackops.Snapshot{ID: "rev-1", StackID: st.ID, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood, Copy: copy},
		have: true, pins: pins,
	}

	web := func() dockerx.ContainerRow {
		t.Helper()
		rows, err := cli.ContainersWithLabel(ctx, stacks.LabelComposeProject, name)
		if err != nil || len(rows) != 1 {
			t.Fatalf("the project's containers = %+v, %v, want one", rows, err)
		}
		return rows[0]
	}
	readSecret := func(id string) string {
		t.Helper()
		out, err := node.Exec(ctx, "docker", "exec", id, "cat", "/run/secrets/tok")
		if err != nil {
			t.Fatalf("read the secret in %s: %v\n%s", id, err, out)
		}
		return strings.TrimSpace(out)
	}
	first := web()
	if first.State != "running" || readSecret(first.ID) != secret {
		t.Fatalf("before the stop: container %+v, secret %q, want it running with the stack file's secret", first, readSecret(first.ID))
	}

	stopRes, stopJob := rig.lifecycleRunOn(t, stackops.LifecycleStop, st, rig.envID, store, stackops.Caller{})
	if !stopRes.OK() {
		t.Fatalf("stop = %+v items %v", stopRes, itemStates(stopJob))
	}
	stopped := web()
	if stopped.ID != first.ID || stopped.State == "running" {
		t.Errorf("after the stop: %+v, want the same container, not running", stopped)
	}
	nets, err := cli.NetworksWithLabel(ctx, stacks.LabelComposeProject, name)
	if err != nil || len(nets) != 1 {
		t.Errorf("networks after a stop = %+v, %v, want the project's one network kept", nets, err)
	}
	vols, err := cli.VolumesWithLabel(ctx, stacks.LabelComposeProject, name)
	if err != nil || len(vols) != 1 {
		t.Errorf("volumes after a stop = %+v, %v, want the named volume kept", vols, err)
	}
	if !store.isStopped(st.ID) {
		t.Errorf("the stack is not recorded as stopped")
	}

	startRes, startJob := rig.lifecycleRunOn(t, stackops.LifecycleStart, st, rig.envID, store, stackops.Caller{})
	if !startRes.OK() {
		t.Fatalf("start = %+v items %v", startRes, itemStates(startJob))
	}
	started := web()
	if started.ID != first.ID || started.State != "running" {
		t.Fatalf("after the start: %+v, want the same container running", started)
	}
	if got := readSecret(started.ID); got != secret {
		t.Errorf("after the start /run/secrets/tok holds %q, want the stack file's secret %q written again", got, secret)
	}
	if !strings.HasSuffix(started.Image, pushed) {
		t.Errorf("the container runs %q after a start, want the digest %s it ran", started.Image, pushed)
	}
	if store.isStopped(st.ID) {
		t.Errorf("the stack is still recorded as stopped after the start")
	}
}

// TestVolumeConfigEditKeepsTheVolume redeploys a stack whose compose file changed the labels of a named
// volume that holds data, on a real engine through the Compose SDK (plan 08-07, STK-13, D-46). The SDK's
// reconciler asks before it recreates a volume whose settings differ from the file, and TierPlan answers no:
// the volume is the same one (its creation time is unchanged), the marker file written into it is still
// there, and the deploy went on.
func TestVolumeConfigEditKeepsTheVolume(t *testing.T) {
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/cm/vol:1"
	dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)
	cli, err := dockerx.New(dockertest.TCPTransportFor(node.Host()))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	rig := newRigOver(t, dockertest.UnixProxy(t, node.Host()))
	rig.deps.ConvergeTimeout = 3 * time.Minute
	pins := &memPins{}
	const name = "ivolkeep"
	const volume = name + "_data"
	compose := func(volumeLabels string) string {
		return "services:\n  app:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n" +
			"    volumes:\n      - data:/data\nvolumes:\n  data:" + volumeLabels + "\n"
	}
	// The test's own context: a 30 s one would run out during the deploys on a slow runner.
	ctx := t.Context()
	revision := 0
	deploy := func(text string) {
		t.Helper()
		revision++
		in := standaloneInput(name, text)
		in.Pins, in.RevisionID = pins, "rev-"+strconv.Itoa(revision)
		res, job := rig.deployStandalone(t, in)
		if !res.OK() {
			t.Fatalf("deploy %d: %+v items %v", revision, res, itemStates(job))
		}
	}
	created := func() (time.Time, map[string]string) {
		t.Helper()
		rows, err := cli.VolumesWithLabel(ctx, stacks.LabelComposeProject, name)
		if err != nil {
			t.Fatalf("list the project's volumes: %v", err)
		}
		for _, v := range rows {
			if v.Name == volume {
				return v.Created, v.Labels
			}
		}
		t.Fatalf("volumes = %+v, want %s", rows, volume)
		return time.Time{}, nil
	}

	deploy(compose(" {}"))
	if out, err := node.Sh(ctx, "docker run --rm -v "+volume+":/data busybox:1.36.1 sh -c 'echo kept > /data/marker'"); err != nil {
		t.Fatalf("write the marker into %s: %v\n%s", volume, err, out)
	}
	before, _ := created()
	if before.IsZero() {
		t.Fatalf("the volume %s has no creation time to compare", volume)
	}

	// The edit: the volume gains a label, so its settings differ from the volume the engine holds.
	deploy(compose("\n    labels:\n      tier: db"))

	after, labels := created()
	if !after.Equal(before) {
		t.Errorf("the volume was created at %v after the redeploy, want the one made at %v kept", after, before)
	}
	if _, edited := labels["tier"]; edited {
		t.Errorf("the volume carries the label of the edit %v, want its settings left as they were", labels)
	}
	out, err := node.Sh(ctx, "docker run --rm -v "+volume+":/data busybox:1.36.1 cat /data/marker")
	if err != nil {
		t.Fatalf("read the marker from %s: %v\n%s", volume, err, out)
	}
	if got := strings.TrimSpace(out); got != "kept" {
		t.Errorf("the marker in %s reads %q, want %q: the redeploy must not recreate a volume", volume, got, "kept")
	}
	rows, err := cli.ContainersWithLabel(ctx, stacks.LabelComposeProject, name)
	if err != nil || len(rows) != 1 || rows[0].State != "running" {
		t.Errorf("containers after the redeploy = %+v, %v, want the stack's one running", rows, err)
	}
}

// startRequestLog serves a unix socket in front of the engine at host (a DockerHost value such as
// tcp://localhost:32771) that forwards every request and remembers its method and path without the API version.
// It returns the socket's path and a function that reads what was remembered, so a test shows which requests a
// step sent to the engine, in particular the distribution checks that look a tag up in a registry.
func startRequestLog(t *testing.T, host string) (string, func() []string) {
	t.Helper()
	target, err := url.Parse("http://" + strings.TrimPrefix(host, "tcp://"))
	if err != nil {
		t.Fatal(err)
	}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(target) }, FlushInterval: -1}
	var mu sync.Mutex
	var seen []string
	// A short path: a unix socket path may not be longer than about 100 bytes.
	dir, err := os.MkdirTemp("", "cmx")
	if err != nil {
		t.Fatalf("make the log's directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+apiVersion.ReplaceAllString(r.URL.Path, "/"))
		mu.Unlock()
		rp.ServeHTTP(w, r)
	}), ReadHeaderTimeout: time.Minute}
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.Serve(l) })
	t.Cleanup(func() {
		_ = srv.Close()
		wg.Wait()
	})
	return path, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// TestStandaloneRollbackOnARealEngine rolls a stack back to its first revision on a real engine (plan 08-05, STK-08,
// D-02, D-06, T-08-18). The stack is deployed at a tag, the tag is moved in the registry and the stack is deployed
// again (an update, which looks the tag up), so the host runs the second image. The roll back is reviewed against
// the host, then run through the Compose SDK with the pins revision 1 stored: the container runs the first image
// again although the tag now names the second, the review of that revision then lists no change, and the engine was
// asked for no registry lookup from the review to the end of the roll back.
func TestStandaloneRollbackOnARealEngine(t *testing.T) {
	// The SDK's process-wide state: its private configuration directory and its logging.
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/cm/web:1"
	first := dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)
	// TierPlan's own client to the node, for what the test reads back: the project's containers.
	cli, err := dockerx.New(dockertest.TCPTransportFor(node.Host()))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	socket, requests := startRequestLog(t, node.Host())
	rig := newRigOver(t, socket)
	rig.deps.ConvergeTimeout = 3 * time.Minute
	pins := &memPins{}
	const name = "itest"
	compose := "services:\n  web:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n"
	// The test's own context: a 30 s one would run out during the deploys on a slow runner.
	ctx := t.Context()
	web := func() dockerx.ContainerRow {
		t.Helper()
		rows, err := cli.ContainersWithLabel(ctx, stacks.LabelComposeProject, name)
		if err != nil || len(rows) != 1 || rows[0].State != "running" {
			t.Fatalf("the project's containers = %+v (%v), want one running", rows, err)
		}
		return rows[0]
	}
	deploy := func(rev string, in stackops.StandaloneDeployInput) stackops.DeployResult {
		t.Helper()
		in.Pins, in.RevisionID = pins, rev
		res, job := rig.deployStandalone(t, in)
		if !res.OK() {
			var words []string
			for _, it := range job.Items {
				if it.Detail != "" {
					words = append(words, it.ID+": "+it.Detail)
				}
			}
			t.Fatalf("deploy %s: %+v items %v details %q", rev, res, itemStates(job), words)
		}
		return res
	}

	// Revision 1 at the first image, then the tag moves and revision 2 updates to the second.
	deploy("rev-1", standaloneInput(name, compose))
	pin1 := image + "@" + first
	if got := pins.pinsOf("rev-1")["web"]; got != pin1 {
		t.Fatalf("revision 1 stored %q, want %q", got, pin1)
	}
	second := dockertest.PushImage(t, node, "alpine:3", image, nil)
	if second == first {
		t.Fatalf("the tag did not move: %s", second)
	}
	update := standaloneInput(name, compose)
	update.Update = map[string]bool{"web": true}
	if res := deploy("rev-2", update); res.Updated != 1 {
		t.Fatalf("update = %+v, want the container recreated on the second image", res)
	}
	pin2 := image + "@" + second
	before := web()
	if before.Image != pin2 {
		t.Fatalf("the container runs %q after the update, want %q", before.Image, pin2)
	}

	// The roll back review: revision 1 against the host, with its stored pin, asking no registry.
	mark := len(requests())
	review := rig.standaloneRollbackInput(t, name, compose, pins.pinsOf("rev-1"))
	rv, err := rig.deps.Review(ctx, review)
	if err != nil {
		t.Fatalf("roll back review: %v", err)
	}
	if got := changesOf(rv); !slices.Equal(got, []string{"changed web"}) || rv.Problems.Errors() != 0 {
		t.Fatalf("roll back review: services %v problems %v, want web changed and nothing blocking", got, rv.Problems)
	}
	if len(rv.Images) != 1 || rv.Images[0].State != stackops.ImagePinned || rv.Images[0].Digest != first || rv.Images[0].Current != second {
		t.Fatalf("roll back review images = %+v, want the first digest %s against the running %s", rv.Images, first, second)
	}

	// The roll back: the pins revision 1 stored, run as revision 3.
	rollback := standaloneInput(name, compose)
	rollback.Reviewed, rollback.Rollback = rv.Pins, true
	if res := deploy("rev-3", rollback); res.Updated != 1 || res.Services != 0 {
		t.Fatalf("roll back = %+v, want the one container recreated", res)
	}
	after := web()
	if after.ID == before.ID || after.Image != pin1 {
		t.Fatalf("the container is %s on %q after the roll back, want a new one on %q although the tag names %s", after.ID, after.Image, pin1, second)
	}
	if got := pins.pinsOf("rev-3")["web"]; got != pin1 {
		t.Errorf("the roll back's revision stored %q, want revision 1's %q", got, pin1)
	}

	// Revision 1 reviewed again against the host that runs it has nothing left to change.
	rv, err = rig.deps.Review(ctx, rig.standaloneRollbackInput(t, name, compose, pins.pinsOf("rev-1")))
	if err != nil {
		t.Fatalf("review after the roll back: %v", err)
	}
	if len(rv.Services) != 0 || rv.Problems.Errors() != 0 {
		t.Errorf("review after the roll back: services %v problems %v, want nothing to roll back", changesOf(rv), rv.Problems)
	}

	// From the review to the end of the roll back the engine was asked for no registry lookup.
	for _, req := range requests()[mark:] {
		if strings.Contains(req, "/distribution/") {
			t.Errorf("the engine was asked %q during the roll back, a roll back looks no tag up", req)
		}
	}
}

// TestSmallSubnetHashStableOnARealEngine shows on a real engine that small stack subnets never make the Compose SDK
// recreate a network (plan 10-25, D-20, D-22, RESEARCH F5, Pitfall 1, NET-06): the SDK stamps a hash of the whole
// network on it, so a deploy that sent the network a different subnet than it was made with would recreate it under its
// running containers. A stack deployed with small subnets on at /26 keeps the same network, ID and subnet, when it is
// deployed again with the setting off, when a service changes, and when it is rolled back; and a stack deployed before
// the setting was turned on keeps its network, with no subnet override, when it is deployed again with the setting on.
func TestSmallSubnetHashStableOnARealEngine(t *testing.T) {
	// The SDK's process-wide state: its private configuration directory and its logging.
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/cm/web:1"
	dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)
	cli, err := dockerx.New(dockertest.TCPTransportFor(node.Host()))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	rig := newRigOver(t, dockertest.UnixProxy(t, node.Host()))
	rig.deps.ConvergeTimeout = 3 * time.Minute
	repo := subnetRepo(t, rig)
	// The test's own context: a 30 s one would run out during the deploys on a slow runner.
	ctx := t.Context()
	compose := func(extra string) string {
		return "services:\n  web:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n" + extra +
			"    networks: [backend]\nnetworks:\n  backend: {}\n"
	}

	// stack is a stack in the store, deployed revision by revision.
	type stack struct {
		name string
		st   stackops.Stack
		revs []stackops.Snapshot
	}
	newStack := func(name string) *stack {
		t.Helper()
		copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose(""))}}}
		st, _, err := repo.CreateStack(ctx, rig.envID, name, stacks.TargetStandalone, "root", copy)
		if err != nil {
			t.Fatal(err)
		}
		return &stack{name: name, st: st}
	}
	// deploy runs the next revision of the stack and marks it good. A rollback restores the revision seq.
	deploy := func(s *stack, text string, rollback int) stackops.DeployResult {
		t.Helper()
		rev, err := repo.NewRevision(ctx, s.st.ID, "root", stackops.OriginDeploy)
		if err != nil {
			t.Fatal(err)
		}
		s.revs = append(s.revs, rev)
		in := stackops.StandaloneDeployInput{
			Stack: s.st, Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(text)}}},
			Pins: repo, Subnets: repo, RevisionID: rev.ID,
		}
		if rollback > 0 {
			target := s.revs[rollback-1]
			pins, err := repo.RevisionPins(ctx, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := repo.Revision(ctx, s.st.ID, target.Seq)
			if err != nil {
				t.Fatal(err)
			}
			in.Rollback, in.Reviewed, in.RestoredSubnets = true, pins, got.Subnets
		}
		res, job := rig.deployStandalone(t, in)
		if !res.OK() {
			var words []string
			for _, it := range job.Items {
				if it.Detail != "" {
					words = append(words, it.ID+": "+it.Detail)
				}
			}
			t.Fatalf("deploy of %s revision %d: %+v items %v details %q", s.name, len(s.revs), res, itemStates(job), words)
		}
		if err := repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
			t.Fatal(err)
		}
		return res
	}
	// network and web read back the stack's network and its one container with the network it is attached to.
	network := func(s *stack) dockerx.NetworkRow {
		t.Helper()
		rows, err := cli.NetworksWithLabel(ctx, stacks.LabelComposeProject, s.name)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range rows {
			if n.Name == s.name+"_backend" {
				return n
			}
		}
		t.Fatalf("%s has no network %s_backend: %+v", s.name, s.name, rows)
		return dockerx.NetworkRow{}
	}
	web := func(s *stack) (dockerx.ContainerDetail, dockerx.NetAttachment) {
		t.Helper()
		rows, err := cli.ContainersWithLabel(ctx, stacks.LabelComposeProject, s.name)
		if err != nil || len(rows) != 1 || rows[0].State != "running" {
			t.Fatalf("%s's containers = %+v (%v), want one running", s.name, rows, err)
		}
		d, _, err := cli.ContainerDetail(ctx, rows[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range d.Networks {
			if a.Name == s.name+"_backend" {
				return d, a
			}
		}
		t.Fatalf("%s's container is on %+v, want %s_backend", s.name, d.Networks, s.name)
		return d, dockerx.NetAttachment{}
	}
	subnetOf := func(n dockerx.NetworkRow) string {
		if len(n.Subnets) != 1 {
			t.Fatalf("network %s has the subnets %v, want one", n.Name, n.Subnets)
		}
		return n.Subnets[0]
	}

	// legacy is deployed before the setting is turned on: its network has the daemon's own subnet and a hash made without one.
	legacy := newStack("legacy")
	deploy(legacy, compose(""), 0)
	legacyNet := network(legacy)
	if bits := netip.MustParsePrefix(subnetOf(legacyNet)).Bits(); bits == 26 {
		t.Fatalf("legacy's network has %s before the setting was on", subnetOf(legacyNet))
	}

	rig.setSmallSubnets(t, true, 26)
	itest := newStack("itest")
	if res := deploy(itest, compose(""), 0); res.Networks != 1 {
		t.Fatalf("first deploy = %+v, want one network created", res)
	}
	net1 := network(itest)
	subnet1 := subnetOf(net1)
	if p := netip.MustParsePrefix(subnet1); p.Bits() != 26 {
		t.Fatalf("itest's network has %s, want a /26 chosen by small stack subnets", subnet1)
	}
	if rec := itest.revs[0]; rec.ID == "" {
		t.Fatal("no first revision")
	}
	if got, err := repo.Revision(ctx, itest.st.ID, itest.revs[0].Seq); err != nil || got.Subnets["itest_backend"] != subnet1 {
		t.Fatalf("revision 1 recorded %v (%v), want itest_backend on %s", got.Subnets, err, subnet1)
	}
	c1, att1 := web(itest)
	if att1.NetworkID != net1.ID {
		t.Fatalf("the container is on network %s, want %s", att1.NetworkID, net1.ID)
	}
	// same asserts the network is the one first made, with the subnet first chosen.
	same := func(step string) {
		t.Helper()
		n := network(itest)
		if n.ID != net1.ID || subnetOf(n) != subnet1 {
			t.Errorf("%s: the network is %s on %s, want %s on %s: the SDK recreated it", step, n.ID, subnetOf(n), net1.ID, subnet1)
		}
		if _, att := web(itest); att.NetworkID != net1.ID {
			t.Errorf("%s: the container is on network %s, want %s", step, att.NetworkID, net1.ID)
		}
	}

	// 1. Deployed again with the setting off: nothing changes, the record still sends the same subnet.
	rig.setSmallSubnets(t, false, 26)
	if res := deploy(itest, compose(""), 0); res.Networks != 0 || res.Updated != 0 {
		t.Errorf("redeploy with the setting off = %+v, want nothing created or recreated", res)
	}
	same("redeploy with the setting off")
	if c, att := web(itest); c.ID != c1.ID || att.IPv4 != att1.IPv4 {
		t.Errorf("redeploy with the setting off: container %s at %s, want %s at %s untouched", c.ID, att.IPv4, c1.ID, att1.IPv4)
	}

	// 2. A service changes: the container is recreated, the network is not.
	if res := deploy(itest, compose("    environment:\n      MODE: two\n"), 0); res.Updated != 1 {
		t.Errorf("changed service = %+v, want the container recreated", res)
	}
	same("a changed service")
	c2, _ := web(itest)
	if c2.ID == c1.ID {
		t.Errorf("the container %s is the old one after the change", c2.ID)
	}

	// 3. Rolled back to revision 1: the network is kept, with the subnet revision 1 recorded.
	if res := deploy(itest, compose(""), 1); res.Networks != 0 {
		t.Errorf("roll back = %+v, want no network created", res)
	}
	same("the roll back")

	// 4. The stack deployed before the setting existed keeps its network, with no subnet override, with the setting on.
	rig.setSmallSubnets(t, true, 26)
	if res := deploy(legacy, compose(""), 0); res.Networks != 0 || res.Updated != 0 {
		t.Errorf("legacy redeploy with the setting on = %+v, want nothing created or recreated", res)
	}
	if n := network(legacy); n.ID != legacyNet.ID || subnetOf(n) != subnetOf(legacyNet) {
		t.Errorf("legacy's network is %s on %s, want %s on %s: the setting changed a network that was made without it", n.ID, subnetOf(n), legacyNet.ID, subnetOf(legacyNet))
	}
	if got, err := repo.Revision(ctx, legacy.st.ID, legacy.revs[len(legacy.revs)-1].Seq); err != nil || len(got.Subnets) != 0 {
		t.Errorf("legacy's revision recorded %v (%v), want no subnet", got.Subnets, err)
	}
}
