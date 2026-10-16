// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/proto/hostpath"
)

// vectorLookup answers for a vector: the IDs it lists are the host network, every
// other target is an ordinary overlay network.
func vectorLookup(v hostpath.SpecVector) networkLookup {
	return func(_ context.Context, target string) (string, string, error) {
		if slices.Contains(v.HostNetworkIDs, target) {
			return "host", "host", nil
		}
		return "app_net", "overlay", nil
	}
}

// absentVolumes answers every volume name as one the engine does not have.
func absentVolumes(context.Context, string) (string, map[string]string, bool, error) {
	return "", nil, false, nil
}

func decodeVector(t *testing.T, v hostpath.SpecVector) swarm.ServiceSpec {
	t.Helper()
	var spec swarm.ServiceSpec
	dec := json.NewDecoder(bytes.NewReader(v.Spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		t.Fatalf("vector %q does not decode as a service spec: %v", v.Name, err)
	}
	return spec
}

// TestServicePolicyVectors runs the embedded vectors, the same ones the server's
// policy is held to (06-18), against the agent's check.
func TestServicePolicyVectors(t *testing.T) {
	vs := hostpath.Vectors()
	if len(vs) < 14 {
		t.Fatalf("only %d vectors", len(vs))
	}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			spec := decodeVector(t, v)
			ctx := context.Background()
			if got := escalations(ctx, spec, vectorLookup(v)); (len(got) > 0) != v.Escalates {
				t.Fatalf("escalations = %v, want escalates %v", got, v.Escalates)
			}

			// Without the opt-in: refused with errHostPolicy, or forwarded as rebuilt bytes.
			env := bodyEnv{policy: Policy{}, lookup: vectorLookup(v), volumes: absentVolumes, ctx: ctx}
			out, err := validateBody(BodyServiceSpec, bytes.NewReader(v.Spec), env)
			if v.Escalates {
				if !errors.Is(err, errHostPolicy) {
					t.Fatalf("without the opt-in: err = %v, want errHostPolicy", err)
				}
			} else {
				if err != nil {
					t.Fatalf("a clean vector was refused: %v", err)
				}
				var back swarm.ServiceSpec
				if err := json.Unmarshal(out, &back); err != nil || !reflect.DeepEqual(back, spec) {
					t.Fatalf("rebuilt bytes %s do not decode to the vector's spec (%v)", out, err)
				}
			}

			// With the opt-in every vector is forwarded.
			env.policy.AllowPrivilegedServices = true
			if _, err := validateBody(BodyServiceSpec, bytes.NewReader(v.Spec), env); err != nil {
				t.Fatalf("with the opt-in: %v", err)
			}
		})
	}
}

func TestServicePolicyAZeroEnvIsTheStrictestReading(t *testing.T) {
	spec := sampleSpec()
	spec.TaskTemplate.ContainerSpec.CapabilityAdd = []string{"SYS_ADMIN"}
	if _, err := validateBody(BodyServiceSpec, bytes.NewReader([]byte(enc(t, spec))), bodyEnv{}); !errors.Is(err, errHostPolicy) {
		t.Fatalf("err = %v, want errHostPolicy", err)
	}
}

func TestEscalationsNameTheSpecPathOfEachFinding(t *testing.T) {
	spec := sampleSpec()
	cs := spec.TaskTemplate.ContainerSpec
	cs.Mounts = []mount.Mount{
		{Type: mount.TypeBind, Source: "/srv/ok", Target: "/a"},
		{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/b"},
		{Type: mount.TypeNamedPipe, Source: `\\.\pipe\docker_engine`, Target: "/c"},
		{Type: mount.TypeVolume, Source: "v", Target: "/d", VolumeOptions: &mount.VolumeOptions{
			DriverConfig: &mount.Driver{Name: "local", Options: map[string]string{"type": "none", "o": "bind", "device": "/etc"}},
		}},
	}
	cs.CapabilityAdd = []string{"NET_ADMIN", "SYS_PTRACE"}
	cs.Privileges = &swarm.Privileges{
		SELinuxContext: &swarm.SELinuxContext{Disable: true},
		Seccomp:        &swarm.SeccompOpts{Mode: swarm.SeccompModeUnconfined},
		AppArmor:       &swarm.AppArmorOpts{Mode: swarm.AppArmorModeDisabled},
	}
	spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "app_net"}, {Target: "host"}}
	got := escalations(context.Background(), spec, func(context.Context, string) (string, string, error) { return "app_net", "overlay", nil })
	want := []string{
		"TaskTemplate.ContainerSpec.Mounts[1].Source",
		"TaskTemplate.ContainerSpec.Mounts[2].Source",
		"TaskTemplate.ContainerSpec.Mounts[3].VolumeOptions.DriverConfig.Options.device",
		"TaskTemplate.ContainerSpec.CapabilityAdd[0]",
		"TaskTemplate.ContainerSpec.CapabilityAdd[1]",
		"TaskTemplate.ContainerSpec.Privileges.SELinuxContext",
		"TaskTemplate.ContainerSpec.Privileges.Seccomp.Mode",
		"TaskTemplate.ContainerSpec.Privileges.AppArmor.Mode",
		"TaskTemplate.Networks[1].Target",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("escalations =\n  %v\nwant\n  %v", got, want)
	}
}

func TestEscalationsLookUpANetworkTargetOnTheAgentsOwnEngine(t *testing.T) {
	spec := sampleSpec()
	spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "abc123"}}
	for name, tc := range map[string]struct {
		lookup networkLookup
		want   bool
	}{
		"driver host":                 {func(context.Context, string) (string, string, error) { return "mine", "host", nil }, true},
		"name host":                   {func(context.Context, string) (string, string, error) { return "host", "bridge", nil }, true},
		"an overlay network":          {func(context.Context, string) (string, string, error) { return "mine", "overlay", nil }, false},
		"a lookup that fails":         {func(context.Context, string) (string, string, error) { return "", "", errors.New("no such network") }, false},
		"no lookup at all":            {nil, false},
		"driver host in upper case":   {func(context.Context, string) (string, string, error) { return "mine", "Host", nil }, true},
		"an overlay named host-alias": {func(context.Context, string) (string, string, error) { return "host-alias", "overlay", nil }, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := escalations(context.Background(), spec, tc.lookup)
			if (len(got) > 0) != tc.want {
				t.Fatalf("escalations = %v, want escalation %v", got, tc.want)
			}
		})
	}
	t.Run("the network named host needs no lookup", func(t *testing.T) {
		spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "host"}}
		got := escalations(context.Background(), spec, func(context.Context, string) (string, string, error) {
			t.Error("the lookup ran for the literal name host")
			return "", "", errors.New("unused")
		})
		if !slices.Equal(got, []string{"TaskTemplate.Networks[0].Target"}) {
			t.Fatalf("escalations = %v", got)
		}
	})
	t.Run("one lookup per distinct target", func(t *testing.T) {
		spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "n1"}, {Target: "n1"}, {Target: "n2"}}
		calls := 0
		escalations(context.Background(), spec, func(context.Context, string) (string, string, error) {
			calls++
			return "x", "overlay", nil
		})
		if calls != 2 {
			t.Fatalf("lookups = %d, want 2", calls)
		}
	})
}

func TestEscalationsLeaveAnOrdinarySpecAlone(t *testing.T) {
	spec := sampleSpec()
	spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{
		{Type: mount.TypeVolume, Source: "data", Target: "/data"},
		{Type: mount.TypeTmpfs, Target: "/tmp"},
		{Type: mount.TypeBind, Source: "/srv/app", Target: "/app"},
	}
	spec.TaskTemplate.ContainerSpec.CapabilityDrop = []string{"ALL"}
	if got := escalations(context.Background(), spec, nil); len(got) != 0 {
		t.Fatalf("escalations = %v", got)
	}
}
