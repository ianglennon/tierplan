// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func stackReadsFake(t *testing.T) *dockertest.Fake {
	t.Helper()
	return dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{
			{ID: "a1", Name: "shop_web", Mode: "replicated", Image: "nginx:1", Version: 3, Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "a2", Name: "shop_db", Mode: "replicated", Image: "postgres:17", Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "b1", Name: "blog_web", Mode: "replicated", Image: "nginx:1", Labels: map[string]string{"com.docker.stack.namespace": "blog"}},
			{ID: "c1", Name: "lonely", Mode: "global"},
		},
		Networks: []dockertest.FakeNetwork{
			{ID: "n1", Name: "shop_default", Driver: "overlay", Scope: "swarm", Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "n2", Name: "blog_default", Driver: "overlay", Scope: "swarm", Labels: map[string]string{"com.docker.stack.namespace": "blog"}},
			{ID: "n3", Name: "bridge", Driver: "bridge", Scope: "local"},
		},
	})
}

func TestServicesWithLabelListsOnlyTheServicesThatCarryIt(t *testing.T) {
	c := newClient(t, stackReadsFake(t))
	got, err := c.ServicesWithLabel(context.Background(), "com.docker.stack.namespace", "shop")
	if err != nil {
		t.Fatalf("ServicesWithLabel: %v", err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"shop_db", "shop_web"}) {
		t.Errorf("services = %v, want shop_db and shop_web", names)
	}
	none, err := c.ServicesWithLabel(context.Background(), "com.docker.stack.namespace", "nope")
	if err != nil || len(none) != 0 {
		t.Errorf("ServicesWithLabel(nope) = %v, %v; want none", none, err)
	}
}

func TestServicesWithLabelEncodesTheValueRatherThanJoiningItIntoTheQuery(t *testing.T) {
	// A value that tries to widen the filter matches nothing: it is one filter term, whatever it holds.
	c := newClient(t, stackReadsFake(t))
	got, err := c.ServicesWithLabel(context.Background(), "com.docker.stack.namespace", `shop","x":{"y":true`)
	if err != nil || len(got) != 0 {
		t.Errorf("ServicesWithLabel = %v, %v; want no services and no error", got, err)
	}
}

func TestNetworksWithLabelListsOnlyTheNetworksThatCarryIt(t *testing.T) {
	c := newClient(t, stackReadsFake(t))
	got, err := c.NetworksWithLabel(context.Background(), "com.docker.stack.namespace", "shop")
	if err != nil {
		t.Fatalf("NetworksWithLabel: %v", err)
	}
	if len(got) != 1 || got[0].Name != "shop_default" || got[0].ID != "n1" {
		t.Errorf("networks = %+v, want shop_default only", got)
	}
}

func objectReadsFake(t *testing.T) *dockertest.Fake {
	t.Helper()
	shop := map[string]string{"com.docker.stack.namespace": "shop", "bayplan.stack.id": "s1"}
	return dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Secrets: []dockertest.FakeSecret{
			{ID: "sec1", Name: "shop_tok_aaaaaaaa", Labels: shop},
			{ID: "sec2", Name: "blog_tok_bbbbbbbb", Labels: map[string]string{"com.docker.stack.namespace": "blog"}},
			{ID: "sec3", Name: "plain"},
		},
		Configs: []dockertest.FakeConfigObject{
			{ID: "cfg1", Name: "shop_conf_cccccccc", Labels: shop, Data: []byte("content-that-must-not-leak")},
			{ID: "cfg2", Name: "plain", Data: []byte("x")},
		},
	})
}

func TestSecretsWithLabelListsOnlyTheSecretsThatCarryIt(t *testing.T) {
	c := newClient(t, objectReadsFake(t))
	got, err := c.SecretsWithLabel(context.Background(), "com.docker.stack.namespace", "shop")
	if err != nil {
		t.Fatalf("SecretsWithLabel: %v", err)
	}
	if len(got) != 1 || got[0].Name != "shop_tok_aaaaaaaa" || got[0].ID != "sec1" {
		t.Errorf("secrets = %+v, want shop_tok_aaaaaaaa only", got)
	}
	none, err := c.SecretsWithLabel(context.Background(), "com.docker.stack.namespace", "nope")
	if err != nil || len(none) != 0 {
		t.Errorf("SecretsWithLabel(nope) = %v, %v; want none", none, err)
	}
}

func TestConfigsWithLabelListsOnlyTheConfigsThatCarryItWithoutTheirContent(t *testing.T) {
	c := newClient(t, objectReadsFake(t))
	got, err := c.ConfigsWithLabel(context.Background(), "bayplan.stack.id", "s1")
	if err != nil {
		t.Fatalf("ConfigsWithLabel: %v", err)
	}
	if len(got) != 1 || got[0].Name != "shop_conf_cccccccc" || got[0].ID != "cfg1" {
		t.Errorf("configs = %+v, want shop_conf_cccccccc only", got)
	}
}

func TestObjectLabelReadsEncodeTheValueRatherThanJoiningItIntoTheQuery(t *testing.T) {
	c := newClient(t, objectReadsFake(t))
	hostile := `shop","x":{"y":true`
	if got, err := c.SecretsWithLabel(context.Background(), "com.docker.stack.namespace", hostile); err != nil || len(got) != 0 {
		t.Errorf("SecretsWithLabel = %v, %v; want none and no error", got, err)
	}
	if got, err := c.ConfigsWithLabel(context.Background(), "com.docker.stack.namespace", hostile); err != nil || len(got) != 0 {
		t.Errorf("ConfigsWithLabel = %v, %v; want none and no error", got, err)
	}
}
