// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

func secretFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Secrets: []dockertest.FakeSecret{
			{ID: "s2", Name: "dbpw", Labels: map[string]string{"app": "db"}, CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), UpdatedAt: time.Date(2026, 3, 5, 5, 6, 7, 0, time.UTC)},
			{ID: "s1", Name: "apikey"},
			{ID: "s3", Name: "apikey"},
			{ID: "s4", Name: "unused"},
		},
		ServiceRows: []dockertest.FakeService{
			{ID: "svc1", Name: "usesec", Image: "nginx:1", Desired: 0,
				Secrets: []dockertest.FakeServiceObject{{ID: "s2", Name: "dbpw", File: "dbpw", UID: "0", GID: "0", Mode: 0o444}}},
		},
	})
	return newClient(t, fake), fake
}

func TestSecretListSortsByNameThenIDAndCarriesLabelsAndTimesButNoValue(t *testing.T) {
	c, _ := secretFake(t)
	rows, err := c.SecretList(context.Background())
	if err != nil {
		t.Fatalf("SecretList: %v", err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name+"/"+r.ID)
	}
	if want := []string{"apikey/s1", "apikey/s3", "dbpw/s2", "unused/s4"}; !slices.Equal(names, want) {
		t.Fatalf("rows = %v, want %v", names, want)
	}
	db := rows[2]
	if db.Labels["app"] != "db" || !db.CreatedAt.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) ||
		!db.UpdatedAt.Equal(time.Date(2026, 3, 5, 5, 6, 7, 0, time.UTC)) || db.CreatedAt.Location() != time.UTC {
		t.Errorf("dbpw = %+v", db)
	}
	if rows[0].Labels == nil {
		t.Error("labels are nil, want a map that is never nil")
	}
}

func TestSecretInspectAnswersTheRowAndARawDocumentWithNoData(t *testing.T) {
	c, _ := secretFake(t)
	row, raw, err := c.SecretInspect(context.Background(), "s2")
	if err != nil {
		t.Fatalf("SecretInspect: %v", err)
	}
	if row.ID != "s2" || row.Name != "dbpw" || row.Labels["app"] != "db" {
		t.Errorf("row = %+v", row)
	}
	if !strings.Contains(string(raw), `"dbpw"`) || strings.Contains(string(raw), "Data") {
		t.Errorf("raw = %s, want the inspect document with no Data", raw)
	}
	if _, _, err := c.SecretInspect(context.Background(), "nope"); !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("inspect of a missing secret = %v, want ErrObjectNotFound", err)
	}
}

func TestSecretCreateSendsTheSpecOnceReturnsTheIDAndTheFakeKeepsOnlyTheLength(t *testing.T) {
	c, fake := secretFake(t)
	ctx := context.Background()
	id, err := c.SecretCreate(ctx, dockerx.SecretSpec{Name: "newone", Labels: map[string]string{"a": "b"}, Data: []byte("s3cr3t-value")})
	if err != nil || id == "" {
		t.Fatalf("SecretCreate = %q, %v", id, err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /secrets/create"}) {
		t.Fatalf("writes = %v, want one create and no retry", got)
	}
	got := fake.SecretCreates()
	if len(got) != 1 || got[0].Name != "newone" || got[0].Labels["a"] != "b" || got[0].DataBytes != len("s3cr3t-value") ||
		got[0].Driver != "" || got[0].Templating != "" {
		t.Fatalf("creates = %+v", got)
	}
	rows, err := c.SecretList(ctx)
	if err != nil || !slices.ContainsFunc(rows, func(r dockerx.SecretRow) bool { return r.ID == id && r.Name == "newone" }) {
		t.Errorf("the new secret is not in the list: %v, %v", rows, err)
	}
}

func TestSecretCreateAcceptsTheLargestValueAndTheManagerRefusesOneByteMore(t *testing.T) {
	c, _ := secretFake(t)
	ctx := context.Background()
	if _, err := c.SecretCreate(ctx, dockerx.SecretSpec{Name: "big", Data: bytes.Repeat([]byte("x"), 511999)}); err != nil {
		t.Fatalf("511,999 bytes were refused: %v", err)
	}
	if _, err := c.SecretCreate(ctx, dockerx.SecretSpec{Name: "toobig", Data: bytes.Repeat([]byte("x"), 512000)}); err == nil {
		t.Error("512,000 bytes were accepted")
	}
}

func TestSecretCreateOfATakenNameIsErrNameTaken(t *testing.T) {
	c, _ := secretFake(t)
	_, err := c.SecretCreate(context.Background(), dockerx.SecretSpec{Name: "dbpw", Data: []byte("v")})
	if !errors.Is(err, dockerx.ErrNameTaken) {
		t.Fatalf("create of a taken name = %v, want ErrNameTaken", err)
	}
}

func TestSecretRemoveDeletesOnceAndMapsInUseAndNotFound(t *testing.T) {
	c, fake := secretFake(t)
	ctx := context.Background()
	if err := c.SecretRemove(ctx, "s4"); err != nil {
		t.Fatalf("SecretRemove of an unused secret: %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"DELETE /secrets/s4"}) {
		t.Fatalf("writes = %v, want one remove", got)
	}
	rows, _ := c.SecretList(ctx)
	if slices.ContainsFunc(rows, func(r dockerx.SecretRow) bool { return r.ID == "s4" }) {
		t.Error("the removed secret is still listed")
	}
	// A service scaled to zero still uses the secret (D-27).
	if err := c.SecretRemove(ctx, "s2"); !errors.Is(err, dockerx.ErrObjectInUse) {
		t.Errorf("remove of a secret in use = %v, want ErrObjectInUse", err)
	}
	if err := c.SecretRemove(ctx, "gone"); !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("remove of a missing secret = %v, want ErrObjectNotFound", err)
	}
}

func TestServiceRefsListEverySecretAndConfigReferenceEvenOfAServiceScaledToZero(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{
			{ID: "svc1", Name: "usesec", Image: "nginx:1", Desired: 0,
				Secrets: []dockertest.FakeServiceObject{{ID: "s2", Name: "dbpw", File: "dbpw", UID: "0", GID: "0", Mode: 0o444}},
				Configs: []dockertest.FakeServiceObject{
					{ID: "c1", Name: "nginx_conf", File: "/etc/nginx/nginx.conf", UID: "101", GID: "101", Mode: 0o400},
					{ID: "c2", Name: "runtimecfg"},
				}},
			{ID: "svc2", Name: "plain", Image: "redis:8"},
		},
	})
	c := newClient(t, fake)
	refs, err := c.ServiceRefs(context.Background())
	if err != nil {
		t.Fatalf("ServiceRefs: %v", err)
	}
	by := map[string]dockerx.ServiceRef{}
	for _, r := range refs {
		by[r.Name] = r
	}
	u := by["usesec"]
	if len(u.Secrets) != 1 || u.Secrets[0] != (dockerx.ServiceObjectRef{ID: "s2", Name: "dbpw", File: "dbpw", UID: "0", GID: "0", Mode: 0o444}) {
		t.Errorf("usesec secrets = %+v", u.Secrets)
	}
	if len(u.Configs) != 2 || u.Configs[0] != (dockerx.ServiceObjectRef{ID: "c1", Name: "nginx_conf", File: "/etc/nginx/nginx.conf", UID: "101", GID: "101", Mode: 0o400}) ||
		u.Configs[1].Name != "runtimecfg" || u.Configs[1].File != "" {
		t.Errorf("usesec configs = %+v", u.Configs)
	}
	p := by["plain"]
	if p.Secrets == nil || p.Configs == nil || len(p.Secrets) != 0 || len(p.Configs) != 0 {
		t.Errorf("plain refs = %#v / %#v, want empty and never nil", p.Secrets, p.Configs)
	}
	// Cluster carries the same references.
	cl, err := c.Cluster(context.Background())
	if err != nil || len(cl.ServiceRefs) != 2 {
		t.Fatalf("Cluster = %v, %v", cl.ServiceRefs, err)
	}
	for _, r := range cl.ServiceRefs {
		if r.Name == "usesec" && len(r.Secrets) != 1 {
			t.Errorf("Cluster usesec secrets = %+v", r.Secrets)
		}
	}
}
