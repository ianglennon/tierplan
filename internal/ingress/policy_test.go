// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"reflect"
	"strings"
	"testing"
)

// policyCodes lists the codes of ps in order.
func policyCodes(ps []Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Code)
	}
	return out
}

// policyDomain builds a registered domain.
func policyDomain(id, suffix string) Domain {
	return Domain{ID: id, EnvID: "env-a", Suffix: suffix}
}

// policyWorld builds a world with the given domains, nothing reserved and nothing claimed.
func policyWorld(domains ...Domain) World {
	return World{Domains: domains, Reserved: map[string]bool{}, Claims: map[string]ClaimInfo{}}
}

// policyDecl builds a declaration of one route of a service.
func policyDecl(service, name, host, path string) Declaration {
	return Declaration{
		Service: service, Name: name, Host: host, Label: LabelPrefix + "host",
		PathPrefix: path, Port: 8080, Scheme: SchemeHTTP,
	}
}

// policyHold records a claim of host held by a stack of another environment.
func policyHold(w World, host, stackID, stackName, envID, envName string) {
	w.Claims[host] = ClaimInfo{
		Claim:     Claim{EnvID: envID, Hostname: host, OwnerKind: OwnerStack, StackID: stackID},
		EnvName:   envName,
		StackName: stackName,
	}
}

func TestCheckNoDeclarationsPassesWhateverTheWorld(t *testing.T) {
	worlds := map[string]World{
		"zero value":    {},
		"no domains":    policyWorld(),
		"with a domain": policyWorld(policyDomain("d1", "apps.example.com")),
	}
	for name, w := range worlds {
		t.Run(name, func(t *testing.T) {
			plan, problems := Check("s1", nil, w)
			if len(plan.Claims) != 0 || problems != nil {
				t.Fatalf("Check with no declarations = %+v, %v, want an empty plan and nil problems", plan, problems)
			}
			plan, problems = Check("s1", []Declaration{}, w)
			if len(plan.Claims) != 0 || problems != nil {
				t.Fatalf("Check with empty declarations = %+v, %v, want an empty plan and nil problems", plan, problems)
			}
		})
	}
}

func TestCheckRules(t *testing.T) {
	const (
		inside    = "app.apps.example.com"
		other     = "api.apps.example.com"
		outside   = "app.other.org"
		apex      = "apps.example.com"
		lookalike = "badapps.example.com"
	)
	tests := []struct {
		name     string
		decls    []Declaration
		setup    func(w World)
		domains  []Domain
		want     []string // problem codes, in order
		wantHost []string // hostnames in the plan
	}{
		{
			name:     "a host under a registered domain is planned",
			decls:    []Declaration{policyDecl("web", "", inside, "/")},
			domains:  []Domain{policyDomain("d1", "apps.example.com")},
			wantHost: []string{inside},
		},
		{
			name:    "the same host and path twice in one stack is one duplicate",
			decls:   []Declaration{policyDecl("web", "", inside, "/"), policyDecl("api", "", inside, "/")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			want:    []string{CodeRouteDuplicate}, wantHost: []string{inside},
		},
		{
			name:     "the same host on different paths is not a duplicate",
			decls:    []Declaration{policyDecl("web", "", inside, "/"), policyDecl("api", "api", inside, "/api")},
			domains:  []Domain{policyDomain("d1", "apps.example.com")},
			wantHost: []string{inside},
		},
		{
			name:    "a host outside every domain",
			decls:   []Declaration{policyDecl("web", "", outside, "/")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			want:    []string{CodeDomainNotAllowed},
		},
		{
			name:    "the domain suffix itself is never claimable",
			decls:   []Declaration{policyDecl("web", "", apex, "/")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			want:    []string{CodeDomainNotAllowed},
		},
		{
			name:    "a look-alike without the dot boundary is outside the domain",
			decls:   []Declaration{policyDecl("web", "", lookalike, "/")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			want:    []string{CodeDomainNotAllowed},
		},
		{
			name:    "an environment with no domains refuses every host",
			decls:   []Declaration{policyDecl("web", "", inside, "/"), policyDecl("api", "api", other, "/")},
			domains: nil,
			want:    []string{CodeDomainNotAllowed, CodeDomainNotAllowed},
		},
		{
			name:    "a reserved name is refused",
			decls:   []Declaration{policyDecl("web", "", inside, "/")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			// Reserved under another environment's domain with the same suffix: install-wide (D-11).
			setup: func(w World) { w.Reserved[inside] = true },
			want:  []string{CodeHostReserved},
		},
		{
			name:    "a name the system holds is taken",
			decls:   []Declaration{policyDecl("web", "", inside, "/")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			setup: func(w World) {
				w.Claims[inside] = ClaimInfo{Claim: Claim{EnvID: "env-z", Hostname: inside, OwnerKind: OwnerSystem}, EnvName: "Zed"}
			},
			want: []string{CodeHostTaken},
		},
		{
			name:    "a name another stack holds in another environment is taken",
			decls:   []Declaration{policyDecl("web", "", inside, "/")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			setup:   func(w World) { policyHold(w, inside, "s2", "other-stack", "env-b", "Beta") },
			want:    []string{CodeHostTaken},
		},
		{
			name:    "a name another stack holds in the same environment is taken",
			decls:   []Declaration{policyDecl("web", "", inside, "/")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			setup:   func(w World) { policyHold(w, inside, "s2", "other-stack", "env-a", "Alpha") },
			want:    []string{CodeHostTaken},
		},
		{
			name:     "a stack may re-declare a name it already holds",
			decls:    []Declaration{policyDecl("web", "", inside, "/")},
			domains:  []Domain{policyDomain("d1", "apps.example.com")},
			setup:    func(w World) { policyHold(w, inside, "s1", "mine", "env-a", "Alpha") },
			wantHost: []string{inside},
		},
		{
			name:  "one refused host does not stop the others",
			decls: []Declaration{policyDecl("web", "", outside, "/"), policyDecl("api", "api", other, "/")},
			domains: []Domain{
				policyDomain("d1", "apps.example.com"),
			},
			want: []string{CodeDomainNotAllowed}, wantHost: []string{other},
		},
		{
			name:    "a host refused once is judged once",
			decls:   []Declaration{policyDecl("web", "", outside, "/"), policyDecl("api", "api", outside, "/api")},
			domains: []Domain{policyDomain("d1", "apps.example.com")},
			want:    []string{CodeDomainNotAllowed},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := policyWorld(tt.domains...)
			if tt.setup != nil {
				tt.setup(w)
			}
			plan, problems := Check("s1", tt.decls, w)
			if got := policyCodes(problems); !reflect.DeepEqual(got, tt.want) && (len(got) != 0 || len(tt.want) != 0) {
				t.Fatalf("codes = %v, want %v (problems %+v)", got, tt.want, problems)
			}
			if got := plan.Hostnames(); !reflect.DeepEqual(got, tt.wantHost) && (len(got) != 0 || len(tt.wantHost) != 0) {
				t.Fatalf("planned hosts = %v, want %v", got, tt.wantHost)
			}
			for _, p := range problems {
				if p.Severity != SeverityError {
					t.Errorf("problem %s severity = %q, want error", p.Code, p.Severity)
				}
				if p.Service == "" || p.Label == "" {
					t.Errorf("problem %s lost its service %q or label %q", p.Code, p.Service, p.Label)
				}
				if p.Message == "" {
					t.Errorf("problem %s has no message", p.Code)
				}
			}
		})
	}
}

func TestCheckDuplicateKeepsTheFirstDeclaration(t *testing.T) {
	w := policyWorld(policyDomain("d1", "apps.example.com"))
	first := policyDecl("web", "main", "app.apps.example.com", "/")
	second := policyDecl("api", "other", "app.apps.example.com", "/")
	plan, problems := Check("s1", []Declaration{first, second}, w)
	if len(problems) != 1 || problems[0].Code != CodeRouteDuplicate {
		t.Fatalf("problems = %+v, want one %s", problems, CodeRouteDuplicate)
	}
	if problems[0].Service != "api" {
		t.Errorf("the duplicate is reported on service %q, want the second declaration's api", problems[0].Service)
	}
	if len(plan.Claims) != 1 || len(plan.Claims[0].Routes) != 1 || plan.Claims[0].Routes[0].Service != "web" {
		t.Fatalf("plan = %+v, want only the first declaration kept", plan)
	}
}

func TestCheckLongestSuffixWins(t *testing.T) {
	w := policyWorld(policyDomain("broad", "example.com"), policyDomain("narrow", "apps.example.com"))
	plan, problems := Check("s1", []Declaration{
		policyDecl("web", "", "app.apps.example.com", "/"),
		policyDecl("www", "www", "www.example.com", "/"),
	}, w)
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
	got := map[string]string{}
	for _, c := range plan.Claims {
		got[c.Hostname] = c.DomainID
	}
	want := map[string]string{"app.apps.example.com": "narrow", "www.example.com": "broad"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("domain ids = %v, want %v", got, want)
	}
}

func TestCheckApexOfNarrowDomainFallsToBroaderOne(t *testing.T) {
	// apps.example.com is the apex of the narrow domain but a name under the broad one: it is allowed.
	w := policyWorld(policyDomain("broad", "example.com"), policyDomain("narrow", "apps.example.com"))
	plan, problems := Check("s1", []Declaration{policyDecl("web", "", "apps.example.com", "/")}, w)
	if len(problems) != 0 || len(plan.Claims) != 1 || plan.Claims[0].DomainID != "broad" {
		t.Fatalf("plan = %+v, problems = %+v, want the name planned under the broad domain", plan, problems)
	}
}

func TestCheckOutputIsDeterministic(t *testing.T) {
	w := policyWorld(policyDomain("d1", "apps.example.com"))
	decls := []Declaration{
		policyDecl("web", "", "z.apps.example.com", "/"),
		policyDecl("web", "b", "a.apps.example.com", "/b"),
		policyDecl("api", "a", "a.apps.example.com", "/a"),
		policyDecl("web", "c", "a.apps.example.com", "/b/c"),
		policyDecl("worker", "w", "a.apps.example.com", "/b/c"),
		policyDecl("web", "x", "m.apps.example.com", "/"),
		policyDecl("web", "bad", "outside.example.org", "/"),
		policyDecl("web", "bad2", "outside2.example.org", "/"),
	}
	// "a.apps.example.com /b/c" is declared twice (web c, worker w): the second is a duplicate.
	var firstPlan Plan
	var firstProblems []Problem
	for i := 0; i < 5; i++ {
		// Rotate nothing: the same input must give the same output every call.
		plan, problems := Check("s1", decls, w)
		if i == 0 {
			firstPlan, firstProblems = plan, problems
			continue
		}
		if !reflect.DeepEqual(plan, firstPlan) || !reflect.DeepEqual(problems, firstProblems) {
			t.Fatalf("call %d differs from the first", i)
		}
	}
	if got, want := firstPlan.Hostnames(), []string{"a.apps.example.com", "m.apps.example.com", "z.apps.example.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hostnames = %v, want %v", got, want)
	}
	var paths []string
	for _, r := range firstPlan.Claims[0].Routes {
		paths = append(paths, r.PathPrefix+"|"+r.Service+"|"+r.Name)
	}
	if want := []string{"/a|api|a", "/b|web|b", "/b/c|web|c"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("routes of the first claim = %v, want %v", paths, want)
	}
	// Problems come in declaration order: the duplicate (declared fifth), then the two outside names.
	wantCodes := []string{CodeRouteDuplicate, CodeDomainNotAllowed, CodeDomainNotAllowed}
	if got := policyCodes(firstProblems); !reflect.DeepEqual(got, wantCodes) {
		t.Fatalf("problem codes = %v, want %v", got, wantCodes)
	}
}

func TestCheckSortsRoutesOfOneHostByPathThenServiceThenName(t *testing.T) {
	w := policyWorld(policyDomain("d1", "apps.example.com"))
	const h = "app.apps.example.com"
	plan, problems := Check("s1", []Declaration{
		policyDecl("web", "z", h, "/x"),
		policyDecl("web", "a", h, "/"),
		policyDecl("api", "m", h, "/x/y"),
		policyDecl("api", "n", h, "/x/z"),
		policyDecl("abc", "q", h, "/x/zz"),
	}, w)
	if len(problems) != 0 || len(plan.Claims) != 1 {
		t.Fatalf("plan = %+v, problems = %+v, want one claim and no problems", plan, problems)
	}
	var got []string
	for _, r := range plan.Claims[0].Routes {
		got = append(got, r.PathPrefix)
	}
	if want := []string{"/", "/x", "/x/y", "/x/z", "/x/zz"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("route paths = %v, want %v", got, want)
	}
}

func TestCheckSeveralServicesOnOneHostAreOneClaim(t *testing.T) {
	w := policyWorld(policyDomain("d1", "apps.example.com"))
	const h = "app.apps.example.com"
	plan, problems := Check("s1", []Declaration{
		policyDecl("web", "", h, "/"),
		policyDecl("api", "api", h, "/api"),
	}, w)
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
	if len(plan.Claims) != 1 || len(plan.Claims[0].Routes) != 2 {
		t.Fatalf("plan = %+v, want one claim with two routes", plan)
	}
}

func TestCheckHostClashMessageIsRedactedUnlessTheCallerCanView(t *testing.T) {
	const h = "app.apps.example.com"
	decls := []Declaration{policyDecl("web", "", h, "/")}
	build := func(canView func(string) bool) Problem {
		w := policyWorld(policyDomain("d1", "apps.example.com"))
		policyHold(w, h, "s2", "secret-stack", "env-b", "Secret-Env")
		w.CanView = canView
		_, problems := Check("s1", decls, w)
		if len(problems) != 1 || problems[0].Code != CodeHostTaken {
			t.Fatalf("problems = %+v, want one %s", problems, CodeHostTaken)
		}
		return problems[0]
	}

	visible := build(func(envID string) bool { return envID == "env-b" })
	for _, name := range []string{"secret-stack", "Secret-Env"} {
		if !strings.Contains(visible.Message, name) {
			t.Errorf("a visible clash message %q must name %q", visible.Message, name)
		}
	}

	cases := map[string]func(string) bool{
		"nil CanView":           nil,
		"CanView says no":       func(string) bool { return false },
		"CanView says no to it": func(envID string) bool { return envID != "env-b" },
	}
	for name, cv := range cases {
		t.Run(name, func(t *testing.T) {
			p := build(cv)
			for _, leak := range []string{"secret-stack", "Secret-Env", "s2", "env-b"} {
				if strings.Contains(p.Message, leak) {
					t.Errorf("a redacted clash message %q must not contain %q", p.Message, leak)
				}
			}
			if !strings.Contains(p.Message, "an environment you can't see") {
				t.Errorf("a redacted clash message %q must say the environment cannot be seen", p.Message)
			}
		})
	}
}

func TestCheckSystemClashNamesNoStack(t *testing.T) {
	const h = "app.apps.example.com"
	w := policyWorld(policyDomain("d1", "apps.example.com"))
	w.Claims[h] = ClaimInfo{Claim: Claim{EnvID: "env-z", Hostname: h, OwnerKind: OwnerSystem}, EnvName: "Zed"}
	w.CanView = func(string) bool { return true }
	_, problems := Check("s1", []Declaration{policyDecl("web", "", h, "/")}, w)
	if len(problems) != 1 || problems[0].Code != CodeHostTaken {
		t.Fatalf("problems = %+v, want one %s", problems, CodeHostTaken)
	}
	if !strings.Contains(problems[0].Message, "TierPlan itself is served on") {
		t.Errorf("message %q must say TierPlan itself is served on the name", problems[0].Message)
	}
	if strings.Contains(problems[0].Message, "Zed") {
		t.Errorf("message %q must not name the system claim's environment", problems[0].Message)
	}
}

func TestCheckRefusesReservedNamesOfAnyEnvironment(t *testing.T) {
	// The store fills Reserved from every environment (D-11): a name reserved under another
	// environment's domain is refused for a stack here, and the message names no environment.
	const h = "app.apps.example.com"
	w := policyWorld(policyDomain("d1", "apps.example.com"))
	w.Reserved[h] = true
	w.CanView = func(string) bool { return false }
	plan, problems := Check("s1", []Declaration{policyDecl("web", "", h, "/")}, w)
	if len(plan.Claims) != 0 || len(problems) != 1 || problems[0].Code != CodeHostReserved {
		t.Fatalf("plan = %+v, problems = %+v, want one %s and nothing planned", plan, problems, CodeHostReserved)
	}
	if !strings.Contains(problems[0].Message, h) {
		t.Errorf("message %q should name the hostname", problems[0].Message)
	}
}

func TestCheckNeverReadsAnIngressSwitch(t *testing.T) {
	// D-18: the policy refuses nothing because ingress is switched off. World has no field for a
	// switch, and a world with domains and no flag at all plans a declared host.
	typ := reflect.TypeOf(World{})
	for i := 0; i < typ.NumField(); i++ {
		n := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(n, "enabled") || strings.Contains(n, "switch") {
			t.Errorf("World has a switch-like field %q; Check must not read one (D-18)", typ.Field(i).Name)
		}
	}
	w := policyWorld(policyDomain("d1", "apps.example.com"))
	plan, problems := Check("s1", []Declaration{policyDecl("web", "", "app.apps.example.com", "/")}, w)
	if len(problems) != 0 || len(plan.Claims) != 1 {
		t.Fatalf("plan = %+v, problems = %+v, want the host planned", plan, problems)
	}
}

func TestDescribeHolder(t *testing.T) {
	const h = "app.apps.example.com"
	system := ClaimInfo{Claim: Claim{EnvID: "env-z", Hostname: h, OwnerKind: OwnerSystem}, EnvName: "Zed-Env"}
	stack := ClaimInfo{
		Claim:     Claim{EnvID: "env-b", Hostname: h, OwnerKind: OwnerStack, StackID: "stack-id-9"},
		EnvName:   "Beta-Env",
		StackName: "billing-stack",
	}
	yes := func(string) bool { return true }
	no := func(string) bool { return false }

	t.Run("system names TierPlan and no stack or environment", func(t *testing.T) {
		for name, cv := range map[string]func(string) bool{"nil": nil, "yes": yes, "no": no} {
			got := DescribeHolder(system, cv)
			if !strings.Contains(got, h) || !strings.Contains(got, "TierPlan itself is served on") {
				t.Errorf("%s: DescribeHolder(system) = %q, want the host and TierPlan's own address", name, got)
			}
			for _, leak := range []string{"Zed-Env", "env-z"} {
				if strings.Contains(got, leak) {
					t.Errorf("%s: DescribeHolder(system) = %q must not contain %q", name, got, leak)
				}
			}
		}
	})

	t.Run("a visible stack is named with its environment", func(t *testing.T) {
		got := DescribeHolder(stack, func(envID string) bool { return envID == "env-b" })
		for _, want := range []string{h, "“billing-stack”", "“Beta-Env”"} {
			if !strings.Contains(got, want) {
				t.Errorf("DescribeHolder(visible) = %q, want %q in it", got, want)
			}
		}
		if strings.Contains(got, "stack-id-9") || strings.Contains(got, "env-b") {
			t.Errorf("DescribeHolder(visible) = %q must not carry ids", got)
		}
	})

	for name, cv := range map[string]func(string) bool{"nil canView": nil, "canView false": no} {
		t.Run("an invisible stack is not named: "+name, func(t *testing.T) {
			got := DescribeHolder(stack, cv)
			for _, leak := range []string{"billing-stack", "Beta-Env", "stack-id-9", "env-b"} {
				if strings.Contains(got, leak) {
					t.Errorf("DescribeHolder(invisible) = %q must not contain %q", got, leak)
				}
			}
			for _, want := range []string{h, "an environment you can't see"} {
				if !strings.Contains(got, want) {
					t.Errorf("DescribeHolder(invisible) = %q, want %q in it", got, want)
				}
			}
		})
	}
}

func TestCheckClashMessageStartsWithDescribeHolder(t *testing.T) {
	// One redaction rule: the problem Check reports is the holder sentence plus its guidance.
	const h = "app.apps.example.com"
	for name, cv := range map[string]func(string) bool{"visible": func(string) bool { return true }, "hidden": nil} {
		w := policyWorld(policyDomain("d1", "apps.example.com"))
		policyHold(w, h, "s2", "other-stack", "env-b", "Beta")
		w.CanView = cv
		_, problems := Check("s1", []Declaration{policyDecl("web", "", h, "/")}, w)
		if len(problems) != 1 {
			t.Fatalf("%s: problems = %+v, want one", name, problems)
		}
		if want := DescribeHolder(w.Claims[h], cv); !strings.HasPrefix(problems[0].Message, want) {
			t.Errorf("%s: message %q does not start with DescribeHolder's %q", name, problems[0].Message, want)
		}
	}
}

// TestCheckRequest covers the rule for a hostname asked for before any stack claims it (D-08): one
// row per policy bullet, over a pure world.
func TestCheckRequest(t *testing.T) {
	apps := policyDomain("d1", "apps.example.com")
	system := func(w World, host string) {
		w.Claims[host] = ClaimInfo{Claim: Claim{EnvID: "env-a", Hostname: host, OwnerKind: OwnerSystem}, EnvName: "Alpha"}
	}
	sameEnv := func(w World, host string) {
		policyHold(w, host, "s-own", "web-own", "env-a", "Alpha")
	}
	otherEnv := func(w World, host string) {
		policyHold(w, host, "s-other", "web-other", "env-b", "Beta")
	}
	reserve := func(w World, host string) { w.Reserved[host] = true }

	tests := []struct {
		name     string
		host     string
		setup    func(w World, host string)
		wantCode string // "" means allowed
		wantDom  string
	}{
		{name: "unclaimed under a registered domain", host: "web.apps.example.com", wantDom: "d1"},
		{name: "claimed by a stack of the same environment", host: "web.apps.example.com", setup: sameEnv, wantDom: "d1"},
		{name: "under no registered domain", host: "web.other.org", wantCode: CodeDomainNotAllowed},
		{name: "reserved", host: "web.apps.example.com", setup: reserve, wantCode: CodeHostReserved},
		{name: "claimed by another environment's stack", host: "web.apps.example.com", setup: otherEnv, wantCode: CodeHostTaken},
		{name: "claimed by the system", host: "web.apps.example.com", setup: system, wantCode: CodeHostTaken},
		{name: "an underscore label", host: "bad_name.apps.example.com", wantCode: CodeHostInvalid},
		{name: "empty", host: "", wantCode: CodeHostInvalid},
		{name: "an address", host: "10.0.0.1", wantCode: CodeHostInvalid},
		{name: "a single label", host: "localhost", wantCode: CodeHostInvalid},
		{name: "the domain's own suffix", host: "apps.example.com", wantCode: CodeDomainNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := policyWorld(apps)
			if tc.setup != nil {
				tc.setup(w, tc.host)
			}
			dom, p, bad := CheckRequest(tc.host, w)
			if tc.wantCode == "" {
				if bad || dom.ID != tc.wantDom {
					t.Fatalf("CheckRequest(%q) = %+v, %+v, %v, want allowed under %s", tc.host, dom, p, bad, tc.wantDom)
				}
				return
			}
			if !bad || p.Code != tc.wantCode || p.Severity != SeverityError || p.Message == "" {
				t.Fatalf("CheckRequest(%q) = %+v, %v, want a refusal with code %s", tc.host, p, bad, tc.wantCode)
			}
			if dom.ID != "" {
				t.Fatalf("a refusal returned the domain %+v", dom)
			}
		})
	}
}

// TestCheckRequestTakenIsRedactedAndWordedThroughDescribeHolder is T-20-09: a claim of another
// environment's stack names neither stack nor environment to a caller who cannot see it, and does
// when the caller can; a system claim names no stack.
func TestCheckRequestTakenIsRedactedAndWordedThroughDescribeHolder(t *testing.T) {
	const host = "web.apps.example.com"
	w := policyWorld(policyDomain("d1", "apps.example.com"))
	policyHold(w, host, "s-other", "secret-stack", "env-b", "Secret Env")

	_, p, bad := CheckRequest(host, w)
	if !bad || p.Code != CodeHostTaken {
		t.Fatalf("CheckRequest = %+v, %v, want ingress_host_taken", p, bad)
	}
	for _, leak := range []string{"secret-stack", "Secret Env", "s-other", "env-b"} {
		if strings.Contains(p.Message, leak) {
			t.Fatalf("message %q names %q to a caller who cannot see the environment", p.Message, leak)
		}
	}
	want := DescribeHolder(w.Claims[host], nil)
	if !strings.HasPrefix(p.Message, want) {
		t.Fatalf("message %q does not start with DescribeHolder's %q", p.Message, want)
	}

	w.CanView = func(envID string) bool { return envID == "env-b" }
	_, p, _ = CheckRequest(host, w)
	if !strings.Contains(p.Message, "secret-stack") || !strings.Contains(p.Message, "Secret Env") {
		t.Fatalf("message %q should name the stack and environment to a caller who may see them", p.Message)
	}

	sys := policyWorld(policyDomain("d1", "apps.example.com"))
	sys.Claims[host] = ClaimInfo{Claim: Claim{EnvID: "env-a", Hostname: host, OwnerKind: OwnerSystem}, EnvName: "Alpha"}
	_, p, bad = CheckRequest(host, sys)
	if !bad || p.Code != CodeHostTaken || !strings.HasPrefix(p.Message, DescribeHolder(sys.Claims[host], nil)) {
		t.Fatalf("a system claim gave %+v, %v", p, bad)
	}
}
