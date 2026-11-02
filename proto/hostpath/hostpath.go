// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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
// places for data a stack is meant to bind. The TierPlan data directory and
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

// Bad reports whether a bind source (a compose or service bind mount) gives the
// container something it should not have without env Admin. The path is
// cleaned first, so .., repeated separators and a trailing slash do not hide
// it. Compose reads a relative bind source against the project directory; that
// reading does not hold for a local-driver volume device, which dockerd passes
// to mount(2) unchanged, so LocalVolumeHostAccess judges that and not Bad.
//
//   - A runtime socket name or the Docker pipe is bad wherever it is, absolute
//     or relative.
//   - An absolute path is bad unless it is under a data root (DataRoots) or
//     strictly below /var/log.
//   - A path starting with ~ is bad: compose expands it to a home directory.
//   - A named volume or a relative path inside the project is fine, for a bind
//     source (not for a local-driver device, see above). A relative path that
//     climbs out of the project with .. cannot be placed without the project
//     directory, so what follows the climb is read as if it started at the
//     root: ../../../../var/run/docker.sock is bad, ../shared is not. Two or
//     more .. with nothing after them (../..) could be the root and are bad.
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
// mounted. The values are compared case-insensitively. A volume judged as the
// engine mounts it uses LocalVolumeHostAccess instead.
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

// LocalVolumeHostAccess judges the options of a local-driver volume the way
// dockerd uses them, and returns the option key that gives tasks host access
// ("device" or "type"), or "" when none does.
//
// dockerd hands device, type and o to mount(2) unchanged, so a relative device
// is read against the daemon's own working directory (usually /), not against a
// project directory. The rule is therefore different from Bad's:
//
//  1. When o contains bind (compared case-insensitively) or the trimmed,
//     lower-cased type is none, the daemon binds device as given. An empty
//     device mounts nothing and gives "". A device that does not start with /
//     gives "device", as does one with leading or trailing white space (Bad
//     would trim it first, but the daemon does not). Otherwise Bad decides.
//     This holds whatever the type is, so a bind given with a network type is
//     judged as a bind.
//  2. Otherwise the trimmed, lower-cased type decides. No type (nothing is
//     mounted), tmpfs (memory; the device is only a label) and the members of
//     NetworkFSTypes (the device is a remote share) give "". Any other type
//     gives "type": a block-device filesystem, an overlay whose options name
//     host directories and a host pseudo filesystem (proc, sysfs, devtmpfs,
//     cgroup2) get host access from the type, which a path check cannot see.
//
// Per D-36/D-38 as amended this is what "or a device" means. The set of
// allowed types is closed on purpose: widening it needs an agent release.
func LocalVolumeHostAccess(opts map[string]string) string {
	kind := strings.ToLower(strings.TrimSpace(opts["type"]))
	if strings.Contains(strings.ToLower(opts["o"]), "bind") || kind == "none" {
		device := opts["device"]
		switch {
		case device == "":
			return ""
		case !strings.HasPrefix(device, "/"), device != strings.TrimSpace(device):
			return "device"
		case Bad(device):
			return "device"
		}
		return ""
	}
	if kind == "" || kind == "tmpfs" || slices.Contains(NetworkFSTypes, kind) {
		return ""
	}
	return "type"
}
