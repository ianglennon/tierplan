// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmspec_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/rbac/compose"
	"github.com/ianglennon/bayplan/internal/rbac/swarmspec"
	"github.com/ianglennon/bayplan/proto/hostpath"
)

func decode(t *testing.T, raw []byte) swarm.ServiceSpec {
	t.Helper()
	var spec swarm.ServiceSpec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		t.Fatalf("does not decode as a service spec: %v", err)
	}
	return spec
}

// resolverFor answers for a vector: the IDs it lists are the host network, every
// other target is an ordinary overlay network.
func resolverFor(v hostpath.SpecVector) swarmspec.NetworkResolver {
	return func(target string) (string, string, bool) {
		if slices.Contains(v.HostNetworkIDs, target) {
			return "host", "host", true
		}
		return "app_net", "overlay", true
	}
}

// TestSwarmSpecVectorsMatchTheAgent holds the server's check to the vectors the
// agent's check passes (06-03), so the two cannot disagree on what host access is.
func TestSwarmSpecVectorsMatchTheAgent(t *testing.T) {
	vs := hostpath.Vectors()
	if len(vs) < 14 {
		t.Fatalf("only %d vectors", len(vs))
	}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			got := swarmspec.Check(decode(t, v.Spec), resolverFor(v))
			if (len(got) > 0) != v.Escalates {
				t.Fatalf("findings = %+v, want escalates %v", got, v.Escalates)
			}
			for _, f := range got {
				if f.Key == "" || f.Rule == "" || f.Why == "" || f.Line != 0 {
					t.Errorf("finding %+v lacks a path, rule or reason, or has a line", f)
				}
			}
		})
	}
}

func specWith(cs swarm.ContainerSpec) swarm.ServiceSpec {
	return swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "web"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &cs},
	}
}

func TestCheckABindOfTheDockerSocketNamesThePathAndTheSource(t *testing.T) {
	spec := specWith(swarm.ContainerSpec{
		Image: "nginx",
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: "data", Target: "/data"},
			{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/sock"},
		},
	})
	got := swarmspec.Check(spec, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.Key != "TaskTemplate.ContainerSpec.Mounts[1].Source" || f.Value != "/var/run/docker.sock" || f.Rule != "bind_path" || f.Service != "web" {
		t.Errorf("finding = %+v", f)
	}
	if s := swarmspec.FormatSpec(f); s != "TaskTemplate.ContainerSpec.Mounts[1].Source: /var/run/docker.sock" {
		t.Errorf("FormatSpec = %q", s)
	}
}

func TestCheckABindUnderSrvIsNotAFindingBecauseTheStackPolicyAllowsIt(t *testing.T) {
	spec := specWith(swarm.ContainerSpec{
		Image:  "nginx",
		Mounts: []mount.Mount{{Type: mount.TypeBind, Source: "/srv/web", Target: "/data"}},
	})
	if got := swarmspec.Check(spec, nil); len(got) != 0 {
		t.Fatalf("findings = %+v, want none (D-38)", got)
	}
}

func TestCheckARuntimeOfPluginIsAFindingWithTheRulePluginRuntime(t *testing.T) {
	spec := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "drv"},
		TaskTemplate: swarm.TaskSpec{Runtime: swarm.RuntimePlugin, PluginSpec: &swarm.RuntimeSpec{Name: "vendor/driver"}},
	}
	got := swarmspec.Check(spec, nil)
	if len(got) == 0 {
		t.Fatal("a plugin runtime was not a finding")
	}
	for _, f := range got {
		if f.Rule != "plugin_runtime" {
			t.Errorf("finding %+v, want rule plugin_runtime", f)
		}
	}
	if got[0].Key != "TaskTemplate.Runtime" || got[0].Value != "plugin" {
		t.Errorf("first finding = %+v", got[0])
	}
}

func TestCheckEachRuleHasItsName(t *testing.T) {
	cases := map[string]struct {
		cs   swarm.ContainerSpec
		rule string
	}{
		"npipe":    {swarm.ContainerSpec{Mounts: []mount.Mount{{Type: mount.TypeNamedPipe, Source: `\\.\pipe\docker_engine`, Target: "x"}}}, "npipe"},
		"cap add":  {swarm.ContainerSpec{CapabilityAdd: []string{"NET_ADMIN"}}, "cap_add"},
		"selinux":  {swarm.ContainerSpec{Privileges: &swarm.Privileges{SELinuxContext: &swarm.SELinuxContext{Disable: true}}}, "selinux"},
		"seccomp":  {swarm.ContainerSpec{Privileges: &swarm.Privileges{Seccomp: &swarm.SeccompOpts{Mode: swarm.SeccompModeUnconfined}}}, "seccomp"},
		"apparmor": {swarm.ContainerSpec{Privileges: &swarm.Privileges{AppArmor: &swarm.AppArmorOpts{Mode: swarm.AppArmorModeDisabled}}}, "apparmor"},
		"device": {swarm.ContainerSpec{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "v", Target: "/v", VolumeOptions: &mount.VolumeOptions{
			DriverConfig: &mount.Driver{Name: "local", Options: map[string]string{"type": "none", "o": "bind", "device": "/"}}}}}}, "volume_device"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := swarmspec.Check(specWith(c.cs), nil)
			if len(got) != 1 || got[0].Rule != c.rule {
				t.Fatalf("findings = %+v, want one with rule %s", got, c.rule)
			}
		})
	}
}

func TestCheckTheHostNetworkIsFoundByNameAndThroughTheResolver(t *testing.T) {
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "web"},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: "nginx"},
			Networks:      []swarm.NetworkAttachmentConfig{{Target: "overlay1"}, {Target: "abc123"}},
		},
	}
	resolve := func(target string) (string, string, bool) {
		if target == "abc123" {
			return "host", "host", true
		}
		return "app", "overlay", true
	}
	got := swarmspec.Check(spec, resolve)
	if len(got) != 1 || got[0].Key != "TaskTemplate.Networks[1].Target" || got[0].Rule != "host_network" {
		t.Fatalf("findings = %+v", got)
	}
	// A nil resolver, or one that does not know the network, finds nothing for an ID.
	if got := swarmspec.Check(spec, nil); len(got) != 0 {
		t.Errorf("nil resolver: %+v", got)
	}
	if got := swarmspec.Check(spec, func(string) (string, string, bool) { return "", "", false }); len(got) != 0 {
		t.Errorf("unknown network: %+v", got)
	}
	// The literal name host is a finding without any resolver.
	spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "host"}}
	if got := swarmspec.Check(spec, nil); len(got) != 1 {
		t.Errorf("host by name: %+v", got)
	}
}

var _ = compose.Finding{}
