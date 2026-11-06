// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

func containerWriteFake(t *testing.T, cfg dockertest.FakeConfig) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
		ID: "c1", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 1 hour",
	})
	cfg.Networks = append(cfg.Networks, dockertest.FakeNetwork{ID: "n1", Name: "front", Driver: "bridge", Scope: "local"})
	fake := dockertest.NewFake(t, cfg)
	return newClient(t, fake), fake
}

func createOnce(c *dockerx.Client, name string) error {
	_, _, err := c.ContainerCreate(context.Background(), name, &container.Config{Image: "nginx:1.28"}, &container.HostConfig{}, nil)
	return err
}

func TestContainerWriteSendsOnceAndKeepsTheEnginesAnswer(t *testing.T) {
	c, fake := containerWriteFake(t, dockertest.FakeConfig{})
	ctx := context.Background()
	id, _, err := c.ContainerCreate(ctx, "web-new", &container.Config{Image: "nginx:1.28", Labels: map[string]string{"a": "b"}}, &container.HostConfig{},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"front": {Aliases: []string{"w"}}}})
	if err != nil || id == "" {
		t.Fatalf("ContainerCreate = %q, %v", id, err)
	}
	if err := c.ContainerRename(ctx, id, "web-renamed"); err != nil {
		t.Fatalf("ContainerRename: %v", err)
	}
	creates := fake.ContainerCreates()
	if len(creates) != 1 || creates[0].Name != "web-new" || creates[0].Config.Labels["a"] != "b" ||
		creates[0].NetworkingConfig.EndpointsConfig["front"].Aliases[0] != "w" {
		t.Errorf("creates = %+v", creates)
	}
	if got := fake.Writes(); len(got) != 2 {
		t.Errorf("writes = %v, want one create and one rename and no retry", got)
	}
	det, err := c.ContainerInspectFull(ctx, id)
	if err != nil || det.ID != id || det.Name != "/web-renamed" || det.Config.Image != "nginx:1.28" {
		t.Errorf("inspect of the new container = %+v, %v", det, err)
	}
	if _, err := c.ContainerInspectFull(ctx, "nope"); !dockerx.IsNotFound(err) {
		t.Errorf("inspect of an unknown container = %v, want not found", err)
	}
}

func TestContainerWriteErrors(t *testing.T) {
	t.Run("a name in use is a conflict and nothing is created", func(t *testing.T) {
		c, fake := containerWriteFake(t, dockertest.FakeConfig{})
		err := createOnce(c, "web")
		if !dockerx.IsConflict(err) {
			t.Fatalf("create under a name in use = %v, want a conflict", err)
		}
		if got := fake.ContainerCreates(); len(got) != 0 {
			t.Errorf("creates = %+v, want none", got)
		}
	})
	t.Run("a bad configuration is a refusal", func(t *testing.T) {
		c, _ := containerWriteFake(t, dockertest.FakeConfig{RefuseCreate: map[string]dockertest.FakeRefusal{
			"bad": {Status: http.StatusBadRequest, Message: "invalid mount config for type \"bind\": bind source path does not exist: /secret/path"},
		}})
		err := createOnce(c, "bad")
		if !dockerx.IsRefusal(err) || dockerx.IsConflict(err) || dockerx.IsPolicyDenied(err) {
			t.Fatalf("create with a bad configuration = %v, want a refusal that is neither a conflict nor a policy denial", err)
		}
	})
	t.Run("an agent's policy refusal is a policy denial", func(t *testing.T) {
		c, _ := containerWriteFake(t, dockertest.FakeConfig{RefuseCreate: map[string]dockertest.FakeRefusal{
			"denied": {Status: http.StatusForbidden, Message: "agent_policy_denied: POST /containers/create"},
		}})
		err := createOnce(c, "denied")
		if !dockerx.IsPolicyDenied(err) || dockerx.IsConflict(err) {
			t.Fatalf("create refused by the agent = %v, want a policy denial", err)
		}
		c, _ = containerWriteFake(t, dockertest.FakeConfig{PolicyDeny: true})
		if err := c.ContainerRename(context.Background(), "c1", "x"); !dockerx.IsPolicyDenied(err) {
			t.Errorf("rename refused by the agent = %v, want a policy denial", err)
		}
		if err := c.NetworkConnectEndpoint(context.Background(), "front", "c1", nil); !dockerx.IsPolicyDenied(err) {
			t.Errorf("connect refused by the agent = %v, want a policy denial", err)
		}
	})
	t.Run("an engine that is not an agent's, with a plain 403, is not a policy denial", func(t *testing.T) {
		c, _ := containerWriteFake(t, dockertest.FakeConfig{RefuseCreate: map[string]dockertest.FakeRefusal{
			"plain": {Status: http.StatusForbidden, Message: "authorization denied by plugin"},
		}})
		if err := createOnce(c, "plain"); dockerx.IsPolicyDenied(err) || err == nil {
			t.Errorf("create with a plain 403 = %v, want an error that is not a policy denial", err)
		}
	})
	t.Run("a rename onto a name in use is a conflict and an unknown container is not found", func(t *testing.T) {
		c, fake := containerWriteFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{
			{ID: "c2", Names: []string{"/db"}, Image: "postgres:17", State: "running"},
		}})
		ctx := context.Background()
		if err := c.ContainerRename(ctx, "c1", "db"); !dockerx.IsConflict(err) {
			t.Errorf("rename onto a name in use = %v, want a conflict", err)
		}
		if err := c.ContainerRename(ctx, "gone", "x"); !dockerx.IsNotFound(err) {
			t.Errorf("rename of an unknown container = %v, want not found", err)
		}
		if got := fake.ContainerRenames(); len(got) != 0 {
			t.Errorf("renames = %+v, want none", got)
		}
	})
	t.Run("a connect to an unknown network or by an unknown container is not found", func(t *testing.T) {
		c, _ := containerWriteFake(t, dockertest.FakeConfig{})
		ctx := context.Background()
		if err := c.NetworkConnectEndpoint(ctx, "nowhere", "c1", nil); !dockerx.IsNotFound(err) {
			t.Errorf("connect to an unknown network = %v, want not found", err)
		}
		if err := c.NetworkConnectEndpoint(ctx, "front", "gone", nil); !dockerx.IsNotFound(err) {
			t.Errorf("connect of an unknown container = %v, want not found", err)
		}
	})
	t.Run("an engine error is wrapped with the node and keeps its type", func(t *testing.T) {
		c, _ := containerWriteFake(t, dockertest.FakeConfig{WriteError: "boom"})
		err := createOnce(c, "x")
		if err == nil || !strings.Contains(err.Error(), "create container on") {
			t.Fatalf("create = %v, want an error naming the call", err)
		}
		if dockerx.IsConflict(err) || dockerx.IsNotFound(err) || dockerx.IsRefusal(err) {
			t.Errorf("a server error %v is not a refusal, a conflict or a missing object", err)
		}
	})
	t.Run("nothing is written by a read", func(t *testing.T) {
		c, fake := containerWriteFake(t, dockertest.FakeConfig{})
		if _, err := c.ContainerInspectFull(context.Background(), "c1"); err != nil {
			t.Fatal(err)
		}
		if w := fake.Writes(); len(w) != 0 {
			t.Errorf("a read wrote: %v", w)
		}
	})
}
