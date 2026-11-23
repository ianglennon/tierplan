// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// shopCompose is the stack shop: one routed web, with an environment value when mode is not empty.
func (e *hostEnv) shopCompose(host, mode string) string {
	out := "services:\n" + e.routedService("web", webBody("shop"), host)
	if mode != "" {
		out += "    environment:\n      MODE: " + mode + "\n"
	}
	return out
}

// requireInSync fails the test unless the stack's drift check reads in sync.
func (e *hostEnv) requireInSync(st *stack, when string) {
	e.t.Helper()
	if res := e.drift(st); res.State != stackops.DriftNone || len(res.Services) != 0 {
		var names []string
		for _, s := range res.Services {
			names = append(names, s.Change+" "+s.Name)
		}
		e.t.Fatalf("drift of %s %s = %s %v, want none", st.Name, when, res.State, names)
	}
}

// requireDeployed fails the test unless the deploy ended well, naming the job's items when it did not.
func requireDeployed(t *testing.T, name string, res stackops.DeployResult, job jobs.Job) {
	t.Helper()
	if res.OK() {
		return
	}
	requireJobDone(t, job)
	t.Fatalf("the deploy of %s ended %+v", name, res)
}

// TestStandaloneStackLifecycleOnIngress is SC 2's lifecycle on a real standalone host (D-16, D-17, D-18, D-22). A routed stack
// deployed while ingress was off reads in sync once it is on, and its review lists web joining the ingress network with its
// container recreated. After the deploy the route is served and drift reads in sync; a change deploys, a roll back to the first
// served revision keeps the route served; removing the stack releases its claim and the route stops being served.
func TestStandaloneStackLifecycleOnIngress(t *testing.T) {
	e := startHostEnv(t, false)
	e.startRegistry()
	host := "shop." + testDomain

	// Deployed while ingress is off: the claim is stored, the container is not on the ingress network.
	shop, res, job := e.deployStack("shop", e.shopCompose(host, ""))
	requireDeployed(t, "shop", res, job)
	if nets := e.networksOf("shop-web-1"); slices.Contains(nets, ingress.NetworkName) {
		t.Fatalf("shop-web-1 is on networks %v while ingress is off", nets)
	}
	e.requireInSync(shop, "deployed with ingress off")

	requireJobDone(t, e.enable())
	e.awaitReady()
	e.requireInSync(shop, "after ingress is switched on (the attachment is TierPlan's)")

	// The review of the same file says what the deploy will do to the running container.
	rv := e.review(shop)
	want := []stackops.IngressAttachRow{{Service: "web", Change: stackops.IngressAttachJoins, ReplacesTasks: true}}
	if rv.Ingress == nil || !slices.Equal(rv.Ingress.Network, want) {
		t.Fatalf("the review's ingress section is %+v, want web joining %s with its container replaced (%+v)", rv.Ingress, ingress.NetworkName, want)
	}

	// The deploy attaches it: the route is served from the stack's own web and drift still reads in sync.
	e.uploadCertificate(host)
	shop, res, job = e.redeploy(shop, e.shopCompose(host, ""))
	requireDeployed(t, "shop", res, job)
	if page := e.awaitServed(host); !strings.Contains(page, webBody("shop")) {
		t.Fatalf("%s answered %q, want the stack's web", host, strings.TrimSpace(page))
	}
	if nets := e.networksOf("shop-web-1"); !slices.Contains(nets, ingress.NetworkName) {
		t.Errorf("shop-web-1 is on networks %v after the deploy, want %s among them", nets, ingress.NetworkName)
	}
	e.requireInSync(shop, "after the deploy")
	good, ok, err := e.repo.LatestGoodRevision(testsupport.Context(t), shop.ID)
	if err != nil || !ok {
		t.Fatalf("read the deployed revision: %v (found %v)", err, ok)
	}
	servedSeq := good.Seq

	// A change deploys and is served; the environment value is what the container holds.
	shop, res, job = e.redeploy(shop, e.shopCompose(host, "v2"))
	requireDeployed(t, "shop", res, job)
	e.awaitServed(host)
	if got := strings.TrimSpace(e.sh("docker exec shop-web-1 printenv MODE")); got != "v2" {
		t.Errorf("shop-web-1 has MODE=%q after the change, want v2", got)
	}
	e.requireInSync(shop, "after the change")

	// Rolling back to the first served revision puts that revision's container back, and the route is still served.
	shop, res, job = e.rollback(shop, servedSeq)
	requireDeployed(t, "shop", res, job)
	e.awaitServed(host)
	if out, err := e.host.Sh(testsupport.Context(t), "docker exec shop-web-1 printenv MODE"); err == nil {
		t.Errorf("shop-web-1 still has MODE=%q after the roll back to revision %d, want it unset", strings.TrimSpace(out), servedSeq)
	}
	if held := e.claimsOf(shop); !slices.Equal(held, []string{host}) {
		t.Errorf("shop holds %v after the roll back, want %s", held, host)
	}
	e.requireInSync(shop, "after the roll back")

	// Removing the stack releases its claim, and the proxy stops serving the name.
	if r := e.remove(shop); !r.OK() {
		t.Fatalf("remove = %+v", r)
	}
	if held := e.claimsOf(shop); len(held) != 0 {
		t.Errorf("a removed stack holds %v, want nothing", held)
	}
	e.awaitNotServed(host, webBody("shop"))
}

// TestStandaloneDisableOnHost is SC 3 and D-20 on a real standalone host. Switching ingress off is refused while a stack holds its
// route, with nothing changed. Once the stack is gone a container of someone else's on the ingress network stops the switch-off at
// the network item, naming it, and the steps after it are not run; once it is gone a second switch-off finishes, and the host
// holds no proxy container, no ingress network and no certificate cache volume (PROH-25-06).
func TestStandaloneDisableOnHost(t *testing.T) {
	e := startHostEnv(t, false)
	e.startRegistry()
	requireJobDone(t, e.enable())
	e.awaitReady()
	host := "closing." + testDomain
	closing, res, job := e.deployStack("closing", "services:\n"+e.routedService("web", webBody("closing"), host))
	requireDeployed(t, "closing", res, job)

	// Refused while the route is held, and the host is as it was.
	_, err := e.deploy.Disable(testsupport.Context(t), testActor, testOwner, e.envID, func(string) bool { return true })
	var refused *ingressdeploy.DisableRefusedError
	if !errors.As(err, &refused) || len(refused.Findings) != 1 || refused.Findings[0].Code != ingressdeploy.CodeRoutesLive {
		t.Fatalf("Disable with a live route returned %v, want a refusal with the one finding %s", err, ingressdeploy.CodeRoutesLive)
	}
	if got := e.hostObjects(); !slices.Equal(got, []string{"container", "network", "volume"}) {
		t.Fatalf("after the refusal the host holds %v, want the proxy container, the network and the volume", got)
	}
	if st, err := e.deploy.Status(testsupport.Context(t), e.envID); err != nil || !st.Enabled {
		t.Fatalf("status after the refusal = %+v (err %v), want ingress still on", st, err)
	}

	if r := e.remove(closing); !r.OK() {
		t.Fatalf("remove = %+v", r)
	}

	// A container of someone else's on the ingress network: the switch-off names it and does not go on.
	e.sh("docker run -d --name stray --network " + ingress.NetworkName + " " + e.webImage + " sleep 600")
	failed := e.disable()
	if got := itemStateOf(failed, ingressdeploy.ItemNetwork); got != "failed:"+ingressdeploy.CodeDisableNetworkFailed {
		t.Fatalf("the network item of the switch-off with a stray container is %q, want failed:%s (items %+v)", got, ingressdeploy.CodeDisableNetworkFailed, failed.Items)
	}
	for _, it := range failed.Items {
		if it.ID == ingressdeploy.ItemNetwork && !strings.Contains(it.Detail, "stray") {
			t.Errorf("the network item's detail is %q, want it to name the container stray", it.Detail)
		}
	}
	for _, id := range []string{ingressdeploy.ItemVolumes, ingressdeploy.ItemToken, ingressdeploy.ItemNodes} {
		if got := itemStateOf(failed, id); !strings.HasPrefix(got, jobs.ItemSkipped+":") {
			t.Errorf("item %s of the failed switch-off is %q, want it not run", id, got)
		}
	}
	// The key cache is still there: it is the last thing to go.
	if got := e.hostObjects(); !slices.Contains(got, "volume") {
		t.Errorf("after the failed switch-off the host holds %v, want the volume kept for the next attempt", got)
	}

	// Without the stray container the same switch-off finishes, and nothing of the proxy's is left.
	e.sh("docker rm -f stray")
	requireJobDone(t, e.disable())
	if got := e.hostObjects(); len(got) != 0 {
		t.Errorf("after the switch-off the host holds %v, want no proxy container, no ingress network and no cache volume", got)
	}
}

// hostObjects are the kinds of the proxy's objects the host holds: container (of the proxy's compose project), network and
// volume, in that order.
func (e *hostEnv) hostObjects() []string {
	e.t.Helper()
	var got []string
	if out := strings.TrimSpace(e.sh("docker ps -aq --filter label=com.docker.compose.project=" + ingress.ProjectName)); out != "" {
		got = append(got, "container")
	}
	if out := strings.TrimSpace(e.sh("docker network ls -q --filter name=^" + ingress.NetworkName + "$")); out != "" {
		got = append(got, "network")
	}
	if out := strings.TrimSpace(e.sh("docker volume ls -q --filter name=^" + ingress.VolumeName + "$")); out != "" {
		got = append(got, "volume")
	}
	return got
}
