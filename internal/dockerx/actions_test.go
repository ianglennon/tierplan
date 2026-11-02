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

func actionFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{
		{ID: "web-id", Names: []string{"/web"}, Image: "nginx", State: "running", Status: "Up 3 hours", Labels: map[string]string{"tierplan.agent": "true"}},
		{ID: "db-id", Names: []string{"/db"}, Image: "postgres", State: "exited", Status: "Exited (0) 1 hour ago"},
	}})
	return newClient(t, fake), fake
}

func TestContainerActionSendsTheMatchingTypedCall(t *testing.T) {
	timeout := 7
	cases := []struct {
		verb dockerx.ContainerVerb
		opts dockerx.ActionOptions
		want string
	}{
		{dockerx.VerbStop, dockerx.ActionOptions{}, "POST /containers/web-id/stop"},
		{dockerx.VerbStop, dockerx.ActionOptions{Timeout: &timeout, Signal: "SIGINT"}, "POST /containers/web-id/stop?signal=SIGINT&t=7"},
		{dockerx.VerbRestart, dockerx.ActionOptions{}, "POST /containers/web-id/restart"},
		{dockerx.VerbKill, dockerx.ActionOptions{Signal: "SIGHUP"}, "POST /containers/web-id/kill?signal=SIGHUP"},
		{dockerx.VerbPause, dockerx.ActionOptions{}, "POST /containers/web-id/pause"},
		{dockerx.VerbUnpause, dockerx.ActionOptions{}, "POST /containers/web-id/unpause"},
		{dockerx.VerbStart, dockerx.ActionOptions{}, "POST /containers/web-id/start"},
	}
	for _, tc := range cases {
		t.Run(string(tc.verb), func(t *testing.T) {
			c, fake := actionFake(t)
			if err := c.ContainerAction(context.Background(), "web-id", tc.verb, tc.opts); err != nil {
				t.Fatalf("ContainerAction(%s): %v", tc.verb, err)
			}
			if got := fake.Writes(); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("writes = %v, want [%s]", got, tc.want)
			}
		})
	}
}

func TestContainerActionStopsNeverStartsWithAnOwnSettingByDefault(t *testing.T) {
	c, fake := actionFake(t)
	if err := c.ContainerAction(context.Background(), "web-id", dockerx.VerbStop, dockerx.ActionOptions{}); err != nil {
		t.Fatal(err)
	}
	// No t and no signal in the query: the container's own settings apply (D-09).
	if got := fake.Writes(); len(got) != 1 || got[0] != "POST /containers/web-id/stop" {
		t.Fatalf("writes = %v", got)
	}
}

func TestContainerActionAnAlreadyStoppedContainerIsNotAnError(t *testing.T) {
	c, _ := actionFake(t)
	if err := c.ContainerAction(context.Background(), "db-id", dockerx.VerbStop, dockerx.ActionOptions{}); err != nil {
		t.Fatalf("stopping an exited container (304) must succeed: %v", err)
	}
}

func TestContainerActionClassifiesAnUnknownContainer(t *testing.T) {
	c, _ := actionFake(t)
	err := c.ContainerAction(context.Background(), "nope", dockerx.VerbStop, dockerx.ActionOptions{})
	if err == nil || !dockerx.IsNotFound(err) {
		t.Fatalf("err = %v, want a not-found error", err)
	}
	if dockerx.IsConflict(err) {
		t.Fatal("a missing container is not a conflict")
	}
}

func TestContainerActionRefusesAnUnknownVerb(t *testing.T) {
	c, fake := actionFake(t)
	if err := c.ContainerAction(context.Background(), "web-id", dockerx.ContainerVerb("exec"), dockerx.ActionOptions{}); err == nil {
		t.Fatal("an unknown verb must be refused")
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Fatalf("an unknown verb sent %v", got)
	}
}

func TestContainerOwnershipReadsLabelsAndStateFromAFreshInspect(t *testing.T) {
	c, _ := actionFake(t)
	got, err := c.ContainerOwnership(context.Background(), "web-id")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "web-id" || got.Name != "web" || !got.Running || got.Labels["tierplan.agent"] != "true" {
		t.Fatalf("ownership = %+v", got)
	}
	if got.Image == "" {
		t.Errorf("ownership has no Image; a remembered shell choice is keyed by it: %+v", got)
	}
	if _, err := c.ContainerOwnership(context.Background(), "nope"); !dockerx.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestContainerRemoveSendsNeitherForceNorVolumesByDefault(t *testing.T) {
	c, fake := actionFake(t)
	if err := c.ContainerRemove(context.Background(), "db-id", dockerx.RemoveOptions{}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"DELETE /containers/db-id"}) {
		t.Fatalf("writes = %v", got)
	}
	// The daemon no longer knows it.
	if _, err := c.ContainerOwnership(context.Background(), "db-id"); !dockerx.IsNotFound(err) {
		t.Fatalf("after a remove: err = %v, want not found", err)
	}
}

func TestContainerRemoveSendsForceAndVolumesOnlyWhenAsked(t *testing.T) {
	cases := []struct {
		name string
		opts dockerx.RemoveOptions
		want string
	}{
		{"force", dockerx.RemoveOptions{Force: true}, "DELETE /containers/web-id?force=1"},
		{"volumes", dockerx.RemoveOptions{Volumes: true}, "DELETE /containers/db-id?v=1"},
		{"both", dockerx.RemoveOptions{Force: true, Volumes: true}, "DELETE /containers/web-id?force=1&v=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fake := actionFake(t)
			id, _, _ := strings.Cut(strings.TrimPrefix(tc.want, "DELETE /containers/"), "?")
			if err := c.ContainerRemove(context.Background(), id, tc.opts); err != nil {
				t.Fatalf("ContainerRemove: %v", err)
			}
			if got := fake.Writes(); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("writes = %v, want [%s]", got, tc.want)
			}
		})
	}
}

func TestContainerRemoveOfARunningContainerWithoutForceIsAConflict(t *testing.T) {
	c, _ := actionFake(t)
	err := c.ContainerRemove(context.Background(), "web-id", dockerx.RemoveOptions{})
	if err == nil || !dockerx.IsConflict(err) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if err := c.ContainerRemove(context.Background(), "nope", dockerx.RemoveOptions{}); !dockerx.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestContainerOwnershipReadsTheContainersOwnStopSettings(t *testing.T) {
	ten := 25
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{
		{ID: "slow-id", Names: []string{"/slow"}, Image: "x", State: "running", Status: "Up", StopTimeout: &ten, StopSignal: "SIGQUIT"},
		{ID: "plain-id", Names: []string{"/plain"}, Image: "x", State: "running", Status: "Up"},
	}})
	c := newClient(t, fake)
	got, err := c.ContainerOwnership(context.Background(), "slow-id")
	if err != nil {
		t.Fatal(err)
	}
	if got.StopTimeout == nil || *got.StopTimeout != 25 || got.StopSignal != "SIGQUIT" {
		t.Fatalf("ownership = %+v", got)
	}
	if plain, _ := c.ContainerOwnership(context.Background(), "plain-id"); plain.StopTimeout != nil || plain.StopSignal != "" {
		t.Fatalf("a container with no settings reported %+v", plain)
	}
}
