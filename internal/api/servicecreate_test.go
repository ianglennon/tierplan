// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// Create a service (plan 06-27, RES-03, D-04, D-07, D-08, D-13). Docker is a fake daemon behind the
// real monitor, aggregator and cluster reader; the router, policy, CSRF guard, registry resolver and
// audit writer are the real ones.

const chatImageRef = "localhost:5000/app/chat:1"

// chatBody is the body of the create routes: the smallest service the editor makes, tuned by mut.
func chatBody(t *testing.T, mut func(spec map[string]any), objects any) string {
	t.Helper()
	spec := map[string]any{
		"Name":         "chat",
		"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": chatImageRef}},
		"Mode":         map[string]any{"Replicated": map[string]any{"Replicas": 1}},
	}
	if mut != nil {
		mut(spec)
	}
	body := map[string]any{"spec": spec}
	if objects != nil {
		body["objects"] = objects
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (c *roleCaller) createReview(body string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "services/review", body)
}

func (c *roleCaller) create(body string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "services", body)
}

type createAnswer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	Steps []struct {
		Step, Kind, Name, State, Code string
	} `json:"steps"`
	Cleanup struct {
		Removed    []struct{ Kind, Name, ID string } `json:"removed"`
		NotRemoved []struct{ Kind, Name, ID string } `json:"notRemoved"`
	} `json:"cleanup"`
}

func TestCreateServiceNeedsServicesManageAnOperatorIsForbiddenOnBothRoutes(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil, editableService(nil))
	body := chatBody(t, nil, nil)
	for _, role := range []string{"viewer", "operator"} {
		who := f.caller(t, e, role+"-1", role)
		for name, rec := range map[string]*httptest.ResponseRecorder{"review": who.createReview(body), "create": who.create(body)} {
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s by a %s: %d %s, want 403", name, role, rec.Code, rec.Body)
			}
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
	if rows := f.serviceAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, a refused caller reaches no handler", rows)
	}
}

func TestCreateServiceMakesAPinnedServiceAndAuditsIt(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	rec := admin.create(chatBody(t, nil, nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var got createAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	digest := dockertest.FakeDigestOf(chatImageRef)
	if got.ID == "" || got.Name != "chat" || got.Image != chatImageRef+"@"+digest {
		t.Errorf("answer = %+v, want chat pinned to %s", got, digest)
	}
	if !strings.Contains(rec.Body.String(), `"removed":[]`) || !strings.Contains(rec.Body.String(), `"notRemoved":[]`) {
		t.Errorf("body = %s, want empty cleanup lists, not null", rec.Body)
	}
	creates := f.fake.ServiceCreates()
	if len(creates) != 1 || creates[0].Spec.TaskTemplate.ContainerSpec.Image != chatImageRef+"@"+digest || creates[0].Query.Get("registryAuthFrom") != "" {
		t.Fatalf("creates = %+v, want one with the pinned image and no registry query", creates)
	}
	if got := writeVerbs(f.fake.Writes()); !slices.Equal(got, []string{"POST /services"}) {
		t.Errorf("writes = %v", got)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionServiceCreated || rows[0].Result != string(audit.ResultSuccess) || rows[0].TargetID != got.ID || rows[0].EnvID != e.ID {
		t.Fatalf("audit rows = %+v, want one service.created success for the new service", rows)
	}
	for k, want := range map[string]string{"name": "chat", "image": chatImageRef + "@" + digest, "digest": digest, "objects": "0", "code": "done"} {
		if rows[0].Details[k] != want {
			t.Errorf("audit detail %s = %q, want %q (%+v)", k, rows[0].Details[k], want, rows[0].Details)
		}
	}
}

func TestCreateServiceWithStagedObjectsMakesThemFirstAndNeverEchoesTheirValues(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	rec := admin.create(chatBody(t, func(spec map[string]any) {
		refsStaged(spec)
		containerSpecOf(spec)["Image"] = chatImageRef
	}, stagedObjects()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	noStagedValue(t, "the answer", rec.Body.String())
	var got createAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, s := range got.Steps {
		steps = append(steps, fmt.Sprintf("%s/%s/%s/%s", s.Step, s.Kind, s.Name, s.State))
	}
	if want := []string{"image///done", "object/network/front/done", "object/secret/db-pass/done", "object/config/app.conf/done", "service//chat/done"}; !slices.Equal(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
	if got, want := writeVerbs(f.fake.Writes()), []string{"POST /networks", "POST /secrets", "POST /configs", "POST /services"}; !slices.Equal(got, want) {
		t.Errorf("writes = %v, want %v", got, want)
	}
	if rows := f.serviceAudits(t); len(rows) != 1 || rows[0].Details["objects"] != "3" {
		t.Errorf("service rows = %+v, want one with objects 3", rows)
	}
	for _, action := range []string{"network.created", "secret.created", "config.created"} {
		rows := f.auditLike(t, action)
		if len(rows) != 1 || rows[0].Details["source"] != "service_apply" {
			t.Errorf("%s rows = %+v, want one with source service_apply", action, rows)
		}
		noStagedValue(t, action+" rows", rows)
	}
}

func TestCreateServiceRefusals(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	for name, c := range map[string]struct {
		mut    func(spec map[string]any)
		status int
		typ    string
		detail string
	}{
		"no image": {func(s map[string]any) {
			containerSpecOf(s)["Image"] = ""
		}, 422, "image_required", "Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."},
		"bad name":   {func(s map[string]any) { s["Name"] = "has space" }, 422, "name_invalid", "Use letters, numbers, dot, dash or underscore"},
		"name taken": {func(s map[string]any) { s["Name"] = "api" }, 409, "service_name_taken", "A service named api already exists."},
		"placeholder": {func(s map[string]any) {
			containerSpecOf(s)["Env"] = []any{"DB=<hidden>"}
		}, 422, "placeholder_unmatched", ""},
		"stack label":   {func(s map[string]any) { s["Labels"] = map[string]any{"com.docker.stack.namespace": "shop"} }, 422, "stack_label_reserved", ""},
		"unknown field": {func(s map[string]any) { s["Bogus"] = 1 }, 422, "spec_invalid", ""},
	} {
		before := len(f.serviceAudits(t))
		rec := admin.create(chatBody(t, c.mut, nil))
		if rec.Code != c.status || problemType(t, rec) != c.typ {
			t.Errorf("%s: %d %s, want %d %s", name, rec.Code, rec.Body, c.status, c.typ)
			continue
		}
		var prob actionProblem
		if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(prob.Detail, c.detail) {
			t.Errorf("%s: detail = %q, want it to contain %q", name, prob.Detail, c.detail)
		}
		rows := f.serviceAudits(t)
		if len(rows) != before+1 {
			t.Errorf("%s: audit rows = %d, want one more than %d", name, len(rows), before)
			continue
		}
		last := rows[len(rows)-1]
		if last.Action != audit.ActionServiceCreated || last.Result != string(audit.ResultFailure) || last.Reason != c.typ || last.Details["code"] != c.typ {
			t.Errorf("%s: audit row = %+v, want a service.created failure with code %s", name, last, c.typ)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a refused create writes nothing", w)
	}
}

func TestCreateServiceOfAnUnresolvableImageIs422AndWritesNothing(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) {
		c.PullStatus = dockertest.FakePullStatus{Code: http.StatusUnauthorized, Message: "unauthorized: authentication required"}
	}, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	rec := admin.create(chatBody(t, nil, nil))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "image_unresolved" {
		t.Fatalf("create: %d %s, want 422 image_unresolved", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v: an unresolvable image is never created unpinned", w)
	}
}

func TestCreateServiceThatFailsRemovesTheObjectsItMade(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "the manager refuses this spec" }, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	rec := admin.create(chatBody(t, refsStaged, stagedObjects()))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "spec_rejected" {
		t.Fatalf("create: %d %s, want 422 spec_rejected", rec.Code, rec.Body)
	}
	noStagedValue(t, "the problem", rec.Body.String())
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prob.Detail, "Couldn't create chat. Docker rejected these settings: ") || !strings.HasSuffix(prob.Detail, " The 3 objects created for this change were removed.") {
		t.Errorf("detail = %q, want Couldn't create chat, the reason and the objects were removed", prob.Detail)
	}
	if got, want := writeVerbs(f.fake.Writes()), []string{"POST /networks", "POST /secrets", "POST /configs", "POST /services", "DELETE /configs", "DELETE /secrets", "DELETE /networks"}; !slices.Equal(got, want) {
		t.Errorf("writes = %v, want %v", got, want)
	}
	if rows := f.serviceAudits(t); len(rows) != 1 || rows[0].Action != audit.ActionServiceCreated || rows[0].Reason != "spec_rejected" || rows[0].Details["objects"] != "3" {
		t.Errorf("service rows = %+v, want one failure spec_rejected", rows)
	}
}

func TestCreateServiceReviewAnswersWhatACreateWouldDoAndChangesNothing(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	rec := admin.createReview(chatBody(t, refsStaged, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	var got reviewAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	digest := dockertest.FakeDigestOf(chatImageRef)
	if !got.Allowed || got.Image.Ref != chatImageRef+"@"+digest || got.Spec["Name"] != "chat" {
		t.Errorf("answer = %+v", got)
	}
	var missing struct {
		Missing struct{ Networks, Secrets, Configs []string } `json:"missing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &missing); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing.Missing.Secrets, []string{"db-pass"}) || !slices.Equal(missing.Missing.Configs, []string{"app.conf"}) || !slices.Equal(missing.Missing.Networks, []string{"front"}) {
		t.Errorf("missing = %+v", missing.Missing)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a review changes nothing", w)
	}
	if rows := f.serviceAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, a review is not audited", rows)
	}
	// A name that exists is told at review time too.
	if rec := admin.createReview(chatBody(t, func(s map[string]any) { s["Name"] = "api" }, nil)); rec.Code != http.StatusConflict || problemType(t, rec) != "service_name_taken" {
		t.Errorf("review of a taken name: %d %s, want 409 service_name_taken", rec.Code, rec.Body)
	}
}

func TestCreateServiceRefusesTooLargeABodyAndTooManyObjects(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	big := `{"spec":{"Name":"chat"},"objects":[{"kind":"config","name":"big","data":"` + strings.Repeat("A", 9<<20) + `"}]}`
	if rec := admin.create(big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a 9 MiB body: %d, want 413", rec.Code)
	}
	many := make([]map[string]any, 17)
	for i := range many {
		many[i] = map[string]any{"kind": "network", "name": fmt.Sprintf("n%d", i)}
	}
	if rec := admin.create(chatBody(t, nil, many)); rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "too_many_objects" {
		t.Errorf("17 objects: want 422 too_many_objects")
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}
