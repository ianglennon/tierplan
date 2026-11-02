// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The guard the Compose SDK's client is wrapped in (plan 07-20 task 2, T-07-96, RESEARCH Q6). It is proved
// against the whole of client.APIClient: every method is either refused or let through, each list is written
// out here, and a method a later client version adds is in neither, which fails TestEveryClientMethodIsDecided
// until someone decides.

// forbiddenMethods are every method of the client the guard refuses with ErrComposeForbidden.
var forbiddenMethods = []string{
	// building images, and the build cache
	"ImageBuild", "BuildCachePrune", "BuildCancel",
	// moving image content in or out of the engine
	"ImageSave", "ImageLoad", "ImagePush", "ImageImport", "ImageSearch", "ImageAttestations",
	// reading a container's filesystem out, committing it, changing it in place
	"ContainerExport", "ContainerCommit", "ContainerDiff", "CopyFromContainer", "ContainerUpdate",
	// exec and attach
	"ContainerAttach", "ExecCreate", "ExecStart", "ExecAttach", "ExecInspect", "ExecResize",
	// sweeping prunes
	"ContainerPrune", "ImagePrune", "NetworkPrune", "VolumePrune",
	// the swarm, every part of it
	"SwarmInit", "SwarmJoin", "SwarmInspect", "SwarmUpdate", "SwarmLeave", "SwarmGetUnlockKey", "SwarmUnlock",
	"NodeInspect", "NodeList", "NodeUpdate", "NodeRemove",
	"ServiceCreate", "ServiceInspect", "ServiceList", "ServiceUpdate", "ServiceRemove", "ServiceLogs",
	"TaskInspect", "TaskList", "TaskLogs",
	"SecretCreate", "SecretInspect", "SecretList", "SecretUpdate", "SecretRemove",
	"ConfigCreate", "ConfigInspect", "ConfigList", "ConfigUpdate", "ConfigRemove",
	// plugins
	"PluginCreate", "PluginInstall", "PluginInspect", "PluginList", "PluginRemove", "PluginEnable", "PluginDisable",
	"PluginUpgrade", "PluginPush", "PluginSet",
	// registry sign-in and checkpoints
	"RegistryLogin", "CheckpointCreate", "CheckpointRemove", "CheckpointList",
}

// forwardedMethods are every method the guard passes through to the engine client (Close is the guard's own
// and does nothing). The ones a stack deploy uses are exercised by TestGuardForwardsTheMethodsTheSDKNeeds.
var forwardedMethods = []string{
	"ContainerCreate", "ContainerInspect", "ContainerList", "ContainerRemove", "ContainerLogs", "ContainerStart",
	"ContainerStop", "ContainerRestart", "ContainerPause", "ContainerUnpause", "ContainerWait", "ContainerKill",
	"ContainerRename", "ContainerResize", "ContainerStats", "ContainerTop", "ContainerStatPath", "CopyToContainer",
	"DistributionInspect",
	"ImageList", "ImagePull", "ImageRemove", "ImageTag", "ImageInspect", "ImageHistory",
	"NetworkCreate", "NetworkInspect", "NetworkList", "NetworkRemove", "NetworkConnect", "NetworkDisconnect",
	"VolumeCreate", "VolumeInspect", "VolumeList", "VolumeUpdate", "VolumeRemove",
	"Events", "Info", "DiskUsage", "Ping", "ServerVersion", "ClientVersion", "DaemonHost", "DialHijack", "Dialer", "Close",
}

// refusedByTheGuard reports whether the guard answers a call of the method with ErrComposeForbidden. The
// guard wraps nothing here, so a method that is let through panics on the missing client, which is not a
// refusal.
func refusedByTheGuard(t *testing.T, name string) (refused bool) {
	t.Helper()
	defer func() {
		if recover() != nil {
			refused = false
		}
	}()
	m := reflect.ValueOf(guardClient{}).MethodByName(name)
	if !m.IsValid() {
		t.Fatalf("the guard has no method %s", name)
	}
	typ := m.Type()
	args := make([]reflect.Value, 0, typ.NumIn())
	for i := range typ.NumIn() {
		if typ.IsVariadic() && i == typ.NumIn()-1 {
			break
		}
		if in := typ.In(i); in == reflect.TypeFor[context.Context]() {
			args = append(args, reflect.ValueOf(context.Background()))
		} else {
			args = append(args, reflect.Zero(in))
		}
	}
	out := m.Call(args)
	if len(out) == 0 {
		return false
	}
	last := out[len(out)-1]
	if !last.Type().Implements(reflect.TypeFor[error]()) || last.IsNil() {
		return false
	}
	err, _ := last.Interface().(error)
	return errors.Is(err, ErrComposeForbidden)
}

func TestGuardRefusesEveryForbiddenMethod(t *testing.T) {
	t.Parallel()
	var notRefused []string
	for _, name := range forbiddenMethods {
		if !refusedByTheGuard(t, name) {
			notRefused = append(notRefused, name)
		}
	}
	if len(notRefused) > 0 {
		t.Errorf("the guard lets %d forbidden methods through: %v", len(notRefused), notRefused)
	}
	// What is refused is exactly the list, no more: a method refused here that the list does not name would
	// be a call the SDK may need, decided by accident.
	var extra []string
	for _, name := range forwardedMethods {
		if name != "Close" && refusedByTheGuard(t, name) {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		t.Errorf("the guard refuses methods that are listed as forwarded: %v", extra)
	}
}

func TestEveryClientMethodIsDecided(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[client.APIClient]()
	var undecided []string
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if !slices.Contains(forbiddenMethods, name) && !slices.Contains(forwardedMethods, name) {
			undecided = append(undecided, name)
		}
	}
	sort.Strings(undecided)
	if len(undecided) > 0 {
		t.Errorf("the client has methods that are neither refused nor forwarded by the guard: %v: decide each", undecided)
	}
	for _, name := range append(slices.Clone(forbiddenMethods), forwardedMethods...) {
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("%s is listed but the client has no such method any more", name)
		}
	}
	if overlap := intersect(forbiddenMethods, forwardedMethods); len(overlap) > 0 {
		t.Errorf("methods in both lists: %v", overlap)
	}
}

func intersect(a, b []string) []string {
	var out []string
	for _, s := range a {
		if slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// sdkCalls are the calls the Compose SDK makes for up, stop, start and down (read from its source at v5.6.0:
// every s.apiClient() call outside build, exec, attach, export, commit and push), each with the arguments
// that make the client send a request, and a part of the path that request has.
var sdkCalls = []struct {
	name string
	path string
	call func(ctx context.Context, g client.APIClient)
}{
	{"Ping", "/_ping", func(ctx context.Context, g client.APIClient) { _, _ = g.Ping(ctx, client.PingOptions{}) }},
	{"Info", "/info", func(ctx context.Context, g client.APIClient) { _, _ = g.Info(ctx, client.InfoOptions{}) }},
	{"ContainerList", "/containers/json", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerList(ctx, client.ContainerListOptions{All: true})
	}},
	{"ContainerInspect", "/containers/c1/json", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerInspect(ctx, "c1", client.ContainerInspectOptions{})
	}},
	{"ContainerCreate", "/containers/create", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: "x"}})
	}},
	{"ContainerStart", "/containers/c1/start", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerStart(ctx, "c1", client.ContainerStartOptions{})
	}},
	{"ContainerStop", "/containers/c1/stop", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerStop(ctx, "c1", client.ContainerStopOptions{})
	}},
	{"ContainerRestart", "/containers/c1/restart", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerRestart(ctx, "c1", client.ContainerRestartOptions{})
	}},
	{"ContainerKill", "/containers/c1/kill", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerKill(ctx, "c1", client.ContainerKillOptions{})
	}},
	{"ContainerPause", "/containers/c1/pause", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerPause(ctx, "c1", client.ContainerPauseOptions{})
	}},
	{"ContainerUnpause", "/containers/c1/unpause", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerUnpause(ctx, "c1", client.ContainerUnpauseOptions{})
	}},
	{"ContainerRename", "/containers/c1/rename", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerRename(ctx, "c1", client.ContainerRenameOptions{NewName: "c2"})
	}},
	{"ContainerRemove", "/containers/c1", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerRemove(ctx, "c1", client.ContainerRemoveOptions{})
	}},
	{"ContainerLogs", "/containers/c1/logs", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerLogs(ctx, "c1", client.ContainerLogsOptions{ShowStdout: true})
	}},
	{"ContainerStatPath", "/containers/c1/archive", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ContainerStatPath(ctx, "c1", client.ContainerStatPathOptions{Path: "/"})
	}},
	{"CopyToContainer", "/containers/c1/archive", func(ctx context.Context, g client.APIClient) {
		_, _ = g.CopyToContainer(ctx, "c1", client.CopyToContainerOptions{DestinationPath: "/", Content: strings.NewReader("")})
	}},
	{"ImageInspect", "/images/x/json", func(ctx context.Context, g client.APIClient) { _, _ = g.ImageInspect(ctx, "x") }},
	{"ImageList", "/images/json", func(ctx context.Context, g client.APIClient) { _, _ = g.ImageList(ctx, client.ImageListOptions{}) }},
	{"ImagePull", "/images/create", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ImagePull(ctx, "x", client.ImagePullOptions{})
	}},
	{"ImageRemove", "/images/x", func(ctx context.Context, g client.APIClient) {
		_, _ = g.ImageRemove(ctx, "x", client.ImageRemoveOptions{})
	}},
	{"NetworkCreate", "/networks/create", func(ctx context.Context, g client.APIClient) {
		_, _ = g.NetworkCreate(ctx, "n", client.NetworkCreateOptions{})
	}},
	{"NetworkInspect", "/networks/n1", func(ctx context.Context, g client.APIClient) {
		_, _ = g.NetworkInspect(ctx, "n1", client.NetworkInspectOptions{})
	}},
	{"NetworkList", "/networks", func(ctx context.Context, g client.APIClient) { _, _ = g.NetworkList(ctx, client.NetworkListOptions{}) }},
	{"NetworkConnect", "/networks/n1/connect", func(ctx context.Context, g client.APIClient) {
		_, _ = g.NetworkConnect(ctx, "n1", client.NetworkConnectOptions{Container: "c1"})
	}},
	{"NetworkDisconnect", "/networks/n1/disconnect", func(ctx context.Context, g client.APIClient) {
		_, _ = g.NetworkDisconnect(ctx, "n1", client.NetworkDisconnectOptions{Container: "c1"})
	}},
	{"NetworkRemove", "/networks/n1", func(ctx context.Context, g client.APIClient) {
		_, _ = g.NetworkRemove(ctx, "n1", client.NetworkRemoveOptions{})
	}},
	{"VolumeCreate", "/volumes/create", func(ctx context.Context, g client.APIClient) {
		_, _ = g.VolumeCreate(ctx, client.VolumeCreateOptions{Name: "v"})
	}},
	{"VolumeInspect", "/volumes/v", func(ctx context.Context, g client.APIClient) {
		_, _ = g.VolumeInspect(ctx, "v", client.VolumeInspectOptions{})
	}},
	{"VolumeList", "/volumes", func(ctx context.Context, g client.APIClient) { _, _ = g.VolumeList(ctx, client.VolumeListOptions{}) }},
	{"VolumeRemove", "/volumes/v", func(ctx context.Context, g client.APIClient) {
		_, _ = g.VolumeRemove(ctx, "v", client.VolumeRemoveOptions{})
	}},
	{"Events", "/events", func(ctx context.Context, g client.APIClient) {
		res := g.Events(ctx, client.EventsListOptions{})
		select {
		case <-res.Messages:
		case <-res.Err:
		case <-time.After(50 * time.Millisecond): // wall-clock: the fake answers /events with an open stream and no event, so there is nothing to wait for but the request itself
		}
	}},
}

func TestGuardForwardsTheMethodsTheSDKNeeds(t *testing.T) {
	t.Parallel()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	g := guardClient{APIClient: c.cur()}
	var missed []string
	for _, tc := range sdkCalls {
		ctx, cancel := context.WithCancel(testsupport.Context(t))
		before := len(fake.Requests())
		tc.call(ctx, g)
		cancel()
		reqs := fake.Requests()
		reached := false
		for _, r := range reqs[before:] {
			if strings.Contains(r, tc.path) {
				reached = true
			}
		}
		if !reached {
			missed = append(missed, tc.name+" ("+tc.path+")")
		}
	}
	if len(missed) > 0 {
		t.Errorf("calls the SDK makes that did not reach the engine through the guard: %v", missed)
	}
	for _, tc := range sdkCalls {
		if refusedByTheGuard(t, tc.name) {
			t.Errorf("%s is one the SDK needs and the guard refuses it", tc.name)
		}
	}
}
