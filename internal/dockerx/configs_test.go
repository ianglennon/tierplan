// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func configFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Configs: []dockertest.FakeConfigObject{
			{ID: "c2", Name: "nginx_conf", Labels: map[string]string{"app": "web"}, Data: []byte("worker_processes 1;\n"), Templating: "golang",
				CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), UpdatedAt: time.Date(2026, 3, 5, 5, 6, 7, 0, time.UTC)},
			{ID: "c1", Name: "app_env", Data: []byte("A=1\n")},
			{ID: "c3", Name: "app_env", Data: []byte("A=2\n")},
			{ID: "c4", Name: "unused", Data: []byte("x")},
		},
		ServiceRows: []dockertest.FakeService{
			{ID: "svc1", Name: "useconf", Image: "nginx:1", Desired: 0,
				Configs: []dockertest.FakeServiceObject{{ID: "c2", Name: "nginx_conf", File: "/etc/nginx/nginx.conf", UID: "0", GID: "0", Mode: 0o444}}},
		},
	})
	return newClient(t, fake), fake
}

func TestConfigListHasNoData(t *testing.T) {
	c, fake := configFake(t)
	rows, err := c.ConfigList(context.Background())
	if err != nil {
		t.Fatalf("ConfigList: %v", err)
	}
	// The manager did send the content with every row (RESEARCH E17): the client is what drops it.
	if got := strings.Join(fake.Requests(), " "); !strings.Contains(got, "/configs") {
		t.Fatalf("the manager was never asked for the configs: %s", got)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name+"/"+r.ID)
	}
	if want := []string{"app_env/c1", "app_env/c3", "nginx_conf/c2", "unused/c4"}; !slices.Equal(names, want) {
		t.Fatalf("rows = %v, want %v", names, want)
	}
	nginx := rows[2]
	if nginx.Labels["app"] != "web" || nginx.Templating != "golang" || !nginx.CreatedAt.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) ||
		!nginx.UpdatedAt.Equal(time.Date(2026, 3, 5, 5, 6, 7, 0, time.UTC)) || nginx.CreatedAt.Location() != time.UTC {
		t.Errorf("nginx_conf = %+v", nginx)
	}
	if rows[0].Templating != "" || rows[0].Labels == nil {
		t.Errorf("app_env = %+v, want no templating and labels that are never nil", rows[0])
	}
	// The row type has no content field, so a row cannot carry any (RESEARCH Pitfall 15).
	typ := reflect.TypeOf(dockerx.ConfigRow{})
	for i := range typ.NumField() {
		if n := typ.Field(i).Name; n == "Data" || n == "Content" || n == "Spec" {
			t.Errorf("ConfigRow has field %s, which can hold the content", n)
		}
	}
}

func TestConfigInspectAnswersTheContentTemplatingAndTheRawDocument(t *testing.T) {
	c, _ := configFake(t)
	d, raw, err := c.ConfigInspect(context.Background(), "c2")
	if err != nil {
		t.Fatalf("ConfigInspect: %v", err)
	}
	if d.ID != "c2" || d.Name != "nginx_conf" || d.Labels["app"] != "web" || d.Templating != "golang" || string(d.Data) != "worker_processes 1;\n" {
		t.Errorf("detail = %+v", d)
	}
	if !strings.Contains(string(raw), `"nginx_conf"`) || !strings.Contains(string(raw), "Data") {
		t.Errorf("raw = %s, want the manager's inspect document", raw)
	}
	byName, _, err := c.ConfigInspect(context.Background(), "unused")
	if err != nil || byName.ID != "c4" || byName.Templating != "" {
		t.Errorf("inspect by name = %+v, %v", byName, err)
	}
	if _, _, err := c.ConfigInspect(context.Background(), "nope"); !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("inspect of a missing config = %v, want ErrObjectNotFound", err)
	}
}

func TestConfigCreateSendsTheSpecOnceAndReturnsTheID(t *testing.T) {
	c, fake := configFake(t)
	ctx := context.Background()
	id, err := c.ConfigCreate(ctx, dockerx.ConfigSpec{Name: "newone", Labels: map[string]string{"a": "b"}, Data: []byte("hello {{.Service.Name}}"), Templating: "golang"})
	if err != nil || id == "" {
		t.Fatalf("ConfigCreate = %q, %v", id, err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /configs/create"}) {
		t.Fatalf("writes = %v, want one create and no retry", got)
	}
	got := fake.ConfigCreates()
	if len(got) != 1 || got[0].Name != "newone" || got[0].Labels["a"] != "b" || string(got[0].Data) != "hello {{.Service.Name}}" || got[0].Templating != "golang" {
		t.Fatalf("creates = %+v", got)
	}
	d, _, err := c.ConfigInspect(ctx, id)
	if err != nil || d.Name != "newone" || d.Templating != "golang" {
		t.Errorf("the new config does not inspect: %+v, %v", d, err)
	}
	// A config with no templating sends none.
	if _, err := c.ConfigCreate(ctx, dockerx.ConfigSpec{Name: "plain", Data: []byte("v")}); err != nil {
		t.Fatal(err)
	}
	if cs := fake.ConfigCreates(); len(cs) != 2 || cs[1].Templating != "" {
		t.Errorf("creates = %+v, want a second with no templating", cs)
	}
}

func TestConfigCreateAcceptsTheLargestContentAndTheManagerRefusesOneByteMore(t *testing.T) {
	c, _ := configFake(t)
	ctx := context.Background()
	if _, err := c.ConfigCreate(ctx, dockerx.ConfigSpec{Name: "big", Data: bytes.Repeat([]byte("x"), 1023999)}); err != nil {
		t.Fatalf("1,023,999 bytes were refused: %v", err)
	}
	if _, err := c.ConfigCreate(ctx, dockerx.ConfigSpec{Name: "toobig", Data: bytes.Repeat([]byte("x"), 1024000)}); err == nil {
		t.Error("1,024,000 bytes were accepted")
	}
}

func TestConfigCreateOfATakenNameIsErrNameTaken(t *testing.T) {
	c, _ := configFake(t)
	_, err := c.ConfigCreate(context.Background(), dockerx.ConfigSpec{Name: "nginx_conf", Data: []byte("v")})
	if !errors.Is(err, dockerx.ErrNameTaken) {
		t.Fatalf("create of a taken name = %v, want ErrNameTaken", err)
	}
}

func TestConfigRemoveDeletesOnceAndMapsInUseAndNotFound(t *testing.T) {
	c, fake := configFake(t)
	ctx := context.Background()
	if err := c.ConfigRemove(ctx, "c4"); err != nil {
		t.Fatalf("ConfigRemove of an unused config: %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"DELETE /configs/c4"}) {
		t.Fatalf("writes = %v, want one remove", got)
	}
	rows, _ := c.ConfigList(ctx)
	if slices.ContainsFunc(rows, func(r dockerx.ConfigRow) bool { return r.ID == "c4" }) {
		t.Error("the removed config is still listed")
	}
	// A service scaled to zero still uses the config (D-27).
	if err := c.ConfigRemove(ctx, "c2"); !errors.Is(err, dockerx.ErrObjectInUse) {
		t.Errorf("remove of a config in use = %v, want ErrObjectInUse", err)
	}
	if err := c.ConfigRemove(ctx, "gone"); !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("remove of a missing config = %v, want ErrObjectNotFound", err)
	}
}
