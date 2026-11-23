// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/hostpath"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// standaloneVectorName is the vector that holds the create body the agent forwards for the standalone proxy.
const standaloneVectorName = "tierplan-ingress standalone proxy container"

// fixtureTokenValue is a stand-in for a join token's clear text; it is not a real token.
const fixtureTokenValue = "tok-fixture-not-a-real-token"

// standaloneVectorInput is the input the vector was recorded from.
func standaloneVectorInput() ingressdeploy.StandaloneInput {
	return ingressdeploy.StandaloneInput{
		Image:  "ghcr.io/ianglennon/tierplan-ingress:edge",
		URL:    "wss://tierplan.example:7480/ingress/connect",
		Pin:    "sha256/AAAA",
		Subnet: "10.99.0.0/24",
		Token:  &ingressdeploy.StandaloneToken{ID: "01TOKENID", Value: fixtureTokenValue},
	}
}

// standaloneVector returns the container vector of the standalone proxy.
func standaloneVector(t *testing.T) hostpath.ContainerVector {
	t.Helper()
	for _, v := range hostpath.ContainerVectors() {
		if v.Name == standaloneVectorName {
			return v
		}
	}
	t.Fatalf("no container vector named %q", standaloneVectorName)
	return hostpath.ContainerVector{}
}

// TestStandaloneCreateBodyMatchesVector: the container create body the Compose SDK sends for StandaloneProject
// equals, in every field a container policy reads, the vector the agent forwards with no opt-in (D-03).
func TestStandaloneCreateBodyMatchesVector(t *testing.T) {
	data := t.TempDir()
	if err := dockerx.InitCompose(data, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	in := standaloneVectorInput()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Images: []dockertest.FakeImage{{ID: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RepoTags: []string{in.Image}}},
	})
	c, err := dockerx.New(fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// The fake cannot answer everything an up asks after the create, so the up may fail; the create is
	// recorded whatever happens next.
	_ = c.ComposeUp(testsupport.Context(t), ingressdeploy.StandaloneProject(in), dockerx.ComposeUpOptions{NoWait: true}, nil)

	creates := fake.ContainerCreates()
	if len(creates) != 1 {
		t.Fatalf("the up made %d container creates, want exactly one (requests: %v)", len(creates), fake.Requests())
	}
	got := creates[0]

	vec := standaloneVector(t)
	var want container.CreateRequest
	if err := json.Unmarshal(vec.Body, &want); err != nil {
		t.Fatalf("decode the vector: %v", err)
	}
	if got.HostConfig == nil || got.Config == nil {
		t.Fatalf("the create carried no config or host config: %+v", got)
	}

	cmp := func(field string, g, w any) {
		t.Helper()
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s = %#v, want %#v", field, g, w)
		}
	}
	cmp("Image", got.Config.Image, want.Config.Image)
	cmp("User", got.Config.User, want.Config.User)
	cmp("HostConfig.PortBindings", got.HostConfig.PortBindings, want.HostConfig.PortBindings)
	cmp("HostConfig.RestartPolicy", got.HostConfig.RestartPolicy, want.HostConfig.RestartPolicy)
	cmp("HostConfig.Mounts", got.HostConfig.Mounts, want.HostConfig.Mounts)
	cmp("HostConfig.LogConfig", got.HostConfig.LogConfig, want.HostConfig.LogConfig)
	cmp("HostConfig.NetworkMode", got.HostConfig.NetworkMode, want.HostConfig.NetworkMode)
	cmp("HostConfig.Privileged", got.HostConfig.Privileged, want.HostConfig.Privileged)
	cmp("HostConfig.CapAdd", got.HostConfig.CapAdd, want.HostConfig.CapAdd)
	cmp("HostConfig.Devices", got.HostConfig.Devices, want.HostConfig.Devices)
	cmp("HostConfig.Binds", got.HostConfig.Binds, want.HostConfig.Binds)
	cmp("HostConfig.PidMode", got.HostConfig.PidMode, want.HostConfig.PidMode)
	cmp("HostConfig.IpcMode", got.HostConfig.IpcMode, want.HostConfig.IpcMode)
	cmp("HostConfig.VolumesFrom", got.HostConfig.VolumesFrom, want.HostConfig.VolumesFrom)

	// The TierPlan labels and the compose project and service labels are the vector's; the container may carry
	// more (the SDK's config hash, the token label), never fewer.
	for k, v := range want.Config.Labels {
		if gv, ok := got.Config.Labels[k]; !ok || gv != v {
			t.Errorf("label %s = %q (present %v), want %q", k, gv, ok, v)
		}
	}
	if got.Config.Labels[ingressdeploy.TokenLabel] != in.Token.ID {
		t.Errorf("label %s = %q, want the token ID %q", ingressdeploy.TokenLabel, got.Config.Labels[ingressdeploy.TokenLabel], in.Token.ID)
	}

	// The environment is compared by name, and by value for the three the vector holds.
	gotEnv := envMap(got.Config.Env)
	wantEnv := envMap(want.Config.Env)
	for k, v := range wantEnv {
		if gv, ok := gotEnv[k]; !ok || gv != v {
			t.Errorf("env %s = %q (present %v), want %q", k, gv, ok, v)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(gotEnv)) {
		if _, ok := wantEnv[k]; ok {
			continue
		}
		// Whatever the SDK or the image adds is not the proxy's own setting; the proxy's names are the vector's.
		if strings.HasPrefix(k, "TIERPLAN_INGRESS_") {
			t.Errorf("env %s is a proxy setting the vector does not have", k)
		}
	}
	if _, ok := gotEnv[ingressbundle.EnvNodeID]; ok {
		t.Errorf("env %s is set on a standalone proxy", ingressbundle.EnvNodeID)
	}
}

// envMap splits "NAME=value" entries.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

// onlyService returns the project's one service, failing when there is not exactly one.
func onlyService(t *testing.T, p *types.Project) types.ServiceConfig {
	t.Helper()
	if len(p.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(p.Services))
	}
	s, ok := p.Services[ingress.ProxyComposeService]
	if !ok {
		t.Fatalf("no service %q in %v", ingress.ProxyComposeService, slices.Collect(maps.Keys(p.Services)))
	}
	return s
}

// TestStandaloneProjectShape: every value of the project the plan pins, and nothing more privileged than a
// plain container (PROH-25-01).
func TestStandaloneProjectShape(t *testing.T) {
	in := standaloneVectorInput()
	p := ingressdeploy.StandaloneProject(in)

	if p.Name != "tierplan-ingress" {
		t.Errorf("project = %q, want tierplan-ingress", p.Name)
	}
	s := onlyService(t, p)
	if s.Name != ingress.ProxyComposeService {
		t.Errorf("service Name = %q, want %q", s.Name, ingress.ProxyComposeService)
	}
	if s.Image != in.Image {
		t.Errorf("Image = %q, want %q", s.Image, in.Image)
	}
	if s.User != "1000:1000" {
		t.Errorf("User = %q, want 1000:1000", s.User)
	}
	if s.Restart != "unless-stopped" {
		t.Errorf("Restart = %q, want unless-stopped", s.Restart)
	}
	if s.ReadOnly {
		t.Error("ReadOnly is true: the SDK refuses a secret in a read-only service (A-2, D-25)")
	}

	wantPorts := []types.ServicePortConfig{
		{Target: 8080, Published: "80", Protocol: "tcp"},
		{Target: 8443, Published: "443", Protocol: "tcp"},
	}
	if !reflect.DeepEqual(s.Ports, wantPorts) {
		t.Errorf("Ports = %+v, want %+v", s.Ports, wantPorts)
	}

	if len(s.Volumes) != 1 {
		t.Fatalf("Volumes = %+v, want exactly the cache volume", s.Volumes)
	}
	if v := s.Volumes[0]; v.Type != types.VolumeTypeVolume || v.Target != "/var/lib/tierplan-ingress" || v.Bind != nil {
		t.Errorf("volume mount = %+v, want a named volume at /var/lib/tierplan-ingress", v)
	}
	for _, v := range s.Volumes {
		if strings.Contains(v.Source, "docker.sock") || v.Type == types.VolumeTypeBind {
			t.Errorf("mount %+v is a bind or a socket", v)
		}
	}

	// A plain container: nothing that gives host access.
	if len(s.CapAdd) != 0 || s.Privileged || s.NetworkMode != "" || s.Pid != "" || s.Ipc != "" || len(s.Devices) != 0 || len(s.VolumesFrom) != 0 {
		t.Errorf("the service asks for more than a plain container: CapAdd=%v Privileged=%v NetworkMode=%q Pid=%q Ipc=%q Devices=%v VolumesFrom=%v",
			s.CapAdd, s.Privileged, s.NetworkMode, s.Pid, s.Ipc, s.Devices, s.VolumesFrom)
	}

	if s.HealthCheck == nil {
		t.Fatal("no health check")
	}
	if !reflect.DeepEqual([]string(s.HealthCheck.Test), []string{"CMD", "/usr/local/bin/tierplan-ingress", "healthcheck"}) {
		t.Errorf("health check test = %v", s.HealthCheck.Test)
	}
	durs := map[string]struct {
		got  *types.Duration
		want time.Duration
	}{
		"interval":     {s.HealthCheck.Interval, ingressbundle.HealthcheckInterval},
		"timeout":      {s.HealthCheck.Timeout, ingressbundle.HealthcheckTimeout},
		"start period": {s.HealthCheck.StartPeriod, ingressbundle.HealthcheckStartPeriod},
		"stop grace":   {s.StopGracePeriod, ingressbundle.StopGracePeriod},
	}
	for name, d := range durs {
		if d.got == nil || time.Duration(*d.got) != d.want {
			t.Errorf("%s = %v, want %v", name, d.got, d.want)
		}
	}
	if s.HealthCheck.Retries == nil || *s.HealthCheck.Retries != uint64(ingressbundle.HealthcheckRetries) {
		t.Errorf("health check retries = %v, want %d", s.HealthCheck.Retries, ingressbundle.HealthcheckRetries)
	}

	if s.Logging == nil || s.Logging.Driver != "json-file" || s.Logging.Options["max-size"] != "10m" || s.Logging.Options["max-file"] != "3" || len(s.Logging.Options) != 2 {
		t.Errorf("Logging = %+v, want json-file max-size 10m max-file 3", s.Logging)
	}

	// The network.
	if _, ok := s.Networks["ingress"]; !ok || len(s.Networks) != 1 {
		t.Errorf("service networks = %v, want only the project's ingress network", slices.Collect(maps.Keys(s.Networks)))
	}
	if len(p.Networks) != 1 {
		t.Fatalf("networks = %d, want 1", len(p.Networks))
	}
	n := p.Networks["ingress"]
	if n.Name != "tierplan-ingress" || n.Driver != "bridge" || n.Internal || n.Attachable || bool(n.External) {
		t.Errorf("network = %+v, want the bridge tierplan-ingress, not internal", n)
	}
	if len(n.Ipam.Config) != 1 || n.Ipam.Config[0].Subnet != in.Subnet {
		t.Errorf("network IPAM = %+v, want the one pool %s", n.Ipam, in.Subnet)
	}
	if noSubnet := ingressdeploy.StandaloneProject(ingressdeploy.StandaloneInput{Image: in.Image}); len(noSubnet.Networks["ingress"].Ipam.Config) != 0 {
		t.Errorf("an empty Subnet still made an IPAM pool: %+v", noSubnet.Networks["ingress"].Ipam)
	}

	// The volume.
	if len(p.Volumes) != 1 {
		t.Fatalf("volumes = %d, want 1", len(p.Volumes))
	}
	if v := p.Volumes["cache"]; v.Name != "tierplan-ingress-cache" {
		t.Errorf("volume name = %q, want tierplan-ingress-cache", v.Name)
	}
	if ingressdeploy.StandaloneNetworkPrefix != 24 {
		t.Errorf("StandaloneNetworkPrefix = %d, want 24", ingressdeploy.StandaloneNetworkPrefix)
	}

	// The SDK finds the project's containers by the compose labels, which a typed project has to bring itself.
	for k, v := range map[string]string{
		"com.docker.compose.project": "tierplan-ingress",
		"com.docker.compose.service": "proxy",
		"com.docker.compose.oneoff":  "False",
	} {
		if s.CustomLabels[k] != v {
			t.Errorf("custom label %s = %q, want %q", k, s.CustomLabels[k], v)
		}
	}

	// The labels are on all three, and a fresh map each: changing one changes no other.
	want := ingress.ObjectLabels()
	labels := map[string]map[string]string{"service": s.Labels, "network": n.Labels, "volume": p.Volumes["cache"].Labels}
	for name, l := range labels {
		if !reflect.DeepEqual(map[string]string(l), want) {
			t.Errorf("%s labels = %v, want %v", name, l, want)
		}
	}
	s.Labels["probe"] = "x"
	if _, leaked := n.Labels["probe"]; leaked {
		t.Error("the service's labels and the network's are one map")
	}
	if _, leaked := p.Volumes["cache"].Labels["probe"]; leaked {
		t.Error("the service's labels and the volume's are one map")
	}
	n.Labels["probe2"] = "y"
	if _, leaked := p.Volumes["cache"].Labels["probe2"]; leaked {
		t.Error("the network's labels and the volume's are one map")
	}
}

// TestStandaloneProjectEnv: the environment with the access log off and on, and never the node ID.
func TestStandaloneProjectEnv(t *testing.T) {
	in := standaloneVectorInput()
	for _, tc := range []struct {
		name      string
		accessLog bool
	}{{"access log off", false}, {"access log on", true}} {
		t.Run(tc.name, func(t *testing.T) {
			in := in
			in.AccessLog = tc.accessLog
			s := onlyService(t, ingressdeploy.StandaloneProject(in))
			got := map[string]string{}
			for k, v := range s.Environment {
				if v == nil {
					t.Fatalf("env %s has no value", k)
				}
				got[k] = *v
			}
			want := map[string]string{
				"TIERPLAN_INGRESS_URL":             in.URL,
				"TIERPLAN_INGRESS_PIN":             in.Pin,
				"TIERPLAN_INGRESS_JOIN_TOKEN_FILE": "/run/secrets/tierplan-ingress-token",
			}
			if tc.accessLog {
				want["TIERPLAN_INGRESS_ACCESS_LOG"] = "1"
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("environment = %v, want %v", got, want)
			}
			if _, ok := got["TIERPLAN_INGRESS_NODE_ID"]; ok {
				t.Error("TIERPLAN_INGRESS_NODE_ID is set on a standalone proxy")
			}
		})
	}
}

// TestStandaloneProjectSecret: the token is an environment-sourced secret and its clear text is nowhere in the
// project but project.Environment (T-25-18).
func TestStandaloneProjectSecret(t *testing.T) {
	in := standaloneVectorInput()
	p := ingressdeploy.StandaloneProject(in)
	s := onlyService(t, p)

	if len(p.Secrets) != 1 {
		t.Fatalf("secrets = %d, want 1", len(p.Secrets))
	}
	sec := p.Secrets["token"]
	if sec.Environment != "TIERPLAN_SECRET_token" || sec.File != "" || sec.Content != "" || bool(sec.External) {
		t.Errorf("secret = %+v, want only Environment TIERPLAN_SECRET_token", sec)
	}
	if len(s.Secrets) != 1 {
		t.Fatalf("service secrets = %+v, want 1", s.Secrets)
	}
	ref := s.Secrets[0]
	if ref.Source != "token" || ref.Target != "/run/secrets/tierplan-ingress-token" || ref.UID != "" || ref.GID != "" || ref.Mode != nil {
		t.Errorf("service secret = %+v, want source token at the token file with no UID, GID or Mode", ref)
	}
	if got := s.CustomLabels[ingressdeploy.TokenLabel]; got != in.Token.ID {
		t.Errorf("custom label %s = %q, want %q", ingressdeploy.TokenLabel, got, in.Token.ID)
	}
	if ingressdeploy.TokenLabel != "tierplan.ingress.token" {
		t.Errorf("TokenLabel = %q", ingressdeploy.TokenLabel)
	}
	if _, inLabels := s.Labels[ingressdeploy.TokenLabel]; inLabels {
		t.Error("the token label is in Labels, so it is inside the SDK's config hash; it must be a custom label (D-26)")
	}
	if got := p.Environment["TIERPLAN_SECRET_token"]; got != fixtureTokenValue {
		t.Errorf("project.Environment holds %q, want the token's clear text", got)
	}

	// The clear text is nowhere else: marshal the project without its Environment and look for it.
	bare := *p
	bare.Environment = nil
	raw, err := json.Marshal(&bare)
	if err != nil {
		t.Fatalf("marshal the project: %v", err)
	}
	if strings.Contains(string(raw), fixtureTokenValue) {
		t.Error("the token's clear text is in the project's JSON form outside Environment")
	}
	// The service's own fields, including those json leaves out, hold it nowhere either.
	if strings.Contains(strings.Join(s.Command, " ")+strings.Join(s.Entrypoint, " "), fixtureTokenValue) {
		t.Error("the token's clear text is in the service command")
	}
	for k, v := range s.Environment {
		if v != nil && strings.Contains(*v, fixtureTokenValue) {
			t.Errorf("the token's clear text is in the service environment %s", k)
		}
	}
	for k, v := range s.Labels {
		if strings.Contains(v, fixtureTokenValue) {
			t.Errorf("the token's clear text is in label %s", k)
		}
	}

	// With no token: no secret, no token label, no secret variable.
	in.Token = nil
	p = ingressdeploy.StandaloneProject(in)
	s = onlyService(t, p)
	if len(p.Secrets) != 0 || len(s.Secrets) != 0 {
		t.Errorf("a project without a token has secrets: %v, %v", p.Secrets, s.Secrets)
	}
	if _, ok := s.CustomLabels[ingressdeploy.TokenLabel]; ok {
		t.Error("a project without a token has a token label")
	}
	if _, ok := p.Environment["TIERPLAN_SECRET_token"]; ok {
		t.Error("a project without a token has a secret variable")
	}
}
