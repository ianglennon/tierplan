// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rebuild_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/rebuild"
)

// The renderings of an existing network or volume that a compose file could declare (plan 08-12, D-46). The
// host keeps a hash of the config the project declared; which rendering it was cannot be read from the object,
// so the caller tries them, fewest settings first, against that hash.

func yamlOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func bridgeWithEverything() rebuild.NetworkInfo {
	return rebuild.NetworkInfo{
		ID: "n1", Name: "shop_back", Driver: "bridge",
		Options: map[string]string{"com.docker.network.bridge.name": "br-shop"},
		Pools:   []rebuild.Pool{{Subnet: "172.28.0.0/16", Gateway: "172.28.0.1"}},
		Labels: map[string]string{
			"com.docker.compose.project": "shop", "com.docker.compose.network": "back",
			"com.docker.compose.config-hash": "abc", "team": "web", "tierplan.stack.id": "x",
		},
	}
}

func TestNetworkCandidatesAreOrderedFromTheMinimalToTheMostFaithful(t *testing.T) {
	t.Parallel()

	cands := rebuild.NetworkCandidates(bridgeWithEverything(), "shop", "back")

	// An explicit driver bridge or none, driver options or none, no address pool, its subnet, or its subnet and
	// gateway: twelve renderings.
	if len(cands) != 12 {
		t.Fatalf("%d candidates, want 12", len(cands))
	}
	first, last := yamlOf(t, cands[0]), yamlOf(t, cands[len(cands)-1])
	for _, unwanted := range []string{"driver", "ipam", "subnet", "gateway", "name:"} {
		if strings.Contains(first, unwanted) {
			t.Errorf("the minimal rendering has %q:\n%s", unwanted, first)
		}
	}
	if !strings.Contains(first, "team: web") {
		t.Errorf("the minimal rendering lacks the label a person set:\n%s", first)
	}
	for _, want := range []string{"driver: bridge", "driver_opts:", "com.docker.network.bridge.name: br-shop", "subnet: 172.28.0.0/16", "gateway: 172.28.0.1", "team: web"} {
		if !strings.Contains(last, want) {
			t.Errorf("the most faithful rendering lacks %q:\n%s", want, last)
		}
	}
	for _, unwanted := range []string{"com.docker.compose", "tierplan"} {
		for i, c := range cands {
			if y := yamlOf(t, c); strings.Contains(y, unwanted) {
				t.Errorf("candidate %d carries the label %q of a tool:\n%s", i, unwanted, y)
			}
		}
	}
	seen := map[string]bool{}
	for i, c := range cands {
		y := yamlOf(t, c)
		if seen[y] {
			t.Errorf("candidate %d is a copy of an earlier one:\n%s", i, y)
		}
		seen[y] = true
	}
	// Fewer settings never come after more.
	settings := func(c types.NetworkConfig) int {
		n := 0
		if c.Driver != "" {
			n++
		}
		if len(c.DriverOpts) > 0 {
			n++
		}
		if len(c.Ipam.Config) > 0 {
			n += 1 + strings.Count(yamlOf(t, c.Ipam), "gateway")
		}
		return n
	}
	counts := make([]int, len(cands))
	for i, c := range cands {
		counts[i] = settings(c)
	}
	if !slices.IsSorted(counts) {
		t.Errorf("settings per candidate = %v, want fewest first", counts)
	}
}

func TestNetworkCandidatesKeepWhatTheObjectMustHave(t *testing.T) {
	t.Parallel()
	info := rebuild.NetworkInfo{
		ID: "n1", Name: "lan", Driver: "macvlan", Internal: true, Attachable: true, IPv6: true,
		Labels: map[string]string{"com.docker.compose.project": "shop"},
	}

	cands := rebuild.NetworkCandidates(info, "shop", "back")

	if len(cands) != 1 {
		t.Fatalf("%d candidates, want 1: nothing about this network is optional", len(cands))
	}
	y := yamlOf(t, cands[0])
	for _, want := range []string{"name: lan", "driver: macvlan", "internal: true", "attachable: true", "enable_ipv6: true"} {
		if !strings.Contains(y, want) {
			t.Errorf("the rendering lacks %q:\n%s", want, y)
		}
	}
}

func TestVolumeCandidates(t *testing.T) {
	t.Parallel()
	info := rebuild.VolumeInfo{
		Name: "shop_data", Driver: "local",
		Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.5"},
		Labels:  map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.volume": "data", "team": "db"},
	}

	cands := rebuild.VolumeCandidates(info, "shop", "data")

	if len(cands) != 2 {
		t.Fatalf("%d candidates, want 2: with the driver options and without", len(cands))
	}
	first, last := yamlOf(t, cands[0]), yamlOf(t, cands[1])
	if strings.Contains(first, "driver") || strings.Contains(first, "name:") || !strings.Contains(first, "team: db") {
		t.Errorf("the minimal rendering is:\n%s\nwant the labels a person set and nothing else (driver local is the default)", first)
	}
	for _, want := range []string{"driver_opts:", "type: nfs", "o: addr=10.0.0.5", "team: db"} {
		if !strings.Contains(last, want) {
			t.Errorf("the most faithful rendering lacks %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "com.docker.compose") {
		t.Errorf("the rendering carries the SDK's own labels:\n%s", last)
	}
}

func TestCandidatesWriteTheNameOnlyWhenItIsNotTheProjects(t *testing.T) {
	t.Parallel()

	nets := rebuild.NetworkCandidates(rebuild.NetworkInfo{Name: "shop-net"}, "shop", "back")
	vols := rebuild.VolumeCandidates(rebuild.VolumeInfo{Name: "shop_data", Driver: "local"}, "shop", "data")

	if y := yamlOf(t, nets[0]); !strings.Contains(y, "name: shop-net") {
		t.Errorf("a network with a name of its own is written without it:\n%s", y)
	}
	if y := yamlOf(t, vols[0]); strings.Contains(y, "name") {
		t.Errorf("a volume named as the project names it is written with a name:\n%s", y)
	}
}

// TestCandidatesKeepKeepLabel: a network or volume that carries tierplan.keep=true keeps the label when it is rebuilt
// as a declaration, so adopting the stack does not drop the user's opt-out (JAN-07). The bookkeeping labels of TierPlan
// and the SDK are still left out.
func TestCandidatesKeepKeepLabel(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"com.docker.compose.project": "shop", "tierplan.stack.id": "x", stacks.LabelKeep: "true", "team": "web",
	}
	want := types.Labels{stacks.LabelKeep: "true", "team": "web"}

	nets := rebuild.NetworkCandidates(rebuild.NetworkInfo{ID: "n1", Name: "shop_back", Driver: "bridge", Labels: labels}, "shop", "back")
	if len(nets) == 0 {
		t.Fatal("no network candidates")
	}
	for i, c := range nets {
		if !maps.Equal(c.Labels, want) {
			t.Errorf("network candidate %d labels = %v, want %v", i, c.Labels, want)
		}
	}

	vol := rebuild.VolumeCandidates(rebuild.VolumeInfo{Name: "shop_data", Driver: "local", Labels: labels}, "shop", "data")
	if len(vol) == 0 {
		t.Fatal("no volume candidates")
	}
	for i, c := range vol {
		if !maps.Equal(c.Labels, want) {
			t.Errorf("volume candidate %d labels = %v, want %v", i, c.Labels, want)
		}
	}
}
