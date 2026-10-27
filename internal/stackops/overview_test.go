// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The Overview of a deployed stack (plan 07-23, STK-10, D-22): the services the stack owns on the engine, with
// what runs, the image as written and as pinned, and the ports. Docker is the source of truth.

var overviewDigest = stackopstest.Digest("b")

func swarmStack() stackops.Stack {
	return stackops.Stack{ID: "stack-web", Name: "web", Target: stacks.TargetSwarm}
}

func standaloneStack(name string) stackops.Stack {
	return stackops.Stack{ID: "stack-" + name, Name: name, Target: stacks.TargetStandalone}
}

func overviewLabels(written string) map[string]string {
	l := map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stack-web"}
	if written != "" {
		l["bayplan.stack.image"] = written
	}
	return l
}

func TestOverviewOfASwarmStackListsEachServiceItOwnsWithItsCountsImageAndPorts(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil,
		dockertest.FakeService{
			ID: "svc-web", Name: "web_app", Mode: "replicated", Running: 2, Desired: 3, Image: "nginx:1.27@" + overviewDigest,
			Labels: overviewLabels("nginx:1.27"), Ports: []swarm.PortConfig{{PublishedPort: 8080, TargetPort: 80, Protocol: "tcp", PublishMode: "ingress"}},
		},
		dockertest.FakeService{ID: "svc-db", Name: "web_db", Mode: "replicated", Running: 1, Desired: 1, Image: "postgres:16@" + overviewDigest, Labels: overviewLabels("postgres:16")},
		dockertest.FakeService{ID: "svc-other", Name: "blog_app", Mode: "replicated", Running: 1, Desired: 1, Image: "x", Labels: map[string]string{"com.docker.stack.namespace": "blog", "bayplan.stack.id": "stack-blog"}},
		dockertest.FakeService{ID: "svc-foreign", Name: "web_foreign", Mode: "replicated", Running: 1, Desired: 1, Image: "y", Labels: map[string]string{"com.docker.stack.namespace": "web"}},
	)

	rows, err := rig.deps.Overview(testsupport.Context(t), rig.envID, swarmStack())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two services the stack owns", rows)
	}
	app, db := rows[0], rows[1]
	if db.Name != "web_db" || db.Short != "db" || db.ID != "svc-db" || db.State != stackops.ServiceRunning || db.Running != 1 || db.Desired != 1 {
		t.Errorf("db = %+v", db)
	}
	if app.Short != "app" || app.State != stackops.ServiceDegraded || app.Running != 2 || app.Desired != 3 || app.Mode != "replicated" {
		t.Errorf("app = %+v", app)
	}
	if app.Image != "nginx:1.27" || app.Pinned != "nginx:1.27@"+overviewDigest || app.PinState != stackops.PinPinned {
		t.Errorf("app image = %q pinned = %q state = %q", app.Image, app.Pinned, app.PinState)
	}
	if !slices.Equal(app.Ports, []string{"8080->80/tcp"}) {
		t.Errorf("ports = %v", app.Ports)
	}
	if len(app.Containers) != 0 {
		t.Errorf("a swarm service lists no containers, got %v", app.Containers)
	}
}

func TestSwarmRowsRollUpTheStateOfEachService(t *testing.T) {
	t.Parallel()
	row := func(mode string, running, desired uint64, update string) dockerx.ServiceRow {
		return dockerx.ServiceRow{
			ID: "s", Name: "web_a", Mode: mode, Running: running, Desired: desired, UpdateState: update, Stack: "web",
			Labels: map[string]string{"bayplan.stack.id": "stack-web"}, Image: "a",
		}
	}
	for name, tc := range map[string]struct {
		row  dockerx.ServiceRow
		want string
	}{
		"all running":                {row("replicated", 3, 3, ""), stackops.ServiceRunning},
		"some running":               {row("replicated", 1, 3, ""), stackops.ServiceDegraded},
		"none running":               {row("replicated", 0, 3, ""), stackops.ServiceNotRunning},
		"scaled to zero":             {row("replicated", 0, 0, ""), stackops.ServiceStopped},
		"an update that stopped":     {row("replicated", 3, 3, "paused"), stackops.ServiceDegraded},
		"a roll back that stopped":   {row("replicated", 3, 3, "rollback_paused"), stackops.ServiceDegraded},
		"an update under way":        {row("replicated", 3, 3, "updating"), stackops.ServiceRunning},
		"a global service on a node": {row("global", 1, 1, ""), stackops.ServiceRunning},
		"a job that finished":        {row("replicated-job", 0, 0, ""), stackops.ServiceRunning},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := stackops.SwarmRows(swarmStack(), []dockerx.ServiceRow{tc.row})
			if len(got) != 1 || got[0].State != tc.want {
				t.Errorf("state = %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestSwarmRowsTellAPinnedImageFromANewOneAndALocalOne(t *testing.T) {
	t.Parallel()
	id := "sha256:" + strings.Repeat("c", 64)
	for name, tc := range map[string]struct {
		image, written, want string
	}{
		"a digest":           {"nginx:1.27@" + overviewDigest, "nginx:1.27", stackops.PinPinned},
		"a tag with none":    {"nginx:1.27", "nginx:1.27", stackops.PinNew},
		"an image ID":        {id, "built:local", stackops.PinLocal},
		"no label to read":   {"nginx:1.27@" + overviewDigest, "", stackops.PinPinned},
		"a short image ID":   {"0123456789ab", "built:local", stackops.PinLocal},
		"a name that is hex": {"deadbeefdeadbeef/app:1", "deadbeefdeadbeef/app:1", stackops.PinNew},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := dockerx.ServiceRow{ID: "s", Name: "web_a", Stack: "web", Image: tc.image, Labels: overviewLabels(tc.written)}
			got := stackops.SwarmRows(swarmStack(), []dockerx.ServiceRow{r})[0]
			if got.PinState != tc.want {
				t.Errorf("pin state = %s, want %s", got.PinState, tc.want)
			}
			if tc.written == "" && got.Image != "nginx:1.27" {
				t.Errorf("image without a label = %q, want the reference without its digest", got.Image)
			}
		})
	}
}

func TestOverviewOfAStandaloneStackGroupsContainersUnderTheirService(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	rig := hostRig(t, h)

	rows, err := rig.deps.Overview(testsupport.Context(t), rig.envID, standaloneStack("demo"))
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	if len(rows) != 2 || rows[0].Name != "db" || rows[1].Name != "web" {
		t.Fatalf("rows = %+v", rows)
	}
	web := rows[1]
	if web.ID != "" || web.Short != "web" || web.State != stackops.ServiceRunning || web.Running != 1 || web.Desired != 1 {
		t.Errorf("web = %+v", web)
	}
	if web.Image != "nginx:1.27-alpine" || web.Pinned != h.Pins["web"] || web.PinState != stackops.PinPinned {
		t.Errorf("web image = %q pinned = %q state = %q", web.Image, web.Pinned, web.PinState)
	}
	if len(web.Containers) != 1 || web.Containers[0].ID != h.IDs["web"] || web.Containers[0].State != "running" {
		t.Errorf("containers = %+v", web.Containers)
	}
}

func TestOverviewOfAStandaloneStackLeavesOutAnotherProjectsContainers(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	h.Containers = append(h.Containers, dockertest.FakeContainer{
		ID: "other-1", Names: []string{"/other-web-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{stacks.LabelComposeProject: "other", stacks.LabelComposeService: "web"},
	}, dockertest.FakeContainer{
		ID: "foreign-1", Names: []string{"/demo-foreign-1"}, Image: "nginx", State: "running",
		// Another stack's container in the project. A container with no ID beside the stack's own is the host CLI's
		// addition and is listed since G-08-6 (TestStandaloneRowsCountAContainerTheHostCLIAdded); this one is not.
		Labels: map[string]string{stacks.LabelComposeProject: "demo", stacks.LabelComposeService: "foreign", stacks.LabelStackID: "stack-other"},
	})
	rig := hostRig(t, h)

	rows, err := rig.deps.Overview(testsupport.Context(t), rig.envID, standaloneStack("demo"))
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	for _, r := range rows {
		if r.Name != "db" && r.Name != "web" {
			t.Errorf("row %q is not the stack's", r.Name)
		}
	}
	if len(rows) != 2 {
		t.Errorf("rows = %+v, want the two services the stack owns", rows)
	}
}

func TestStandaloneRowsRollUpTheContainersOfAService(t *testing.T) {
	t.Parallel()
	c := func(name, state, health string) dockerx.ContainerRow {
		return dockerx.ContainerRow{
			ID: name, Name: "/" + name, State: state, Health: health, Image: "nginx:1@" + overviewDigest,
			Labels: map[string]string{stacks.LabelComposeProject: "demo", stacks.LabelComposeService: "web", stacks.LabelStackID: "stack-demo"},
		}
	}
	for name, tc := range map[string]struct {
		in      []dockerx.ContainerRow
		want    string
		running int
	}{
		"all running":               {[]dockerx.ContainerRow{c("a", "running", "none"), c("b", "running", "healthy")}, stackops.ServiceRunning, 2},
		"one of two":                {[]dockerx.ContainerRow{c("a", "running", "none"), c("b", "exited", "none")}, stackops.ServiceDegraded, 1},
		"a failing health check":    {[]dockerx.ContainerRow{c("a", "running", "unhealthy")}, stackops.ServiceDegraded, 1},
		"all exited":                {[]dockerx.ContainerRow{c("a", "exited", "none"), c("b", "exited", "none")}, stackops.ServiceStopped, 0},
		"one that keeps restarting": {[]dockerx.ContainerRow{c("a", "restarting", "none")}, stackops.ServiceNotRunning, 0},
		"one never started":         {[]dockerx.ContainerRow{c("a", "created", "none")}, stackops.ServiceNotRunning, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rows := stackops.StandaloneRows(standaloneStack("demo"), tc.in)
			if len(rows) != 1 || rows[0].State != tc.want || rows[0].Running != tc.running || rows[0].Desired != len(tc.in) {
				t.Errorf("rows = %+v, want %s with %d of %d running", rows, tc.want, tc.running, len(tc.in))
			}
		})
	}
}

func TestStandaloneRowsListContainersByNameAndLeaveOutOneOffs(t *testing.T) {
	t.Parallel()
	labels := func(extra ...string) map[string]string {
		l := map[string]string{stacks.LabelComposeProject: "demo", stacks.LabelComposeService: "web", stacks.LabelStackID: "stack-demo"}
		for i := 0; i+1 < len(extra); i += 2 {
			l[extra[i]] = extra[i+1]
		}
		return l
	}
	rows := stackops.StandaloneRows(standaloneStack("demo"), []dockerx.ContainerRow{
		{ID: "2", Name: "/demo-web-2", State: "running", Image: "x", Labels: labels()},
		{ID: "1", Name: "/demo-web-1", State: "running", Image: "x", Labels: labels()},
		{ID: "3", Name: "/demo-web-run-1", State: "running", Image: "x", Labels: labels("com.docker.compose.oneoff", "True")},
	})

	if len(rows) != 1 || len(rows[0].Containers) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Containers[0].Name != "demo-web-1" || rows[0].Containers[1].Name != "demo-web-2" {
		t.Errorf("containers = %+v, want them by name without the slash the engine adds", rows[0].Containers)
	}
}

func TestStandaloneRowsShowPublishedPortsOnceWithoutTheHostAddress(t *testing.T) {
	t.Parallel()
	rows := stackops.StandaloneRows(standaloneStack("demo"), []dockerx.ContainerRow{{
		ID: "1", Name: "/demo-web-1", State: "running", Image: "x",
		Ports:  []string{"0.0.0.0:8080->80/tcp", "[::]:8080->80/tcp", "443/tcp", "127.0.0.1:9000->9000/udp"},
		Labels: map[string]string{stacks.LabelComposeProject: "demo", stacks.LabelComposeService: "web", stacks.LabelStackID: "stack-demo"},
	}})

	if len(rows) != 1 || !slices.Equal(rows[0].Ports, []string{"8080->80/tcp", "9000->9000/udp"}) {
		t.Errorf("ports = %+v", rows)
	}
}

// TestStandaloneRowsCountAContainerTheHostCLIAdded: docker compose up --scale web=2 adds web-2 to the project with none of
// Bayplan's labels. Beside web-1, which carries the stack's ID, it is a container of the service on the stack's page and
// in the lists' counts. Another stack's container, a project the stack owns no container of, and a one-off stay out
// (G-08-6, user decision 2026-10-27, D-09).
func TestStandaloneRowsCountAContainerTheHostCLIAdded(t *testing.T) {
	t.Parallel()
	row := func(name, service string, extra map[string]string) dockerx.ContainerRow {
		l := map[string]string{stacks.LabelComposeProject: "demo", stacks.LabelComposeService: service}
		for k, v := range extra {
			l[k] = v
		}
		return dockerx.ContainerRow{ID: name, Name: "/" + name, State: "running", Image: "nginx:1", Labels: l}
	}
	own := map[string]string{stacks.LabelStackID: "stack-demo"}
	other := map[string]string{stacks.LabelStackID: "stack-other"}
	names := func(rows []stackops.ServiceRow) []string {
		var out []string
		for _, r := range rows {
			for _, c := range r.Containers {
				out = append(out, r.Short+":"+c.Name)
			}
		}
		return out
	}
	for name, tc := range map[string]struct {
		in   []dockerx.ContainerRow
		want []string
	}{
		"added from the host CLI":          {[]dockerx.ContainerRow{row("demo-web-1", "web", own), row("demo-web-2", "web", nil)}, []string{"web:demo-web-1", "web:demo-web-2"}},
		"a service only the CLI added":     {[]dockerx.ContainerRow{row("demo-web-1", "web", own), row("demo-ghost-1", "ghost", nil)}, []string{"ghost:demo-ghost-1", "web:demo-web-1"}},
		"another stack's container":        {[]dockerx.ContainerRow{row("demo-web-1", "web", own), row("demo-web-2", "web", other)}, []string{"web:demo-web-1"}},
		"a project the stack owns none of": {[]dockerx.ContainerRow{row("demo-web-1", "web", nil), row("demo-web-2", "web", nil)}, nil},
		"a one-off beside the stack's own": {[]dockerx.ContainerRow{row("demo-web-1", "web", own), row("demo-web-run-1", "web", map[string]string{"com.docker.compose.oneoff": "True"})}, []string{"web:demo-web-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rows := stackops.StandaloneRows(standaloneStack("demo"), tc.in)
			if got := names(rows); !slices.Equal(got, tc.want) {
				t.Errorf("containers = %v, want %v", got, tc.want)
			}
			for _, r := range rows {
				n := 0
				for _, w := range tc.want {
					if strings.HasPrefix(w, r.Short+":") {
						n++
					}
				}
				if r.Desired != n || r.Running != n {
					t.Errorf("%s = %+v, want %d containers counted", r.Short, r, n)
				}
			}
		})
	}
}
