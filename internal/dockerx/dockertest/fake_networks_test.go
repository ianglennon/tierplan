// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

const fakeBigID = "skvqz4pk12gec9lje3akizuef"

func fakeNetManager(extra ...func(*dockertest.FakeConfig)) dockertest.FakeConfig {
	cfg := dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Networks: []dockertest.FakeNetwork{
			{ID: fakeBigID, Name: "big_default", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.2.0/24"}},
		},
	}
	for _, e := range extra {
		e(&cfg)
	}
	return cfg
}

func inspectNetwork(t *testing.T, f *dockertest.Fake, ref string) network.Inspect {
	t.Helper()
	ctx := awaitCtx(t)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/v1.52/networks/"+ref, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fakeClient(f).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", ref, resp.StatusCode)
	}
	var out network.Inspect
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// statusOf is the subnet counts of an inspect's Status, or nil for no Status.
func statusOf(in network.Inspect) map[netip.Prefix][2]uint64 {
	if in.Status == nil {
		return nil
	}
	out := map[netip.Prefix][2]uint64{}
	for p, u := range in.Status.IPAM.Subnets {
		out[p] = [2]uint64{u.IPsInUse, u.DynamicIPsAvailable}
	}
	return out
}

func TestTheFakeAnswersAFullIDFromTheNodesOwnInstanceAndANameOrPrefixFromTheSwarm(t *testing.T) {
	subnet := netip.MustParsePrefix("10.0.2.0/24")
	swarmCount := map[netip.Prefix][2]uint64{subnet: {256, 0}}
	localCount := map[netip.Prefix][2]uint64{subnet: {46, 210}}
	refs := []struct {
		name, ref string
		local     bool
	}{
		{"the full ID", fakeBigID, true},
		{"the name", "big_default", false},
		{"a 12 character prefix", fakeBigID[:12], false},
		{"the ID less its last character", fakeBigID[:len(fakeBigID)-1], false},
	}
	both := func(f *dockertest.Fake) {
		f.SetNetworkUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 256, Available: 0})
		f.SetNetworkLocalUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 46, Available: 210})
	}
	t.Run("both counts set", func(t *testing.T) {
		f := dockertest.NewFake(t, fakeNetManager())
		both(f)
		for _, r := range refs {
			got := inspectNetwork(t, f, r.ref)
			want := swarmCount
			if r.local {
				want = localCount
			}
			if s := statusOf(got); s[subnet] != want[subnet] {
				t.Errorf("%s: status = %v, want %v", r.name, s, want)
			}
			if _, ok := got.Containers["lb-big_default"]; !ok {
				t.Errorf("%s: Containers = %v, want the lb-big_default endpoint", r.name, got.Containers)
			}
		}
		// scope=swarm does not change the rule.
		if s := statusOf(inspectNetwork(t, f, fakeBigID+"?scope=swarm")); s[subnet] != localCount[subnet] {
			t.Errorf("full ID with scope=swarm: status = %v, want the node's own %v", s, localCount)
		}
	})
	t.Run("only local usage set", func(t *testing.T) {
		f := dockertest.NewFake(t, fakeNetManager())
		f.SetNetworkLocalUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 46, Available: 210})
		for _, r := range refs {
			if s := statusOf(inspectNetwork(t, f, r.ref)); s[subnet] != localCount[subnet] {
				t.Errorf("%s: status = %v, want the node's own %v", r.name, s, localCount)
			}
		}
	})
	t.Run("no swarm status", func(t *testing.T) {
		f := dockertest.NewFake(t, fakeNetManager())
		both(f)
		f.SetNetworkNoSwarmStatus("big_default", true)
		if s := statusOf(inspectNetwork(t, f, fakeBigID)); s[subnet] != localCount[subnet] {
			t.Errorf("full ID: status = %v, want the node's own %v", s, localCount)
		}
		for _, ref := range []string{"big_default", fakeBigID[:len(fakeBigID)-1]} {
			if got := inspectNetwork(t, f, ref); got.Status != nil {
				t.Errorf("%s: status = %+v, want none", ref, got.Status)
			}
		}
		f.SetNetworkNoSwarmStatus("big_default", false)
		if s := statusOf(inspectNetwork(t, f, "big_default")); s[subnet] != swarmCount[subnet] {
			t.Errorf("name after turning it off: status = %v, want %v", s, swarmCount)
		}
	})
	t.Run("local usage removed", func(t *testing.T) {
		f := dockertest.NewFake(t, fakeNetManager())
		both(f)
		f.SetNetworkLocalUsage("big_default")
		got := inspectNetwork(t, f, fakeBigID)
		if s := statusOf(got); s[subnet] != swarmCount[subnet] {
			t.Errorf("full ID: status = %v, want the swarm's %v", s, swarmCount)
		}
		if _, ok := got.Containers["lb-big_default"]; ok {
			t.Errorf("Containers = %v, want no load-balancer endpoint", got.Containers)
		}
	})
	t.Run("neither set", func(t *testing.T) {
		f := dockertest.NewFake(t, fakeNetManager())
		for _, r := range refs {
			if got := inspectNetwork(t, f, r.ref); got.Status != nil {
				t.Errorf("%s: status = %+v, want none", r.name, got.Status)
			}
		}
	})
}

func TestTheFakeStoresTheNetworkIDsAServiceWriteNamesWhenAskedTo(t *testing.T) {
	cfg := func(byID bool) dockertest.FakeConfig {
		return dockertest.FakeConfig{
			SwarmState: "active", SwarmControl: true, NetworkTargetsByID: byID,
			Networks: []dockertest.FakeNetwork{{ID: "ovl-id-1", Name: "ovl", Driver: "overlay", Scope: "swarm"}},
		}
	}
	targets := func(names ...string) swarm.ServiceSpec {
		spec := swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "web"}}
		for _, n := range names {
			spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: n})
		}
		return spec
	}
	post := func(t *testing.T, f *dockertest.Fake, path string, spec swarm.ServiceSpec) {
		t.Helper()
		body, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(awaitCtx(t), http.MethodPost, "http://engine/v1.52"+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := fakeClient(f).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("POST %s = %d", path, resp.StatusCode)
		}
	}
	read := func(t *testing.T, f *dockertest.Fake) []string {
		t.Helper()
		req, err := http.NewRequestWithContext(awaitCtx(t), http.MethodGet, "http://engine/v1.52/services/svcnew01", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := fakeClient(f).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var svc swarm.Service
		if err := json.NewDecoder(resp.Body).Decode(&svc); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, n := range svc.Spec.TaskTemplate.Networks {
			got = append(got, n.Target)
		}
		return got
	}
	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	t.Run("with the knob", func(t *testing.T) {
		f := dockertest.NewFake(t, cfg(true))
		post(t, f, "/services/create", targets("ovl", "gone"))
		if got := read(t, f); !equal(got, []string{"ovl-id-1", "gone"}) {
			t.Errorf("targets after create = %v, want [ovl-id-1 gone]", got)
		}
		if got := f.ServiceCreates()[0].Spec.TaskTemplate.Networks[0].Target; got != "ovl" {
			t.Errorf("recorded create target = %q, want the request as sent", got)
		}
		post(t, f, "/services/svcnew01/update?version=1", targets("ovl"))
		if got := read(t, f); !equal(got, []string{"ovl-id-1"}) {
			t.Errorf("targets after update = %v, want [ovl-id-1]", got)
		}
		if got := f.ServiceUpdates()[0].Spec.TaskTemplate.Networks[0].Target; got != "ovl" {
			t.Errorf("recorded update target = %q, want the request as sent", got)
		}
	})
	t.Run("without the knob", func(t *testing.T) {
		f := dockertest.NewFake(t, cfg(false))
		post(t, f, "/services/create", targets("ovl"))
		if got := read(t, f); !equal(got, []string{"ovl"}) {
			t.Errorf("targets = %v, want the name as written", got)
		}
	})
}
