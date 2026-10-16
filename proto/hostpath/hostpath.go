// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package hostpath is the one rule that says which host paths a container may
// be given without extra privilege. The compose policy (stacks) and the agent's
// service-spec check both call it, so a path an Operator may bind in a stack is
// a path an Operator may bind in a service (D-38). Standard library only.
package hostpath

import (
	"path"
	"slices"
	"strings"
)

// Host paths a container may be given without env Admin. The rule for an
// absolute bind source is an allow-list, not a deny-list: every absolute host
// path is a host-escalation feature unless it sits under one of these data
// roots, so a system or runtime path nobody thought to list (the containerd
// state directory, /usr, /home, ...) fails closed. The roots are conventional
// places for data a stack is meant to bind. The Bayplan data directory and
// anything else a user keeps under one of them cannot be known to this pass, so
// an operator who stores secrets under /srv or /mnt should not rely on it.
var (
	// DataRoots are directories a bind may name, or sit under.
	DataRoots = []string{"/srv", "/mnt", "/media"}
	// logRoot may be bound only below itself (/var/log/app), never whole.
	logRoot = "/var/log"
	// socketNames are runtime socket file names, wherever they are mounted from.
	socketNames = []string{"docker.sock", "containerd.sock", "podman.sock", "crio.sock"}
	// systemTrees are directories whose subtree is never a data directory. They
	// are used for a relative path that climbs out of the project directory,
	// where the real location is unknown and so the rest of the path is read as
	// if the climb reached the root.
	systemTrees = []string{
		"/etc", "/proc", "/sys", "/dev", "/boot", "/root", "/home", "/usr", "/bin", "/sbin",
		"/lib", "/lib32", "/lib64", "/opt", "/run", "/var/run", "/var/lib", "/var/spool", "/tmp",
	}
	// NetworkFSTypes are local-driver mount types whose device is a remote share
	// (host:/export, //server/share), not a host path.
	NetworkFSTypes = []string{"nfs", "nfs4", "cifs", "smb3", "smbfs", "ceph", "glusterfs"}
)

// underTree reports whether the cleaned absolute path p is root or below it.
func underTree(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

// Socket reports whether p names a container runtime socket or the Windows
// Docker named pipe.
func Socket(p string) bool {
	if strings.Contains(strings.ToLower(strings.ReplaceAll(p, "\\", "/")), "pipe/docker_engine") {
		return true
	}
	return slices.Contains(socketNames, path.Base(path.Clean(p)))
}

// Bad reports whether a bind source (or local-driver device) gives the
// container something it should not have without env Admin. The path is
// cleaned first, so .., repeated separators and a trailing slash do not hide
// it.
//
//   - A runtime socket name or the Docker pipe is bad wherever it is, absolute
//     or relative.
//   - An absolute path is bad unless it is under a data root (DataRoots) or
//     strictly below /var/log.
//   - A path starting with ~ is bad: compose expands it to a home directory.
//   - A named volume or a relative path inside the project is fine. A relative
//     path that climbs out of the project with .. cannot be placed without the
//     project directory, so what follows the climb is read as if it started at
//     the root: ../../../../var/run/docker.sock is bad, ../shared is not. Two
//     or more .. with nothing after them (../..) could be the root and are bad.
func Bad(p string) bool {
	p = strings.TrimSpace(p)
	if Socket(p) {
		return true
	}
	if strings.HasPrefix(p, "~") {
		return true
	}
	if strings.HasPrefix(p, "/") {
		p = path.Clean(p)
		switch {
		case p == "/":
			return true
		case underTree(p, logRoot):
			return p == logRoot
		}
		for _, root := range DataRoots {
			if underTree(p, root) {
				return false
			}
		}
		return true
	}
	return ClimbingBad(p)
}

// ClimbingBad judges a relative path by where a climb out of the project
// directory could land.
func ClimbingBad(p string) bool {
	p = path.Clean(p)
	climbs := 0
	for strings.HasPrefix(p, "../") || p == ".." {
		p = strings.TrimPrefix(strings.TrimPrefix(p, ".."), "/")
		climbs++
	}
	if climbs == 0 {
		return false
	}
	if p == "" {
		return climbs >= 2
	}
	q := "/" + p
	if q == "/var" {
		return true
	}
	for _, tree := range systemTrees {
		if underTree(q, tree) {
			return true
		}
	}
	return false
}

// MountsDevice reports whether a local-driver volume with these options
// mounts its device: the local driver does so for type none with o bind (a bind
// of a host path) and for any filesystem type other than a network one (a host
// block device such as /dev/sda1). With no type and no bind option nothing is
// mounted. The values are compared case-insensitively.
func MountsDevice(o, fsType string) bool {
	if strings.Contains(strings.ToLower(o), "bind") {
		return true
	}
	kind := strings.ToLower(strings.TrimSpace(fsType))
	switch {
	case kind == "none":
		return true
	case kind != "" && !slices.Contains(NetworkFSTypes, kind):
		return true
	}
	return false
}

// LocalDeviceMounted returns the device of a local-driver volume, and whether
// the options mount it (keys o, type and device). A device that is not mounted
// is a remote share or an unused option and is not returned.
func LocalDeviceMounted(opts map[string]string) (device string, mounted bool) {
	device = opts["device"]
	if device == "" || !MountsDevice(opts["o"], opts["type"]) {
		return "", false
	}
	return device, true
}

// LocalVolumeHostAccess is a stub so the test compiles; replaced in the next commit.
func LocalVolumeHostAccess(opts map[string]string) string { return "" }
