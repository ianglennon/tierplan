// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package agents_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The standalone ingress round trip of plan 25-21 (DEP-02, D-03, D-08, D-20, D-24): a full TierPlan server and the
// real agent image on a dind standalone host. The in-process harness of internal/ingressdeploy proxies straight to
// the engine, so a refusal by the agent's own allowlist (a container create the policy judges as host access, the
// token archive's ownership) only shows here. The agent is installed with the command the server generated and
// nothing added: no opt-in setting (D-03, D-24).

const (
	// ingressJobWait is how long an enable or disable job gets to end, an image lookup, a pull and a health wait included.
	ingressJobWait = 5 * time.Minute
	// ingressEnrolWait is how long the proxy gets to dial the server, enrol and be counted after the enable job ended.
	ingressEnrolWait = 3 * time.Minute
	// releaseRegistry is the registry the proxy image is published to, releaseFixture the host's container that plays it.
	releaseRegistry = "ghcr.io"
	releaseFixture  = "tp-release-registry"
	releaseDir      = "/tmp/tp-release"
)

// proxyContainer is the name Compose gives the one proxy container of the host's project.
const proxyContainer = ingress.ProjectName + "-" + ingress.ProxyComposeService + "-1"

// tokenFileHeader is the tar header of the join token file as `tar -tvf` prints it: read-only for everyone, owned by 0:0.
var tokenFileHeader = regexp.MustCompile(`^-r--r--r--\s+(0/0|root/root|0\s+0)\s`)

// serveReleaseImage makes the host's engine find the proxy image where a real one would: in the release registry. The
// dind host maps that registry's name to its own loopback, which would send the lookup of the image's digest and the
// pull nowhere, so the registry here is a container of the host on its default bridge listening on 443 over TLS with
// a certificate of a test CA the engine is told to trust, and the host's hosts file points the name at it. The
// development image `make ingress-image` builds is pushed there as the tag a development server installs.
func serveReleaseImage(t *testing.T, host *dockertest.Node) {
	t.Helper()
	ctx := testsupport.Context(t)
	dockertest.LoadImage(t, "tierplan-ingress:dev", host)
	dockertest.LoadImage(t, dockertest.RegistryImage, host)
	image := ingressdeploy.ImageRepository + ":edge"
	sh(t, host, "docker tag tierplan-ingress:dev "+image)
	ca := traefiktest.NewCA(t)
	chain, key := ca.Leaf(t, releaseRegistry)
	sh(t, host, "mkdir -p "+releaseDir+" /etc/docker/certs.d/"+releaseRegistry)
	for path, data := range map[string][]byte{
		releaseDir + "/tls.crt": chain, releaseDir + "/tls.key": key, "/etc/docker/certs.d/" + releaseRegistry + "/ca.crt": ca.PEM(),
	} {
		if err := host.WriteFile(ctx, path, data); err != nil {
			t.Fatalf("write %s on the host: %v", path, err)
		}
	}
	sh(t, host, "docker run -d --name "+releaseFixture+" -e REGISTRY_HTTP_ADDR=0.0.0.0:443 "+
		"-e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key "+
		"-v "+releaseDir+"/tls.crt:/certs/tls.crt:ro -v "+releaseDir+"/tls.key:/certs/tls.key:ro "+dockertest.RegistryImage)
	ip := strings.TrimSpace(sh(t, host, "docker inspect -f '{{(index .NetworkSettings.Networks \"bridge\").IPAddress}}' "+releaseFixture))
	dockertest.Wait(t, 90*time.Second, "the release registry fixture to listen on 443", func() (bool, string) {
		out, err := host.Sh(ctx, "nc -z "+ip+" 443")
		return err == nil, strings.TrimSpace(out)
	})
	sh(t, host, "grep -v '"+releaseRegistry+"' /etc/hosts > /tmp/hosts.new; echo '"+ip+" "+releaseRegistry+"' >> /tmp/hosts.new; cat /tmp/hosts.new > /etc/hosts")
	// The engine re-reads its hosts file on its own schedule, so the first push may still go to the old address.
	dockertest.Wait(t, 2*time.Minute, "the engine to push the proxy image to the release registry fixture", func() (bool, string) {
		out, err := host.Sh(ctx, "docker push "+image)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		return err == nil, strings.TrimSpace(lines[len(lines)-1])
	})
}

func (f *saFix) ingressAPI(suffix string) string { return "/api/env/" + f.env.ID + "/ingress" + suffix }

// ingressJob starts an enable or disable at path, which has to answer 202, and waits until its job ends. It returns the
// job as it ended.
func (f *saFix) ingressJob(t *testing.T, path string, body any) saJob {
	t.Helper()
	code, raw := f.srv.Do(http.MethodPost, f.ingressAPI(path), body)
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(raw, &started) != nil || started.JobID == "" {
		t.Fatalf("POST ingress%s = %d %s, want 202 with a job", path, code, raw)
	}
	var job saJob
	testsupport.Poll(t, ingressJobWait, "the ingress job "+started.JobID+" to end", func() (bool, string) {
		code, raw := f.srv.Do(http.MethodGet, f.ingressAPI("/jobs/"+started.JobID), nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, raw)
		}
		job = saJob{}
		if err := json.Unmarshal(raw, &job); err != nil {
			return false, string(raw)
		}
		return job.State != "running", string(raw)
	})
	return job
}

// ingressNodes reads how many proxies the status counts as enrolled and ready.
func (f *saFix) ingressNodes(t *testing.T) (enrolled, ready int) {
	t.Helper()
	code, raw := f.srv.Do(http.MethodGet, f.ingressAPI("/status"), nil)
	var st struct {
		Nodes struct {
			Enrolled int `json:"enrolled"`
			Ready    int `json:"ready"`
		} `json:"nodes"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &st) != nil {
		t.Fatalf("GET ingress status = %d %s", code, raw)
	}
	return st.Nodes.Enrolled, st.Nodes.Ready
}

// agentLogs is what every agent container of the host logged since it started. The agent logs its refusals, and the
// log carries no token.
func (f *saFix) agentLogs(t *testing.T) string {
	t.Helper()
	return sh(t, f.host, "for c in $(docker ps -aq --filter label=tierplan.agent=true); do docker logs $c 2>&1; done")
}

// TestStandaloneIngressThroughTheRealAgent switches ingress on for a standalone host through a real server and a real
// agent and then off again (D-08, D-24, D-20, PROH-25-01). The Compose SDK's container create and the token archive
// cross the agent's own route table and body rules with no opt-in on the agent, the proxy enrols, its secret file is
// readable by the proxy's user and owned by root, its container asks for nothing more than a plain one, and the
// switch-off leaves no container, network or volume of the project behind.
func TestStandaloneIngressThroughTheRealAgent(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	serveReleaseImage(t, f.host)
	// The proxy's own log is read on failure only; it carries no token.
	t.Cleanup(func() {
		if t.Failed() {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			out, _ := f.host.Sh(ctx, "docker logs --tail 40 "+proxyContainer+" 2>&1")
			t.Logf("the proxy's last log lines:\n%s", strings.TrimSpace(out))
		}
	})

	// The preview says what the enable would create and finds no error.
	code, raw := f.srv.Do(http.MethodPost, f.ingressAPI("/enable/preview"), map[string]any{})
	var preview struct {
		Findings []struct {
			Severity string `json:"severity"`
			Code     string `json:"code"`
		} `json:"findings"`
		Creates struct {
			Standalone bool `json:"standalone"`
		} `json:"creates"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &preview) != nil {
		t.Fatalf("POST the enable preview = %d %s", code, raw)
	}
	for _, fd := range preview.Findings {
		if fd.Severity == "error" {
			t.Fatalf("the enable preview found an error: %s", raw)
		}
	}
	if !preview.Creates.Standalone {
		t.Fatalf("the enable preview is not a standalone host's plan: %s", raw)
	}

	// Enable: 202, the job ends done, the proxy enrols.
	job := f.ingressJob(t, "/enable", map[string]any{})
	if job.State != "done" {
		out, _ := json.Marshal(job)
		t.Fatalf("the enable job ended %s, want done: %s", job.State, out)
	}
	testsupport.Poll(t, ingressEnrolWait, "the status to read one proxy enrolled", func() (bool, string) {
		enrolled, ready := f.ingressNodes(t)
		return enrolled == 1, fmt.Sprintf("enrolled %d, ready %d", enrolled, ready)
	})

	// The token's file is readable by the proxy's user (444) and owned by root, written through the agent's archive route.
	header := strings.TrimSpace(sh(t, f.host, "docker cp "+proxyContainer+":/run/secrets/tierplan-ingress-token - | tar -tvf -"))
	if !tokenFileHeader.MatchString(header) {
		t.Errorf("the token file's header = %q, want mode 444 owned by 0:0", header)
	}

	// The container asks for what a plain container does.
	inspect := func(format string) string {
		return strings.TrimSpace(sh(t, f.host, "docker inspect -f '"+format+"' "+proxyContainer))
	}
	for format, want := range map[string][]string{
		"{{.HostConfig.Privileged}}":                                     {"false"},
		"{{json .HostConfig.CapAdd}}":                                    {"null", "[]"},
		"{{json .HostConfig.Devices}}":                                   {"null", "[]"},
		"{{.HostConfig.NetworkMode}}":                                    {ingress.ProjectName},
		"{{.HostConfig.PidMode}}":                                        {""},
		"{{.HostConfig.IpcMode}}":                                        {"private", "shareable", ""},
		"{{range $i, $m := .Mounts}}{{if $i}} {{end}}{{$m.Type}}{{end}}": {"volume", "volume volume"},
	} {
		got := inspect(format)
		if !slices.Contains(want, got) {
			t.Errorf("docker inspect %s = %q, want one of %q", format, got, want)
		}
	}
	// The cache is a named volume: a bind whose source is a host path would be a host path of the host.
	for _, bind := range strings.Fields(inspect("{{range .HostConfig.Binds}}{{println .}}{{end}}")) {
		if strings.HasPrefix(bind, "/") {
			t.Errorf("the proxy binds a host path: %s", bind)
		}
	}
	if mounts := inspect("{{range .Mounts}}{{.Source}} {{end}}"); strings.Contains(mounts, "docker.sock") {
		t.Errorf("the proxy mounts the Docker socket: %s", mounts)
	}

	// Disable: 202, the job ends done, nothing of the project is left on the host.
	job = f.ingressJob(t, "/disable", nil)
	if job.State != "done" {
		out, _ := json.Marshal(job)
		t.Fatalf("the disable job ended %s, want done: %s", job.State, out)
	}
	for what, line := range map[string]string{
		"containers of the project": "docker ps -a --filter label=com.docker.compose.project=" + ingress.ProjectName + " -q",
		"the network":               "docker network ls --filter name=^" + ingress.ProjectName + "$ -q",
		"the cache volume":          "docker volume ls --filter name=^" + ingress.ProjectName + "-cache$ -q",
	} {
		if got := strings.TrimSpace(sh(t, f.host, line)); got != "" {
			t.Errorf("%s after the switch-off = %q, want none", what, got)
		}
	}

	// The agent refused nothing during either job.
	f.noDenials(t)
	logs := f.agentLogs(t)
	for _, bad := range []string{"agent_policy_denied", "host access"} {
		if strings.Contains(logs, bad) {
			t.Errorf("the agent's log holds %q: it refused a request of the enable or the disable", bad)
		}
	}
}

// TestStandaloneReservedStackNameThroughTheServer asks the full server for a stack named like the proxy's project on a
// standalone host (D-10): the answer is 422 stack_name_reserved with the one sentence, and no stack is made.
func TestStandaloneReservedStackNameThroughTheServer(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	compose := "services:\n  web:\n    image: " + f.registry + "/tp/app:1\n"
	code, raw := f.srv.Do(http.MethodPost, f.api(), map[string]any{"name": ingress.ProjectName, "compose": compose})
	var problem struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if code != http.StatusUnprocessableEntity || json.Unmarshal(raw, &problem) != nil || !strings.HasSuffix(problem.Type, "stack_name_reserved") {
		t.Fatalf("POST stacks named %s = %d %s, want 422 stack_name_reserved", ingress.ProjectName, code, raw)
	}
	if !strings.Contains(string(raw), stacks.NameReservedText) {
		t.Errorf("the refusal does not carry the one sentence: %s", raw)
	}
	code, raw = f.srv.Do(http.MethodGet, f.api(), nil)
	if code != http.StatusOK || strings.Contains(string(raw), `"`+ingress.ProjectName+`"`) {
		t.Errorf("GET stacks = %d %s, want a list without %s", code, raw, ingress.ProjectName)
	}
}
