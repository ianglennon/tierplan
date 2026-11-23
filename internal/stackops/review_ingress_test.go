// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The review carries the ingress policy into the draft check (plan 18-11, D-27): a hostname the environment's
// world does not allow is a problem in the review's list, and without a policy only the parser speaks.

const ingressReviewCompose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.elsewhere.org
        tierplan.ingress.port: "80"
`

func TestReviewIngressPolicy(t *testing.T) {
	t.Parallel()
	r := newSwarmRig(t, nil)
	in := r.reviewInput(t, "ingress", ingressReviewCompose, nil)

	// No policy: a host outside every domain is not a problem, the parser accepts the labels.
	rv, err := r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review without a policy: %v", err)
	}
	if hasCode(rv.Problems, ingress.CodeDomainNotAllowed) {
		t.Fatalf("problems = %+v, want no policy problem without a policy", rv.Problems)
	}

	// With a world that registers only apps.example.com the same draft is outside the domains.
	in.IngressPolicy = &stacks.IngressPolicy{StackID: in.Stack.ID, World: ingress.World{
		Domains: []ingress.Domain{{ID: "dom-1", EnvID: r.envID, Suffix: "apps.example.com"}},
	}}
	rv, err = r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review with a policy: %v", err)
	}
	if !hasCode(rv.Problems, ingress.CodeDomainNotAllowed) {
		t.Fatalf("problems = %+v, want %s", rv.Problems, ingress.CodeDomainNotAllowed)
	}
	for _, p := range rv.Problems {
		if p.Code == ingress.CodeDomainNotAllowed && p.Severity != stacks.SeverityError {
			t.Errorf("problem = %+v, want an error that blocks the deploy", p)
		}
	}
}

func TestReviewIngressPolicyServesStandaloneDrafts(t *testing.T) {
	t.Parallel()
	r := newStandaloneRig(t)
	in := r.reviewInput(t, "ingress-sa", `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.elsewhere.org
      tierplan.ingress.port: "80"
`, nil)
	in.Stack.Target = stacks.TargetStandalone
	in.IngressPolicy = &stacks.IngressPolicy{StackID: in.Stack.ID, World: ingress.World{
		Domains: []ingress.Domain{{ID: "dom-1", EnvID: r.envID, Suffix: "apps.example.com"}},
	}}
	// The one draft check serves both targets: the standalone review that follows it starts from what it found.
	rv, err := r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !hasCode(rv.Problems, ingress.CodeDomainNotAllowed) {
		t.Errorf("problems = %+v, want %s on a standalone draft too", rv.Problems, ingress.CodeDomainNotAllowed)
	}
}

func hasCode(ps stacks.Problems, code string) bool {
	for _, p := range ps {
		if p.Code == code {
			return true
		}
	}
	return false
}

// The review lists what a deploy does to ingress (plan 24-22, D-22, DEP-03): the routes it adds, keeps and releases,
// read against the routes the stack holds now.

// routedTwo declares a.example.com (kept) and b.example.com (added) on one service; the stack held a.example.com and
// c.example.com before.
const routedTwo = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.a.host: a.example.com
        tierplan.ingress.a.port: "80"
        tierplan.ingress.b.host: b.example.com
        tierplan.ingress.b.port: "80"
`

// heldRoute is a route as the store holds it.
func heldRoute(name, host, path, service string, port int, scheme ingress.Scheme) ingress.Route {
	return ingress.Route{Hostname: host, Name: name, PathPrefix: path, Service: service, Port: port, Scheme: scheme}
}

// reviewWith reviews a compose file of a swarm stack with the ingress model fake.
func reviewWith(t *testing.T, fake *fakeIngress, name, compose string) stackops.Review {
	t.Helper()
	r := newSwarmRig(t, nil)
	r.deps.Ingress = fake
	rv, err := r.deps.Review(testsupport.Context(t), r.reviewInput(t, name, compose, nil))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	return rv
}

func TestReviewIngressRoutes(t *testing.T) {
	t.Parallel()
	fake := &fakeIngress{held: []ingress.Route{
		heldRoute("c", "c.example.com", "/", "web", 80, ingress.SchemeHTTP),
		heldRoute("a", "a.example.com", "/", "web", 80, ingress.SchemeHTTP),
	}}
	rv := reviewWith(t, fake, "shop", routedTwo)
	if rv.Ingress == nil {
		t.Fatal("review has no ingress section")
	}
	want := []stackops.IngressRouteRow{
		{Change: stackops.IngressRouteKept, Hostname: "a.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
		{Change: stackops.IngressRouteAdded, Hostname: "b.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
		{Change: stackops.IngressRouteReleased, Hostname: "c.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
	}
	if !slices.Equal(rv.Ingress.Routes, want) {
		t.Errorf("routes = %+v, want %+v", rv.Ingress.Routes, want)
	}
}

// A route that is unchanged in hostname, path, service, port and scheme is one kept row; two routes on one hostname with
// different paths are two rows, ordered by path; a change of port is a release and an add.
func TestReviewIngressRouteAdjacency(t *testing.T) {
	t.Parallel()
	fake := &fakeIngress{held: []ingress.Route{
		heldRoute("api", "a.example.com", "/api", "web", 80, ingress.SchemeHTTP),
		heldRoute("root", "a.example.com", "/", "web", 80, ingress.SchemeHTTP),
		heldRoute("z", "z.example.com", "", "web", 80, ""),
	}}
	rv := reviewWith(t, fake, "shop", `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.root.host: a.example.com
        tierplan.ingress.root.port: "80"
        tierplan.ingress.api.host: a.example.com
        tierplan.ingress.api.port: "8080"
        tierplan.ingress.api.path: /api
        tierplan.ingress.z.host: z.example.com
        tierplan.ingress.z.port: "80"
`)
	if rv.Ingress == nil {
		t.Fatal("review has no ingress section")
	}
	var got []string
	for _, r := range rv.Ingress.Routes {
		got = append(got, fmt.Sprintf("%s %s %s %d", r.Change, r.Hostname, r.Path, r.Port))
	}
	want := []string{
		"kept a.example.com / 80",
		"added a.example.com /api 8080",
		"released a.example.com /api 80",
		"kept z.example.com / 80", // an empty held path and scheme read as / and http
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

// A route whose strip flag or name changed is not the route the stack holds: the proxy forwards it differently (strip) or
// the store records it differently (name), so the review reads it as one released and one added, never as kept (IN-08).
func TestReviewIngressRouteChangeOfStripOrName(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.api.host: a.example.com
        tierplan.ingress.api.port: "80"
        tierplan.ingress.api.path: /api
        tierplan.ingress.api.strip: "true"
`
	rows := func(held ingress.Route) []string {
		rv := reviewWith(t, &fakeIngress{held: []ingress.Route{held}}, "shop", compose)
		if rv.Ingress == nil {
			t.Fatal("review has no ingress section")
		}
		var got []string
		for _, r := range rv.Ingress.Routes {
			got = append(got, r.Change+" "+r.Hostname+" "+r.Path)
		}
		return got
	}
	same := heldRoute("api", "a.example.com", "/api", "web", 80, ingress.SchemeHTTP)
	same.Strip = true
	if got, want := rows(same), []string{"kept a.example.com /api"}; !slices.Equal(got, want) {
		t.Errorf("an unchanged route: rows = %q, want %q", got, want)
	}
	unstripped := same
	unstripped.Strip = false
	if got, want := rows(unstripped), []string{"added a.example.com /api", "released a.example.com /api"}; !slices.Equal(got, want) {
		t.Errorf("a changed strip flag: rows = %q, want %q", got, want)
	}
	renamed := same
	renamed.Name = "old"
	if got, want := rows(renamed), []string{"added a.example.com /api", "released a.example.com /api"}; !slices.Equal(got, want) {
		t.Errorf("a changed name: rows = %q, want %q", got, want)
	}
}

func TestReviewIngressEmpty(t *testing.T) {
	t.Parallel()
	// No label and no claim: no section, and no ingress problem of any kind.
	rv := reviewWith(t, &fakeIngress{}, "plain", `services:
  web:
    image: nginx:1.27
`)
	if rv.Ingress != nil {
		t.Errorf("ingress = %+v, want none for a stack with no route and no claim", rv.Ingress)
	}
	for _, p := range rv.Problems {
		if strings.HasPrefix(p.Code, "ingress_") {
			t.Errorf("problem %+v, want no ingress problem", p)
		}
	}
	// A first deploy lists every route as added.
	rv = reviewWith(t, &fakeIngress{}, "shop", routedTwo)
	if rv.Ingress == nil || len(rv.Ingress.Routes) != 2 {
		t.Fatalf("ingress = %+v, want two routes", rv.Ingress)
	}
	for _, r := range rv.Ingress.Routes {
		if r.Change != stackops.IngressRouteAdded {
			t.Errorf("row = %+v, want added on a first deploy", r)
		}
	}
	// A stack whose claims cannot be read gets no section, not a list that calls every route added.
	rv = reviewWith(t, &fakeIngress{heldErr: errors.New("db busy")}, "shop", routedTwo)
	if rv.Ingress != nil {
		t.Errorf("ingress = %+v, want none when the held routes cannot be read", rv.Ingress)
	}
}

func TestReviewIngressCertificates(t *testing.T) {
	t.Parallel()
	states := map[string]stackops.CertState{
		"a.example.com": {State: stackops.CertReused},
		"b.example.com": {State: stackops.CertWillIssue},
	}
	// A third hostname waits, with the reason the certificate store gave.
	compose := routedTwo + `        tierplan.ingress.w.host: w.example.com
        tierplan.ingress.w.port: "80"
`
	states["w.example.com"] = stackops.CertState{State: stackops.CertWaiting, Reason: "The certificate authority did not find the challenge record in DNS."}
	want := []stackops.IngressCertRow{
		{Hostname: "a.example.com", State: stackops.CertReused},
		{Hostname: "b.example.com", State: stackops.CertWillIssue},
		{Hostname: "w.example.com", State: stackops.CertWaiting, Reason: "The certificate authority did not find the challenge record in DNS."},
	}
	for name, off := range map[string]bool{"ingress on": false, "ingress off": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Certificates are queued for claims whether ingress is on or off (D-40): the rows read the same.
			r := newSwarmRig(t, nil)
			r.deps.Ingress = &fakeIngress{states: states}
			in := r.reviewInput(t, "shop", compose, nil)
			in.IngressOff = off
			rv, err := r.deps.Review(testsupport.Context(t), in)
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if rv.Ingress == nil || !slices.Equal(rv.Ingress.Certificates, want) {
				t.Errorf("certificates = %+v, want %+v", rv.Ingress, want)
			}
		})
	}
	// The reason is only for a hostname that waits.
	rv := reviewWith(t, &fakeIngress{states: map[string]stackops.CertState{"a.example.com": {State: stackops.CertReused, Reason: "x"}}}, "shop", routedTwo)
	if rv.Ingress == nil || len(rv.Ingress.Certificates) != 1 || rv.Ingress.Certificates[0].Reason != "" {
		t.Errorf("certificates = %+v, want one reused row with no reason", rv.Ingress)
	}
}

// attachedStackInput is checkedStackInput for a conversion that attaches the routed services to the ingress network.
func attachedStackInput(t *testing.T, name, compose string) stackops.SwarmDeployInput {
	t.Helper()
	files := []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}
	loaded, err := stacks.Deployable(testsupport.Context(t), stacks.CheckInput{
		Name: name, Target: stacks.TargetSwarm, Copy: stacks.WorkingCopy{Files: files}, Ingress: attachedBy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	st := stackops.Stack{ID: "stack-" + name, Name: name, Target: stacks.TargetSwarm}
	return stackops.SwarmDeployInput{
		Stack: st, Project: loaded.Project, Files: stacks.NewFileSet(files), Vars: map[string]string{}, Copy: stacks.WorkingCopy{Files: files},
	}
}

const plainWeb = `services:
  web:
    image: nginx:1.27
`

const routedWeb = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
`

func TestReviewIngressNetwork(t *testing.T) {
	t.Parallel()
	withNetwork := func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{
			ID: "net-tierplan-ingress", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm",
			Labels: map[string]string{ingress.LabelComponent: "network"},
		})
	}
	reviewOf := func(t *testing.T, r *swarmRig, compose string) stackops.Review {
		t.Helper()
		r.deps.Ingress = &fakeIngress{attach: attachedBy()}
		in := r.reviewInput(t, "shop", compose, nil)
		in.Ingress = attachedBy() // the review route reads it from AttachFor
		rv, err := r.deps.Review(testsupport.Context(t), in)
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		return rv
	}

	t.Run("a running service that gains a route joins and its tasks are replaced", func(t *testing.T) {
		t.Parallel()
		r := newSwarmRig(t, withNetwork)
		r.deployedStack(t, "shop", plainWeb, nil)
		rv := reviewOf(t, r, routedWeb)
		want := []stackops.IngressAttachRow{{Service: "web", Change: stackops.IngressAttachJoins, ReplacesTasks: true}}
		if rv.Ingress == nil || !slices.Equal(rv.Ingress.Network, want) {
			t.Errorf("network = %+v, want %+v", rv.Ingress, want)
		}
	})

	t.Run("a service that loses its last route leaves", func(t *testing.T) {
		t.Parallel()
		r := newSwarmRig(t, withNetwork)
		if res, job := r.deploy(t, attachedStackInput(t, "shop", routedWeb)); !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v %v, want it done", res, itemStates(job))
		}
		rv := reviewOf(t, r, plainWeb)
		want := []stackops.IngressAttachRow{{Service: "web", Change: stackops.IngressAttachLeaves, ReplacesTasks: true}}
		if rv.Ingress == nil || !slices.Equal(rv.Ingress.Network, want) {
			t.Errorf("network = %+v, want %+v", rv.Ingress, want)
		}
	})

	t.Run("no row when the live and the desired spec agree", func(t *testing.T) {
		t.Parallel()
		r := newSwarmRig(t, withNetwork)
		if res, job := r.deploy(t, attachedStackInput(t, "shop", routedWeb)); !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v %v, want it done", res, itemStates(job))
		}
		rv := reviewOf(t, r, routedWeb)
		if rv.Ingress != nil && len(rv.Ingress.Network) != 0 {
			t.Errorf("network = %+v, want none", rv.Ingress.Network)
		}
	})

	t.Run("a service made new joins with nothing to replace", func(t *testing.T) {
		t.Parallel()
		r := newSwarmRig(t, withNetwork)
		rv := reviewOf(t, r, routedWeb)
		want := []stackops.IngressAttachRow{{Service: "web", Change: stackops.IngressAttachJoins}}
		if rv.Ingress == nil || !slices.Equal(rv.Ingress.Network, want) {
			t.Errorf("network = %+v, want %+v", rv.Ingress, want)
		}
	})
}

func TestReviewIngressWarnings(t *testing.T) {
	t.Parallel()
	const compose = `services:
  bare:
    image: nginx:1.27
    healthcheck:
      disable: true
    deploy:
      labels:
        tierplan.ingress.host: bare.example.com
        tierplan.ingress.port: "80"
  rr:
    image: nginx:1.27
    healthcheck:
      test: ["CMD", "true"]
    deploy:
      endpoint_mode: dnsrr
      labels:
        tierplan.ingress.host: rr.example.com
        tierplan.ingress.port: "80"
  idle:
    image: nginx:1.27
    healthcheck:
      test: ["CMD", "true"]
    deploy:
      replicas: 0
      labels:
        tierplan.ingress.host: idle.example.com
        tierplan.ingress.port: "80"
  plain:
    image: nginx:1.27
    deploy:
      endpoint_mode: dnsrr
      replicas: 0
`
	r := newSwarmRig(t, nil)
	r.deps.Ingress = &fakeIngress{}
	in := r.reviewInput(t, "shop", compose, nil)
	in.IngressOff = true
	rv, err := r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	got := map[string]string{}
	for _, p := range rv.Problems {
		if !strings.HasPrefix(p.Code, "ingress_") {
			continue
		}
		if p.Severity != stacks.SeverityWarning {
			t.Errorf("problem %+v, want a warning and never an error", p)
		}
		got[p.Code+" "+strings.TrimPrefix(p.Key, "services.")] = p.Message
	}
	want := map[string]string{
		"ingress_disabled bare":       "Ingress is off for this environment. The routes will serve once it is switched on and the stack is redeployed.",
		"ingress_no_healthcheck bare": "bare has no health check, so a starting task can get traffic before it is ready.",
		"ingress_dnsrr rr":            "rr uses endpoint_mode: dnsrr, so its name resolves to task addresses and not one stable address. The proxy can keep sending traffic to a replaced task until it reconnects.",
		"ingress_no_replicas idle":    "idle has no replicas, so its route will show Backend not running.",
	}
	if !maps.Equal(got, want) {
		t.Errorf("warnings = %q, want %q", got, want)
	}

	// With ingress on there is no ingress_disabled, and the service warnings stay.
	in.IngressOff = false
	rv, err = r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if hasCode(rv.Problems, "ingress_disabled") {
		t.Errorf("problems = %+v, want no ingress_disabled with ingress on", rv.Problems)
	}
	if !hasCode(rv.Problems, "ingress_dnsrr") {
		t.Errorf("problems = %+v, want ingress_dnsrr to stay", rv.Problems)
	}
}

// The standalone review, the roll back review and the restore review carry the same section (D-22).

// standaloneRouted declares x.example.com on the service a standalone stack's labels (not deploy.labels).
const standaloneRouted = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.x.host: x.example.com
      tierplan.ingress.x.port: "80"
      tierplan.ingress.y.host: y.example.com
      tierplan.ingress.y.port: "80"
`

func TestReviewIngressStandaloneAndRollback(t *testing.T) {
	t.Parallel()
	stored := "nginx:1.27@sha256:" + strings.Repeat("a", 64)

	t.Run("the standalone review lists added and kept routes as a swarm review does, with no network rows on a host with nothing attached", func(t *testing.T) {
		t.Parallel()
		r := newStandaloneRig(t)
		r.deps.Ingress = &fakeIngress{
			held:   []ingress.Route{heldRoute("x", "x.example.com", "/", "web", 80, ingress.SchemeHTTP)},
			states: map[string]stackops.CertState{"x.example.com": {State: stackops.CertReused}, "y.example.com": {State: stackops.CertWillIssue}},
		}
		rv, err := r.deps.Review(testsupport.Context(t), r.standaloneReviewInput(t, "demo", standaloneRouted, nil))
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		if rv.Ingress == nil {
			t.Fatal("review has no ingress section")
		}
		want := []stackops.IngressRouteRow{
			{Change: stackops.IngressRouteKept, Hostname: "x.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
			{Change: stackops.IngressRouteAdded, Hostname: "y.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
		}
		if !slices.Equal(rv.Ingress.Routes, want) {
			t.Errorf("routes = %+v, want %+v", rv.Ingress.Routes, want)
		}
		if len(rv.Ingress.Certificates) != 2 {
			t.Errorf("certificates = %+v, want the two hostnames", rv.Ingress.Certificates)
		}
		if len(rv.Ingress.Network) != 0 {
			t.Errorf("network = %+v, want none on a standalone host", rv.Ingress.Network)
		}
		for _, p := range rv.Problems {
			if strings.HasPrefix(p.Code, "ingress_") {
				t.Errorf("problem %+v, want no swarm warning on a standalone host", p)
			}
		}
	})

	held := []ingress.Route{heldRoute("c", "c.example.com", "/", "web", 80, ingress.SchemeHTTP)}
	wantSwap := []stackops.IngressRouteRow{
		{Change: stackops.IngressRouteReleased, Hostname: "c.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
		{Change: stackops.IngressRouteAdded, Hostname: "x.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
	}
	for _, mode := range []string{stackops.ReviewModeRollback, stackops.ReviewModeRestore} {
		t.Run("the swarm "+mode+" review lists the revision's routes against the stack's own", func(t *testing.T) {
			t.Parallel()
			r := newSwarmRig(t, nil)
			r.deps.Ingress = &fakeIngress{held: held}
			in := r.rollbackReviewInput(t, "shop", routedWeb2, map[string]string{"shop_web": stored})
			in.Mode = mode
			rv, err := r.deps.Review(testsupport.Context(t), in)
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if rv.Ingress == nil || !slices.Equal(rv.Ingress.Routes, wantSwap) {
				t.Errorf("ingress = %+v, want routes %+v", rv.Ingress, wantSwap)
			}
		})
	}

	t.Run("the standalone roll back review does too", func(t *testing.T) {
		t.Parallel()
		r := newStandaloneRig(t)
		r.deps.Ingress = &fakeIngress{held: held}
		in := r.standaloneRollbackInput(t, "demo", standaloneRoutedX, map[string]string{"web": stored})
		rv, err := r.deps.Review(testsupport.Context(t), in)
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		want := []stackops.IngressRouteRow{
			{Change: stackops.IngressRouteReleased, Hostname: "c.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
			{Change: stackops.IngressRouteAdded, Hostname: "x.example.com", Path: "/", Service: "web", Port: 80, Scheme: "http"},
		}
		if rv.Ingress == nil || !slices.Equal(rv.Ingress.Routes, want) {
			t.Errorf("ingress = %+v, want routes %+v", rv.Ingress, want)
		}
	})
}

// routedWeb2 and standaloneRoutedX are a revision that declares x.example.com only.
const routedWeb2 = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: x.example.com
        tierplan.ingress.port: "80"
`

const standaloneRoutedX = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: x.example.com
      tierplan.ingress.port: "80"
`
