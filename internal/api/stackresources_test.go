// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The Resources tab of a stack (plan 07-25, STK-10, D-22): everything the stack owns on its environment, one card
// for each kind, each read on its own. Docker is a fake engine behind the real monitor, aggregator and cluster
// reader.

type resourcesCardError struct {
	Error string `json:"error"`
}

type stackResourcesAnswer struct {
	Tasks *struct {
		resourcesCardError
		Items []struct {
			Task struct {
				ID, State, DockerState, Node, NodeID string
				Slot                                 int
			} `json:"task"`
			ServiceID, Service string
			Global             bool
		} `json:"items"`
	} `json:"tasks"`
	Containers *struct {
		resourcesCardError
		Items []struct {
			ID, Name, Service, State, Health, Image string
			Ports                                   []string
		} `json:"items"`
	} `json:"containers"`
	Networks struct {
		resourcesCardError
		Items []struct{ ID, Name, Driver, Scope, Ownership string } `json:"items"`
	} `json:"networks"`
	Volumes struct {
		resourcesCardError
		Items []struct {
			Name, Driver, Node, NodeID, Ownership string
		} `json:"items"`
		NotReporting []string `json:"notReporting"`
	} `json:"volumes"`
	Secrets stackObjectsCard `json:"secrets"`
	Configs stackObjectsCard `json:"configs"`
}

type stackObjectsCard struct {
	resourcesCardError
	Items []struct {
		ID, Name, FullName, Hash, Ownership string
		UsedBy                              []struct{ ID, Name string }
	} `json:"items"`
}

func TestStackResourcesOfADeployedSwarmStackListItsTasksAndNetworks(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	// Another stack of the same swarm, under its own namespace, owns tasks and a network of its own.
	other := newStack(t, admin, "other")
	if job := f.deployAndWait(t, operator, other.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}

	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/resources", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackResourcesAnswer](t, rec)
	if got.Tasks == nil || got.Containers != nil {
		t.Fatalf("a swarm stack answers the tasks card and not the containers card: %s", rec.Body)
	}
	if got.Tasks.Error != "" || len(got.Tasks.Items) != 1 {
		t.Fatalf("tasks = %+v, want the one task of this stack's one service", got.Tasks)
	}
	task := got.Tasks.Items[0]
	if task.Service != "web" || task.ServiceID == "" || task.Global || task.Task.Slot != 1 || task.Task.State != "running" || task.Task.ID == "" {
		t.Errorf("task = %+v", task)
	}
	if got.Networks.Error != "" {
		t.Errorf("networks error = %q", got.Networks.Error)
	}
	if len(got.Networks.Items) != 1 || got.Networks.Items[0].Name != "tracer_default" || got.Networks.Items[0].Ownership != "stack" || got.Networks.Items[0].Scope != "swarm" {
		t.Errorf("networks = %+v, want this stack's default network alone, not the other stack's", got.Networks.Items)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestStackResourcesOfAStackNeverDeployedAreEmptyCardsNotAnError(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "fresh")

	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/resources", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tasks":{"items":[],"error":""}`) || !strings.Contains(body, `"networks":{"items":[],"error":""}`) {
		t.Errorf("resources = %s, want empty cards with no error, not a missing card", body)
	}
}

func TestStackResourcesOfAStackThatIsNotThisEnvironmentsIs404(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	viewer := f.caller(t, e, "viewer-1", "viewer")

	rec := viewer.do(http.MethodGet, "stacks/00000000-0000-0000-0000-000000000000/resources", "")

	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("resources of a foreign stack: %d %s, want 404 stack_not_found", rec.Code, rec.Body)
	}
}

func TestStackResourcesOfAStandaloneStackListItsContainersAndNetworks(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	operator, st, h := deployedStandalone(t, f, e, standaloneReviewCompose)
	standOnHost(f, h)

	rec := operator.do(http.MethodGet, "stacks/"+st.ID+"/resources", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackResourcesAnswer](t, rec)
	if got.Containers == nil || got.Tasks != nil {
		t.Fatalf("a standalone stack answers the containers card and not the tasks card: %s", rec.Body)
	}
	if got.Containers.Error != "" || len(got.Containers.Items) != 1 {
		t.Fatalf("containers = %+v", got.Containers)
	}
	web := got.Containers.Items[0]
	if web.ID != h.IDs["web"] || web.Service != "web" || web.State != "running" || web.Name == "" || strings.HasPrefix(web.Name, "/") {
		t.Errorf("web = %+v", web)
	}
	if len(h.Networks) == 0 || len(got.Networks.Items) != len(h.Networks) {
		t.Fatalf("networks = %+v, want the %d the project made", got.Networks.Items, len(h.Networks))
	}
	for _, n := range got.Networks.Items {
		if n.Ownership != "stack" || !strings.HasPrefix(n.Name, "solo_") {
			t.Errorf("network %+v is the project's own, owned by the stack", n)
		}
	}
}

//nolint:gosec // a compose file that names a secret, not a credential
const composeWithAFileSecret = "services:\n  web:\n    image: nginx:1.27-alpine\n    secrets: [tok]\nsecrets:\n  tok:\n    file: tok.txt\n"

// saveWithSecretFile saves composeWithAFileSecret with the secret file tok.txt holding value.
func saveWithSecretFile(t *testing.T, c *roleCaller, stackID, value string) int64 {
	t.Helper()
	cb, _ := json.Marshal(composeWithAFileSecret)
	body := `{"version":` + itoaT(draftVersion(t, c, stackID)) + `,"files":[{"path":"compose.yaml","content":` + string(cb) + `,"secret":false},` +
		`{"path":"tok.txt","content":"` + value + `","secret":true}],"vars":[]}`
	if rec := c.saveDraft(stackID, body); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	return draftVersion(t, c, stackID)
}

func TestStackResourcesReadADeployedSecretAsItsNameAndVersionWithItsUsers(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "tracer")
	version := saveWithSecretFile(t, operator, st.ID, "s3cret-value")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}

	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/resources", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackResourcesAnswer](t, rec)
	if got.Secrets.Error != "" || len(got.Secrets.Items) != 1 {
		t.Fatalf("secrets = %+v, want the one the deploy made", got.Secrets)
	}
	tok := got.Secrets.Items[0]
	if tok.Name != "tok" || tok.Hash == "" || tok.FullName != "tracer_tok_"+tok.Hash || tok.Ownership != "stack" || len(tok.UsedBy) != 1 || tok.UsedBy[0].Name != "web" || tok.UsedBy[0].ID == "" {
		t.Errorf("tok = %+v", tok)
	}
	if strings.Contains(rec.Body.String(), "s3cret-value") {
		t.Errorf("the answer carries a secret's value: %s", rec.Body)
	}
	if got.Configs.Error != "" || len(got.Configs.Items) != 0 {
		t.Errorf("configs = %+v", got.Configs)
	}
}

func TestStackResourcesListAnExternalSecretTheFileReferencesAsExternal(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-cert", Name: "shared_cert"}, {ID: "sec-free", Name: "unrelated"}}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	version := saveCompose(t, operator, st.ID, `services:
  web:
    image: nginx:1.27-alpine
    secrets: [cert]
secrets:
  cert:
    external: true
    name: shared_cert
`, `[]`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}

	got := answerOf[stackResourcesAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/resources", ""))

	if len(got.Secrets.Items) != 1 {
		t.Fatalf("secrets = %+v, want the external one alone", got.Secrets)
	}
	cert := got.Secrets.Items[0]
	if cert.Name != "shared_cert" || cert.Ownership != "external" || cert.Hash != "" || len(cert.UsedBy) != 1 || cert.UsedBy[0].Name != "web" {
		t.Errorf("cert = %+v, want an external secret the stack's web service uses", cert)
	}
}

func TestStackResourcesListTheVolumesOfAStackOnTheNodeThatHoldsThem(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "tracer")
	cfg := swarmManagerEngine()
	cfg.TasksFollowWrites = true
	cfg.Volumes = []dockertest.FakeVolume{
		{Name: "tracer_data", Driver: "local", Labels: map[string]string{"com.docker.stack.namespace": "tracer", "tierplan.stack.id": st.ID}},
		{Name: "tracer_hand", Driver: "local", Labels: map[string]string{"com.docker.stack.namespace": "tracer"}},
		{Name: "other_data", Driver: "local", Labels: map[string]string{"com.docker.stack.namespace": "other", "tierplan.stack.id": "someone-else"}},
	}
	f.fake.Reconfigure(cfg)

	got := answerOf[stackResourcesAnswer](t, viewer.do(http.MethodGet, "stacks/"+st.ID+"/resources", ""))

	if got.Volumes.Error != "" || len(got.Volumes.Items) != 1 {
		t.Fatalf("volumes = %+v, want the one the stack owns", got.Volumes)
	}
	v := got.Volumes.Items[0]
	if v.Name != "tracer_data" || v.Driver != "local" || v.Node == "" || v.NodeID == "" || v.Ownership != "stack" {
		t.Errorf("volume = %+v", v)
	}
	if got.Volumes.NotReporting == nil {
		t.Errorf("notReporting is null, want a list (empty)")
	}
}

func TestStackResourcesAnswerOneCardsErrorBesideTheOthers(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "tracer")
	// The manager can no longer list its secrets; everything else it still answers.
	cfg := swarmManagerEngine()
	cfg.TasksFollowWrites = true
	cfg.SecretListError = "secrets are not available"
	f.fake.Reconfigure(cfg)

	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/resources", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackResourcesAnswer](t, rec)
	if got.Secrets.Error != "read_failed" || got.Secrets.Items == nil || len(got.Secrets.Items) != 0 {
		t.Errorf("secrets = %+v, want its own error and an empty list", got.Secrets)
	}
	if got.Tasks.Error != "" || got.Networks.Error != "" || got.Volumes.Error != "" || got.Configs.Error != "" {
		t.Errorf("the other cards carry errors: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "secrets are not available") {
		t.Errorf("the engine's own text reached the answer: %s", rec.Body)
	}
}

func TestStackResourcesOfAStandaloneStackListItsConfigsFromTheDeployedDefinition(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	operator, st, h := deployedStandalone(t, f, e, "services:\n  web:\n    image: nginx:1.27-alpine\n    configs: [site]\nconfigs:\n  site:\n    content: hello\n")
	standOnHost(f, h)

	got := answerOf[stackResourcesAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/resources", ""))

	if got.Configs.Error != "" || len(got.Configs.Items) != 1 {
		t.Fatalf("configs = %+v", got.Configs)
	}
	site := got.Configs.Items[0]
	if site.Name != "site" || site.ID != "" || site.Hash != "" || site.Ownership != "stack" || len(site.UsedBy) != 1 || site.UsedBy[0].Name != "web" {
		t.Errorf("site = %+v, want a file of the containers, the stack's own, used by web", site)
	}
	if len(got.Secrets.Items) != 0 || got.Secrets.Error != "" {
		t.Errorf("secrets = %+v", got.Secrets)
	}
}
