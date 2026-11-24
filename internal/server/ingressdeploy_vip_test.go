// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// signalClock tells a test every wait a loop starts, and never ends one, so the loop runs a single round and then blocks.
type signalClock struct {
	seen chan time.Duration
}

func (c *signalClock) Now() time.Time { return time.Now() }

func (c *signalClock) After(d time.Duration) <-chan time.Time {
	select {
	case c.seen <- d:
	default:
	}
	return nil
}

// The server gives the deployer the cluster reader as the table of the managers' privileged-services opt-in (D-19), and
// starts the reader of the virtual IP's moves with the deployer's other loops (D-21): the loop list holds it, and running that
// entry takes a round of the reader, which then waits the reader's own interval.
func TestNewIngressDeployWiresTheVIP(t *testing.T) {
	ctx := testsupport.Context(t)
	log := slog.New(slog.DiscardHandler)
	srv, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": filepath.Join(t.TempDir(), "data")}), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	notifier := ingress.NewNotifier()
	clock := &signalClock{seen: make(chan time.Duration, 4)}
	deps := ingressDeployDeps{
		Log: log, Store: srv.store, Audit: srv.audit, Aggregator: srv.aggregator, Clusters: srv.clusters, Envs: srv.envs,
		Ingress:  ingressstore.New(ingressstore.Deps{St: srv.store, Vault: srv.vault, Notifier: notifier}),
		Notifier: notifier, Tokens: ingresshub.NewService(srv.store, srv.audit, time.Now), Jobs: srv.jobs,
		Locks: stackops.NewLocks(), Resolver: srv.resolver, Instance: srv.inst, Clock: clock,
		AddressSpaces: envs.NewAddressSpaces(srv.envs, srv.aggregator, srv.clusters, envs.AddressSpaceOptions{Log: log}),
	}

	got := ingressDeployOf(deps).Managers
	if got == nil {
		t.Fatal("the deployer has no manager table to read")
	}
	if c, ok := got.(*envs.Clusters); !ok || c != srv.clusters {
		t.Errorf("Managers = %T %v, want the server's cluster reader", got, got)
	}
	// A server without a cluster reader leaves the table out altogether and not a nil pointer in an interface.
	deps.Clusters = nil
	if m := ingressDeployOf(deps).Managers; m != nil {
		t.Errorf("Managers without a cluster reader = %v, want nil", m)
	}
	deps.Clusters = srv.clusters

	svc := newIngressDeploy(deps)
	var names []string
	loops := ingressDeployLoops(svc)
	for _, l := range loops {
		names = append(names, l.Name)
	}
	if !slices.Contains(names, "vip_reader") || !slices.Contains(names, "ensure") || !slices.Contains(names, "wipes") {
		t.Fatalf("the server's ingress loops are %v, want ensure, wipes and vip_reader", names)
	}

	// Running the reader's entry takes a round and then waits the reader's interval.
	i := slices.IndexFunc(loops, func(l ingressLoop) bool { return l.Name == "vip_reader" })
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		loops[i].Run(runCtx)
	}()
	select {
	case d := <-clock.seen:
		if d != ingressdeploy.VIPReadInterval {
			t.Errorf("the loop waited %v after its round, want the reader's interval %v", d, ingressdeploy.VIPReadInterval)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the reader's loop never finished a round")
	}
	cancel()
	<-done
}
