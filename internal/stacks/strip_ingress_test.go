// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

func specWithNetworks(targets ...string) swarm.ServiceSpec {
	var spec swarm.ServiceSpec
	for _, t := range targets {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: t, Aliases: []string{"a-" + t}})
	}
	return spec
}

func targetsOf(spec swarm.ServiceSpec) []string {
	var out []string
	for _, n := range spec.TaskTemplate.Networks {
		out = append(out, n.Target)
	}
	return out
}

// TestStripIngressAttachment is D-20: the attachment is dropped by name, or by the network's ID when one is given,
// every other attachment stays, and the spec given is not changed.
func TestStripIngressAttachment(t *testing.T) {
	t.Parallel()

	t.Run("by name", func(t *testing.T) {
		t.Parallel()
		in := specWithNetworks("shop_default", ingress.NetworkName, "shop_back")
		got := stacks.StripIngressAttachment(in)
		if want := []string{"shop_default", "shop_back"}; !reflect.DeepEqual(targetsOf(got), want) {
			t.Errorf("targets = %v, want %v", targetsOf(got), want)
		}
		if want := []string{"shop_default", ingress.NetworkName, "shop_back"}; !reflect.DeepEqual(targetsOf(in), want) {
			t.Errorf("the input has targets %v, want it unchanged", targetsOf(in))
		}
	})

	t.Run("by ID", func(t *testing.T) {
		t.Parallel()
		in := specWithNetworks("net-default", "net-ingress")
		if got := stacks.StripIngressAttachment(in); !reflect.DeepEqual(targetsOf(got), []string{"net-default", "net-ingress"}) {
			t.Errorf("without an ID given, targets = %v, want both kept: an ID alone says nothing", targetsOf(got))
		}
		got := stacks.StripIngressAttachment(in, "net-ingress")
		if want := []string{"net-default"}; !reflect.DeepEqual(targetsOf(got), want) {
			t.Errorf("targets = %v, want %v", targetsOf(got), want)
		}
		if len(in.TaskTemplate.Networks) != 2 {
			t.Errorf("the input has %d attachments, want 2", len(in.TaskTemplate.Networks))
		}
	})

	t.Run("only the ingress attachment", func(t *testing.T) {
		t.Parallel()
		if got := stacks.StripIngressAttachment(specWithNetworks(ingress.NetworkName)); len(got.TaskTemplate.Networks) != 0 {
			t.Errorf("attachments = %v, want none", got.TaskTemplate.Networks)
		}
	})

	t.Run("none to strip", func(t *testing.T) {
		t.Parallel()
		in := specWithNetworks("shop_default")
		if got := stacks.StripIngressAttachment(in, ""); !reflect.DeepEqual(got, in) {
			t.Errorf("spec = %+v, want it as it was", got)
		}
		if got := stacks.StripIngressAttachment(swarm.ServiceSpec{}); got.TaskTemplate.Networks != nil {
			t.Errorf("attachments = %v, want nil", got.TaskTemplate.Networks)
		}
	})
}
