// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"golang.org/x/crypto/bcrypt"
)

const (
	// RegistryImage is the registry the fixtures run. It is loaded from this
	// machine's engine into each node, so no node reaches a public registry.
	RegistryImage = "registry:2"
	// registryHost is how every node reaches its registry: dind trusts a
	// 127.0.0.0/8 registry over plain HTTP by default.
	registryHost = "localhost:5000"
	// swarmRegistryHost is how every swarm node reaches the ingress-published
	// registry. It is the IPv4 loopback, not "localhost": Engine 24's routing mesh
	// answers 127.0.0.1:5000 but leaves [::1]:5000 hanging, and its daemon dials
	// "localhost" as ::1, so a push or login there times out. 127.0.0.0/8 is still
	// trusted over plain HTTP by default.
	swarmRegistryHost = "127.0.0.1:5000"

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
// published through the ingress mesh, so every node reaches it at 127.0.0.1:5000
// (swarmRegistryHost), and waits until every node does.
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
	return swarmRegistryHost
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
	return pushTagged(t, node, target, login)
}

// pushTagged pushes target, an image the node already holds under that name, signing in
// first when login is given, and returns the digest the registry answered with.
func pushTagged(t testing.TB, node *Node, target string, login *Login) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if login != nil {
		host, _, _ := strings.Cut(target, "/")
		out, err := node.Sh(ctx, fmt.Sprintf("echo '%s' | docker login -u '%s' --password-stdin %s", login.Password, login.User, host))
		if err != nil {
			t.Fatalf("sign in to %s on %s: %v\n%s", host, node.Hostname, err, out)
		}
	}
	// A push to the fixture registry goes through the node's own port 5000 (the routing
	// mesh on a swarm). Wait until that answers /v2/ from this node, so the push never
	// starts against a mesh that is not answering yet; the push itself runs once.
	if host, _, _ := strings.Cut(target, "/"); host == registryHost || host == swarmRegistryHost {
		Wait(t, 2*time.Minute, "the registry to answer /v2/ from "+node.Hostname+" before the push", func() (bool, string) {
			return registryUp(ctx, node, RegistryOptions{})
		})
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

// HtpasswdFor returns the content of an htpasswd file with one user, hashed with bcrypt as the
// registry requires. It is for test setup only: Bayplan never writes one.
func HtpasswdFor(user, password string) []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		// Only a password over 72 bytes fails, and a fixture's is never that long.
		panic("dockertest: bcrypt: " + err.Error())
	}
	return []byte(user + ":" + string(hash) + "\n")
}

// Logout signs the node's Docker client out of the fixture registry and removes its client
// configuration, so what follows proves a pull needs nothing the node holds: PushImage signs
// in only to push.
func Logout(t testing.TB, node *Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := node.Sh(ctx, "docker logout "+registryHost+" >/dev/null 2>&1; docker logout "+swarmRegistryHost+" >/dev/null 2>&1; rm -f \"$HOME/.docker/config.json\""); err != nil {
		t.Fatalf("sign out on %s: %v\n%s", node.Hostname, err, out)
	}
}

// HasClientConfig reports whether the node has a Docker client configuration file, which is
// where a docker login would have written a credential.
func HasClientConfig(t testing.TB, node *Node) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := node.Sh(ctx, "if [ -e \"$HOME/.docker/config.json\" ]; then echo present; else echo absent; fi")
	if err != nil {
		t.Fatalf("look for the client configuration on %s: %v\n%s", node.Hostname, err, out)
	}
	return strings.TrimSpace(out) == "present"
}

// CreateServiceWithAuth creates a one-replica service of image that runs only on a worker,
// through the manager's own client, with encodedAuth as the registry header of the create (the
// way a stack deploy will send the resolver's header), and returns its ID. The task sleeps, so
// it stays running. A worker that has never signed in to the registry can only start it if the
// engine carried the header from the create to that worker.
func CreateServiceWithAuth(t testing.TB, manager *Node, name, image, encodedAuth string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	one := uint64(1)
	res, err := manager.Client(t).ServiceCreate(ctx, client.ServiceCreateOptions{
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: name},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{Image: image, Command: []string{"sleep", "3600"}},
				Placement:     &swarm.Placement{Constraints: []string{"node.role==worker"}},
			},
			Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
		},
		EncodedRegistryAuth: encodedAuth,
	})
	if err != nil {
		// The error is the engine's and never holds the header.
		t.Fatalf("create the service %s on %s: %v", name, manager.Hostname, err)
	}
	return res.ID
}

// InspectServiceRaw is the engine's JSON answer to inspecting a service, for a test that checks
// what it does and does not hold.
func InspectServiceRaw(t testing.TB, manager *Node, id string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := manager.Client(t).ServiceInspect(ctx, id, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("inspect the service %s on %s: %v", id, manager.Hostname, err)
	}
	return string(res.Raw)
}
