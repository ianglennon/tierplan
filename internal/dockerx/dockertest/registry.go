// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	// RegistryImage is the registry the fixtures run. It is loaded from this
	// machine's engine into each node, so no node reaches a public registry.
	RegistryImage = "registry:2"
	// registryHost is how every node reaches its registry: dind trusts a
	// 127.0.0.0/8 registry over plain HTTP by default.
	registryHost = "localhost:5000"

	registryName = "bp-registry"
	registryDir  = "/tmp/bp-registry"
)

// RegistryOptions turns a fixture registry's authentication and transport on.
type RegistryOptions struct {
	// Htpasswd, when set, is the content of an htpasswd file (bcrypt) the registry
	// requires a sign-in against.
	Htpasswd []byte
	// TLSCert and TLSKey, when both set, make the registry serve HTTPS.
	TLSCert, TLSKey []byte
}

// Login is a user and password for a fixture registry. Only test setup signs in
// with it, on the node that pushes; Bayplan never does.
type Login struct {
	User, Password string
}

// env is the registry container's configuration, with the files it names already
// written to the node.
func (o RegistryOptions) env() []string {
	var env []string
	if len(o.Htpasswd) > 0 {
		env = append(env,
			"REGISTRY_AUTH=htpasswd",
			"REGISTRY_AUTH_HTPASSWD_REALM=bayplan-test",
			"REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd")
	}
	if len(o.TLSCert) > 0 && len(o.TLSKey) > 0 {
		env = append(env,
			"REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt",
			"REGISTRY_HTTP_TLS_KEY=/certs/tls.key")
	}
	return env
}

func (o RegistryOptions) tls() bool { return len(o.TLSCert) > 0 && len(o.TLSKey) > 0 }

// writeFiles puts the htpasswd and certificate files in the node.
func (o RegistryOptions) writeFiles(ctx context.Context, n *Node) error {
	if _, err := n.Exec(ctx, "mkdir", "-p", registryDir); err != nil {
		return err
	}
	if len(o.Htpasswd) > 0 {
		if err := n.WriteFile(ctx, registryDir+"/htpasswd", o.Htpasswd); err != nil {
			return err
		}
	}
	if o.tls() {
		if err := n.WriteFile(ctx, registryDir+"/tls.crt", o.TLSCert); err != nil {
			return err
		}
		if err := n.WriteFile(ctx, registryDir+"/tls.key", o.TLSKey); err != nil {
			return err
		}
	}
	return nil
}

// registryUp reports whether the registry on the node answers. A sign-in wall
// (401) still counts: something answered /v2/.
func registryUp(ctx context.Context, n *Node, o RegistryOptions) (bool, string) {
	line := "wget -S -O /dev/null -T 3 http://127.0.0.1:5000/v2/ 2>&1 | grep -q '^ *HTTP/'"
	if o.tls() {
		line = "nc -z 127.0.0.1 5000"
	}
	out, err := n.Sh(ctx, line)
	if err != nil {
		return false, strings.TrimSpace(out + " " + err.Error())
	}
	return true, ""
}

// StartRegistry loads registry:2 into the node, runs it published on
// 127.0.0.1:5000 of that node (the htpasswd file and certificate are written and
// mounted when o gives them), waits until /v2/ answers, and returns the address
// the node and its engine reach it at: "localhost:5000".
func StartRegistry(t testing.TB, node *Node, o RegistryOptions) string {
	t.Helper()
	LoadImage(t, RegistryImage, node)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := o.writeFiles(ctx, node); err != nil {
		t.Fatalf("write the registry files: %v", err)
	}
	args := []string{"docker", "run", "-d", "--name", registryName, "-p", "127.0.0.1:5000:5000"}
	for _, e := range o.env() {
		args = append(args, "-e", e)
	}
	if len(o.Htpasswd) > 0 {
		args = append(args, "-v", registryDir+"/htpasswd:/auth/htpasswd:ro")
	}
	if o.tls() {
		args = append(args, "-v", registryDir+"/tls.crt:/certs/tls.crt:ro", "-v", registryDir+"/tls.key:/certs/tls.key:ro")
	}
	args = append(args, RegistryImage)
	if out, err := node.Exec(ctx, args...); err != nil {
		t.Fatalf("start the registry on %s: %v\n%s", node.Hostname, err, out)
	}
	Wait(t, 90*time.Second, "the registry on "+node.Hostname+" to answer /v2/", func() (bool, string) {
		return registryUp(ctx, node, o)
	})
	return registryHost
}

// StartSwarmRegistry runs a one-replica registry:2 service with port 5000
// published through the ingress mesh, so every node reaches it at localhost:5000,
// and waits until every node does.
func StartSwarmRegistry(t testing.TB, s *Swarm, o RegistryOptions) string {
	t.Helper()
	LoadImage(t, RegistryImage, s.Nodes()...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	mgr := s.Managers[0]
	// The service is pinned to the manager that holds the files, since a bind mount
	// of a file written on one node means nothing on another.
	if err := o.writeFiles(ctx, mgr); err != nil {
		t.Fatalf("write the registry files: %v", err)
	}
	args := []string{
		"docker", "service", "create", "--name", registryName, "--replicas", "1",
		"--constraint", "node.hostname==" + mgr.Hostname,
		"--publish", "published=5000,target=5000", "--no-resolve-image", "--detach",
	}
	for _, e := range o.env() {
		args = append(args, "--env", e)
	}
	if len(o.Htpasswd) > 0 {
		args = append(args, "--mount", "type=bind,source="+registryDir+"/htpasswd,target=/auth/htpasswd,readonly")
	}
	if o.tls() {
		args = append(args,
			"--mount", "type=bind,source="+registryDir+"/tls.crt,target=/certs/tls.crt,readonly",
			"--mount", "type=bind,source="+registryDir+"/tls.key,target=/certs/tls.key,readonly")
	}
	args = append(args, RegistryImage)
	if out, err := mgr.Exec(ctx, args...); err != nil {
		t.Fatalf("create the registry service: %v\n%s", err, out)
	}
	for _, n := range s.Nodes() {
		Wait(t, 3*time.Minute, "the registry to answer /v2/ on "+n.Hostname, func() (bool, string) {
			return registryUp(ctx, n, o)
		})
	}
	return registryHost
}

// PushImage copies src (from this machine's engine, pulled there first when it is
// missing) into the node, tags it as target and pushes it from that node. It signs
// in on that node only when login is given: this is test setup, never Bayplan. It
// returns the digest the registry answered with.
func PushImage(t testing.TB, node *Node, src, target string, login *Login) string {
	t.Helper()
	LoadImage(t, src, node)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := node.Exec(ctx, "docker", "tag", src, target); err != nil {
		t.Fatalf("tag %s as %s: %v\n%s", src, target, err, out)
	}
	if login != nil {
		host, _, _ := strings.Cut(target, "/")
		out, err := node.Sh(ctx, fmt.Sprintf("echo '%s' | docker login -u '%s' --password-stdin %s", login.Password, login.User, host))
		if err != nil {
			t.Fatalf("sign in to %s on %s: %v\n%s", host, node.Hostname, err, out)
		}
	}
	out, err := node.Exec(ctx, "docker", "push", target)
	if err != nil {
		t.Fatalf("push %s from %s: %v\n%s", target, node.Hostname, err, out)
	}
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "sha256:") {
			return f
		}
	}
	t.Fatalf("push %s printed no digest:\n%s", target, out)
	return ""
}
