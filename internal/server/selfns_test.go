// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

// TestSelfFactsFromContainerLabels reads TierPlan's own swarm service, stack and networks from one container's inspect (D-03, D-05).
func TestSelfFactsFromContainerLabels(t *testing.T) {
	net := func(name string) dockerx.NetAttachment {
		return dockerx.NetAttachment{ContainerNet: dockerx.ContainerNet{Name: name}}
	}
	t.Run("a swarm task", func(t *testing.T) {
		got := selfFactsOf(dockerx.ContainerDetail{
			ID: "c1",
			Labels: map[string]string{
				"com.docker.swarm.service.name": "tierplan_tierplan",
				"com.docker.swarm.service.id":   "svc1",
				"com.docker.stack.namespace":    "tierplan",
			},
			Networks: []dockerx.NetAttachment{net("tierplan_default"), net("tierplan-self")},
		})
		if !got.Swarm || got.ContainerID != "c1" || got.ServiceName != "tierplan_tierplan" || got.ServiceID != "svc1" || got.StackNamespace != "tierplan" {
			t.Errorf("facts = %+v, want the service, ID and namespace of the labels", got)
		}
		if want := []string{"tierplan_default", "tierplan-self"}; !slices.Equal(got.Networks, want) {
			t.Errorf("networks = %v, want %v", got.Networks, want)
		}
	})
	t.Run("a plain container", func(t *testing.T) {
		got := selfFactsOf(dockerx.ContainerDetail{ID: "c2", Networks: []dockerx.NetAttachment{net("bridge")}})
		if got.Swarm || got.ServiceName != "" || got.ServiceID != "" || got.StackNamespace != "" {
			t.Errorf("facts = %+v, want no swarm facts without labels", got)
		}
		if got.ContainerID != "c2" || !slices.Equal(got.Networks, []string{"bridge"}) {
			t.Errorf("facts = %+v, want the container and its network", got)
		}
	})
}

// A start-up read that cannot be made answers an error, which readSelfNamespace turns into no namespace.
func TestReadSelfFactsNeedsContainerAndSocket(t *testing.T) {
	if _, err := readSelfFacts(t.Context(), "", "c1"); err == nil {
		t.Error("no socket: want an error")
	}
	if _, err := readSelfFacts(t.Context(), "/nonexistent.sock", ""); err == nil {
		t.Error("no container: want an error")
	}
}
