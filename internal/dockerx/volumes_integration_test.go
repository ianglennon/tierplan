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

// runToExit runs a throwaway container to the end and returns its exit code, with the
// binds it was given. The container is removed when the test ends.
func runToExit(t *testing.T, ctx context.Context, raw *client.Client, binds []string, script string) int64 {
	t.Helper()
	name := "bp-vol-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       name,
		Config:     &container.Config{Image: "busybox:1", Cmd: []string{"sh", "-c", script}},
		HostConfig: &container.HostConfig{Binds: binds},
	}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, name, client.ContainerRemoveOptions{Force: true})
	})
	wait := raw.ContainerWait(ctx, name, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := raw.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	select {
	case res := <-wait.Result:
		return res.StatusCode
	case err := <-wait.Error:
		t.Fatalf("wait for %s: %v", name, err)
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
	}
	return -1
}

// TestVolumeCreateHostFolder creates a volume with the host-folder options the create
// dialog writes (type none, o bind, device the folder) against a real engine (plan
// 04-16, RES-07, D-44): the engine has the volume with those options, VolumeExists sees
// it, and a container that mounts the volume sees the files in the folder.
func TestVolumeCreateHostFolder(t *testing.T) {
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

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	folder := "/tmp/bp-hostfolder-" + suffix
	// The folder is on the engine's host, which can be another machine than this one, so
	// it is made there: a bind of a folder that does not exist makes it, and a file in it
	// is what the volume must show.
	if code := runToExit(t, ctx, raw, []string{folder + ":/seed"}, "echo hello > /seed/marker"); code != 0 {
		t.Fatalf("seeding the folder exited %d", code)
	}

	name := "bp-vol-" + suffix
	if ok, err := cli.VolumeExists(ctx, name); err != nil || ok {
		t.Fatalf("VolumeExists before the create = %v, %v, want false", ok, err)
	}
	if err := cli.VolumeCreate(ctx, dockerx.VolumeCreateSpec{
		Name:       name,
		DriverOpts: map[string]string{"type": "none", "o": "bind", "device": folder},
		Labels:     map[string]string{"com.example.purpose": "test"},
	}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.VolumeRemove(c, name, client.VolumeRemoveOptions{Force: true})
	})

	if ok, err := cli.VolumeExists(ctx, name); err != nil || !ok {
		t.Fatalf("VolumeExists after the create = %v, %v, want true", ok, err)
	}
	got, err := raw.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	v := got.Volume
	if v.Driver != "local" || v.Options["type"] != "none" || v.Options["o"] != "bind" || v.Options["device"] != folder ||
		v.Labels["com.example.purpose"] != "test" {
		t.Fatalf("the engine has %+v, want a local volume with the bind options and the label", v)
	}

	// The folder is mounted: the marker written through the other container is there.
	if code := runToExit(t, ctx, raw, []string{name + ":/data"}, `test "$(cat /data/marker)" = hello`); code != 0 {
		t.Fatalf("a container that mounts the volume did not see the folder's file (exit %d)", code)
	}
}
