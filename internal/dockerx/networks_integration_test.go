// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// TestNetworks creates and removes a bridge network with a subnet against a real engine
// (plan 04-13, RES-06): the subnet is the one asked for, a network with a running
// container attached is refused as in use, and once the container is gone the network
// can be removed and is no longer listed.
func TestNetworks(t *testing.T) {
	host := dockertest.DockerHost(t)
	cli, err := dockerx.New(dockertest.TCPTransportFor(host))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	raw := dockertest.RawClient(t, host)
	pullImage(t, ctx, raw, "busybox:1")

	name := "bp-net-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	id, _, err := cli.NetworkCreate(ctx, dockerx.NetworkCreateSpec{
		Name: name, Driver: dockerx.DriverBridge, Subnet: "10.231.77.0/24", Gateway: "10.231.77.1",
		Labels: map[string]string{"com.docker.stack.namespace": "bp-test"},
	})
	if err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.NetworkRemove(c, id, client.NetworkRemoveOptions{})
	})

	row := func() (dockerx.NetworkRow, bool) {
		t.Helper()
		snap, err := cli.Snapshot(ctx, false)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		for _, n := range snap.Networks {
			if n.ID == id {
				return n, true
			}
		}
		return dockerx.NetworkRow{}, false
	}
	n, ok := row()
	if !ok || len(n.Subnets) != 1 || n.Subnets[0] != "10.231.77.0/24" || n.Labels["com.docker.stack.namespace"] != "bp-test" ||
		dockerx.IsBuiltinNetwork(n.Name, n.Ingress) {
		t.Fatalf("the created network is listed as %+v (%v)", n, ok)
	}

	// A running container on the network makes the engine refuse the remove.
	cname := "bp-netuser-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       cname,
		Config:     &container.Config{Image: "busybox:1", Cmd: []string{"sleep", "300"}},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(name)},
	}); err != nil {
		t.Fatalf("create the container: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, cname, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := raw.ContainerStart(ctx, cname, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start the container: %v", err)
	}
	snap, err := cli.Snapshot(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	attached := 0
	for _, c := range snap.Containers {
		for _, cn := range c.Networks {
			if cn.NetworkID == id {
				attached++
			}
		}
	}
	if attached != 1 {
		t.Fatalf("%d containers are listed on the network, want the one", attached)
	}
	err = cli.NetworkRemove(ctx, id)
	if err == nil || !dockerx.IsNetworkInUse(err) {
		t.Fatalf("NetworkRemove with a container attached = %v, want the engine's in-use refusal", err)
	}
	if _, ok := row(); !ok {
		t.Fatal("the refused remove took the network away")
	}

	if _, err := raw.ContainerRemove(ctx, cname, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove the container: %v", err)
	}
	if err := cli.NetworkRemove(ctx, id); err != nil {
		t.Fatalf("NetworkRemove: %v", err)
	}
	if _, ok := row(); ok {
		t.Fatal("the removed network is still listed")
	}
	if err := cli.NetworkRemove(ctx, id); !dockerx.IsNotFound(err) {
		t.Fatalf("a second remove = %v, want not found", err)
	}
}
