// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/hostpath"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// vectorName is the S4 vector that holds the spec every validator accepted.
const vectorName = "tierplan-ingress proxy spec"

// The inputs of the S4 vector.
func vectorInput() ingressdeploy.SpecInput {
	return ingressdeploy.SpecInput{
		Image:      "ghcr.io/ianglennon/tierplan-ingress:edge",
		URL:        "wss://tierplan.example:7480/ingress/connect",
		Pin:        "sha256/AAAA",
		SecretName: "tierplan-ingress-token-g1-abc123def456-0123abcd",
	}
}

// asTree marshals v and decodes it again into maps, slices and numbers, so two values compare by
// content and not by field order.
func asTree(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// firstDiff returns the path of the first difference between two decoded JSON trees, or "" when they
// are equal. Map keys are visited in sorted order so the answer is the same on every run.
func firstDiff(path string, a, b any) string {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return path + " (type)"
		}
		keys := slices.Sorted(maps.Keys(av))
		for k := range bv {
			if _, ok := av[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range slices.Compact(keys) {
			x, inA := av[k]
			y, inB := bv[k]
			switch {
			case !inA:
				return fmt.Sprintf("%s.%s only in the vector", path, k)
			case !inB:
				return fmt.Sprintf("%s.%s only in the builder", path, k)
			}
			if d := firstDiff(path+"."+k, x, y); d != "" {
				return d
			}
		}
		return ""
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return fmt.Sprintf("%s (length or type: builder %v, vector %v)", path, a, b)
		}
		for i := range av {
			if d := firstDiff(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i]); d != "" {
				return d
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(a, b) {
			return fmt.Sprintf("%s: builder %v, vector %v", path, a, b)
		}
		return ""
	}
}

// proxySpec builds the spec for in.
func proxySpec(in ingressdeploy.SpecInput) swarm.ServiceSpec {
	return ingressdeploy.ProxyServiceSpec(in)
}

// TestProxySpecMatchesS4Vector is the tracer: the spec the deployer sends is the spec every validator
// (the server's policy, the agent's proxy, the RBAC walker) was shown and accepted.
func TestProxySpecMatchesS4Vector(t *testing.T) {
	var vector *hostpath.SpecVector
	for _, v := range hostpath.Vectors() {
		if v.Name == vectorName {
			vector = &v
			break
		}
	}
	if vector == nil {
		t.Fatalf("vector %q not found in proto/hostpath/testdata/service-specs.json", vectorName)
	}
	if vector.Escalates {
		t.Fatalf("vector %q escalates; the proxy spec must not", vectorName)
	}
	var want any
	if err := json.Unmarshal(vector.Spec, &want); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	got := asTree(t, proxySpec(vectorInput()))
	if d := firstDiff("$", got, want); d != "" {
		t.Errorf("the builder's spec differs from the S4 vector at %s", d)
	}
}

// TestLabelsOnEveryObject proves the labels a consumer reads are on every object the switch creates,
// each in its own map.
func TestLabelsOnEveryObject(t *testing.T) {
	want := ingress.ObjectLabels()
	spec := proxySpec(vectorInput())
	cs := spec.TaskTemplate.ContainerSpec
	objects := map[string]map[string]string{
		"service annotations": spec.Labels,
		"container spec":      cs.Labels,
		"volume options":      cs.Mounts[0].VolumeOptions.Labels,
		"network":             ingressdeploy.NetworkSpec("10.0.0.0/22").Labels,
		"secret":              ingressdeploy.SecretSpec("tierplan-ingress-token-g1-a-b", []byte("x")).Labels,
	}
	for name, got := range objects {
		if !maps.Equal(got, want) {
			t.Errorf("%s labels = %v, want %v", name, got, want)
		}
		if _, ok := got["com.docker.stack.namespace"]; ok {
			t.Errorf("%s carries the stack namespace label", name)
		}
	}
	// Each map is its own: a change to one must not reach another.
	spec.Labels["probe"] = "x"
	for name, m := range objects {
		if name != "service annotations" && m["probe"] != "" {
			t.Errorf("%s shares its label map with the service", name)
		}
	}
	cs.Labels["probe2"] = "y"
	if spec.Labels["probe2"] != "" || cs.Mounts[0].VolumeOptions.Labels["probe2"] != "" {
		t.Error("the container labels share a map with another object")
	}
}

// TestObjectLabelKeysMatchStacks proves the pure ingress package's copies of the janitor's keep label
// and the update detector's ignore label equal the originals.
func TestObjectLabelKeysMatchStacks(t *testing.T) {
	got := ingress.ObjectLabels()
	if len(got) != 3 {
		t.Fatalf("ObjectLabels has %d keys, want 3: %v", len(got), got)
	}
	if got[stacks.LabelKeep] != "true" {
		t.Errorf("label %q = %q, want true", stacks.LabelKeep, got[stacks.LabelKeep])
	}
	if got[stacks.LabelUpdates] != "ignore" {
		t.Errorf("label %q = %q, want ignore", stacks.LabelUpdates, got[stacks.LabelUpdates])
	}
	if got[ingress.LabelComponent] != ingress.ComponentIngress {
		t.Errorf("label %q = %q, want %q", ingress.LabelComponent, got[ingress.LabelComponent], ingress.ComponentIngress)
	}
}

// TestAccessLogEnv proves the access-log variable is present once when the switch is on and absent
// when it is off.
func TestAccessLogEnv(t *testing.T) {
	for _, on := range []bool{true, false} {
		in := vectorInput()
		in.AccessLog = on
		var n int
		for _, e := range proxySpec(in).TaskTemplate.ContainerSpec.Env {
			if strings.HasPrefix(e, ingressbundle.EnvAccessLog) {
				n++
				if e != ingressbundle.EnvAccessLog+"=1" {
					t.Errorf("access log entry = %q, want %s=1", e, ingressbundle.EnvAccessLog)
				}
			}
		}
		if want := map[bool]int{true: 1, false: 0}[on]; n != want {
			t.Errorf("AccessLog=%v: %d access log entries, want %d", on, n, want)
		}
	}
}

// TestLogDriverBounded proves the proxy's log is rotated whatever the access-log switch says.
func TestLogDriverBounded(t *testing.T) {
	for _, on := range []bool{true, false} {
		in := vectorInput()
		in.AccessLog = on
		d := proxySpec(in).TaskTemplate.LogDriver
		if d == nil || d.Name != "json-file" || d.Options["max-size"] != "10m" || d.Options["max-file"] != "3" || len(d.Options) != 2 {
			t.Errorf("AccessLog=%v: LogDriver = %+v, want json-file max-size 10m max-file 3", on, d)
		}
	}
}

// TestUpdateConfigStopFirstRollback proves an update stops the old task first, one node at a time, and
// rolls back when the new task does not come up.
func TestUpdateConfigStopFirstRollback(t *testing.T) {
	spec := proxySpec(vectorInput())
	u, r := spec.UpdateConfig, spec.RollbackConfig
	if u == nil || r == nil {
		t.Fatal("UpdateConfig or RollbackConfig is missing")
	}
	if u.Parallelism != 1 || u.Order != swarm.UpdateOrderStopFirst || u.FailureAction != swarm.UpdateFailureActionRollback {
		t.Errorf("UpdateConfig = %+v, want parallelism 1, stop-first, rollback", u)
	}
	if r.Parallelism != 1 || r.Order != swarm.UpdateOrderStopFirst {
		t.Errorf("RollbackConfig = %+v, want parallelism 1, stop-first", r)
	}
	for name, m := range map[string]int64{"update": int64(u.Monitor), "rollback": int64(r.Monitor)} {
		if m != int64(ingressdeploy.UpdateMonitor) || m < int64(ingressbundle.HealthcheckInterval) {
			t.Errorf("%s monitor = %d, want UpdateMonitor and at least the health check interval", name, m)
		}
	}
}

// TestNoTokenInSpec proves the spec carries the token file's path and the secret's name and nothing
// that could be a token.
func TestNoTokenInSpec(t *testing.T) {
	const token = "tpl_ingress_SECRET_TOKEN_VALUE" //nolint:gosec // G101: a fake token the test checks never reaches the spec
	in := vectorInput()
	spec := proxySpec(in)
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	if strings.Contains(js, token) {
		t.Error("the spec contains the token")
	}
	for _, want := range []string{ingressdeploy.TokenFile, in.SecretName} {
		if !strings.Contains(js, want) {
			t.Errorf("the spec lacks %q", want)
		}
	}
	// The only token-like variable names the file path, never a value.
	for _, e := range spec.TaskTemplate.ContainerSpec.Env {
		k, v, _ := strings.Cut(e, "=")
		if strings.Contains(strings.ToLower(k), "token") && v != ingressdeploy.TokenFile {
			t.Errorf("env %s holds %q, want only the token file's path", k, v)
		}
		if k == ingressbundle.EnvJoinToken {
			t.Errorf("the spec sets %s", k)
		}
	}
	// The secret is the only place the value goes.
	sec := ingressdeploy.SecretSpec(in.SecretName, []byte(token))
	if string(sec.Data) != token {
		t.Error("SecretSpec does not carry the token as data")
	}
	if ref := spec.TaskTemplate.ContainerSpec.Secrets; len(ref) != 1 || ref[0].SecretID != "" ||
		ref[0].File.Mode != 0o400 || ref[0].File.UID != "1000" || ref[0].File.GID != "1000" || ref[0].File.Name != ingressdeploy.SecretTarget {
		t.Errorf("secret reference = %+v, want one 0400 file for 1000:1000 and no ID yet", ref)
	}
}

// TestNoHostAccess proves the proxy gets no bind mount, no capability, no privileges, no host network
// and no Docker socket.
func TestNoHostAccess(t *testing.T) {
	spec := proxySpec(vectorInput())
	cs := spec.TaskTemplate.ContainerSpec
	if len(cs.Mounts) != 1 || cs.Mounts[0].Type != mount.TypeVolume || cs.Mounts[0].Source != ingress.VolumeName || cs.Mounts[0].Target != ingressdeploy.StateDir {
		t.Errorf("mounts = %+v, want the one cache volume", cs.Mounts)
	}
	if len(cs.CapabilityAdd) != 0 || len(cs.CapabilityDrop) != 0 {
		t.Errorf("capabilities = +%v -%v, want none", cs.CapabilityAdd, cs.CapabilityDrop)
	}
	if cs.Privileges != nil {
		t.Errorf("Privileges = %+v, want nil", cs.Privileges)
	}
	if cs.User != "1000:1000" {
		t.Errorf("User = %q, want 1000:1000", cs.User)
	}
	nets := spec.TaskTemplate.Networks
	if len(nets) != 1 || nets[0].Target != ingress.NetworkName {
		t.Errorf("networks = %+v, want only %s", nets, ingress.NetworkName)
	}
	b, _ := json.Marshal(spec)
	for _, bad := range []string{"docker.sock", `"bind"`} {
		if strings.Contains(string(b), bad) {
			t.Errorf("the spec contains %q", bad)
		}
	}
	if spec.Mode.Global == nil || spec.Mode.Replicated != nil {
		t.Errorf("mode = %+v, want global", spec.Mode)
	}
}

// TestProxySpecPublishesTheReadinessPort is the tracer of the readiness port (D-13, A-06): the spec always publishes three
// host-mode TCP ports, 80 to 8080, 443 to 8443 and the readiness 7483 to 8083, whether or not a virtual IP is on, so the
// virtual IP never rolls the proxies. The three target ports are distinct, because published ports that share one target
// collapse into one mapping (S2).
func TestProxySpecPublishesTheReadinessPort(t *testing.T) {
	ports := proxySpec(vectorInput()).EndpointSpec.Ports
	if len(ports) != 3 {
		t.Fatalf("ports = %+v, want three", ports)
	}
	targets := map[uint32]bool{}
	for _, p := range ports {
		targets[p.TargetPort] = true
		if p.PublishMode != swarm.PortConfigPublishModeHost || p.Protocol != "tcp" {
			t.Errorf("port %+v is not host-mode tcp", p)
		}
	}
	if len(targets) != 3 {
		t.Errorf("ports = %+v, want three distinct targets", ports)
	}
	if ingressdeploy.ReadyPublished != ingressbundle.ReadyPublishedPort || ingressdeploy.ReadyTarget != ingressbundle.ReadyPort {
		t.Errorf("ReadyPublished, ReadyTarget = %d, %d, want the ingressbundle constants", ingressdeploy.ReadyPublished, ingressdeploy.ReadyTarget)
	}
	want := [][2]uint32{{80, 8080}, {443, 8443}, {7483, 8083}}
	for i, p := range ports {
		if p.PublishedPort != want[i][0] || p.TargetPort != want[i][1] {
			t.Errorf("port %d = %d to %d, want %d to %d", i, p.PublishedPort, p.TargetPort, want[i][0], want[i][1])
		}
	}
}

// TestNetworkSpec proves the ingress network is a swarm overlay on the given subnet and that its
// attachable flag is the one S5 decided.
func TestNetworkSpec(t *testing.T) {
	n := ingressdeploy.NetworkSpec("10.200.4.0/22")
	if n.Name != ingress.NetworkName || n.Driver != "overlay" || n.Scope != "swarm" || n.Subnet != "10.200.4.0/22" {
		t.Errorf("network = %+v", n)
	}
	if n.Attachable != ingressdeploy.Attachable {
		t.Errorf("Attachable = %v, want the S5 constant %v", n.Attachable, ingressdeploy.Attachable)
	}
	if ingressdeploy.Attachable {
		t.Error("S5 recorded a non-attachable overlay (24-S5-RESULT.md); the constant must be false")
	}
	if n.Internal || n.IPv6 {
		t.Errorf("network = %+v, want neither internal nor IPv6", n)
	}
}

// TestSecretName proves the name is stable, unique per issued token and well formed.
func TestSecretName(t *testing.T) {
	const env = "0192f3a4-7b1c-7d2e-8f90-abc123def456"
	a := ingressdeploy.SecretName(env, 1, "0192f3a4-0000-7000-8000-00000123abcd")
	if a != "tierplan-ingress-token-g1-abc123def456-0123abcd" {
		t.Errorf("SecretName = %q", a)
	}
	if b := ingressdeploy.SecretName(env, 1, "0192f3a4-0000-7000-8000-00000123abcd"); a != b {
		t.Errorf("not deterministic: %q then %q", a, b)
	}
	if c := ingressdeploy.SecretName(env, 1, "0192f3a4-0000-7000-8000-00000fffffff"); a == c {
		t.Error("two token IDs gave the same name")
	}
	if d := ingressdeploy.SecretName(env, 2, "0192f3a4-0000-7000-8000-00000123abcd"); a == d {
		t.Error("two generations gave the same name")
	}
	pattern := regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,62}[A-Za-z0-9])?$`)
	for _, gen := range []int{1, 9, 12345678} {
		n := ingressdeploy.SecretName(env, gen, "FFFFFFFF-FFFF-7FFF-8FFF-FFFFFFFFFFFF")
		if len(n) > 64 || !pattern.MatchString(n) || !strings.HasPrefix(n, ingress.SecretPrefix) {
			t.Errorf("SecretName(gen %d) = %q (%d chars): too long, malformed or without the prefix", gen, n, len(n))
		}
	}
	if !ingress.OwnsSecret(a, ingress.ObjectLabels()) {
		t.Errorf("%q is not owned by the ingress secret predicate", a)
	}
}

// TestIngressImage proves the proxy image follows the server's version through the agent's tag rule.
func TestIngressImage(t *testing.T) {
	for server, want := range map[string]string{
		"v1.2.3":          ingressdeploy.ImageRepository + ":1.2.3",
		"1.2.3-rc.1+meta": ingressdeploy.ImageRepository + ":1.2.3-rc.1",
		"dev":             ingressdeploy.ImageRepository + ":edge",
	} {
		if got := ingressdeploy.IngressImage(server); got != want {
			t.Errorf("IngressImage(%q) = %q, want %q", server, got, want)
		}
	}
}
