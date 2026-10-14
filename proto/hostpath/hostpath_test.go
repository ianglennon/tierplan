// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package hostpath

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestBad(t *testing.T) {
	cases := []struct {
		path string
		bad  bool
	}{
		{"/var/run/docker.sock", true},
		{"/", true},
		{"/srv/data", false},
		{"/mnt", false},
		{"/media/usb", false},
		{"/var/log", true},
		{"/var/log/app", false},
		{"/etc", true},
		{"~/x", true},
		{"data", false},
		{"../../../../var/run/docker.sock", true},
		{"/srv/../etc", true},
		{"/srv//a/./b", false},
		{"//usr", true},
		{"docker.sock", true},
		{"../shared", false},
		{"../..", true},
		{"..", false},
		{`\\.\pipe\docker_engine`, true},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := Bad(c.path); got != c.bad {
				t.Fatalf("Bad(%q) = %v, want %v", c.path, got, c.bad)
			}
		})
	}
}

func TestSocketAndClimbing(t *testing.T) {
	if !Socket("/run/podman/podman.sock") || Socket("/srv/data") {
		t.Fatal("Socket must name runtime sockets only")
	}
	if !ClimbingBad("../../../../etc/passwd") || ClimbingBad("./data") || ClimbingBad("../shared") {
		t.Fatal("ClimbingBad must judge only a climb that lands in a system tree")
	}
	if !slices.Contains(DataRoots, "/srv") || !slices.Contains(DataRoots, "/mnt") || !slices.Contains(DataRoots, "/media") {
		t.Fatalf("DataRoots = %v", DataRoots)
	}
	if !slices.Contains(NetworkFSTypes, "nfs") {
		t.Fatalf("NetworkFSTypes = %v", NetworkFSTypes)
	}
}

func TestLocalDeviceMounted(t *testing.T) {
	cases := []struct {
		name    string
		opts    map[string]string
		device  string
		mounted bool
	}{
		{"bind of the root", map[string]string{"type": "none", "o": "bind", "device": "/"}, "/", true},
		{"nfs share", map[string]string{"type": "nfs", "device": "host:/export"}, "", false},
		{"no mount options", map[string]string{"o": "rw"}, "", false},
		{"block device", map[string]string{"type": "ext4", "device": "/dev/sda1"}, "/dev/sda1", true},
		{"bind option alone", map[string]string{"o": "Bind,ro", "device": "/etc"}, "/etc", true},
		{"upper-case type none", map[string]string{"type": "NONE", "device": "/home"}, "/home", true},
		{"bind with no device", map[string]string{"type": "none", "o": "bind"}, "", false},
		{"empty", nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dev, ok := LocalDeviceMounted(c.opts)
			if dev != c.device || ok != c.mounted {
				t.Fatalf("LocalDeviceMounted(%v) = (%q, %v), want (%q, %v)", c.opts, dev, ok, c.device, c.mounted)
			}
		})
	}
}

func TestVectors(t *testing.T) {
	vs := Vectors()
	if len(vs) < 14 {
		t.Fatalf("Vectors() returned %d, want at least 14", len(vs))
	}
	seen := map[string]bool{}
	var escalating, clean int
	for _, v := range vs {
		if v.Name == "" || seen[v.Name] {
			t.Fatalf("vector name %q is empty or repeated", v.Name)
		}
		seen[v.Name] = true
		var spec map[string]any
		if err := json.Unmarshal(v.Spec, &spec); err != nil {
			t.Fatalf("vector %q: spec is not a JSON object: %v", v.Name, err)
		}
		if v.Escalates {
			escalating++
		} else {
			clean++
		}
	}
	if escalating < 8 || clean < 4 {
		t.Fatalf("want a mix of vectors, got %d escalating and %d clean", escalating, clean)
	}
	for _, want := range []string{
		"clean spec", "bind of the docker socket", "bind of the host root", "bind below /srv",
		"bind below /var/log", "bind of /var/log whole", "local volume that binds the host root",
		"local nfs volume", "named pipe mount", "added capability", "SELinux disabled", "SELinux spc_t",
		"seccomp unconfined", "AppArmor disabled", "host network by name", "host network by id",
	} {
		if !seen[want] {
			t.Errorf("missing vector %q", want)
		}
	}
	for _, v := range vs {
		if v.Name == "host network by id" && len(v.HostNetworkIDs) == 0 {
			t.Error("the host-network-by-id vector must list its host network IDs")
		}
	}
}
