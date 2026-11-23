// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// TestStandaloneEnableOnAgentHost is SC 1 on a real engine, through the agent path: on one dind host reached through a tunnel
// test agent the deployer's Enable ends done, the status reads one node enrolled and ready, and the host holds what the
// proxy's project asks for: the container with its restart policy, its user and its two ports, the secret file the token is
// read from, the bridge network with a /24 and the certificate cache volume.
func TestStandaloneEnableOnAgentHost(t *testing.T) {
	e := startHostEnv(t, false)
	requireJobDone(t, e.enable())
	st := e.awaitReady()
	if !st.Enabled {
		t.Errorf("the status reads ingress off after a successful enable: %+v", st)
	}
	e.requireProxyObjects()
}

// TestStandaloneEnableOnLocalSocket is SC 1 through the local socket (D-01, D-02): the same host, but the environment is
// TierPlan's own local socket, whose engine is the dind host's. Enable ends done and the proxy enrols into the standalone slot, which
// the local environment holds with no swarm node ID, and reads ready.
func TestStandaloneEnableOnLocalSocket(t *testing.T) {
	e := startHostEnv(t, true)
	requireJobDone(t, e.enable())
	e.awaitReady()
	e.requireProxyObjects()
}

// TestTwoWebStacksServedOnStandalone is SC 2 on the data plane of a standalone host (D-15, D-27): zoo and blog each have a routed
// service called web, and zoo has a routed api too. After ingress is on and both are deployed, each hostname on the host's 443
// answers with the test CA's certificate from its own stack's web, and from zoo's api, which is on both zoo_default and the ingress
// network (whose name sorts before zoo_default's), the name web reaches zoo's web and never blog's. No container of either
// stack carries a name on the ingress network but its own stack-and-service alias.
func TestTwoWebStacksServedOnStandalone(t *testing.T) {
	e := startHostEnv(t, false)
	e.startRegistry()
	requireJobDone(t, e.enable())
	e.awaitReady()

	zooHost, blogHost, apiHost := "zoo."+testDomain, "blog."+testDomain, "api.zoo."+testDomain
	zoo, res, job := e.deployStack("zoo", "services:\n"+e.routedService("web", webBody("zoo"), zooHost)+e.routedService("api", apiBody("zoo"), apiHost))
	if !res.OK() {
		requireJobDone(t, job)
		t.Fatalf("the deploy of zoo ended %+v", res)
	}
	blog, res, job := e.deployStack("blog", "services:\n"+e.routedService("web", webBody("blog"), blogHost))
	if !res.OK() {
		requireJobDone(t, job)
		t.Fatalf("the deploy of blog ended %+v", res)
	}
	if held := e.claimsOf(zoo); !slices.Equal(held, []string{apiHost, zooHost}) {
		t.Fatalf("zoo holds %v, want %v", held, []string{apiHost, zooHost})
	}
	if held := e.claimsOf(blog); !slices.Equal(held, []string{blogHost}) {
		t.Fatalf("blog holds %v, want %s", held, blogHost)
	}

	// Each hostname is served by its own stack's web, over a certificate of the test CA.
	e.uploadCertificate(zooHost)
	e.uploadCertificate(blogHost)
	if page := e.awaitServed(zooHost); !strings.Contains(page, webBody("zoo")) || strings.Contains(page, webBody("blog")) {
		t.Errorf("%s answered %q, want zoo's web (%s)", zooHost, strings.TrimSpace(page), webBody("zoo"))
	}
	if page := e.awaitServed(blogHost); !strings.Contains(page, webBody("blog")) || strings.Contains(page, webBody("zoo")) {
		t.Errorf("%s answered %q, want blog's web (%s)", blogHost, strings.TrimSpace(page), webBody("blog"))
	}

	// The api sits on zoo_default and on the ingress network, and its own network sorts after the ingress network's: the order in
	// which embedded DNS was seen to pick the shared network. The name web is still its own stack's web (T-25-40, PROH-25-05).
	if nets := e.networksOf("zoo-api-1"); !slices.Equal(nets, []string{"tierplan-ingress", "zoo_default"}) {
		t.Fatalf("zoo's api is on networks %v, want tierplan-ingress and zoo_default (sorted)", nets)
	}
	if got := strings.TrimSpace(e.sh("docker exec zoo-api-1 wget -q -O- http://web/")); got != webBody("zoo") {
		t.Errorf("from zoo's api the name web answered %q, want %q: the name must reach zoo's own web and never blog's", got, webBody("zoo"))
	}
	if got := strings.TrimSpace(e.sh("docker exec blog-web-1 wget -q -O- http://web/")); got != webBody("blog") {
		t.Errorf("from blog's web the name web answered %q, want %q", got, webBody("blog"))
	}

	// On the ingress network a routed service has the one alias that names its stack and service, and no other name: neither
	// the bare service name nor the container's name (PROH-25-05).
	for _, c := range []struct{ container, stack, service string }{
		{"zoo-web-1", "zoo", "web"}, {"zoo-api-1", "zoo", "api"}, {"blog-web-1", "blog", "web"},
	} {
		if got := e.aliasesOn(c.container, ingress.NetworkName); !slices.Equal(got, []string{ingress.Alias(c.stack, c.service)}) {
			t.Errorf("%s has the aliases %v on %s, want exactly [%s]", c.container, got, ingress.NetworkName, ingress.Alias(c.stack, c.service))
		}
	}
	// Its own default network still knows it by its service name.
	if got := e.aliasesOn("zoo-web-1", "zoo_default"); !slices.Contains(got, "web") {
		t.Errorf("zoo-web-1 has the aliases %v on zoo_default, want web among them", got)
	}
}

// requireProxyObjects asserts what a standalone host holds once the proxy's project is up.
func (e *hostEnv) requireProxyObjects() {
	t := e.t
	t.Helper()
	c := e.proxy()
	if got := c.HostConfig.RestartPolicy.Name; got != "unless-stopped" {
		t.Errorf("the proxy's restart policy is %q, want unless-stopped", got)
	}
	if c.Config.User != "1000:1000" {
		t.Errorf("the proxy runs as %q, want 1000:1000", c.Config.User)
	}
	// The proxy listens on 8080 and 8443 inside (it runs as a user that may not bind below 1024) and the host's 80 and 443 reach them.
	published := map[string]bool{}
	for _, bindings := range c.HostConfig.PortBindings {
		for _, b := range bindings {
			published[b.HostPort] = true
		}
	}
	if len(c.HostConfig.PortBindings) != 2 || !published["80"] || !published["443"] {
		t.Errorf("the proxy's port bindings are %+v, want exactly the host's ports 80 and 443", c.HostConfig.PortBindings)
	}
	if _, ok := c.NetworkSettings.Networks[ingress.NetworkName]; !ok {
		t.Errorf("the proxy is on networks %v, want %s", keysOf(c.NetworkSettings.Networks), ingress.NetworkName)
	}
	// The join token is a secret file, readable by the proxy's own user and owned by root. The image has no stat, so the file's
	// header is read from the archive the engine makes of it.
	header := strings.TrimSpace(e.sh("docker cp " + ingress.ProjectName + "-" + ingress.ProxyComposeService + "-1:/run/secrets/tierplan-ingress-token - | tar -tvf -"))
	if !tokenFileHeader.MatchString(header) {
		t.Errorf("the token file's header is %q, want mode 444 (-r--r--r--) owned by 0:0", header)
	}
	subnet := strings.TrimSpace(e.sh("docker network inspect " + ingress.NetworkName + " --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'"))
	if !strings.HasSuffix(subnet, "/24") {
		t.Errorf("the ingress network's subnet is %q, want a /24", subnet)
	}
	vol := strings.Fields(e.sh("docker volume ls --filter name=^" + ingress.VolumeName + "$ --format '{{.Name}} {{.Label \"" + ingress.LabelComponent + "\"}}'"))
	if len(vol) != 2 || vol[0] != ingress.VolumeName || vol[1] != ingress.ComponentIngress {
		t.Errorf("the cache volume as listed is %v, want %s labelled %s", vol, ingress.VolumeName, ingress.ComponentIngress)
	}
}

// tokenFileHeader is a tar listing of a file of mode 444 owned by user 0 and group 0, as busybox prints it with or without names.
var tokenFileHeader = regexp.MustCompile(`^-r--r--r--\s+(0/0|root/root|0\s+0)\s`)

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
