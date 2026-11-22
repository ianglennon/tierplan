// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rebuild_test

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks/rebuild"
)

// TestRebuildDropsIngressAttachment is D-20: a service that carries TierPlan's ingress attachment is rebuilt without
// it, and the network is declared nowhere, so no compose file TierPlan writes pins one of its own objects.
func TestRebuildDropsIngressAttachment(t *testing.T) {
	t.Parallel()
	networks := []rebuild.NetworkInfo{
		{ID: "n1", Name: "shop_default", Driver: "overlay", Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
		{ID: "n2", Name: ingress.NetworkName, Driver: "overlay", Labels: map[string]string{ingress.LabelComponent: "network"}},
	}

	t.Run("beside the default network", func(t *testing.T) {
		t.Parallel()
		svc := coreService()
		svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{
			{Target: "shop_default", Aliases: []string{"web"}},
			{Target: ingress.NetworkName},
		}
		res, err := rebuild.Swarm(rebuild.SwarmInput{Namespace: "shop", Services: []rebuild.SwarmService{svc}, Networks: networks})
		if err != nil {
			t.Fatalf("Swarm: %v", err)
		}
		if strings.Contains(string(res.YAML), ingress.NetworkName) {
			t.Errorf("the file names %s:\n%s", ingress.NetworkName, res.YAML)
		}
		if strings.Contains(string(res.YAML), "networks:") {
			t.Errorf("the file has a networks block, which a service on the default network alone would not:\n%s", res.YAML)
		}
		for _, o := range res.Objects {
			if o.Name == ingress.NetworkName {
				t.Errorf("objects = %+v, want the ingress network not among them", res.Objects)
			}
		}
		if _, ok := res.Project.Networks[ingress.NetworkName]; ok {
			t.Errorf("the project declares %s", ingress.NetworkName)
		}
	})

	t.Run("beside a network of the stack's own", func(t *testing.T) {
		t.Parallel()
		svc := coreService()
		svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "shop_default", Aliases: []string{"web"}}, {Target: "shared"}, {Target: ingress.NetworkName}}
		res, err := rebuild.Swarm(rebuild.SwarmInput{
			Namespace: "shop", Services: []rebuild.SwarmService{svc},
			Networks: append(networks, rebuild.NetworkInfo{ID: "n3", Name: "shared", Driver: "overlay"}),
		})
		if err != nil {
			t.Fatalf("Swarm: %v", err)
		}
		if strings.Contains(string(res.YAML), ingress.NetworkName) {
			t.Errorf("the file names %s:\n%s", ingress.NetworkName, res.YAML)
		}
		if !strings.Contains(string(res.YAML), "shared") {
			t.Errorf("the file lost the other network:\n%s", res.YAML)
		}
	})

	t.Run("the only attachment", func(t *testing.T) {
		t.Parallel()
		svc := coreService()
		svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: ingress.NetworkName}}
		res, err := rebuild.Swarm(rebuild.SwarmInput{Namespace: "shop", Services: []rebuild.SwarmService{svc}, Networks: networks})
		if err != nil {
			t.Fatalf("Swarm: %v", err)
		}
		if strings.Contains(string(res.YAML), ingress.NetworkName) || strings.Contains(string(res.YAML), "networks:") {
			t.Errorf("the file has a network for a service whose only attachment is TierPlan's:\n%s", res.YAML)
		}
	})
}
