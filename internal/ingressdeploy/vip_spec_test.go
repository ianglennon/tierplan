// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"maps"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// keepalivedInput is the fixed input of the keepalived spec in the tests and in the policy vector.
func keepalivedInput() ingressdeploy.KeepalivedSpecInput {
	return ingressdeploy.KeepalivedSpecInput{
		Image:      "ghcr.io/ianglennon/tierplan-keepalived:edge",
		Address:    netip.MustParsePrefix("192.168.1.50/24"),
		RouterID:   77,
		SecretName: "tierplan-keepalived-key-g1-abcd",
	}
}

// TestKeepalivedServiceSpec pins every field of the keepalived service to what S3 measured, with
// literals: a change to one is a change to what every enabled swarm runs, and rolls every task.
func TestKeepalivedServiceSpec(t *testing.T) {
	spec := ingressdeploy.KeepalivedServiceSpec(keepalivedInput())

	if spec.Name != "tierplan-keepalived" {
		t.Errorf("service name = %q", spec.Name)
	}
	wantLabels := map[string]string{
		"tierplan.component": "keepalived",
		"tierplan.keep":      "true",
		"tierplan.updates":   "ignore",
	}
	if !maps.Equal(spec.Labels, wantLabels) {
		t.Errorf("service labels = %v, want %v", spec.Labels, wantLabels)
	}
	if spec.Mode.Global == nil || spec.Mode.Replicated != nil {
		t.Errorf("mode = %+v, want global", spec.Mode)
	}
	if spec.EndpointSpec != nil {
		t.Errorf("EndpointSpec = %+v, want none: a host-network service publishes no port", spec.EndpointSpec)
	}

	tt := spec.TaskTemplate
	cs := tt.ContainerSpec
	if cs == nil {
		t.Fatal("no container spec")
	}
	if cs.Image != "ghcr.io/ianglennon/tierplan-keepalived:edge" {
		t.Errorf("image = %q", cs.Image)
	}
	if !maps.Equal(cs.Labels, wantLabels) {
		t.Errorf("container labels = %v, want %v (service labels do not reach the task)", cs.Labels, wantLabels)
	}
	// The container's label map is its own: a change to it must not reach the service's.
	cs.Labels["probe"] = "x"
	if spec.Labels["probe"] != "" {
		t.Error("the container labels share a map with the service labels")
	}
	if cs.User != "" {
		t.Errorf("User = %q, want none: keepalived runs as root with only the four capabilities", cs.User)
	}
	if cs.Healthcheck != nil {
		t.Errorf("Healthcheck = %+v, want none (A-08)", cs.Healthcheck)
	}
	if !cs.ReadOnly {
		t.Error("the root filesystem is not read-only")
	}
	if !slices.Equal(cs.CapabilityDrop, []string{"ALL"}) {
		t.Errorf("CapabilityDrop = %v, want [ALL]", cs.CapabilityDrop)
	}
	// S3 caps: NET_ADMIN,NET_RAW,SETUID,SETGID (SETUID and SETGID because the check is a vrrp_script, A-01)
	if want := []string{"NET_ADMIN", "NET_RAW", "SETUID", "SETGID"}; !slices.Equal(cs.CapabilityAdd, want) {
		t.Errorf("CapabilityAdd = %v, want %v", cs.CapabilityAdd, want)
	}
	if want := []mount.Mount{{Type: mount.TypeTmpfs, Target: "/run"}, {Type: mount.TypeTmpfs, Target: "/tmp"}}; !reflect.DeepEqual(cs.Mounts, want) {
		t.Errorf("Mounts = %+v, want only tmpfs on /run and /tmp", cs.Mounts)
	}
	if len(cs.Configs) != 0 {
		t.Errorf("Configs = %v, want none: the helper renders keepalived.conf in memory", cs.Configs)
	}
	if cs.StopGracePeriod == nil || *cs.StopGracePeriod != 10*time.Second {
		t.Errorf("StopGracePeriod = %v, want 10s", cs.StopGracePeriod)
	}

	// The environment is exactly these six, in this order, and none holds key material.
	wantEnv := []string{
		"TIERPLAN_VIP_ADDRESS=192.168.1.50/24",
		"TIERPLAN_VIP_ROUTER_ID=77",
		"TIERPLAN_VIP_PROBE_PORT=7483",
		"TIERPLAN_VIP_KEY_FILE=/run/secrets/tierplan-keepalived-key",
		"TIERPLAN_VIP_NODE_ID={{.Node.ID}}",
		"TIERPLAN_VIP_NODE_NAME={{.Node.Hostname}}",
	}
	if !slices.Equal(cs.Env, wantEnv) {
		t.Errorf("Env = %q, want %q", cs.Env, wantEnv)
	}
	for _, e := range cs.Env {
		if strings.Contains(e, "hex:") {
			t.Errorf("env value %q holds key material", e)
		}
	}

	// The secret reference: mode 0400 owned by root, at the S3 default target under /run/secrets.
	if len(cs.Secrets) != 1 {
		t.Fatalf("Secrets = %v, want one", cs.Secrets)
	}
	ref := cs.Secrets[0]
	if ref.SecretName != "tierplan-keepalived-key-g1-abcd" || ref.SecretID != "" {
		t.Errorf("secret reference = %+v", ref)
	}
	if ref.File == nil || ref.File.Name != "tierplan-keepalived-key" || ref.File.UID != "0" || ref.File.GID != "0" || ref.File.Mode != os.FileMode(0o400) {
		t.Errorf("secret file = %+v, want tierplan-keepalived-key, UID 0, GID 0, mode 0400", ref.File)
	}

	// Host network only: no overlay, no published port.
	if want := []swarm.NetworkAttachmentConfig{{Target: "host"}}; !reflect.DeepEqual(tt.Networks, want) {
		t.Errorf("Networks = %+v, want only the host network", tt.Networks)
	}
	if tt.LogDriver == nil || tt.LogDriver.Name != "json-file" || !maps.Equal(tt.LogDriver.Options, map[string]string{"max-size": "1m", "max-file": "3"}) {
		t.Errorf("LogDriver = %+v, want json-file with max-size 1m and max-file 3", tt.LogDriver)
	}

	// The restart policy: any exit restarts after 2 s, not swarm's default 5 s. A crashed keepalived
	// leaves its address on the node's interface until the replacement starts, so the delay bounds how
	// long a dead holder can still answer ARP; 2 s plus the 0.4 s start is below the 3.3 s election.
	rp := tt.RestartPolicy
	if rp == nil || rp.Condition != swarm.RestartPolicyConditionAny || rp.Delay == nil || *rp.Delay != 2*time.Second || rp.MaxAttempts != nil || rp.Window != nil {
		t.Errorf("RestartPolicy = %+v, want condition any, delay 2s, no attempt limit", rp)
	}

	// The roll: one node at a time, the old task stopped first, 10 s between nodes, back on failure.
	if u := spec.UpdateConfig; u == nil || u.Parallelism != 1 || u.Order != swarm.UpdateOrderStopFirst || u.FailureAction != swarm.UpdateFailureActionRollback || u.Delay != 10*time.Second {
		t.Errorf("UpdateConfig = %+v, want parallelism 1, stop-first, rollback, delay 10s", u)
	}
	// S3 update-delay decision: 10s
	if ingressdeploy.KeepalivedUpdateDelay != 10*time.Second {
		t.Errorf("KeepalivedUpdateDelay = %v, want 10s", ingressdeploy.KeepalivedUpdateDelay)
	}
	if r := spec.RollbackConfig; r == nil || r.Parallelism != 1 || r.Order != swarm.UpdateOrderStopFirst {
		t.Errorf("RollbackConfig = %+v, want parallelism 1, stop-first", r)
	}

	// The same labels as the package's own helper, so the consumers' ownership tests see the service.
	if !ingress.OwnsKeepalivedService(spec.Name, spec.Labels) {
		t.Error("the spec is not recognised as the keepalived service")
	}
}

// TestKeepalivedServiceSpecFreshSlices proves the capability list is a new slice on every call, so a
// caller that edits one spec cannot change the next.
func TestKeepalivedServiceSpecFreshSlices(t *testing.T) {
	a := ingressdeploy.KeepalivedCaps()
	a[0] = "SYS_ADMIN"
	if got := ingressdeploy.KeepalivedCaps(); got[0] != "NET_ADMIN" {
		t.Errorf("KeepalivedCaps()[0] = %q after a caller edited an earlier result", got[0])
	}
}

// TestKeepalivedImage is the tag rule of the other images: the exact X.Y.Z for a release, edge for a
// development server.
func TestKeepalivedImage(t *testing.T) {
	for in, want := range map[string]string{
		"0.1.1": "ghcr.io/ianglennon/tierplan-keepalived:0.1.1",
		"dev":   "ghcr.io/ianglennon/tierplan-keepalived:edge",
		"":      "ghcr.io/ianglennon/tierplan-keepalived:edge",
	} {
		if got := ingressdeploy.KeepalivedImage(in); got != want {
			t.Errorf("KeepalivedImage(%q) = %q, want %q", in, got, want)
		}
	}
}
