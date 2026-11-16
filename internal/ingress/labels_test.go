// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"reflect"
	"strings"
	"testing"
)

// labelCodes lists the codes of ps in order.
func labelCodes(ps []Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Code)
	}
	return out
}

// labelFind returns the first problem with code, and whether there was one.
func labelFind(ps []Problem, code string) (Problem, bool) {
	for _, p := range ps {
		if p.Code == code {
			return p, true
		}
	}
	return Problem{}, false
}

// labelSet builds a label map from key, value pairs.
func labelSet(kv ...string) map[string]string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestParseServiceLabels(t *testing.T) {
	const p = LabelPrefix

	t.Run("short form is the route default", func(t *testing.T) {
		ds, ps := ParseServiceLabels("web", labelSet(p+"host", "App.Apps.Example.com.", p+"port", "8080"))
		if len(ps) != 0 {
			t.Fatalf("problems = %v", ps)
		}
		want := []Declaration{{
			Service: "web", Name: "default", Host: "app.apps.example.com", Label: p + "host",
			PathPrefix: "/", Port: 8080, Scheme: SchemeHTTP, Strip: false,
		}}
		if !reflect.DeepEqual(ds, want) {
			t.Errorf("declarations = %+v, want %+v", ds, want)
		}
	})

	t.Run("named form with every field", func(t *testing.T) {
		ds, ps := ParseServiceLabels("web", labelSet(
			p+"api.host", "api.apps.example.com", p+"api.port", "9000", p+"api.path", "/api/",
			p+"api.scheme", "HTTPS", p+"api.strip", "TRUE"))
		if len(ps) != 0 {
			t.Fatalf("problems = %v", ps)
		}
		want := []Declaration{{
			Service: "web", Name: "api", Host: "api.apps.example.com", Label: p + "api.host",
			PathPrefix: "/api", Port: 9000, Scheme: SchemeHTTPS, Strip: true,
		}}
		if !reflect.DeepEqual(ds, want) {
			t.Errorf("declarations = %+v, want %+v", ds, want)
		}
	})

	t.Run("short and named form give two declarations sorted by name", func(t *testing.T) {
		ds, ps := ParseServiceLabels("web", labelSet(
			p+"zeta.host", "z.apps.example.com", p+"zeta.port", "1",
			p+"host", "d.apps.example.com", p+"port", "2",
			p+"alpha.host", "a.apps.example.com", p+"alpha.port", "3"))
		if len(ps) != 0 {
			t.Fatalf("problems = %v", ps)
		}
		var names []string
		for _, d := range ds {
			names = append(names, d.Name)
		}
		if !reflect.DeepEqual(names, []string{"alpha", "default", "zeta"}) {
			t.Errorf("names = %v, want sorted by name", names)
		}
	})

	t.Run("a service with no ingress label has no declaration and no problem", func(t *testing.T) {
		ds, ps := ParseServiceLabels("db", labelSet("com.example.other", "x", "tierplan.keep", "true"))
		if len(ds) != 0 || len(ps) != 0 {
			t.Errorf("declarations = %v, problems = %v, want none", ds, ps)
		}
		ds, ps = ParseServiceLabels("db", nil)
		if len(ds) != 0 || len(ps) != 0 {
			t.Errorf("nil labels: declarations = %v, problems = %v, want none", ds, ps)
		}
	})

	t.Run("scheme", func(t *testing.T) {
		for _, v := range []string{"http", "HTTP", "Https", "https"} {
			_, ps := ParseServiceLabels("web", labelSet(p+"host", "a.apps.example.com", p+"port", "80", p+"scheme", v))
			if len(ps) != 0 {
				t.Errorf("scheme %q: problems = %v", v, ps)
			}
		}
		_, ps := ParseServiceLabels("web", labelSet(p+"host", "a.apps.example.com", p+"port", "80", p+"scheme", "ftp"))
		if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodeSchemeInvalid}) {
			t.Errorf("scheme ftp: codes = %v", got)
		}
		if pr, _ := labelFind(ps, CodeSchemeInvalid); pr.Label != p+"scheme" || pr.Service != "web" || pr.Severity != SeverityError {
			t.Errorf("scheme problem = %+v", pr)
		}
	})

	t.Run("strip", func(t *testing.T) {
		base := func(extra ...string) map[string]string {
			return labelSet(append([]string{p + "host", "a.apps.example.com", p + "port", "80"}, extra...)...)
		}
		cases := []struct {
			name  string
			extra []string
			code  string // "" for no problem
			strip bool
		}{
			{"true with a path", []string{p + "path", "/api", p + "strip", "true"}, "", true},
			{"false with no path", []string{p + "strip", "false"}, "", false},
			{"False is false", []string{p + "strip", "False"}, "", false},
			{"empty is not set", []string{p + "strip", ""}, "", false},
			{"empty with a path is not set", []string{p + "path", "/api", p + "strip", ""}, "", false},
			{"yes is invalid", []string{p + "path", "/api", p + "strip", "yes"}, CodeStripInvalid, false},
			{"1 is invalid", []string{p + "path", "/api", p + "strip", "1"}, CodeStripInvalid, false},
			{"true with no path is root", []string{p + "strip", "true"}, CodeStripRoot, false},
			{"true with path / is root", []string{p + "path", "/", p + "strip", "true"}, CodeStripRoot, false},
			{"true with path // after trim is invalid path only", []string{p + "path", "//", p + "strip", "true"}, CodePathInvalid, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ds, ps := ParseServiceLabels("web", base(tc.extra...))
				if tc.code == "" {
					if len(ps) != 0 || len(ds) != 1 || ds[0].Strip != tc.strip {
						t.Errorf("declarations = %+v, problems = %v, want strip %v and none", ds, ps, tc.strip)
					}
					return
				}
				if got := labelCodes(ps); !reflect.DeepEqual(got, []string{tc.code}) {
					t.Errorf("codes = %v, want [%s]", got, tc.code)
				}
				if len(ds) != 0 {
					t.Errorf("a route with an error must be left out, got %+v", ds)
				}
			})
		}
	})

	t.Run("port", func(t *testing.T) {
		for _, v := range []string{"+80", "0x50", "0", "70000", "-1", "80.5", "８０", "99999999999999999999", " "} {
			_, ps := ParseServiceLabels("web", labelSet(p+"host", "a.apps.example.com", p+"port", v))
			want := CodePortInvalid
			if strings.TrimSpace(v) == "" {
				want = CodePortMissing
			}
			if got := labelCodes(ps); !reflect.DeepEqual(got, []string{want}) {
				t.Errorf("port %q: codes = %v, want [%s]", v, got, want)
			}
		}
		for _, v := range []string{"1", "80", "8080", "65535"} {
			ds, ps := ParseServiceLabels("web", labelSet(p+"host", "a.apps.example.com", p+"port", v))
			if len(ps) != 0 || len(ds) != 1 {
				t.Errorf("port %q: declarations = %v, problems = %v", v, ds, ps)
			}
		}
	})

	t.Run("missing host or port is never dropped silently", func(t *testing.T) {
		cases := []struct {
			name   string
			labels map[string]string
			codes  []string
		}{
			{"only a port", labelSet(p+"port", "80"), []string{CodeHostMissing}},
			{"only a host", labelSet(p+"host", "a.apps.example.com"), []string{CodePortMissing}},
			{"empty host", labelSet(p+"host", "", p+"port", "80"), []string{CodeHostMissing}},
			{"empty port", labelSet(p+"host", "a.apps.example.com", p+"port", ""), []string{CodePortMissing}},
			{"both empty", labelSet(p+"host", "", p+"port", ""), []string{CodeHostMissing, CodePortMissing}},
			{"only an empty path", labelSet(p+"path", ""), []string{CodeHostMissing, CodePortMissing}},
			{"only an empty strip", labelSet(p+"api.strip", ""), []string{CodeHostMissing, CodePortMissing}},
		}
		for _, tc := range cases {
			ds, ps := ParseServiceLabels("web", tc.labels)
			if got := labelCodes(ps); !reflect.DeepEqual(got, tc.codes) {
				t.Errorf("%s: codes = %v, want %v", tc.name, got, tc.codes)
			}
			if len(ds) != 0 {
				t.Errorf("%s: declarations = %+v, want none", tc.name, ds)
			}
			for _, pr := range ps {
				if pr.Severity != SeverityError || pr.Service != "web" {
					t.Errorf("%s: problem = %+v", tc.name, pr)
				}
			}
		}
	})

	t.Run("host", func(t *testing.T) {
		for _, v := range []string{"*.apps.example.com", "localhost", "192.168.1.10", "app_1.apps.example.com", "bücher.apps.example.com"} {
			ds, ps := ParseServiceLabels("web", labelSet(p+"host", v, p+"port", "80"))
			if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodeHostInvalid}) {
				t.Errorf("host %q: codes = %v", v, got)
			}
			if len(ds) != 0 {
				t.Errorf("host %q: declarations = %v", v, ds)
			}
		}
		_, ps := ParseServiceLabels("web", labelSet(p+"host", "bücher.apps.example.com", p+"port", "80"))
		if pr, _ := labelFind(ps, CodeHostInvalid); !strings.Contains(pr.Message, "xn--bcher-kva.apps.example.com") {
			t.Errorf("message %q does not name the punycode form", pr.Message)
		}
		if pr, _ := labelFind(ps, CodeHostInvalid); pr.Label != p+"host" {
			t.Errorf("label = %q, want %q", pr.Label, p+"host")
		}
		// An xn-- host is accepted as written.
		ds, ps := ParseServiceLabels("web", labelSet(p+"host", "xn--bcher-kva.apps.example.com", p+"port", "80"))
		if len(ps) != 0 || len(ds) != 1 {
			t.Errorf("xn-- host: declarations = %v, problems = %v", ds, ps)
		}
	})

	t.Run("path", func(t *testing.T) {
		for _, v := range []string{
			"api", "/a`b", "/a{b", "/a}b", "/a?b", "/a#b", "/a\\b", "/a b", "/a\tb", "/a\x00b", "/a\x7fb",
			"/a/../b", "/a/./b", "/..", "/./", "//", "/a//b", "/" + strings.Repeat("a", 255),
		} {
			ds, ps := ParseServiceLabels("web", labelSet(p+"host", "a.apps.example.com", p+"port", "80", p+"path", v))
			if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodePathInvalid}) {
				t.Errorf("path %q: codes = %v", v, got)
			}
			if len(ds) != 0 {
				t.Errorf("path %q: declarations = %v", v, ds)
			}
		}
		for in, want := range map[string]string{"/": "/", "/api": "/api", "/api/": "/api", "/a/b/": "/a/b", "/v1.2/x": "/v1.2/x"} {
			ds, ps := ParseServiceLabels("web", labelSet(p+"host", "a.apps.example.com", p+"port", "80", p+"path", in))
			if len(ps) != 0 || len(ds) != 1 || ds[0].PathPrefix != want {
				t.Errorf("path %q: declarations = %v, problems = %v, want prefix %q", in, ds, ps, want)
			}
		}
	})

	t.Run("unknown keys and route names", func(t *testing.T) {
		cases := []struct {
			key  string
			code string
		}{
			{p + "foo", CodeLabelUnknown},
			{p + "host.port", CodeLabelUnknown},
			{p, CodeLabelUnknown},
			{p + "api.", CodeLabelUnknown},
			{p + "a.b.host", CodeLabelUnknown},
			{p + "api.nope", CodeLabelUnknown},
			{p + "Host", CodeLabelUnknown},
			{p + "Bad_Name.host", CodeRouteNameInvalid},
			{p + "-x.host", CodeRouteNameInvalid},
			{p + strings.Repeat("a", 33) + ".host", CodeRouteNameInvalid},
			{p + "default.host", CodeRouteNameInvalid},
		}
		for _, tc := range cases {
			ds, ps := ParseServiceLabels("web", labelSet(tc.key, "x"))
			if got := labelCodes(ps); !reflect.DeepEqual(got, []string{tc.code}) {
				t.Errorf("%q: codes = %v, want [%s]", tc.key, got, tc.code)
			}
			if len(ds) != 0 {
				t.Errorf("%q: declarations = %v", tc.key, ds)
			}
			if len(ps) == 1 && ps[0].Label != tc.key {
				t.Errorf("%q: problem label = %q", tc.key, ps[0].Label)
			}
		}
	})

	t.Run("a bad route does not hide a good one", func(t *testing.T) {
		ds, ps := ParseServiceLabels("web", labelSet(
			p+"good.host", "g.apps.example.com", p+"good.port", "80",
			p+"bad.host", "b.apps.example.com", p+"bad.port", "0"))
		if len(ds) != 1 || ds[0].Name != "good" {
			t.Errorf("declarations = %+v, want only good", ds)
		}
		if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodePortInvalid}) {
			t.Errorf("codes = %v", got)
		}
	})

	t.Run("output is deterministic", func(t *testing.T) {
		labels := labelSet(
			p+"b.host", "b.apps.example.com", p+"b.port", "x", p+"a.host", "", p+"a.port", "0",
			p+"nope", "1", p+"c.strip", "maybe")
		wantD, wantP := ParseServiceLabels("web", labels)
		for i := 0; i < 50; i++ {
			d, pr := ParseServiceLabels("web", labels)
			if !reflect.DeepEqual(d, wantD) || !reflect.DeepEqual(pr, wantP) {
				t.Fatal("two runs gave different results")
			}
		}
	})
}

func TestParseIsLabel(t *testing.T) {
	if !IsLabel(LabelPrefix+"host") || IsLabel("tierplan.keep") || IsLabel("tierplan.ingress") {
		t.Error("IsLabel misjudged a key")
	}
	if !HasLabels(labelSet("a", "b", LabelPrefix+"port", "")) || HasLabels(labelSet("a", "b")) || HasLabels(nil) {
		t.Error("HasLabels misjudged a map")
	}
}

func TestParseStack(t *testing.T) {
	const p = LabelPrefix
	route := func(host, port string, extra ...string) map[string]string {
		return labelSet(append([]string{p + "host", host, p + "port", port}, extra...)...)
	}

	t.Run("a swarm stack reads deploy.labels", func(t *testing.T) {
		ds, ps := ParseStack([]ServiceLabels{{Name: "web", DeployLabels: route("a.apps.example.com", "80")}}, TargetSwarm)
		if len(ps) != 0 || len(ds) != 1 || ds[0].Service != "web" {
			t.Errorf("declarations = %+v, problems = %v", ds, ps)
		}
	})

	t.Run("a standalone stack reads labels", func(t *testing.T) {
		ds, ps := ParseStack([]ServiceLabels{{Name: "web", Labels: route("a.apps.example.com", "80")}}, TargetStandalone)
		if len(ps) != 0 || len(ds) != 1 {
			t.Errorf("declarations = %+v, problems = %v", ds, ps)
		}
	})

	t.Run("swarm with labels only under labels is the wrong place", func(t *testing.T) {
		ds, ps := ParseStack([]ServiceLabels{{Name: "web", Labels: route("a.apps.example.com", "80")}}, TargetSwarm)
		if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodeLabelsWrongPlace}) {
			t.Fatalf("codes = %v", got)
		}
		if ps[0].Severity != SeverityError || ps[0].Service != "web" || !strings.Contains(ps[0].Message, "deploy.labels") {
			t.Errorf("problem = %+v", ps[0])
		}
		if len(ds) != 0 {
			t.Errorf("declarations = %v, want none", ds)
		}
	})

	t.Run("standalone with labels only under deploy.labels is the wrong place", func(t *testing.T) {
		_, ps := ParseStack([]ServiceLabels{{Name: "web", DeployLabels: route("a.apps.example.com", "80")}}, TargetStandalone)
		if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodeLabelsWrongPlace}) {
			t.Fatalf("codes = %v", got)
		}
		if ps[0].Severity != SeverityError || !strings.Contains(ps[0].Message, "labels") {
			t.Errorf("problem = %+v", ps[0])
		}
	})

	t.Run("both places warn and the right one is read", func(t *testing.T) {
		svc := ServiceLabels{
			Name: "web", Labels: route("wrong.apps.example.com", "80"), DeployLabels: route("right.apps.example.com", "81"),
		}
		ds, ps := ParseStack([]ServiceLabels{svc}, TargetSwarm)
		if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodeLabelsIgnored}) {
			t.Fatalf("codes = %v", got)
		}
		if ps[0].Severity != SeverityWarning {
			t.Errorf("severity = %s, want warning", ps[0].Severity)
		}
		if len(ds) != 1 || ds[0].Host != "right.apps.example.com" || ds[0].Port != 81 {
			t.Errorf("declarations = %+v", ds)
		}
		ds, ps = ParseStack([]ServiceLabels{svc}, TargetStandalone)
		if labelCodes(ps)[0] != CodeLabelsIgnored || len(ds) != 1 || ds[0].Host != "wrong.apps.example.com" {
			t.Errorf("standalone: declarations = %+v, problems = %v", ds, ps)
		}
	})

	t.Run("a service with no ingress labels in either place is silent", func(t *testing.T) {
		ds, ps := ParseStack([]ServiceLabels{{Name: "db", Labels: labelSet("a", "b"), DeployLabels: labelSet("c", "d")}}, TargetSwarm)
		if len(ds) != 0 || len(ps) != 0 {
			t.Errorf("declarations = %v, problems = %v", ds, ps)
		}
	})

	t.Run("two services with the same host and path are a duplicate", func(t *testing.T) {
		ds, ps := ParseStack([]ServiceLabels{
			{Name: "b", DeployLabels: route("a.apps.example.com", "81")},
			{Name: "a", DeployLabels: route("a.apps.example.com", "80")},
		}, TargetSwarm)
		if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodeRouteDuplicate}) {
			t.Fatalf("codes = %v", got)
		}
		if ps[0].Severity != SeverityError || ps[0].Service != "b" || ps[0].Label != p+"host" {
			t.Errorf("problem = %+v", ps[0])
		}
		if len(ds) != 1 || ds[0].Service != "a" {
			t.Errorf("declarations = %+v, want the first (a) kept", ds)
		}
	})

	t.Run("two routes of one service with the same host and path are a duplicate", func(t *testing.T) {
		_, ps := ParseStack([]ServiceLabels{{Name: "web", DeployLabels: labelSet(
			p+"host", "a.apps.example.com", p+"port", "80",
			p+"api.host", "a.apps.example.com", p+"api.port", "81")}}, TargetSwarm)
		if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodeRouteDuplicate}) {
			t.Errorf("codes = %v", got)
		}
	})

	t.Run("the same host with different paths is allowed", func(t *testing.T) {
		ds, ps := ParseStack([]ServiceLabels{
			{Name: "web", DeployLabels: route("a.apps.example.com", "80")},
			{Name: "api", DeployLabels: route("a.apps.example.com", "81", p+"path", "/api")},
		}, TargetSwarm)
		if len(ps) != 0 || len(ds) != 2 {
			t.Errorf("declarations = %+v, problems = %v", ds, ps)
		}
	})

	t.Run("a path spelled two ways is one prefix", func(t *testing.T) {
		_, ps := ParseStack([]ServiceLabels{
			{Name: "a", DeployLabels: route("a.apps.example.com", "80", p+"path", "/api")},
			{Name: "b", DeployLabels: route("A.apps.example.com.", "81", p+"path", "/api/")},
		}, TargetSwarm)
		if got := labelCodes(ps); !reflect.DeepEqual(got, []string{CodeRouteDuplicate}) {
			t.Errorf("codes = %v", got)
		}
	})

	t.Run("declarations come back sorted by service then route name", func(t *testing.T) {
		ds, ps := ParseStack([]ServiceLabels{
			{Name: "z", DeployLabels: route("z.apps.example.com", "80")},
			{Name: "m", DeployLabels: labelSet(
				p+"b.host", "mb.apps.example.com", p+"b.port", "1", p+"a.host", "ma.apps.example.com", p+"a.port", "2")},
			{Name: "c", DeployLabels: route("c.apps.example.com", "80")},
		}, TargetSwarm)
		if len(ps) != 0 {
			t.Fatalf("problems = %v", ps)
		}
		var got []string
		for _, d := range ds {
			got = append(got, d.Service+"/"+d.Name)
		}
		if want := []string{"c/default", "m/a", "m/b", "z/default"}; !reflect.DeepEqual(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	})

	t.Run("the input slice is not reordered", func(t *testing.T) {
		in := []ServiceLabels{{Name: "z"}, {Name: "a"}}
		ParseStack(in, TargetSwarm)
		if in[0].Name != "z" {
			t.Error("ParseStack sorted its argument")
		}
	})

	t.Run("every error problem carries its service", func(t *testing.T) {
		_, ps := ParseStack([]ServiceLabels{
			{Name: "w", DeployLabels: labelSet(p+"port", "0", p+"zz", "1")},
			{Name: "x", Labels: route("a.apps.example.com", "80")},
		}, TargetSwarm)
		if len(ps) == 0 {
			t.Fatal("no problems")
		}
		for _, pr := range ps {
			if pr.Service == "" || pr.Code == "" || pr.Message == "" {
				t.Errorf("problem = %+v", pr)
			}
		}
	})

	t.Run("nothing is dropped without an error problem", func(t *testing.T) {
		// Every ingress key of a service either ends in a declaration or in an error problem.
		_, ps := ParseStack([]ServiceLabels{{Name: "w", DeployLabels: labelSet(p+"api.path", "/x")}}, TargetSwarm)
		if !HasErrors(ps) {
			t.Errorf("a declared route vanished with problems = %v", ps)
		}
	})
}

// TestParseCodesAreStable pins the text of every code the parser reports: the editor and the API
// tests key on these strings, so a rename is a breaking change.
func TestParseCodesAreStable(t *testing.T) {
	for got, want := range map[string]string{
		CodeLabelUnknown:     "ingress_label_unknown",
		CodeRouteNameInvalid: "ingress_route_name_invalid",
		CodeHostMissing:      "ingress_host_missing",
		CodeHostInvalid:      "ingress_host_invalid",
		CodePortMissing:      "ingress_port_missing",
		CodePortInvalid:      "ingress_port_invalid",
		CodePathInvalid:      "ingress_path_invalid",
		CodeSchemeInvalid:    "ingress_scheme_invalid",
		CodeStripInvalid:     "ingress_strip_invalid",
		CodeStripRoot:        "ingress_strip_root",
		CodeLabelsWrongPlace: "ingress_labels_wrong_place",
		CodeLabelsIgnored:    "ingress_labels_ignored",
		CodeRouteDuplicate:   "ingress_route_duplicate",
	} {
		if got != want {
			t.Errorf("code %q, want %q", got, want)
		}
	}
}
