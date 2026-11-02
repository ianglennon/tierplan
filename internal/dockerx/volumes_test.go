// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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

// Volume sizes (plan 04-18, D-47).

func sizesFake(t *testing.T, api string) *dockerx.Client {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		APIVersion: api,
		Volumes: []dockertest.FakeVolume{
			{Name: "big", Driver: "local", Size: 1 << 20},
			{Name: "empty", Driver: "local"},
			{Name: "share", Driver: "nfs", Size: 99},
		},
		Containers: []dockertest.FakeContainer{{ID: "c1", Names: []string{"/web"}, State: "running", Mounts: []string{"big"}}},
	})
	return newClient(t, fake)
}

func TestVolumeSizesReadsTheSizeAndTheReferenceCountOfEveryVolumeOnTheNewAndTheLegacyShape(t *testing.T) {
	for _, api := range []string{"1.52", "1.51"} {
		t.Run("API "+api, func(t *testing.T) {
			got, err := sizesFake(t, api).VolumeSizes(context.Background())
			if err != nil {
				t.Fatalf("VolumeSizes: %v", err)
			}
			want := map[string]dockerx.VolumeUsage{
				"big":   {Size: 1 << 20, RefCount: 1},
				"empty": {Size: 0, RefCount: 0},
				// A driver other than local cannot say how big it is.
				"share": {Size: -1, RefCount: 0},
			}
			if len(got) != len(want) {
				t.Fatalf("sizes = %+v, want %+v", got, want)
			}
			for name, w := range want {
				if got[name] != w {
					t.Errorf("%s = %+v, want %+v", name, got[name], w)
				}
			}
		})
	}
}

func TestVolumeSizesAnEngineThatCannotWalkItsVolumesIsAnError(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{DiskUsageFail: true})
	if _, err := newClient(t, fake).VolumeSizes(context.Background()); err == nil {
		t.Fatal("VolumeSizes = nil error, want the engine's refusal")
	}
}

// One volume's detail (plan 04-18 task 2).

func TestVolumeDetailReadsTheVolumeWithItsOptionsLabelsAndTheRawDocument(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Volumes: []dockertest.FakeVolume{
		{Name: "share", Driver: "local", CreatedAt: "2026-09-01T10:00:00Z",
			Labels:  map[string]string{"app": "x"},
			Options: map[string]string{"type": "nfs", "device": ":/export"}},
		{Name: "plain", Driver: "local"},
	}})
	c := newClient(t, fake)
	d, raw, err := c.VolumeDetail(context.Background(), "share")
	if err != nil {
		t.Fatalf("VolumeDetail: %v", err)
	}
	if d.Name != "share" || d.Driver != "local" || d.Scope != "local" || d.Mountpoint != "/var/lib/docker/volumes/share/_data" ||
		d.Created.UTC().Format("2006-01-02T15:04:05Z") != "2026-09-01T10:00:00Z" ||
		d.Labels["app"] != "x" || d.Options["type"] != "nfs" || d.Options["device"] != ":/export" {
		t.Errorf("detail = %+v", d)
	}
	if !strings.Contains(string(raw), `"Mountpoint"`) || !strings.Contains(string(raw), `"device"`) {
		t.Errorf("raw = %s, want the engine's own document", raw)
	}
	p, _, err := c.VolumeDetail(context.Background(), "plain")
	if err != nil || p.Options == nil || p.Labels == nil || !p.Created.IsZero() {
		t.Errorf("a volume with none: %+v, %v; want empty maps (never nil) and no created time", p, err)
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Errorf("a read wrote: %v", w)
	}
}

func TestVolumeDetailAVolumeTheEngineLacksIsNotFound(t *testing.T) {
	c, _ := volumeFake(t)
	_, _, err := c.VolumeDetail(context.Background(), "ghost")
	if !dockerx.IsNotFound(err) {
		t.Fatalf("VolumeDetail(ghost) = %v, want a not-found error", err)
	}
}
