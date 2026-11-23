// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"regexp"
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
