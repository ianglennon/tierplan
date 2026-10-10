// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func volumeFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Volumes: []dockertest.FakeVolume{
		{Name: "old", Driver: "local"},
	}})
	return newClient(t, fake), fake
}

func TestVolumeCreateSendsTheSpecOnceWithTheLocalDriverByDefault(t *testing.T) {
	c, fake := volumeFake(t)
	err := c.VolumeCreate(context.Background(), dockerx.VolumeCreateSpec{
		Name: "data1", Labels: map[string]string{"app": "x"},
		DriverOpts: map[string]string{"type": "none", "o": "bind", "device": "/srv/data"},
	})
	if err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /volumes/create"}) {
		t.Fatalf("writes = %v, want one create and no retry", got)
	}
	got := fake.VolumeCreates()
	if len(got) != 1 {
		t.Fatalf("creates = %+v", got)
	}
	req := got[0]
	if req.Name != "data1" || req.Driver != "local" || req.Labels["app"] != "x" ||
		req.DriverOpts["type"] != "none" || req.DriverOpts["o"] != "bind" || req.DriverOpts["device"] != "/srv/data" {
		t.Errorf("body = %+v", req)
	}
}

func TestVolumeExistsSeesAVolumeTheDaemonHasAndNotOneItLacks(t *testing.T) {
	c, _ := volumeFake(t)
	ctx := context.Background()
	if ok, err := c.VolumeExists(ctx, "old"); err != nil || !ok {
		t.Errorf("VolumeExists(old) = %v, %v, want true", ok, err)
	}
	if ok, err := c.VolumeExists(ctx, "new"); err != nil || ok {
		t.Errorf("VolumeExists(new) = %v, %v, want false", ok, err)
	}
	if err := c.VolumeCreate(ctx, dockerx.VolumeCreateSpec{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.VolumeExists(ctx, "new"); err != nil || !ok {
		t.Errorf("VolumeExists(new) after a create = %v, %v, want true", ok, err)
	}
}

func TestVolumeCreateOfAnExistingNameIsNotAnErrorFromTheEngineSoTheCallerChecksFirst(t *testing.T) {
	c, _ := volumeFake(t)
	if err := c.VolumeCreate(context.Background(), dockerx.VolumeCreateSpec{Name: "old"}); err != nil {
		t.Fatalf("a real engine reuses the volume without an error, got %v", err)
	}
}

func TestVolumeRemoveDeletesOnceWithoutForceAndTheVolumeLeavesTheList(t *testing.T) {
	c, fake := volumeFake(t)
	ctx := context.Background()
	if err := c.VolumeRemove(ctx, "old"); err != nil {
		t.Fatalf("VolumeRemove: %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"DELETE /volumes/old"}) {
		t.Fatalf("writes = %v, want one remove, no retry and no force", got)
	}
	if ok, err := c.VolumeExists(ctx, "old"); err != nil || ok {
		t.Errorf("VolumeExists after a remove = %v, %v, want false", ok, err)
	}
	if err := c.VolumeRemove(ctx, "old"); !dockerx.IsNotFound(err) {
		t.Errorf("a second remove = %v, want a not-found error", err)
	}
}

func TestVolumeRemoveOfAVolumeAContainerMountsIsAConflict(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Volumes:    []dockertest.FakeVolume{{Name: "busy", Driver: "local"}},
		Containers: []dockertest.FakeContainer{{ID: "c1", Names: []string{"/db"}, State: "exited", Mounts: []string{"busy"}}},
	})
	c := newClient(t, fake)
	err := c.VolumeRemove(context.Background(), "busy")
	if !dockerx.IsConflict(err) {
		t.Fatalf("remove of a mounted volume = %v, want a conflict", err)
	}
	if ok, _ := c.VolumeExists(context.Background(), "busy"); !ok {
		t.Error("the volume was removed although a stopped container mounts it")
	}
}

func TestVolumeUsersNamesEveryContainerThatMountsTheVolumeRunningOrNotAndNotBindMounts(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Volumes: []dockertest.FakeVolume{{Name: "data", Driver: "local"}, {Name: "idle", Driver: "local"}},
		Containers: []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/web"}, State: "running", Mounts: []string{"data"}},
			{ID: "c2", Names: []string{"/old"}, State: "exited", Mounts: []string{"data", "/data"}},
			{ID: "c3", Names: []string{"/other"}, State: "running", Mounts: []string{"/srv/data"}},
		},
	})
	c := newClient(t, fake)
	got, err := c.VolumeUsers(context.Background(), "data")
	if err != nil || !slices.Equal(got, []string{"old", "web"}) {
		t.Fatalf("VolumeUsers(data) = %v, %v, want old and web (a stopped container counts)", got, err)
	}
	if got, err := c.VolumeUsers(context.Background(), "idle"); err != nil || len(got) != 0 {
		t.Fatalf("VolumeUsers(idle) = %v, %v, want none", got, err)
	}
}

func TestIsAnonymousVolumeIsTheLabelOrA64HexName(t *testing.T) {
	hex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for name, tc := range map[string]struct {
		name   string
		labels map[string]string
		want   bool
	}{
		"the label":                  {"x", map[string]string{"com.docker.volume.anonymous": ""}, true},
		"64 hex characters":          {hex, nil, true},
		"63 hex characters":          {hex[1:], nil, false},
		"64 characters, not all hex": {"z" + hex[1:], nil, false},
		"upper case hex":             {strings.ToUpper(hex), nil, false},
		"a plain name":               {"data", map[string]string{"app": "x"}, false},
	} {
		if got := dockerx.IsAnonymousVolume(tc.name, tc.labels); got != tc.want {
			t.Errorf("%s: IsAnonymousVolume = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSnapshotVolumesCarryTheirLabelsAndWhetherDockerMadeThemWithoutAName(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Volumes: []dockertest.FakeVolume{
		{Name: "named", Driver: "local", Labels: map[string]string{"app": "x"}},
		{Name: "anon", Driver: "local", Labels: map[string]string{"com.docker.volume.anonymous": ""}},
	}})
	snap, err := newClient(t, fake).Snapshot(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]dockerx.VolumeRow{}
	for _, v := range snap.Volumes {
		got[v.Name] = v
	}
	if got["named"].Anonymous || got["named"].Labels["app"] != "x" {
		t.Errorf("named = %+v", got["named"])
	}
	if !got["anon"].Anonymous {
		t.Errorf("anon = %+v, want anonymous", got["anon"])
	}
}
