// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func actionFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{
		{ID: "web-id", Names: []string{"/web"}, Image: "nginx", State: "running", Status: "Up 3 hours", Labels: map[string]string{"bayplan.agent": "true"}},
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
	if got.ID != "web-id" || got.Name != "web" || !got.Running || got.Labels["bayplan.agent"] != "true" {
		t.Fatalf("ownership = %+v", got)
	}
	if _, err := c.ContainerOwnership(context.Background(), "nope"); !dockerx.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}
