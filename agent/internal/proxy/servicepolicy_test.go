// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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

	"github.com/ianglennon/tierplan/proto/hostpath"
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

// vectorVolumes answers for a vector: the volumes it lists exist on the engine with
// the driver and options it gives, every other name is one the engine does not have.
func vectorVolumes(v hostpath.SpecVector) volumeLookup {
	return func(_ context.Context, name string) (string, map[string]string, bool, error) {
		if vol, ok := v.Volumes[name]; ok {
			return vol.Driver, vol.Options, true, nil
		}
		return "", nil, false, nil
	}
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
			env := bodyEnv{policy: Policy{}, lookup: vectorLookup(v), volumes: vectorVolumes(v), ctx: ctx}
			out, err := validateBody(BodyServiceSpec, bytes.NewReader(v.Spec), env)
			if v.Escalates || v.AgentRefuses {
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

	// With no volume lookup the zero env cannot see what backs a name.
	spec = sampleSpec()
	spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "data", Target: "/data"}}
	if _, err := validateBody(BodyServiceSpec, bytes.NewReader([]byte(enc(t, spec))), bodyEnv{}); !errors.Is(err, errHostPolicy) {
		t.Fatalf("a named volume: err = %v, want errHostPolicy", err)
	}
}

func TestPluginVolumesNameEachMountWithAnotherDriver(t *testing.T) {
	spec := sampleSpec()
	spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{
		{Type: mount.TypeVolume, Source: "a", Target: "/a", VolumeOptions: &mount.VolumeOptions{
			DriverConfig: &mount.Driver{Name: "local", Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.5", "device": ":/x"}}}},
		{Type: mount.TypeVolume, Source: "b", Target: "/b", VolumeOptions: &mount.VolumeOptions{
			DriverConfig: &mount.Driver{Name: "rexray/ebs"}}},
		{Type: mount.TypeBind, Source: "/srv/ok", Target: "/c"},
		{Type: mount.TypeCluster, Source: "csi-vol", Target: "/d"},
	}
	want := []string{
		"TaskTemplate.ContainerSpec.Mounts[1].VolumeOptions.DriverConfig.Name",
		"TaskTemplate.ContainerSpec.Mounts[3].Type",
	}
	if got := pluginVolumes(spec); !slices.Equal(got, want) {
		t.Fatalf("pluginVolumes = %v, want %v", got, want)
	}

	for _, name := range []string{"LOCAL", " local ", ""} {
		spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{
			{Type: mount.TypeVolume, Source: "a", Target: "/a", VolumeOptions: &mount.VolumeOptions{
				DriverConfig: &mount.Driver{Name: name}}},
		}
		if got := pluginVolumes(spec); len(got) != 0 {
			t.Errorf("driver %q: pluginVolumes = %v, want none", name, got)
		}
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
	cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeVolume, Source: "w", Target: "/e", VolumeOptions: &mount.VolumeOptions{
		DriverConfig: &mount.Driver{Name: "local", Options: map[string]string{"type": "overlay", "device": "overlay", "o": "lowerdir=/"}},
	}})
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
		"TaskTemplate.ContainerSpec.Mounts[4].VolumeOptions.DriverConfig.Options.type",
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

func TestNamedVolumesJudgeWhatTheEngineHolds(t *testing.T) {
	const path = "TaskTemplate.ContainerSpec.Mounts[0].Source"
	spec := sampleSpec()
	spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "data", Target: "/data"}}
	exists := func(driver string, opts map[string]string) volumeLookup {
		return func(context.Context, string) (string, map[string]string, bool, error) {
			return driver, opts, true, nil
		}
	}
	for name, tc := range map[string]struct {
		lookup volumeLookup
		want   []string
	}{
		"exists, local, binds the host root":           {exists("local", map[string]string{"type": "none", "o": "bind", "device": "/"}), []string{path}},
		"exists, local, a device":                      {exists("local", map[string]string{"type": "ext4", "device": "/dev/sda1"}), []string{path}},
		"exists, driver in capitals, binds /etc":       {exists("LOCAL", map[string]string{"type": "none", "o": "bind", "device": "/etc"}), []string{path}},
		"exists, a plugin driver":                      {exists("rexray/ebs", nil), []string{path}},
		"exists, local, binds the relative device etc": {exists("local", map[string]string{"type": "none", "o": "bind", "device": "etc"}), []string{path}},
		"exists, local, an overlay over the host root": {exists("local", map[string]string{"type": "overlay", "device": "overlay", "o": "lowerdir=/"}), []string{path}},
		"exists, local, tmpfs":                         {exists("local", map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "size=64m"}), nil},
		"exists, local, an nfs share":                  {exists("local", map[string]string{"type": "nfs", "o": "addr=10.0.0.5,rw", "device": ":/export"}), nil},
		"exists, local, binds a path under /srv":       {exists("local", map[string]string{"type": "none", "o": "bind", "device": "/srv/data"}), nil},
		"exists, local, no options":                    {exists("local", nil), nil},
		"does not exist": {func(context.Context, string) (string, map[string]string, bool, error) {
			return "", nil, false, nil
		}, nil},
		"no lookup at all": {nil, []string{path}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := namedVolumes(context.Background(), spec, tc.lookup)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("namedVolumes = %v, %v, want %v and no error", got, err, tc.want)
			}
		})
	}

	t.Run("the lookup fails", func(t *testing.T) {
		got, err := namedVolumes(context.Background(), spec, func(context.Context, string) (string, map[string]string, bool, error) {
			return "", nil, false, errors.New("engine unreachable")
		})
		if len(got) != 0 || !errors.Is(err, errVolumeLookup) {
			t.Fatalf("namedVolumes = %v, %v, want no finding and errVolumeLookup", got, err)
		}
	})

	never := func(t *testing.T) volumeLookup {
		return func(_ context.Context, name string) (string, map[string]string, bool, error) {
			t.Errorf("the lookup ran for %q", name)
			return "", nil, false, errors.New("unused")
		}
	}
	t.Run("a Source that is not a volume name is refused without a lookup", func(t *testing.T) {
		spec := sampleSpec()
		spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "../etc", Target: "/data"}}
		if got, err := namedVolumes(context.Background(), spec, never(t)); err != nil || !slices.Equal(got, []string{path}) {
			t.Fatalf("namedVolumes = %v, %v, want %v", got, err, []string{path})
		}
	})
	t.Run("an anonymous volume is never looked up", func(t *testing.T) {
		spec := sampleSpec()
		spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Target: "/data"}}
		if got, err := namedVolumes(context.Background(), spec, never(t)); err != nil || len(got) != 0 {
			t.Fatalf("namedVolumes = %v, %v, want none", got, err)
		}
		if got, err := namedVolumes(context.Background(), spec, nil); err != nil || len(got) != 0 {
			t.Fatalf("with no lookup, namedVolumes = %v, %v, want none", got, err)
		}
	})
	t.Run("one lookup per distinct Source", func(t *testing.T) {
		spec := sampleSpec()
		spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{
			{Type: mount.TypeVolume, Source: "data", Target: "/a"},
			{Type: mount.TypeVolume, Source: "data", Target: "/b"},
		}
		calls := 0
		got, err := namedVolumes(context.Background(), spec, func(context.Context, string) (string, map[string]string, bool, error) {
			calls++
			return "rexray/ebs", nil, true, nil
		})
		if err != nil {
			t.Fatalf("namedVolumes: %v", err)
		}
		if calls != 1 {
			t.Fatalf("lookups = %d, want 1", calls)
		}
		want := []string{path, "TaskTemplate.ContainerSpec.Mounts[1].Source"}
		if !slices.Equal(got, want) {
			t.Fatalf("namedVolumes = %v, want %v", got, want)
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
