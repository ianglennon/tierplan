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

func TestLocalVolumeHostAccess(t *testing.T) {
	bind := func(device string) map[string]string {
		return map[string]string{"type": "none", "o": "bind", "device": device}
	}
	cases := []struct {
		name string
		opts map[string]string
		want string
	}{
		{"no options", nil, ""},
		{"a bind with no device", map[string]string{"type": "none", "o": "bind"}, ""},
		{"a bind of the root", bind("/"), "device"},
		{"a bind of /etc", bind("/etc"), "device"},
		{"a bind under /srv", bind("/srv/data"), ""},
		{"a bind under /media", bind("/media/disk"), ""},
		{"a bind strictly below /var/log", bind("/var/log/app"), ""},
		{"a bind of /var/log whole", bind("/var/log"), "device"},
		{"a relative bind of etc", bind("etc"), "device"},
		{"a relative bind of var/lib/docker", bind("var/lib/docker"), "device"},
		{"a relative bind of var/run", bind("var/run"), "device"},
		{"a relative bind of the docker socket", bind("var/run/docker.sock"), "device"},
		{"a relative bind of srv, which the daemon reads from its own directory", bind("srv/data"), "device"},
		{"a dot-relative bind", bind("./data"), "device"},
		{"a device with a leading space", bind(" /srv/data"), "device"},
		{"a device with a trailing space", bind("/srv "), "device"},
		{"type NONE in capitals", map[string]string{"type": "NONE", "device": "/home"}, "device"},
		{"o Bind,ro with no type", map[string]string{"o": "Bind,ro", "device": "/etc"}, "device"},
		{"a bind with a network type is judged as a bind", map[string]string{"type": "nfs", "o": "bind", "device": "etc"}, "device"},
		{"tmpfs with bind is judged as a bind", map[string]string{"type": "tmpfs", "o": "bind", "device": "etc"}, "device"},
		{"tmpfs", map[string]string{"type": "tmpfs", "device": "tmpfs"}, ""},
		{"tmpfs with a size", map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "size=64m,uid=1000"}, ""},
		{"an nfs share", map[string]string{"type": "nfs", "o": "addr=10.0.0.2,rw", "device": ":/export"}, ""},
		{"an nfs4 share", map[string]string{"type": "nfs4", "device": "host:/export"}, ""},
		{"a cifs share", map[string]string{"type": "cifs", "device": "//server/share"}, ""},
		{"a relative device with a network type and no bind is a remote share", map[string]string{"type": "nfs", "device": "host:export"}, ""},
		{"options that mount nothing", map[string]string{"o": "rw"}, ""},
		{"a block device", map[string]string{"type": "ext4", "device": "/dev/sda1"}, "type"},
		{"an overlay with the host root as a lower dir", map[string]string{"type": "overlay", "device": "overlay", "o": "lowerdir=/:/srv/empty"}, "type"},
		{"a proc filesystem", map[string]string{"type": "proc", "device": "proc"}, "type"},
		{"a sysfs filesystem", map[string]string{"type": "sysfs", "device": "sysfs"}, "type"},
		{"devtmpfs", map[string]string{"type": "devtmpfs", "device": "devtmpfs"}, "type"},
		{"a type in capitals with spaces", map[string]string{"type": " EXT4 ", "device": "/dev/sda1"}, "type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LocalVolumeHostAccess(c.opts); got != c.want {
				t.Fatalf("LocalVolumeHostAccess(%v) = %q, want %q", c.opts, got, c.want)
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
		"local nfs volume", "local volume that binds a relative device", "local overlay volume over the host root",
		"named pipe mount", "added capability", "SELinux disabled", "SELinux spc_t",
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
