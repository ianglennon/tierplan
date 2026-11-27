// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The addresses of the self route tests.
const (
	srDomain = "apps.example.org"
	srHost   = "tp.apps.example.org"
	srPublic = "https://tierplan.example.org:7480"
	srAgent  = "https://tierplan.internal:7480"
)

// selfRouteKnobs are what a self route test turns after the rig is built: the instance's URLs, the certificate manager's
// answer and whether TierPlan's own certificate renews over HTTP-01. They start off, so the rig's own enable sees none.
type selfRouteKnobs struct {
	mu     sync.Mutex
	urls   ingressdeploy.SelfURLs
	cert   *ingressdeploy.HostCertState
	http01 bool
	// local, when set, is what TierPlan's own engine says of its swarm membership, and localErr a probe that fails.
	local    *envs.SwarmMembership
	localErr error
}

func (k *selfRouteKnobs) set(fn func(*selfRouteKnobs)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	fn(k)
}

func (k *selfRouteKnobs) selfURLs() ingressdeploy.SelfURLs {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.urls
}

func (k *selfRouteKnobs) hostCert(context.Context, string, string) (ingressdeploy.HostCertState, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cert == nil {
		return ingressdeploy.HostCertState{}, nil
	}
	return *k.cert, nil
}

func (k *selfRouteKnobs) ownHTTP01(context.Context) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.http01, nil
}

// newSelfRouteRig is an enabled swarm rig in which TierPlan runs as tierplan_tierplan of the stack tierplan, not on the self
// network, with apps.example.org registered, the public and agent URLs set and TierPlan's own certificate on HTTP-01.
func newSelfRouteRig(t *testing.T, o selfRigOptions) (*rig, *selfFacts, *selfRouteKnobs, ingress.Domain) {
	t.Helper()
	k := &selfRouteKnobs{}
	inner := o.deps
	o.deps = func(d *ingressdeploy.Deps) {
		base := d.LocalSwarm
		d.LocalSwarm = func(ctx context.Context) (envs.SwarmMembership, error) {
			k.mu.Lock()
			local, localErr := k.local, k.localErr
			k.mu.Unlock()
			switch {
			case localErr != nil:
				return envs.SwarmMembership{}, localErr
			case local != nil:
				return *local, nil
			}
			return base(ctx)
		}
		d.SelfURLs = k.selfURLs
		d.HostCertificate = k.hostCert
		d.OwnHTTP01InUse = k.ownHTTP01
		if inner != nil {
			inner(d)
		}
	}
	r, facts := newSelfRig(t, o)
	dom, err := r.istore.CreateDomain(testsupport.Context(t), ingressstore.CreateDomainInput{EnvID: r.envID, Suffix: srDomain, Issuer: ingress.IssuerStaging, Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	k.set(func(k *selfRouteKnobs) {
		k.urls = ingressdeploy.SelfURLs{PublicURL: srPublic, AgentURL: srAgent, ListenPort: 7480}
		k.http01 = true
	})
	return r, facts, k, dom
}

// wantItems checks the codes and states of a preview's items, in order.
func wantItems(t *testing.T, pv ingressdeploy.SelfRoutePreview, want ...[2]string) {
	t.Helper()
	got := make([][2]string, 0, len(pv.Items))
	for _, it := range pv.Items {
		got = append(got, [2]string{it.Code, it.State})
	}
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("items = %v, want %v", got, want)
		}
	}
}

// stackFileLines are UI-SPEC S1a's stack file lines for the service tierplan, line for line.
var stackFileLines = []string{
	"services:",
	"  tierplan:",
	"    networks:",
	"      - tierplan-self",
	"networks:",
	"  tierplan-self:",
	"    external: true",
}

// TestPreviewSelfRouteOrderedItems: a free hostname under a registered domain, with TierPlan a swarm service of a stack that
// is not on the self network, no certificate yet and TierPlan's own certificate on HTTP-01, gives the five items in D-17's
// order, the stack file snippet and the agent URL as the direct address.
func TestPreviewSelfRouteOrderedItems(t *testing.T) {
	r, _, _, _ := newSelfRouteRig(t, selfRigOptions{})
	pv, err := r.svc.PreviewSelfRoute(testsupport.Context(t), r.envID, ingressdeploy.SelfRouteRequest{Hostname: "TP.Apps.Example.org"})
	if err != nil {
		t.Fatalf("PreviewSelfRoute: %v", err)
	}
	wantItems(t, pv,
		[2]string{ingressdeploy.ItemSelfHostname, ingressdeploy.SelfItemOK},
		[2]string{ingressdeploy.ItemSelfCertificate, ingressdeploy.SelfItemWarning},
		[2]string{ingressdeploy.ItemSelfNetwork, ingressdeploy.SelfItemWarning},
		[2]string{ingressdeploy.ItemSelfOrigin, ingressdeploy.SelfItemOK},
		[2]string{ingressdeploy.ItemSelfCarveOut, ingressdeploy.SelfItemOK},
	)
	if pv.Host != srHost {
		t.Errorf("Host = %q, want %q", pv.Host, srHost)
	}
	if pv.Direct != srAgent {
		t.Errorf("Direct = %q, want %q", pv.Direct, srAgent)
	}
	if pv.Refused() {
		t.Error("the preview refuses, want it to go ahead")
	}
	msgs := map[string]string{
		ingressdeploy.ItemSelfHostname:    srHost + " is free and under " + srDomain + ".",
		ingressdeploy.ItemSelfCertificate: "No certificate yet. TierPlan orders one when you turn the route on. " + srHost + " isn't served until it is ready.",
		ingressdeploy.ItemSelfOrigin:      "TierPlan will also accept requests from https://" + srHost + ". Your public URL isn't changed. Cookies are separate, so you sign in on each address.",
		ingressdeploy.ItemSelfCarveOut: "TierPlan's own certificate uses the HTTP-01 challenge. The ingress sends only /.well-known/acme-challenge/ requests for " +
			"tierplan.example.org and tierplan.internal to TierPlan, over tierplan-self, so it keeps renewing.",
	}
	for _, it := range pv.Items {
		if want, ok := msgs[it.Code]; ok && it.Message != want {
			t.Errorf("%s message = %q, want %q", it.Code, it.Message, want)
		}
		if it.Code != ingressdeploy.ItemSelfNetwork && it.Snippet != nil {
			t.Errorf("%s carries a snippet", it.Code)
		}
	}
	net := pv.Items[2]
	if net.Reason != ingressdeploy.CodeSelfNotAttached {
		t.Errorf("network reason = %q, want %q", net.Reason, ingressdeploy.CodeSelfNotAttached)
	}
	if net.Snippet == nil || net.Snippet.Kind != ingressdeploy.SelfSnippetStackFile {
		t.Fatalf("network snippet = %+v, want a stack_file snippet", net.Snippet)
	}
	if got := strings.Split(strings.TrimSuffix(net.Snippet.Text, "\n"), "\n"); strings.Join(got, "\n") != strings.Join(stackFileLines, "\n") {
		t.Errorf("snippet lines = %q, want UI-SPEC S1a's %q", got, stackFileLines)
	}
	if net.Snippet.Text != ingressdeploy.StackFileSnippet("tierplan") {
		t.Error("the snippet is not StackFileSnippet(\"tierplan\")")
	}
}

// TestPreviewSelfRouteCreatesNothing: a preview makes no network, no claim and no route (T-28-44).
func TestPreviewSelfRouteCreatesNothing(t *testing.T) {
	r, _, _, _ := newSelfRouteRig(t, selfRigOptions{})
	ctx := testsupport.Context(t)
	before, err := r.cli.Networks(ctx)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	for range 2 {
		if _, err := r.svc.PreviewSelfRoute(ctx, r.envID, ingressdeploy.SelfRouteRequest{Hostname: srHost}); err != nil {
			t.Fatalf("PreviewSelfRoute: %v", err)
		}
	}
	after, err := r.cli.Networks(ctx)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("networks went from %d to %d", len(before), len(after))
	}
	if _, ok := r.selfNetwork(); ok {
		t.Error("the preview made tierplan-self")
	}
	if _, held, err := r.istore.SystemClaim(ctx); err != nil || held {
		t.Errorf("SystemClaim: held %v, err %v, want none", held, err)
	}
	w, err := r.istore.World(ctx, r.envID, nil)
	if err != nil {
		t.Fatalf("World: %v", err)
	}
	if len(w.Claims) != 0 {
		t.Errorf("claims = %v, want none", w.Claims)
	}
}

// fivePlain are the item codes of a switch-on preview with the carve-out, in order.
var fivePlain = []string{
	ingressdeploy.ItemSelfHostname, ingressdeploy.ItemSelfCertificate, ingressdeploy.ItemSelfNetwork, ingressdeploy.ItemSelfOrigin,
	ingressdeploy.ItemSelfCarveOut,
}

// TestPreviewSelfRouteRefusals: every reason the route cannot be switched on is told in the item it belongs to, with a stable
// code; exactly one item refuses in each case, and the other items are still listed in their order (D-01, D-05, D-07, D-09,
// D-10, Pitfalls 9 and 10).
func TestPreviewSelfRouteRefusals(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		setup    func(t *testing.T, r *rig, facts *selfFacts, k *selfRouteKnobs, dom ingress.Domain)
		item     string
		code     string
		contains string
	}{
		{name: "under no registered domain", host: "tp.other.example.net",
			item: ingressdeploy.ItemSelfHostname, code: ingress.CodeDomainNotAllowed, contains: "not under a domain registered"},
		{name: "reserved", setup: func(t *testing.T, r *rig, _ *selfFacts, _ *selfRouteKnobs, dom ingress.Domain) {
			if _, _, err := r.istore.Reserve(testsupport.Context(t), ingressstore.ReserveInput{EnvID: r.envID, DomainID: dom.ID, Hostname: srHost, Actor: "admin"}, nil); err != nil {
				t.Fatalf("Reserve: %v", err)
			}
		}, item: ingressdeploy.ItemSelfHostname, code: ingress.CodeHostReserved, contains: "reserved by an Admin"},
		{name: "held by a stack", setup: func(t *testing.T, r *rig, _ *selfFacts, _ *selfRouteKnobs, _ ingress.Domain) {
			if _, err := r.st.Write.Exec(`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('st1', ?, 'web', 'swarm', 'tester', 1)`, r.envID); err != nil {
				t.Fatalf("insert stack: %v", err)
			}
			decl := ingress.Declaration{Service: "web", Host: srHost, Label: "tierplan.ingress.host", Port: 80}
			if _, err := r.istore.ReplaceStackRoutes(testsupport.Context(t), ingressstore.ReplaceInput{EnvID: r.envID, StackID: "st1", Actor: "admin", Decls: []ingress.Declaration{decl}}); err != nil {
				t.Fatalf("ReplaceStackRoutes: %v", err)
			}
		}, item: ingressdeploy.ItemSelfHostname, code: ingress.CodeHostTaken, contains: "already held"},
		{name: "the route is already on", setup: func(t *testing.T, r *rig, _ *selfFacts, _ *selfRouteKnobs, dom ingress.Domain) {
			if err := r.istore.SetSystemClaim(testsupport.Context(t), ingressstore.SystemClaimInput{
				EnvID: r.envID, Hostname: "old.apps.example.org", DomainID: dom.ID, Service: tpServiceName, Port: 7480, Actor: "admin",
			}, nil); err != nil {
				t.Fatalf("SetSystemClaim: %v", err)
			}
		}, item: ingressdeploy.ItemSelfHostname, code: ingressdeploy.CodeSelfRouteAlreadyOn, contains: "already on"},
		{name: "the agent URL's host", setup: func(_ *testing.T, _ *rig, _ *selfFacts, k *selfRouteKnobs, _ ingress.Domain) {
			k.set(func(k *selfRouteKnobs) { k.urls.AgentURL = "https://" + srHost + ":7480" })
		}, item: ingressdeploy.ItemSelfHostname, code: ingress.CodeHostIsAgentURL, contains: "host of your agent URL"},
		{name: "terms not accepted", setup: func(_ *testing.T, _ *rig, _ *selfFacts, k *selfRouteKnobs, _ ingress.Domain) {
			k.set(func(k *selfRouteKnobs) { k.cert = &ingressdeploy.HostCertState{TermsNeeded: true} })
		}, item: ingressdeploy.ItemSelfCertificate, code: ingressdeploy.CodeSelfCertificateTerms, contains: "terms are accepted"},
		{name: "certificate work paused", setup: func(_ *testing.T, _ *rig, _ *selfFacts, k *selfRouteKnobs, _ ingress.Domain) {
			k.set(func(k *selfRouteKnobs) { k.cert = &ingressdeploy.HostCertState{Known: true, Paused: true} })
		}, item: ingressdeploy.ItemSelfCertificate, code: ingressdeploy.CodeSelfCertificatePaused, contains: "paused after a restore"},
		{name: "TierPlan runs elsewhere", setup: func(_ *testing.T, _ *rig, _ *selfFacts, k *selfRouteKnobs, _ ingress.Domain) {
			k.set(func(k *selfRouteKnobs) {
				k.local = &envs.SwarmMembership{Known: true, Member: true, ClusterID: "cluster-other", NodeID: "node-9", EngineID: "engine-9"}
			})
		}, item: ingressdeploy.ItemSelfNetwork, code: ingressdeploy.CodeSelfRouteNotHere, contains: "TierPlan"},
		{name: "where TierPlan runs cannot be told", setup: func(_ *testing.T, _ *rig, _ *selfFacts, k *selfRouteKnobs, _ ingress.Domain) {
			k.set(func(k *selfRouteKnobs) { k.localErr = errors.New("no socket") })
		}, item: ingressdeploy.ItemSelfNetwork, code: ingressdeploy.CodeSelfRouteNotHere, contains: "can't tell which environment"},
		{name: "a plain container on the swarm", setup: func(_ *testing.T, _ *rig, facts *selfFacts, _ *selfRouteKnobs, _ ingress.Domain) {
			facts.mu.Lock()
			facts.facts = ingressdeploy.SelfFacts{ContainerID: "c2", Networks: []string{"bridge"}}
			facts.mu.Unlock()
		}, item: ingressdeploy.ItemSelfNetwork, code: ingressdeploy.CodeSelfRouteNeedsService, contains: "install-in-swarm.md"},
		{name: "a public URL with a path", setup: func(_ *testing.T, _ *rig, _ *selfFacts, k *selfRouteKnobs, _ ingress.Domain) {
			k.set(func(k *selfRouteKnobs) { k.urls.PublicURL = srPublic + "/tierplan" })
		}, item: ingressdeploy.ItemSelfOrigin, code: ingressdeploy.CodeSelfRouteBasePath, contains: "/tierplan"},
		{name: "no URL at all", setup: func(_ *testing.T, _ *rig, _ *selfFacts, k *selfRouteKnobs, _ ingress.Domain) {
			k.set(func(k *selfRouteKnobs) { k.urls = ingressdeploy.SelfURLs{} })
		}, item: ingressdeploy.ItemSelfOrigin, code: ingressdeploy.CodeSelfRouteNoURL, contains: "public URL"},
		{name: "a locked public URL on the route host and no agent URL", setup: func(_ *testing.T, _ *rig, _ *selfFacts, k *selfRouteKnobs, _ ingress.Domain) {
			k.set(func(k *selfRouteKnobs) {
				k.urls = ingressdeploy.SelfURLs{PublicURL: "https://" + srHost + ":7480", PublicLocked: true, ListenPort: 7480}
			})
		}, item: ingressdeploy.ItemSelfOrigin, code: ingress.CodeHostIsAgentURL, contains: "TIERPLAN_PUBLIC_URL"},
		{name: "ingress off", setup: func(t *testing.T, r *rig, _ *selfFacts, _ *selfRouteKnobs, _ ingress.Domain) {
			if _, err := r.istore.SetEnabled(testsupport.Context(t), r.envID, false, "admin", nil); err != nil {
				t.Fatalf("SetEnabled: %v", err)
			}
		}, item: ingressdeploy.ItemSelfHostname, code: ingressdeploy.CodeVIPIngressOff, contains: "Turn ingress on"},
	}
	if len(cases) < 14 {
		t.Fatalf("the table has %d cases, want at least 14", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, facts, k, dom := newSelfRouteRig(t, selfRigOptions{})
			if c.setup != nil {
				c.setup(t, r, facts, k, dom)
			}
			host := c.host
			if host == "" {
				host = srHost
			}
			pv, err := r.svc.PreviewSelfRoute(testsupport.Context(t), r.envID, ingressdeploy.SelfRouteRequest{Hostname: host})
			if err != nil {
				t.Fatalf("PreviewSelfRoute: %v", err)
			}
			var codes []string
			var refused []ingressdeploy.SelfRouteItem
			for _, it := range pv.Items {
				codes = append(codes, it.Code)
				if it.State == ingressdeploy.SelfItemRefused {
					refused = append(refused, it)
				}
			}
			if strings.Join(codes, ",") != strings.Join(fivePlain, ",") {
				t.Errorf("items = %v, want %v", codes, fivePlain)
			}
			if len(refused) != 1 {
				t.Fatalf("refused items = %+v, want exactly one", refused)
			}
			got := refused[0]
			if got.Code != c.item || got.Reason != c.code {
				t.Errorf("refused %s with %q, want %s with %q", got.Code, got.Reason, c.item, c.code)
			}
			if !strings.Contains(got.Message, c.contains) {
				t.Errorf("message %q lacks %q", got.Message, c.contains)
			}
			if !pv.Refused() {
				t.Error("Refused() is false")
			}
		})
	}
}

// TestPreviewSelfRouteStandaloneNote: on a standalone host TierPlan runs on, TierPlan connects its own container, so the
// self-network item passes with the standalone sentence and no snippet (D-04).
func TestPreviewSelfRouteStandaloneNote(t *testing.T) {
	r := newRig(t, rigOptions{standalone: true, deps: func(d *ingressdeploy.Deps) {
		d.Self = func(context.Context) (ingressdeploy.SelfFacts, error) {
			return ingressdeploy.SelfFacts{ContainerID: "c9", Networks: []string{"bridge"}}, nil
		}
		d.SelfURLs = func() ingressdeploy.SelfURLs { return ingressdeploy.SelfURLs{PublicURL: srPublic} }
	}})
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Local = true })
	ctx := testsupport.Context(t)
	if _, err := r.istore.SetEnabled(ctx, r.envID, true, "admin", nil); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if _, err := r.istore.CreateDomain(ctx, ingressstore.CreateDomainInput{EnvID: r.envID, Suffix: srDomain, Issuer: ingress.IssuerStaging, Actor: "admin"}, nil); err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	pv, err := r.svc.PreviewSelfRoute(ctx, r.envID, ingressdeploy.SelfRouteRequest{Hostname: srHost})
	if err != nil {
		t.Fatalf("PreviewSelfRoute: %v", err)
	}
	wantItems(t, pv,
		[2]string{ingressdeploy.ItemSelfHostname, ingressdeploy.SelfItemOK},
		[2]string{ingressdeploy.ItemSelfCertificate, ingressdeploy.SelfItemWarning},
		[2]string{ingressdeploy.ItemSelfNetwork, ingressdeploy.SelfItemOK},
		[2]string{ingressdeploy.ItemSelfOrigin, ingressdeploy.SelfItemOK},
	)
	net := pv.Items[2]
	if net.Message != "TierPlan connects its own container to tierplan-self." || net.Snippet != nil {
		t.Errorf("network item = %+v, want the standalone sentence and no snippet", net)
	}
	if pv.Direct != srPublic {
		t.Errorf("Direct = %q, want the public URL's origin %q", pv.Direct, srPublic)
	}
}

// TestPreviewSelfRouteServiceUpdateSnippet: a swarm service with no stack namespace gets the service update command with its
// full name (D-03, UI-SPEC A-04).
func TestPreviewSelfRouteServiceUpdateSnippet(t *testing.T) {
	r, facts, _, _ := newSelfRouteRig(t, selfRigOptions{})
	facts.mu.Lock()
	facts.facts.StackNamespace = ""
	facts.facts.ServiceName = "tierplan-svc"
	facts.mu.Unlock()
	pv, err := r.svc.PreviewSelfRoute(testsupport.Context(t), r.envID, ingressdeploy.SelfRouteRequest{Hostname: srHost})
	if err != nil {
		t.Fatalf("PreviewSelfRoute: %v", err)
	}
	net := pv.Items[2]
	want := ingressdeploy.SelfSnippet{Kind: ingressdeploy.SelfSnippetServiceUpdate, Text: "docker service update --network-add tierplan-self tierplan-svc"}
	if net.Code != ingressdeploy.ItemSelfNetwork || net.Snippet == nil || *net.Snippet != want {
		t.Errorf("network item = %+v, want the snippet %+v", net, want)
	}
	if got := ingressdeploy.ServiceUpdateCommand("odd name;rm"); got != "docker service update --network-add tierplan-self 'odd name;rm'" {
		t.Errorf("an odd name is not quoted: %q", got)
	}
}
