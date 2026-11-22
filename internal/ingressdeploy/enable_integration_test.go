// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestEnableOnSwarm is the tracer of the phase (SC 1, D-10): Enable on a swarm of a manager and a worker makes the join
// token secret, the non-attachable overlay and the global proxy service, which runs one task per node with a cache
// volume on each; every proxy enrols with the hub and the status reads nodes expected 2, enrolled 2, ready 2. It then
// shows on the real engine that a second ensure pass writes nothing: the swarm's own defaults on the service spec are
// all learned by the comparison, or the service would be updated every minute.
func TestEnableOnSwarm(t *testing.T) {
	e := startSwarmEnv(t)

	job := e.enable()
	requireJobDone(t, job)
	st := e.awaitReady()
	if !st.Enabled || !st.ObjectsKnown || !st.Objects.Network || !st.Objects.Service || !st.Objects.Secret {
		t.Fatalf("status after enable = %+v, want ingress on with the network, the service and the secret", st)
	}

	// The network is a swarm overlay that is not attachable (S5, D-05), with TierPlan's label.
	if got := strings.TrimSpace(e.sh(e.mgr, "docker network inspect -f '{{.Driver}} {{.Scope}} {{.Attachable}} {{index .Labels \""+ingress.LabelComponent+"\"}}' "+ingress.NetworkName)); got != "overlay swarm false "+ingress.ComponentIngress {
		t.Errorf("the ingress network is %q, want an overlay of swarm scope, not attachable, with the component label", got)
	}
	// Every node holds the certificate cache volume, with the component label.
	for _, n := range e.sw.Nodes() {
		if present, labelled := e.hasCacheVolume(n); !present || !labelled {
			t.Errorf("%s: cache volume present=%v labelled=%v, want both", n.Hostname, present, labelled)
		}
	}
	// The service is global with one running task on each node.
	d, ok := e.proxyService()
	if !ok || d.Mode != "global" {
		t.Fatalf("the proxy service = %+v (found %v), want a global service", d.Mode, ok)
	}
	if got := strings.Fields(e.sh(e.mgr, "docker service ps --filter desired-state=running --format '{{.Node}}' "+ingress.ServiceName)); len(got) != 2 || got[0] == got[1] {
		t.Errorf("the proxy runs on %v, want one task on each of the two nodes", got)
	}

	// A second ensure pass writes nothing (24-13): the service keeps its version, its update status and its secrets.
	svc0, net0, sec0 := e.objects()
	for pass := 1; pass <= 2; pass++ {
		e.deploy.Round(testsupport.Context(t))
		after, ok := e.proxyService()
		if !ok || after.Version != d.Version {
			t.Fatalf("ensure pass %d changed the proxy service: version %d -> %d (found %v); the comparison does not know a default the engine adds", pass, d.Version, after.Version, ok)
		}
		if !slices.Equal([]string{string(after.Spec)}, []string{string(d.Spec)}) {
			t.Fatalf("ensure pass %d changed the proxy service's spec", pass)
		}
		if s, n, sc := e.objects(); s != svc0 || n != net0 || sc != sec0 {
			t.Fatalf("ensure pass %d changed the swarm's objects: service %d->%d network %d->%d secrets %d->%d", pass, svc0, s, net0, n, sec0, sc)
		}
	}
	if after, err := e.deploy.Status(testsupport.Context(t), e.envID); err != nil || after.LastEnsure.Class != ingressdeploy.EnsureOK || after.LastEnsure.At.IsZero() {
		t.Errorf("last ensure = %+v (%v), want a recorded pass that succeeded", after.LastEnsure, err)
	}
}

// TestImageChangeRollsStopFirst is D-11: a server whose version names another proxy image makes the ensure loop update the
// service, and the swarm rolls it stop-first, one node at a time, to a completed update.
func TestImageChangeRollsStopFirst(t *testing.T) {
	e := startSwarmEnv(t)
	requireJobDone(t, e.enable())
	e.awaitReady()

	const next = "v0.2.0"
	nextImage := ingressdeploy.IngressImage(next)
	for _, n := range e.sw.Nodes() {
		e.sh(n, "docker tag "+devIngressImage+" "+nextImage)
	}
	e.startDeploy(next)
	e.deploy.Wake()

	dockertest.Wait(t, rollBound, "the proxy service's update to the new image to complete", func() (bool, string) {
		d, ok := e.proxyService()
		switch {
		case !ok:
			return false, "no proxy service"
		case d.UpdateStatus == nil:
			return false, "no update yet (image " + d.Image + ")"
		case d.UpdateStatus.State == "rollback_started" || d.UpdateStatus.State == "rollback_completed" || d.UpdateStatus.State == "paused":
			t.Fatalf("the update ended %s: %s", d.UpdateStatus.State, d.UpdateStatus.Message)
		}
		return d.UpdateStatus.State == "completed" && strings.HasPrefix(d.Image, nextImage), d.UpdateStatus.State + " " + d.Image
	})

	d, _ := e.proxyService()
	var spec swarm.ServiceSpec
	if err := jsonUnmarshal(d.Spec, &spec); err != nil {
		t.Fatalf("read the proxy service's spec: %v", err)
	}
	if spec.UpdateConfig == nil || spec.UpdateConfig.Order != swarm.UpdateOrderStopFirst || spec.UpdateConfig.Parallelism != 1 {
		t.Errorf("update config = %+v, want order stop-first and parallelism 1", spec.UpdateConfig)
	}
	if spec.TaskTemplate.ContainerSpec == nil || !strings.HasPrefix(spec.TaskTemplate.ContainerSpec.Image, nextImage) {
		t.Errorf("the service's image = %+v, want %s", spec.TaskTemplate.ContainerSpec, nextImage)
	}
	// The new tasks enrol again and read ready.
	e.awaitReady()
}
