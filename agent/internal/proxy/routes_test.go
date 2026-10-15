// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

func TestRouteContainerActionsReachTheEngineUnchanged(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	cases := []struct{ method, target string }{
		{"POST", "/v1.47/containers/abc/stop?t=10"},
		{"POST", "/containers/abc/stop?t=-1&signal=SIGTERM"},
		{"POST", "/containers/abc/stop?t=86400"},
		{"POST", "/containers/abc/restart?t=0"},
		{"POST", "/containers/abc/kill?signal=SIGTERM"},
		{"POST", "/containers/abc/kill?signal=9"},
		{"POST", "/containers/abc/start"},
		{"POST", "/containers/abc/pause"},
		{"POST", "/containers/abc/unpause"},
		{"DELETE", "/containers/abc?force=1&v=1"},
		{"DELETE", "/containers/abc"},
	}
	for _, tc := range cases {
		resp := do(t, h, request(tc.method, tc.target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: status %d, want 200", tc.method, tc.target, resp.StatusCode)
		}
	}
	got := sock.requests()
	if len(got) != len(cases) {
		t.Fatalf("the engine saw %d requests, want %d: %v", len(got), len(cases), got)
	}
	for i, tc := range cases {
		if want := tc.method + " " + tc.target; got[i] != want {
			t.Errorf("request %d reached the engine as %q, want %q", i, got[i], want)
		}
	}
}

func TestRouteSmuggledVariantsAreRefusedAndNeverReachTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	for _, tc := range []struct {
		name, raw, wantMessage string
	}{
		{"link on a remove", request("DELETE", "/containers/abc?link=1"), "agent_policy_denied: DELETE /containers/abc"},
		{"link beside force", request("DELETE", "/containers/abc?force=1&link=1"), "agent_policy_denied: DELETE /containers/abc"},
		{"a value that is not a bool on a remove", request("DELETE", "/containers/abc?force=yes"), "agent_policy_denied: DELETE /containers/abc"},
		{"an unknown key on stop", request("POST", "/containers/abc/stop?t=10&foo=1"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a timeout that is not a number", request("POST", "/containers/abc/stop?t=abc"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a timeout above a day", request("POST", "/containers/abc/stop?t=86401"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a timeout below -1", request("POST", "/containers/abc/stop?t=-2"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a shell substitution in the signal", request("POST", "/containers/abc/stop?signal=$(x)"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a second value that is not allowed", request("POST", "/containers/abc/stop?t=10&t=abc"), "agent_policy_denied: POST /containers/abc/stop"},
		{"an escaped unknown key", request("POST", "/containers/abc/stop?%66oo=1"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a query the parser rejects", request("POST", "/containers/abc/stop?t=1;foo=2"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a body on stop", request("POST", "/containers/abc/stop", "Content-Length: 2\r\n") + "{}", "agent_policy_denied: POST /containers/abc/stop"},
		{"a form body on start", request("POST", "/containers/abc/start", "Content-Type: application/x-www-form-urlencoded\r\n", "Content-Length: 5\r\n") + "t=1&x", "agent_policy_denied: POST /containers/abc/start"},
		{"a query key on start", request("POST", "/containers/abc/start?detachKeys=x"), "agent_policy_denied: POST /containers/abc/start"},
		{"a signal on a pause", request("POST", "/containers/abc/pause?signal=KILL"), "agent_policy_denied: POST /containers/abc/pause"},
		{"an upgrade on stop", request("POST", "/containers/abc/stop", "Upgrade: tcp\r\n", "Connection: Upgrade\r\n"), "agent_policy_denied: POST /containers/abc/stop"},
		{"an ID that starts with a dot", request("POST", "/containers/.x/stop"), "agent_policy_denied: POST /containers/.x/stop"},
		{"attach", request("POST", "/containers/abc/attach"), "agent_policy_denied: POST /containers/abc/attach"},
		{"archive", get("/containers/abc/archive"), "agent_policy_denied: GET /containers/abc/archive"},
		{"a put to a container", request("PUT", "/containers/abc/archive"), "agent_policy_denied: PUT /containers/abc/archive"},
		{"a GET of stop", get("/containers/abc/stop"), "agent_policy_denied: GET /containers/abc/stop"},
		{"a delete of a container action", request("DELETE", "/containers/abc/stop"), "agent_policy_denied: DELETE /containers/abc/stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, h, tc.raw)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			var body struct{ Message string }
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("the refusal is not JSON: %v", err)
			}
			if body.Message != tc.wantMessage {
				t.Errorf("message %q, want %q", body.Message, tc.wantMessage)
			}
		})
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("a refused request reached the engine: %v", got)
	}
}

func TestRouteReadOnlyRefusesEveryWriteAndStillAllowsTheReads(t *testing.T) {
	sock := newFakeSocket(t)
	var mu sync.Mutex
	var reasons []string
	h := Handler(sock.path, Policy{ReadOnly: true}, func(_, _, reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	})
	writes := []struct{ method, target string }{
		{"POST", "/containers/abc/stop?t=10"},
		{"POST", "/containers/abc/kill?signal=SIGTERM"},
		{"POST", "/containers/abc/start"},
		{"POST", "/containers/abc/restart"},
		{"POST", "/containers/abc/pause"},
		{"POST", "/containers/abc/unpause"},
		{"DELETE", "/containers/abc?force=1&v=1"},
		{"DELETE", "/containers/abc?link=1"},
	}
	for _, tc := range writes {
		resp := do(t, h, request(tc.method, tc.target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: status %d on a read-only host, want 403", tc.method, tc.target, resp.StatusCode)
		}
	}
	if len(sock.requests()) != 0 {
		t.Fatalf("a write reached the engine of a read-only host: %v", sock.requests())
	}
	mu.Lock()
	defer mu.Unlock()
	for i, r := range reasons {
		if r != "read_only_flag" {
			t.Errorf("refusal %d has the reason %q, want read_only_flag", i, r)
		}
	}
	if len(reasons) != len(writes) {
		t.Errorf("onDenied ran %d times, want %d", len(reasons), len(writes))
	}
	for _, target := range []string{"/_ping", "/containers/json?all=1", "/containers/abc/json"} {
		resp := do(t, h, get(target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s on a read-only host: status %d, want 200", target, resp.StatusCode)
		}
	}
}

func TestRouteTheReadSurfaceOfEarlierPhasesStillTakesAnyQuery(t *testing.T) {
	q := "filters=" + strings.Repeat("a", 1000) + "&all=1&whatever=2"
	for _, p := range []string{
		"/_ping", "/version", "/info", "/containers/json", "/images/json", "/volumes", "/networks",
		"/events", "/nodes", "/services", "/tasks", "/containers/abc/json",
		"/v1.47/events", "/v1.52/containers/web-1/json",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			if ok, reason, body := Allowed(method, p, q, false, false, Policy{}); !ok || body != BodyNone {
				t.Errorf("%s %s was refused (%s)", method, p, reason)
			}
		}
	}
	if ok, _, _ := Allowed("GET", "/events", strings.Repeat("a", maxQuery+1), false, false, Policy{}); ok {
		t.Error("a query over the cap was allowed on a read route")
	}
}

func TestRouteMatchTreatsHeadAsGetAndNothingElseAsAnything(t *testing.T) {
	if _, ok := Match("HEAD", "/_ping"); !ok {
		t.Error("HEAD /_ping did not match the GET route")
	}
	if _, ok := Match("HEAD", "/containers/abc/stop"); ok {
		t.Error("HEAD matched a POST route")
	}
	rt, ok := Match("POST", "/v1.47/containers/abc/kill")
	if !ok || rt.name != "container kill" || rt.body != BodyNone || rt.upgrade {
		t.Errorf("POST kill matched %+v, %v", rt, ok)
	}
	if _, ok := Match("GET", "/containers/abc/kill"); ok {
		t.Error("GET matched a POST route")
	}
}

func TestRouteImagePullQueryCases(t *testing.T) {
	digest := "sha256%3A" + strings.Repeat("0", 64)
	for _, tc := range []struct {
		name, query string
		want        bool
	}{
		{"a repository and a tag", "fromImage=docker.io%2Flibrary%2Fbusybox&tag=1.36.1", true},
		{"a registry with a port", "fromImage=reg.example.org%3A5000%2Fa%2Fb&tag=v1", true},
		{"a digest in the tag key", "fromImage=ghcr.io%2Facme%2Fapp&tag=" + digest, true},
		{"a platform with a variant", "fromImage=a&tag=b&platform=linux%2Farm%2Fv7", true},
		{"no query at all", "", true},
		{"fromSrc", "fromSrc=http%3A%2F%2Fx", false},
		{"repo", "fromImage=a&tag=b&repo=x", false},
		{"message", "fromImage=a&tag=b&message=x", false},
		{"changes", "fromImage=a&tag=b&changes=x", false},
		{"a tag with a slash", "fromImage=a&tag=x%2Fy", false},
		{"a sha1 digest", "fromImage=a&tag=sha1%3A" + strings.Repeat("0", 40), false},
		{"a short sha256 digest", "fromImage=a&tag=sha256%3A" + strings.Repeat("0", 63), false},
		{"a platform without an architecture", "fromImage=a&tag=b&platform=linux", false},
		{"an upper-case operating system", "fromImage=a&tag=b&platform=Linux%2Famd64", false},
		{"an image of 256 characters", "fromImage=" + strings.Repeat("a", 256) + "&tag=b", false},
		{"an image of 255 characters", "fromImage=" + strings.Repeat("a", 255) + "&tag=b", true},
		{"an image with a space", "fromImage=a%20b&tag=b", false},
		{"a second fromImage that is not allowed", "fromImage=a&fromImage=-b&tag=b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, _ := Allowed("POST", "/images/create", tc.query, false, false, Policy{})
			if ok != tc.want {
				t.Fatalf("Allowed(POST /images/create?%s) = %v, want %v", tc.query, ok, tc.want)
			}
		})
	}
	rt, ok := Match("POST", "/v1.56/images/create")
	if !ok || rt.name != "image pull" || !rt.registryAuth || rt.body != BodyNone || rt.upgrade {
		t.Errorf("POST images/create matched %+v, %v", rt, ok)
	}
	if rt, ok := Match("GET", "/containers/json"); !ok || rt.registryAuth {
		t.Errorf("a route other than a pull or a distribution check may carry a registry credential: %+v", rt)
	}
}

func TestRouteImageInspectHistoryRemoveAndDistribution(t *testing.T) {
	digest := strings.Repeat("0", 64)
	long255 := "a" + strings.Repeat("b", 254)
	long256 := "a" + strings.Repeat("b", 255)
	for _, tc := range []struct {
		name, method, path, query string
		wantRoute                 string // "" means refused
	}{
		{"the list is only the list", "GET", "/images/json", "all=1&filters=x", "images list"},
		{"an image called a", "GET", "/images/a/json", "", "image inspect"},
		{"an image called a/json has a history", "GET", "/images/a/json/history", "", "image history"},
		{"a registry repository with a tag", "GET", "/images/ghcr.io/acme/app:1/json", "", "image inspect"},
		{"the version prefix", "GET", "/v1.56/images/busybox:1.36.1/json", "", "image inspect"},
		{"an image ID", "GET", "/images/sha256:" + digest + "/json", "", "image inspect"},
		{"a digest reference history", "GET", "/images/repo@sha256:" + digest + "/history", "", "image history"},
		{"a HEAD of an inspect", "HEAD", "/images/a/json", "", "image inspect"},
		{"a remove with no query", "DELETE", "/images/ghcr.io/acme/app:1", "", "image remove"},
		{"a remove with noprune", "DELETE", "/images/ghcr.io/acme/app:1", "noprune=1", "image remove"},
		{"a remove by ID", "DELETE", "/images/sha256:" + digest, "noprune=true", "image remove"},
		{"a distribution check", "GET", "/distribution/ghcr.io/acme/app:1/json", "", "distribution"},
		{"a distribution check with the version prefix", "GET", "/v1.56/distribution/busybox/json", "", "distribution"},
		{"a name of 255 characters", "GET", "/images/" + long255 + "/json", "", "image inspect"},

		{"a remove with force", "DELETE", "/images/x", "force=1", ""},
		{"a remove with force=0", "DELETE", "/images/x", "force=0", ""},
		{"a remove with platforms", "DELETE", "/images/x", "platforms=x", ""},
		{"a remove with a bad noprune", "DELETE", "/images/x", "noprune=yes", ""},
		{"an inspect with manifests", "GET", "/images/a/json", "manifests=1", ""},
		{"an inspect with a platform", "GET", "/images/a/json", "platform=linux", ""},
		{"a history with a query", "GET", "/images/a/history", "platform=x", ""},
		{"a distribution check with a query", "GET", "/distribution/a/json", "x=1", ""},
		{"a dot-dot name", "GET", "/images/../x/json", "", ""},
		{"a dot-dot inside the name", "GET", "/images/a/../b/json", "", ""},
		{"an empty segment", "GET", "/images//x/json", "", ""},
		{"a name of 256 characters", "GET", "/images/" + long256 + "/json", "", ""},
		{"a name starting with a dash", "GET", "/images/-x/json", "", ""},
		{"a name starting with a dot", "GET", "/images/.x/json", "", ""},
		{"a name with a space", "GET", "/images/a b/json", "", ""},
		{"an encoded slash in the name", "GET", "/images/a%2Fb/json", "", ""},
		{"a trailing slash on a remove", "DELETE", "/images/x/", "", ""},
		{"a tag", "POST", "/images/x/tag", "repo=y", ""},
		{"a push", "POST", "/images/x/push", "", ""},
		{"a save of one image", "GET", "/images/x/get", "", ""},
		{"a save of several images", "GET", "/images/get", "names=x", ""},
		{"a load", "POST", "/images/load", "", ""},
		{"a prune", "POST", "/images/prune", "", ""},
		{"a search", "GET", "/images/search", "term=x", ""},
		{"a build", "POST", "/build", "", ""},
		{"a delete of the list", "DELETE", "/images/json/", "", ""},
		{"a post of an inspect", "POST", "/images/x/json", "", ""},
		{"a delete of x/history removes the image of that name", "DELETE", "/images/x/history", "", "image remove"},
		{"a delete of a distribution check", "DELETE", "/distribution/x/json", "", ""},
		{"a post of a distribution check", "POST", "/distribution/x/json", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, _ := Allowed(tc.method, tc.path, tc.query, false, false, Policy{})
			if ok != (tc.wantRoute != "") {
				t.Fatalf("Allowed(%s %s?%s) = %v, want %v", tc.method, tc.path, tc.query, ok, tc.wantRoute != "")
			}
			if !ok {
				return
			}
			rt, found := Match(tc.method, tc.path)
			if !found || rt.name != tc.wantRoute {
				t.Fatalf("Match(%s %s) = %q, want %q", tc.method, tc.path, rt.name, tc.wantRoute)
			}
			wantAuth := tc.wantRoute == "distribution"
			if rt.registryAuth != wantAuth {
				t.Errorf("route %q has registryAuth %v, want %v", rt.name, rt.registryAuth, wantAuth)
			}
		})
	}
}

func TestRouteImageRemoveIsRefusedOnAReadOnlyHostAndInspectsStillAnswer(t *testing.T) {
	if ok, reason, _ := Allowed("DELETE", "/images/x", "", false, false, Policy{ReadOnly: true}); ok || reason != "read_only_flag" {
		t.Errorf("an image remove on a read-only host = %v %q, want refused read_only_flag", ok, reason)
	}
	for _, p := range []string{"/images/a/json", "/images/a/history", "/distribution/a/json"} {
		if ok, reason, _ := Allowed("GET", p, "", false, false, Policy{ReadOnly: true}); !ok {
			t.Errorf("GET %s on a read-only host was refused (%s)", p, reason)
		}
	}
}

func replicas(n uint64) *uint64 { return &n }

func sampleSpec() swarm.ServiceSpec {
	var s swarm.ServiceSpec
	s.Name = "web"
	s.Labels = map[string]string{"com.docker.stack.namespace": "shop"}
	s.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "ghcr.io/acme/web:1", Env: []string{"A=1"}}
	s.Mode.Replicated = &swarm.ReplicatedService{Replicas: replicas(3)}
	return s
}

func TestRouteServiceInspectAndUpdateQueryCases(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, query string
		body                      bool
		wantRoute                 string // "" means refused
	}{
		{"an inspect", "GET", "/services/web", "", false, "service inspect"},
		{"a trailing slash on an inspect", "GET", "/v1.56/services/abc123/", "", false, ""},
		{"an inspect by ID with insertDefaults", "GET", "/v1.56/services/abc123", "insertDefaults=1", false, "service inspect"},
		{"a HEAD of an inspect", "HEAD", "/services/web", "", false, "service inspect"},
		{"the list is only the list", "GET", "/services", "status=true&filters=x", false, "services"},
		{"an update", "POST", "/services/web/update", "version=12", true, "service update"},
		{"an update with the version prefix", "POST", "/v1.56/services/web/update", "version=12&registryAuthFrom=previous-spec", true, "service update"},
		{"an update with rollback", "POST", "/services/web/update", "version=12&rollback=previous", true, "service update"},
		{"an update with rollback none", "POST", "/services/web/update", "version=12&rollback=none", true, "service update"},
		{"an update with spec credentials", "POST", "/services/web/update", "version=12&registryAuthFrom=spec", true, "service update"},

		{"an inspect with an unknown key", "GET", "/services/web", "foo=1", false, ""},
		{"an inspect with a bad bool", "GET", "/services/web", "insertDefaults=yes", false, ""},
		{"an inspect with a body", "GET", "/services/web", "", true, ""},
		{"an update with no body", "POST", "/services/web/update", "version=12", false, ""},
		{"an update with an unknown key", "POST", "/services/web/update", "version=12&foo=1", true, ""},
		{"an update with a non-numeric version", "POST", "/services/web/update", "version=abc", true, ""},
		{"an update with a negative version", "POST", "/services/web/update", "version=-1", true, ""},
		{"an update with a 21 digit version", "POST", "/services/web/update", "version=" + strings.Repeat("1", 21), true, ""},
		{"an update with a 20 digit version", "POST", "/services/web/update", "version=" + strings.Repeat("1", 20), true, "service update"},
		{"an update with a bad registryAuthFrom", "POST", "/services/web/update", "version=1&registryAuthFrom=other", true, ""},
		{"an update with a bad rollback", "POST", "/services/web/update", "version=1&rollback=next", true, ""},
		{"an update with a second bad version", "POST", "/services/web/update", "version=1&version=x", true, ""},
		{"a dot-dot service", "GET", "/services/../x", "", false, ""},
		{"a service starting with a dot", "GET", "/services/.x", "", false, ""},
		{"a delete of a service", "DELETE", "/services/web", "", false, "service remove"},
		{"a delete of a service with the version prefix", "DELETE", "/v1.56/services/abc123", "", false, "service remove"},
		{"a delete of a service with a key", "DELETE", "/services/web", "force=1", false, ""},
		{"a delete of a service with a body", "DELETE", "/services/web", "", true, ""},
		{"a delete of a service with a trailing slash", "DELETE", "/services/web/", "", false, ""},
		{"a service create", "POST", "/services/create", "", true, "service create"},
		{"a service create with the version prefix", "POST", "/v1.56/services/create", "", true, "service create"},
		{"a service create with no body", "POST", "/services/create", "", false, ""},
		{"a service create with a key", "POST", "/services/create", "version=1", true, ""},
		{"a GET of create is an inspect of a service named create", "GET", "/services/create", "", false, "service inspect"},
		{"service logs", "GET", "/services/web/logs", "", false, ""},
		{"a GET of update", "GET", "/services/web/update", "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, kind := Allowed(tc.method, tc.path, tc.query, tc.body, false, Policy{})
			if ok != (tc.wantRoute != "") {
				t.Fatalf("Allowed(%s %s?%s) = %v, want %v", tc.method, tc.path, tc.query, ok, tc.wantRoute != "")
			}
			if !ok {
				return
			}
			rt, found := Match(tc.method, tc.path)
			if !found || rt.name != tc.wantRoute {
				t.Fatalf("Match(%s %s) = %q, want %q", tc.method, tc.path, rt.name, tc.wantRoute)
			}
			if wantKind := map[string]BodyKind{"service update": BodyServiceSpec, "service create": BodyServiceSpec}[tc.wantRoute]; kind != wantKind {
				t.Errorf("body kind = %d, want %d", kind, wantKind)
			}
			if wantAuth := tc.wantRoute == "service update" || tc.wantRoute == "service create"; rt.registryAuth != wantAuth {
				t.Errorf("route %q has registryAuth %v, want %v", rt.name, rt.registryAuth, wantAuth)
			}
		})
	}
}

func TestRouteNodeInspectTakesNoQueryAndNeverShadowsTheList(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, query string
		body                      bool
		wantRoute                 string // "" means refused
	}{
		{"an inspect", "GET", "/nodes/abc123", "", false, "node inspect"},
		{"an inspect with the version prefix", "GET", "/v1.56/nodes/abc123", "", false, "node inspect"},
		{"a HEAD of an inspect", "HEAD", "/nodes/abc123", "", false, "node inspect"},
		{"the list still takes any query", "GET", "/nodes", "filters=x&whatever=1", false, "nodes"},
		{"an inspect with a key", "GET", "/nodes/abc123", "x=1", false, ""},
		{"an inspect with an empty key", "GET", "/nodes/abc123", "x=", false, ""},
		{"an inspect with a body", "GET", "/nodes/abc123", "", true, ""},
		{"a trailing slash on an inspect", "GET", "/nodes/abc123/", "", false, ""},
		{"a dot-dot node", "GET", "/nodes/../x", "", false, ""},
		{"a node starting with a dot", "GET", "/nodes/.x", "", false, ""},
		{"a 129 character node", "GET", "/nodes/" + strings.Repeat("a", 129), "", false, ""},
		{"a 128 character node", "GET", "/nodes/" + strings.Repeat("a", 128), "", false, "node inspect"},
		{"a node's sub-path", "GET", "/nodes/abc123/tasks", "", false, ""},
		{"a node delete is the remove, which has its own test", "DELETE", "/nodes/abc123", "", false, "node remove"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, _ := Allowed(tc.method, tc.path, tc.query, tc.body, false, Policy{})
			if ok != (tc.wantRoute != "") {
				t.Fatalf("Allowed(%s %s?%s) = %v, want %v", tc.method, tc.path, tc.query, ok, tc.wantRoute != "")
			}
			if !ok {
				return
			}
			if rt, found := Match(tc.method, tc.path); !found || rt.name != tc.wantRoute {
				t.Fatalf("Match(%s %s) = %q, want %q", tc.method, tc.path, rt.name, tc.wantRoute)
			}
		})
	}
	if ok, reason, _ := Allowed("GET", "/nodes/abc123", "", false, false, Policy{ReadOnly: true}); !ok {
		t.Errorf("a node inspect on a read-only host was refused (%s)", reason)
	}
}

func TestRouteNodeUpdateTakesOnlyAVersionAndANodeSpecBody(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, query string
		body                      bool
		wantRoute                 string // "" means refused
	}{
		{"an update", "POST", "/nodes/abc123/update", "version=12", true, "node update"},
		{"an update with the version prefix", "POST", "/v1.56/nodes/abc123/update", "version=12", true, "node update"},
		{"an update with a 20 digit version", "POST", "/nodes/abc123/update", "version=" + strings.Repeat("1", 20), true, "node update"},
		{"an update with a 21 digit version", "POST", "/nodes/abc123/update", "version=" + strings.Repeat("1", 21), true, ""},
		{"an update with a non-numeric version", "POST", "/nodes/abc123/update", "version=abc", true, ""},
		{"an update with a negative version", "POST", "/nodes/abc123/update", "version=-1", true, ""},
		{"an update with a second bad version", "POST", "/nodes/abc123/update", "version=1&version=x", true, ""},
		{"an update with another key", "POST", "/nodes/abc123/update", "version=12&force=1", true, ""},
		{"an update with no body", "POST", "/nodes/abc123/update", "version=12", false, ""},
		{"an update with a trailing slash", "POST", "/nodes/abc123/update/", "version=12", true, ""},
		{"an update of a dot-dot node", "POST", "/nodes/../update", "version=12", true, ""},
		{"an update of a node starting with a dot", "POST", "/nodes/.x/update", "version=12", true, ""},
		{"an update of a 129 character node", "POST", "/nodes/" + strings.Repeat("a", 129) + "/update", "version=12", true, ""},
		{"a GET of update", "GET", "/nodes/abc123/update", "", false, ""},
		{"a PUT of update", "PUT", "/nodes/abc123/update", "version=12", true, ""},
		{"a POST to a node", "POST", "/nodes/abc123", "version=12", true, ""},
		{"a node's other sub-path", "POST", "/nodes/abc123/join", "version=12", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, kind := Allowed(tc.method, tc.path, tc.query, tc.body, false, Policy{})
			if ok != (tc.wantRoute != "") {
				t.Fatalf("Allowed(%s %s?%s) = %v, want %v", tc.method, tc.path, tc.query, ok, tc.wantRoute != "")
			}
			if !ok {
				return
			}
			if rt, found := Match(tc.method, tc.path); !found || rt.name != tc.wantRoute || rt.registryAuth {
				t.Fatalf("Match(%s %s) = %q (registryAuth %v), want %q without one", tc.method, tc.path, rt.name, rt.registryAuth, tc.wantRoute)
			}
			if kind != BodyNodeSpec {
				t.Errorf("body kind = %d, want BodyNodeSpec (%d)", kind, BodyNodeSpec)
			}
		})
	}
	if ok, reason, _ := Allowed("POST", "/nodes/abc123/update", "version=1", true, false, Policy{ReadOnly: true}); ok || reason != "read_only_flag" {
		t.Errorf("a node update on a read-only host = %v %q, want refused read_only_flag", ok, reason)
	}
	if ok, _, _ := Allowed("POST", "/nodes/abc123/update", "version=1", true, true, Policy{}); ok {
		t.Error("a node update with an upgrade was allowed")
	}
}

func TestRouteNodeRemoveTakesNoKeysSoForceCanNeverBeSent(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, query string
		wantRoute                 string // "" means refused
	}{
		{"a remove", "DELETE", "/nodes/abc123", "", "node remove"},
		{"a remove with the version prefix", "DELETE", "/v1.56/nodes/abc123", "", "node remove"},
		{"a remove with force=1", "DELETE", "/nodes/abc123", "force=1", ""},
		{"a remove with force=true", "DELETE", "/nodes/abc123", "force=true", ""},
		{"a remove with force=0", "DELETE", "/nodes/abc123", "force=0", ""},
		{"a remove with an empty force", "DELETE", "/nodes/abc123", "force=", ""},
		{"a remove with any other key", "DELETE", "/nodes/abc123", "version=1", ""},
		{"a remove with a trailing slash", "DELETE", "/nodes/abc123/", "", ""},
		{"a remove of a dot-dot node", "DELETE", "/nodes/../update", "", ""},
		{"a remove of a node starting with a dot", "DELETE", "/nodes/.x", "", ""},
		{"a remove of a 129 character node", "DELETE", "/nodes/" + strings.Repeat("a", 129), "", ""},
		{"a remove of the node list", "DELETE", "/nodes", "", ""},
		{"a remove of a node's sub-path", "DELETE", "/nodes/abc123/update", "", ""},
		{"a POST to a node", "POST", "/nodes/abc123", "", ""},
		{"a PUT to a node", "PUT", "/nodes/abc123", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, kind := Allowed(tc.method, tc.path, tc.query, false, false, Policy{})
			if ok != (tc.wantRoute != "") {
				t.Fatalf("Allowed(%s %s?%s) = %v, want %v", tc.method, tc.path, tc.query, ok, tc.wantRoute != "")
			}
			if !ok {
				return
			}
			if rt, found := Match(tc.method, tc.path); !found || rt.name != tc.wantRoute || rt.registryAuth {
				t.Fatalf("Match(%s %s) = %q (registryAuth %v), want %q without one", tc.method, tc.path, rt.name, rt.registryAuth, tc.wantRoute)
			}
			if kind != BodyNone {
				t.Errorf("body kind = %d, want none", kind)
			}
		})
	}
	if ok, _, _ := Allowed("DELETE", "/nodes/abc123", "", true, false, Policy{}); ok {
		t.Error("a node remove with a body was allowed")
	}
	if ok, _, _ := Allowed("DELETE", "/nodes/abc123", "", false, true, Policy{}); ok {
		t.Error("a node remove with an upgrade was allowed")
	}
	if ok, reason, _ := Allowed("DELETE", "/nodes/abc123", "", false, false, Policy{ReadOnly: true}); ok || reason != "read_only_flag" {
		t.Errorf("a node remove on a read-only host = %v %q, want refused read_only_flag", ok, reason)
	}
}

func TestRouteServiceWritesAreRefusedOnAReadOnlyHostAndInspectStillAnswers(t *testing.T) {
	for _, tc := range []struct {
		method, path, query string
		body                bool
	}{
		{"POST", "/services/web/update", "version=1", true},
		{"POST", "/services/create", "", true},
		{"DELETE", "/services/web", "", false},
	} {
		if ok, reason, _ := Allowed(tc.method, tc.path, tc.query, tc.body, false, Policy{ReadOnly: true}); ok || reason != "read_only_flag" {
			t.Errorf("%s %s on a read-only host = %v %q, want refused read_only_flag", tc.method, tc.path, ok, reason)
		}
	}
	if ok, reason, _ := Allowed("GET", "/services/web", "", false, false, Policy{ReadOnly: true}); !ok {
		t.Errorf("a service inspect on a read-only host was refused (%s)", reason)
	}
}

func TestBodyServiceSpecIsRebuiltAndDecodesToTheSameSpec(t *testing.T) {
	want := sampleSpec()
	var got swarm.ServiceSpec
	if err := roundTrip(t, BodyServiceSpec, enc(t, want), &got); err != nil {
		t.Fatalf("a service spec was refused: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rebuilt = %+v, want %+v", got, want)
	}
	// A spec with the container runtime named is the same service.
	withRuntime := sampleSpec()
	withRuntime.TaskTemplate.Runtime = swarm.RuntimeContainer
	if err := roundTrip(t, BodyServiceSpec, enc(t, withRuntime), &got); err != nil {
		t.Errorf("a spec with the container runtime was refused: %v", err)
	}
}

func TestBodyServiceSpecRefusals(t *testing.T) {
	plugin := sampleSpec()
	plugin.TaskTemplate.Runtime = swarm.RuntimePlugin
	pluginSpec := sampleSpec()
	pluginSpec.TaskTemplate.PluginSpec = &swarm.RuntimeSpec{Name: "p"}
	attach := sampleSpec()
	attach.TaskTemplate.NetworkAttachmentSpec = &swarm.NetworkAttachmentSpec{ContainerID: "c"}
	for name, body := range map[string]string{
		"the plugin runtime":      enc(t, plugin),
		"a plugin spec":           enc(t, pluginSpec),
		"a network attachment":    enc(t, attach),
		"an unknown top key":      `{"Name":"web","Foo":1}`,
		"an unknown nested key":   `{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"x","Foo":true}}}`,
		"not an object":           `[1,2]`,
		"data after the value":    `{"Name":"web"} {"Name":"x"}`,
		"a runtime that is a lie": `{"Name":"web","TaskTemplate":{"Runtime":"host"}}`,
		"an empty body":           ``,
	} {
		if _, err := validateBody(BodyServiceSpec, strings.NewReader(body), bodyEnv{}); err == nil {
			t.Errorf("%s: the body was accepted", name)
		}
	}
}

func TestBodyServiceSpecCapIs256KiBAndOtherKindsKeep64KiB(t *testing.T) {
	if bodyCap(BodyServiceSpec) != 256<<10 || bodyCap(BodyVolumeCreate) != 64<<10 || bodyCap(BodyExecCreate) != 64<<10 {
		t.Fatalf("caps = %d / %d / %d", bodyCap(BodyServiceSpec), bodyCap(BodyVolumeCreate), bodyCap(BodyExecCreate))
	}
	spec := func(n int) string {
		s := sampleSpec()
		s.Labels["pad"] = strings.Repeat("a", n)
		b, _ := json.Marshal(s)
		return string(b)
	}
	// Over 64 KiB (four labels' worth is over the per-value limit of other kinds, but a
	// service spec has no such limit) and under 256 KiB.
	under := spec(100 << 10)
	if _, err := validateBody(BodyServiceSpec, strings.NewReader(under), bodyEnv{}); err != nil {
		t.Errorf("a 100 KiB service spec was refused: %v", err)
	}
	over := spec(260 << 10)
	if _, err := validateBody(BodyServiceSpec, strings.NewReader(over), bodyEnv{}); err == nil {
		t.Error("a service spec over 256 KiB was accepted")
	}
	if _, err := validateBody(BodyVolumeCreate, strings.NewReader(`{"Name":"`+strings.Repeat("a", 70<<10)+`"}`), bodyEnv{}); err == nil {
		t.Error("a volume body over 64 KiB was accepted")
	}
}

func TestServiceUpdateForwardsTheRebuiltSpecAndTheCredentialOnlyOnTheUpdate(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	body := enc(t, sampleSpec())
	resp := do(t, h, postJSON("POST", "/v1.56/services/web/update?version=12&registryAuthFrom=previous-spec", body,
		"X-Registry-Auth: "+testRegistryAuth+"\r\n", "X-Registry-Config: e30=\r\n",
		"Content-Type: application/x-www-form-urlencoded\r\n"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	// The inspect carries a credential header too; it must not cross.
	resp = do(t, h, request("GET", "/services/web?insertDefaults=1", "X-Registry-Auth: "+testRegistryAuth+"\r\n"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inspect status %d, want 200", resp.StatusCode)
	}
	seen := sock.requests()
	if len(seen) != 2 || seen[0] != "POST /v1.56/services/web/update?version=12&registryAuthFrom=previous-spec" || seen[1] != "GET /services/web?insertDefaults=1" {
		t.Fatalf("the engine saw %v", seen)
	}
	hdrs := sock.headersSeen()
	if got := hdrs[0].Values("X-Registry-Auth"); len(got) != 1 || got[0] != testRegistryAuth {
		t.Errorf("update X-Registry-Auth = %q, want the one value untouched", got)
	}
	if got := hdrs[1].Values("X-Registry-Auth"); len(got) != 0 {
		t.Errorf("inspect X-Registry-Auth = %q, want none", got)
	}
	if v := hdrs[0].Get("X-Registry-Config"); v != "" {
		t.Errorf("update X-Registry-Config = %q, want none", v)
	}
	if ct := hdrs[0].Get("Content-Type"); ct != "application/json" {
		t.Errorf("update content type = %q", ct)
	}
	var got, want swarm.ServiceSpec
	_ = json.Unmarshal(sock.bodiesSeen()[0], &got)
	_ = json.Unmarshal([]byte(body), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the engine received %s, want the same spec as %s", sock.bodiesSeen()[0], body)
	}
}

func TestServiceUpdateRefusalsNeverReachTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	var reasons []string
	h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
	plugin := sampleSpec()
	plugin.TaskTemplate.Runtime = swarm.RuntimePlugin
	big := sampleSpec()
	big.Labels["pad"] = strings.Repeat("a", 260<<10)
	for _, tc := range []struct{ name, raw string }{
		{"a plugin runtime", postJSON("POST", "/services/web/update?version=1", enc(t, plugin))},
		{"a spec over 256 KiB", postJSON("POST", "/services/web/update?version=1", enc(t, big))},
		{"no body", request("POST", "/services/web/update?version=1")},
		{"a bad version", postJSON("POST", "/services/web/update?version=x", enc(t, sampleSpec()))},
		{"a credential of two values", postJSON("POST", "/services/web/update?version=1", enc(t, sampleSpec()),
			"X-Registry-Auth: YQ==\r\n", "X-Registry-Auth: Yg==\r\n")},
	} {
		resp := do(t, h, tc.raw)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", tc.name, resp.StatusCode)
		}
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("a refused request reached the engine: %v", got)
	}
	if len(reasons) != 5 {
		t.Errorf("onDenied ran %d times, want 5", len(reasons))
	}
}
