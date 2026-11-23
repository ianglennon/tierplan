// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// routedAndPlainStandalone is a stack of a standalone host whose web service declares a route (under labels) and whose db
// service does not.
const routedAndPlainStandalone = `services:
  web:
    image: nginx:1.27-alpine
    labels:
      tierplan.ingress.host: app.apps.example.com
      tierplan.ingress.port: "80"
  db:
    image: postgres:16-alpine
`

// plainStandalone declares no route.
const plainStandalone = "services:\n  web:\n    image: nginx:1.27-alpine\n"

// answer is what one ask of the ingress model returns.
type answer struct {
	attach *stacks.IngressAttach
	err    error
}

// sequencedIngress answers each AttachFor with the next of its answers, the last one again when they run out, so a test
// can say what the switch read is at the gate and at the job.
type sequencedIngress struct {
	fakeIngress
	mu      sync.Mutex
	answers []answer
}

func (s *sequencedIngress) AttachFor(context.Context, string, stacks.Target) (*stacks.IngressAttach, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks++
	a := s.answers[0]
	if len(s.answers) > 1 {
		s.answers = s.answers[1:]
	}
	return a.attach, a.err
}

func (s *sequencedIngress) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asks
}

// TestStandaloneDeployAttachesWhenIngressEnabled is the tracer of the standalone attach (D-16): with ingress on, the SDK
// is handed a project in which the routed service is on the shared network with its alias and still on its default
// network, and the service with no route is where it was.
func TestStandaloneDeployAttachesWhenIngressEnabled(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	rig.deps.Ingress = &fakeIngress{attach: attachedBy()}

	res, job := rig.deployStandalone(t, standaloneInput("demo", routedAndPlainStandalone))

	if !res.OK() || job.State != jobs.JobDone || stand.calls != 1 {
		t.Fatalf("result %+v, job %s %v, up called %d times", res, job.State, itemStates(job), stand.calls)
	}
	web := stand.project.Services["web"]
	cfg, joined := web.Networks[ingress.NetworkName]
	if !joined || cfg == nil || !slices.Equal(cfg.Aliases, []string{ingress.Alias("demo", "web")}) {
		t.Errorf("web joins %+v, want %s with exactly the alias %s", web.Networks, ingress.NetworkName, ingress.Alias("demo", "web"))
	}
	if _, def := web.Networks["default"]; !def {
		t.Errorf("web is off its default network: %v", web.Networks)
	}
	if _, on := stand.project.Services["db"].Networks[ingress.NetworkName]; on {
		t.Error("a service with no route joined the ingress network")
	}
	if n, ok := stand.project.Networks[ingress.NetworkName]; !ok || !bool(n.External) {
		t.Errorf("the project's %s = %+v (declared %v), want an external network", ingress.NetworkName, n, ok)
	}
	if got := res.CreatedNetworks; slices.Contains(got, ingress.NetworkName) {
		t.Errorf("the deploy says it created the shared network: %v", got)
	}
}

// TestStandaloneDeployUnattachedWhenIngressOff: with ingress off, the project has the network nowhere.
func TestStandaloneDeployUnattachedWhenIngressOff(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	rig.deps.Ingress = &fakeIngress{}

	res, _ := rig.deployStandalone(t, standaloneInput("demo", routedAndPlainStandalone))

	if !res.OK() || stand.calls != 1 {
		t.Fatalf("result %+v, up called %d times", res, stand.calls)
	}
	if _, declared := stand.project.Networks[ingress.NetworkName]; declared {
		t.Error("the project declares the ingress network with ingress off")
	}
	for name, svc := range stand.project.Services {
		if _, on := svc.Networks[ingress.NetworkName]; on {
			t.Errorf("service %s joined the ingress network with ingress off", name)
		}
	}
}

// TestStandaloneDeployAttachReadFailure: the job reads the switch itself, so a switch that cannot be read refuses a
// stack with a routed service at the check and writes nothing, and a stack with none deploys unattached (T-25-23).
func TestStandaloneDeployAttachReadFailure(t *testing.T) {
	t.Parallel()

	t.Run("a routed stack is refused", func(t *testing.T) {
		t.Parallel()
		rig := newStandaloneRig(t)
		stand := &upStandIn{}
		rig.deps.Up = stand.up
		ing := &sequencedIngress{answers: []answer{{err: errors.New("the ingress store is down")}}}
		rig.deps.Ingress = ing

		res, job := rig.deployStandalone(t, standaloneInput("demo", routedAndPlainStandalone))

		if res.OK() || res.Code != stackops.CodePolicyUnavailable || stand.calls != 0 || res.Wrote {
			t.Fatalf("result %+v, up called %d times, want a refusal with policy_unavailable and nothing written", res, stand.calls)
		}
		if it := itemByID(t, job, stackops.ItemCheck); it.State != jobs.ItemFailed || it.Code != stackops.CodePolicyUnavailable {
			t.Errorf("check item = %s %q, want failed with policy_unavailable", it.State, it.Code)
		}
		if len(rig.fake.NetworkCreates()) != 0 || len(rig.fake.ServiceCreates()) != 0 {
			t.Error("a refused deploy wrote to the host")
		}
	})
	t.Run("a stack with no route deploys unattached", func(t *testing.T) {
		t.Parallel()
		rig := newStandaloneRig(t)
		stand := &upStandIn{}
		rig.deps.Up = stand.up
		ing := &sequencedIngress{answers: []answer{{err: errors.New("the ingress store is down")}}}
		rig.deps.Ingress = ing

		res, _ := rig.deployStandalone(t, standaloneInput("demo", plainStandalone))

		if !res.OK() || stand.calls != 1 {
			t.Fatalf("result %+v, up called %d times, want the deploy to go through", res, stand.calls)
		}
		if _, declared := stand.project.Networks[ingress.NetworkName]; declared {
			t.Error("the project declares the ingress network after a failed read")
		}
		if ing.asked() != 1 {
			t.Errorf("the job asked the switch %d times, want once", ing.asked())
		}
	})
	t.Run("the job's read is the one that decides", func(t *testing.T) {
		t.Parallel()
		rig := newStandaloneRig(t)
		stand := &upStandIn{}
		rig.deps.Up = stand.up
		// The switch reads as on for the first deploy and as off for the second: each job decides on its own read.
		rig.deps.Ingress = &sequencedIngress{answers: []answer{{attach: attachedBy()}, {}}}
		in := standaloneInput("demo", routedAndPlainStandalone)

		first, _ := rig.deployStandalone(t, in)
		if !first.OK() {
			t.Fatalf("first deploy: %+v", first)
		}
		if _, on := stand.project.Services["web"].Networks[ingress.NetworkName]; !on {
			t.Fatal("the first deploy, with the switch on, did not attach")
		}
		second, _ := rig.deployStandalone(t, in)
		if !second.OK() {
			t.Fatalf("second deploy: %+v", second)
		}
		if _, on := stand.project.Services["web"].Networks[ingress.NetworkName]; on {
			t.Error("the second deploy, with the switch off, still attached")
		}
	})
}
