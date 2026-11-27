// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"strings"
	"sync"
	"testing"

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
