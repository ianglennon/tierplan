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

	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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
