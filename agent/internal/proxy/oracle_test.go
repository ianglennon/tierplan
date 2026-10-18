// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"path"
	"slices"
	"strings"
	"testing"
)

// The oracle below is the fuzz invariant for local volumes. It is written from
// the safety property alone ("a forwarded local volume gives tasks no access to
// the host") and shares no code with the rule it checks: it imports nothing from
// proto/hostpath and calls no function of servicepolicy.go or body.go. A shape
// the rule's author did not think of (as CR-02's relative device and overlay)
// is therefore not also missing from the invariant.
var (
	oracleDataRoots   = []string{"/srv", "/mnt", "/media"}
	oracleLogRoot     = "/var/log"
	oracleSocketNames = []string{"docker.sock", "containerd.sock", "podman.sock", "crio.sock"}
	oracleSafeTypes   = []string{"", "tmpfs", "nfs", "nfs4", "cifs", "smb3", "smbfs", "ceph", "glusterfs"}
)

func oracleUnder(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

// volumeOracle returns "" when a local volume with these options gives tasks no
// host access, and otherwise a short reason. It is an allow-list: anything it
// does not recognise as safe is refused.
func volumeOracle(driver string, opts map[string]string) string {
	if d := strings.ToLower(strings.TrimSpace(driver)); d != "" && d != "local" {
		return "the driver " + driver + " is not the local driver"
	}
	kind := strings.ToLower(strings.TrimSpace(opts["type"]))
	if strings.Contains(strings.ToLower(opts["o"]), "bind") || kind == "none" {
		device := opts["device"]
		if device == "" {
			return ""
		}
		if !strings.HasPrefix(device, "/") {
			return "the bind device " + device + " is not an absolute path"
		}
		c := path.Clean(device)
		if slices.Contains(oracleSocketNames, path.Base(c)) {
			return "the bind device " + device + " is a runtime socket"
		}
		if oracleUnder(c, oracleLogRoot) {
			if c == oracleLogRoot {
				return "the bind device " + device + " is the whole of " + oracleLogRoot
			}
			return ""
		}
		for _, root := range oracleDataRoots {
			if oracleUnder(c, root) {
				return ""
			}
		}
		return "the bind device " + device + " is outside the data roots"
	}
	if !slices.Contains(oracleSafeTypes, kind) {
		return "the filesystem type " + kind + " is not a safe one"
	}
	return ""
}

func TestVolumeOracleRefusesEveryKnownHostAccessShape(t *testing.T) {
	bind := func(device string) map[string]string {
		return map[string]string{"type": "none", "o": "bind", "device": device}
	}
	fs := func(kind, device, o string) map[string]string {
		m := map[string]string{"type": kind, "device": device}
		if o != "" {
			m["o"] = o
		}
		return m
	}
	refused := []struct {
		name   string
		driver string
		opts   map[string]string
	}{
		{"relative etc", "local", bind("etc")},
		{"relative var/lib/docker", "local", bind("var/lib/docker")},
		{"relative var/run", "local", bind("var/run")},
		{"relative docker socket", "local", bind("var/run/docker.sock")},
		{"leading space", "local", bind(" /srv/data")},
		{"root", "local", bind("/")},
		{"etc", "local", bind("/etc")},
		{"climb out of srv", "local", bind("/srv/../etc")},
		{"socket under srv", "local", bind("/srv/docker.sock")},
		{"var log whole", "local", bind("/var/log")},
		{"overlay lowerdir root", "local", fs("overlay", "overlay", "lowerdir=/:/srv/empty")},
		{"proc", "local", fs("proc", "proc", "")},
		{"sysfs", "local", fs("sysfs", "sysfs", "")},
		{"devtmpfs", "local", fs("devtmpfs", "devtmpfs", "")},
		{"ext4 on a block device", "local", fs("ext4", "/dev/sda1", "")},
		{"bind with a network type", "local", fs("nfs", "etc", "bind")},
		{"plugin driver", "rexray/ebs", nil},
	}
	for _, c := range refused {
		if volumeOracle(c.driver, c.opts) == "" {
			t.Errorf("%s: the oracle accepted %q %v", c.name, c.driver, c.opts)
		}
	}
	accepted := []struct {
		name   string
		driver string
		opts   map[string]string
	}{
		{"tmpfs", "local", map[string]string{"type": "tmpfs"}},
		{"tmpfs with a device", "local", fs("tmpfs", "tmpfs", "")},
		{"tmpfs sized", "local", fs("tmpfs", "tmpfs", "size=64m")},
		{"nfs", "local", fs("nfs", ":/export", "")},
		{"cifs", "local", fs("cifs", "//server/share", "")},
		{"bind under srv", "local", bind("/srv/data")},
		{"bind under mnt", "local", bind("/mnt/backups")},
		{"bind under media", "local", bind("/media/usb")},
		{"bind below var log", "local", bind("/var/log/app")},
		{"no options", "local", nil},
		{"upper case local driver", "LOCAL", nil},
		{"empty driver", "", map[string]string{}},
	}
	for _, c := range accepted {
		if reason := volumeOracle(c.driver, c.opts); reason != "" {
			t.Errorf("%s: the oracle refused %q %v: %s", c.name, c.driver, c.opts, reason)
		}
	}
}
