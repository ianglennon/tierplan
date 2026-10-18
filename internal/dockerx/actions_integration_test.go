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
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// state reads what the engine says a container is doing, bypassing dockerx, so the
// checks below do not trust the code under test to report its own effect.
func state(t *testing.T, ctx context.Context, raw *client.Client, name string) container.InspectResponse {
	t.Helper()
	res, err := raw.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	return res.Container
}

// idleContainer creates, without starting, a busybox container that sleeps, and removes
// it when the test ends.
func idleContainer(t *testing.T, ctx context.Context, raw *client.Client, cfg *container.Config, hc *container.HostConfig) string {
	t.Helper()
	name := "bp-act-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cfg.Image = "busybox:1"
	cfg.Cmd = []string{"sleep", "300"}
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name, Config: cfg, HostConfig: hc}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	})
	return name
}

// TestActions runs every container action and remove against a real engine (plan 04-05,
// RES-01): the six verbs each do what they say, repeating one is not an error, a running
// container is not removed without force, and remove with volumes deletes the container's
// anonymous volume and keeps a named one it mounted.
func TestActions(t *testing.T) {
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

	t.Run("the six verbs and a repeat of each", func(t *testing.T) {
		name := idleContainer(t, ctx, raw, &container.Config{}, nil)
		do := func(verb dockerx.ContainerVerb, o dockerx.ActionOptions) {
			t.Helper()
			if err := cli.ContainerAction(ctx, name, verb, o); err != nil {
				t.Fatalf("%s: %v", verb, err)
			}
		}

		do(dockerx.VerbStart, dockerx.ActionOptions{})
		if st := state(t, ctx, raw, name).State; !st.Running {
			t.Fatalf("after start: %+v", st)
		}
		do(dockerx.VerbStart, dockerx.ActionOptions{}) // the engine's 304 is not an error

		do(dockerx.VerbPause, dockerx.ActionOptions{})
		if st := state(t, ctx, raw, name).State; !st.Paused {
			t.Fatalf("after pause: %+v", st)
		}
		do(dockerx.VerbUnpause, dockerx.ActionOptions{})
		if st := state(t, ctx, raw, name).State; !st.Running || st.Paused {
			t.Fatalf("after unpause: %+v", st)
		}

		startedBefore := state(t, ctx, raw, name).State.StartedAt
		do(dockerx.VerbRestart, dockerx.ActionOptions{})
		after := state(t, ctx, raw, name).State
		if !after.Running || after.StartedAt == startedBefore {
			t.Fatalf("after restart: running=%v startedAt %s -> %s", after.Running, startedBefore, after.StartedAt)
		}

		do(dockerx.VerbKill, dockerx.ActionOptions{})
		waitStopped(t, ctx, raw, name)
		if code := state(t, ctx, raw, name).State.ExitCode; code != 137 {
			t.Errorf("a kill left exit code %d, want 137 (SIGKILL)", code)
		}
		do(dockerx.VerbStop, dockerx.ActionOptions{}) // stopping a stopped container is not an error

		do(dockerx.VerbStart, dockerx.ActionOptions{})
		zero := 0
		do(dockerx.VerbStop, dockerx.ActionOptions{Timeout: &zero, Signal: "SIGKILL"})
		waitStopped(t, ctx, raw, name)
	})

	t.Run("a running container is not removed without force", func(t *testing.T) {
		name := idleContainer(t, ctx, raw, &container.Config{}, nil)
		if err := cli.ContainerAction(ctx, name, dockerx.VerbStart, dockerx.ActionOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := cli.ContainerRemove(ctx, name, dockerx.RemoveOptions{}); err == nil || !dockerx.IsConflict(err) {
			t.Fatalf("remove of a running container = %v, want a conflict", err)
		}
		if !state(t, ctx, raw, name).State.Running {
			t.Fatal("the refused remove stopped the container")
		}
		if err := cli.ContainerRemove(ctx, name, dockerx.RemoveOptions{Force: true}); err != nil {
			t.Fatalf("forced remove: %v", err)
		}
		if _, err := raw.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); err == nil || !dockerx.IsNotFound(err) {
			t.Fatalf("after the remove: %v, want not found", err)
		}
		if err := cli.ContainerRemove(ctx, name, dockerx.RemoveOptions{}); !dockerx.IsNotFound(err) {
			t.Fatalf("a second remove = %v, want not found", err)
		}
	})

	t.Run("remove with volumes deletes the anonymous volume and keeps the named one", func(t *testing.T) {
		named := "bp-act-named-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if _, err := raw.VolumeCreate(ctx, client.VolumeCreateOptions{Name: named}); err != nil {
			t.Fatalf("create the named volume: %v", err)
		}
		t.Cleanup(func() {
			c, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _ = raw.VolumeRemove(c, named, client.VolumeRemoveOptions{Force: true})
		})
		name := idleContainer(t, ctx, raw,
			&container.Config{Volumes: map[string]struct{}{"/anon": {}}},
			&container.HostConfig{Binds: []string{named + ":/named"}})

		var anon string
		for _, m := range state(t, ctx, raw, name).Mounts {
			if m.Destination == "/anon" {
				anon = m.Name
			}
		}
		if anon == "" {
			t.Fatalf("the container has no anonymous volume at /anon: %+v", state(t, ctx, raw, name).Mounts)
		}

		if err := cli.ContainerRemove(ctx, name, dockerx.RemoveOptions{Volumes: true}); err != nil {
			t.Fatalf("remove with volumes: %v", err)
		}
		if _, err := raw.VolumeInspect(ctx, anon, client.VolumeInspectOptions{}); err == nil || !dockerx.IsNotFound(err) {
			t.Errorf("the anonymous volume %s survived: %v", anon, err)
		}
		if _, err := raw.VolumeInspect(ctx, named, client.VolumeInspectOptions{}); err != nil {
			t.Errorf("the named volume was removed with the container: %v", err)
		}
	})

	t.Run("ownership reads the containers own stop settings", func(t *testing.T) {
		ten := 12
		name := idleContainer(t, ctx, raw, &container.Config{StopTimeout: &ten, StopSignal: "SIGINT"}, nil)
		own, err := cli.ContainerOwnership(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if own.StopTimeout == nil || *own.StopTimeout != 12 || own.StopSignal != "SIGINT" {
			t.Fatalf("ownership = %+v", own)
		}
	})
}

// waitStopped waits until the engine says the container is no longer running.
func waitStopped(t *testing.T, ctx context.Context, raw *client.Client, name string) {
	t.Helper()
	testsupport.Poll(t, 60*time.Second, name+" to stop running", func() (bool, string) {
		st := state(t, ctx, raw, name).State
		return !st.Running, "status " + string(st.Status)
	})
}
