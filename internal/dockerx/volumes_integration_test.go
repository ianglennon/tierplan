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

// TestVolumeRemoveInUse proves against a real engine that a volume a container mounts
// is not removed while the container exists, started or not (plan 04-17, RES-07, D-46,
// T-04-54): VolumeUsers names the container, the engine answers the remove with a
// conflict, the volume stays, and once the container is gone the remove goes through.
func TestVolumeRemoveInUse(t *testing.T) {
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
	vol := "bp-inuse-" + suffix
	ctr := "bp-user-" + suffix
	if err := cli.VolumeCreate(ctx, dockerx.VolumeCreateSpec{Name: vol}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, ctr, client.ContainerRemoveOptions{Force: true})
		_, _ = raw.VolumeRemove(c, vol, client.VolumeRemoveOptions{Force: true})
	})
	if users, err := cli.VolumeUsers(ctx, vol); err != nil || len(users) != 0 {
		t.Fatalf("VolumeUsers of a volume nothing mounts = %v, %v", users, err)
	}

	// A container that was created and never started still mounts the volume.
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       ctr,
		Config:     &container.Config{Image: "busybox:1", Cmd: []string{"true"}},
		HostConfig: &container.HostConfig{Binds: []string{vol + ":/data"}},
	}); err != nil {
		t.Fatalf("create %s: %v", ctr, err)
	}
	users, err := cli.VolumeUsers(ctx, vol)
	if err != nil || len(users) != 1 || users[0] != ctr {
		t.Fatalf("VolumeUsers = %v, %v, want just %s", users, err, ctr)
	}
	if err := cli.VolumeRemove(ctx, vol); !dockerx.IsConflict(err) {
		t.Fatalf("VolumeRemove of a volume a stopped container mounts = %v, want a conflict", err)
	}
	if ok, err := cli.VolumeExists(ctx, vol); err != nil || !ok {
		t.Fatalf("the refused remove took the volume away (%v, %v)", ok, err)
	}

	if _, err := raw.ContainerRemove(ctx, ctr, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove %s: %v", ctr, err)
	}
	if users, err := cli.VolumeUsers(ctx, vol); err != nil || len(users) != 0 {
		t.Fatalf("VolumeUsers after the container is gone = %v, %v", users, err)
	}
	if err := cli.VolumeRemove(ctx, vol); err != nil {
		t.Fatalf("VolumeRemove of a volume nothing mounts: %v", err)
	}
	if ok, err := cli.VolumeExists(ctx, vol); err != nil || ok {
		t.Fatalf("VolumeExists after the remove = %v, %v, want false", ok, err)
	}
	if err := cli.VolumeRemove(ctx, vol); !dockerx.IsNotFound(err) {
		t.Fatalf("a second VolumeRemove = %v, want not found", err)
	}
}

// TestVolumeAnonymous proves against the engine BAYPLAN_TEST_ENGINE names (29 and 24)
// that a volume Docker made for a container without a name is listed as anonymous, and a
// named one is not (plan 04-17, RES-07, D-46): newer engines label such a volume and
// older ones only give it a 64-hex name, and either is recognised.
func TestVolumeAnonymous(t *testing.T) {
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
	ctr := "bp-anon-" + suffix
	named := "bp-named-" + suffix
	if err := cli.VolumeCreate(ctx, dockerx.VolumeCreateSpec{Name: named}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	// The container declares /data a volume and names none, so the engine makes one.
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: ctr,
		Config: &container.Config{
			Image: "busybox:1", Cmd: []string{"true"},
			Volumes: map[string]struct{}{"/data": {}},
		},
	}); err != nil {
		t.Fatalf("create %s: %v", ctr, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, ctr, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		_, _ = raw.VolumeRemove(c, named, client.VolumeRemoveOptions{Force: true})
	})
	insp, err := raw.ContainerInspect(ctx, ctr, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", ctr, err)
	}
	anon := ""
	for _, m := range insp.Container.Mounts {
		if m.Destination == "/data" {
			anon = m.Name
		}
	}
	if anon == "" {
		t.Fatalf("the container has no volume mount at /data: %+v", insp.Container.Mounts)
	}

	snap, err := cli.Snapshot(ctx, false)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	found := map[string]dockerx.VolumeRow{}
	for _, v := range snap.Volumes {
		found[v.Name] = v
	}
	if v, ok := found[anon]; !ok || !v.Anonymous {
		t.Fatalf("the volume %s the engine made = %+v (listed %v), want anonymous (labels %v)", anon, v, ok, v.Labels)
	}
	if v, ok := found[named]; !ok || v.Anonymous {
		t.Fatalf("the volume %s made by name = %+v (listed %v), want not anonymous", named, v, ok)
	}
	if users, err := cli.VolumeUsers(ctx, anon); err != nil || len(users) != 1 || users[0] != ctr {
		t.Fatalf("VolumeUsers of the anonymous volume = %v, %v, want %s", users, err, ctr)
	}
}

// TestVolumeSizes proves against the engine BAYPLAN_TEST_ENGINE names (29, which answers
// the new disk usage shape, and 24, which answers the legacy one) that VolumeSizes reports a
// real, non-zero size for a volume a container wrote 1 MiB into, none for an empty one, and
// the container count of each (plan 04-18, RES-07, D-47).
func TestVolumeSizes(t *testing.T) {
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
	full, empty := "bp-sized-"+suffix, "bp-unsized-"+suffix
	for _, name := range []string{full, empty} {
		if err := cli.VolumeCreate(ctx, dockerx.VolumeCreateSpec{Name: name}); err != nil {
			t.Fatalf("VolumeCreate %s: %v", name, err)
		}
		t.Cleanup(func() {
			c, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _ = raw.VolumeRemove(c, name, client.VolumeRemoveOptions{Force: true})
		})
	}
	// A container writes 1 MiB into one volume and exits; it stays, so it is counted.
	if code := runToExit(t, ctx, raw, []string{full + ":/data"}, "dd if=/dev/zero of=/data/blob bs=1024 count=1024"); code != 0 {
		t.Fatalf("writing into the volume exited %d", code)
	}

	sizes, err := cli.VolumeSizes(ctx)
	if err != nil {
		t.Fatalf("VolumeSizes: %v", err)
	}
	got, ok := sizes[full]
	if !ok {
		t.Fatalf("VolumeSizes has no entry for %s: %v", full, sizes)
	}
	if got.Size < 1<<20 {
		t.Errorf("size of %s = %d, want at least 1 MiB", full, got.Size)
	}
	if got.RefCount != 1 {
		t.Errorf("refCount of %s = %d, want the one container that wrote to it", full, got.RefCount)
	}
	e, ok := sizes[empty]
	if !ok {
		t.Fatalf("VolumeSizes has no entry for %s: %v", empty, sizes)
	}
	if e.Size < 0 || e.Size >= 1<<20 || e.RefCount != 0 {
		t.Errorf("%s = %+v, want a size under 1 MiB and no container", empty, e)
	}
}
