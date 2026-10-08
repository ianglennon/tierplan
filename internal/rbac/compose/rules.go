// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package compose

import (
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ruleUnresolvable is the rule reported for a checked value that holds an
// interpolation, which cannot be verified before the variables are known.
const (
	ruleUnresolvable = "unresolvable"
	whyUnresolvable  = "contains an interpolation that cannot be verified, so it is treated as a host-escalation feature"
)

// leaf is one checked value: its text as written and where to report it. key
// is only set by the top-level volume rule, which names a sub-key.
type leaf struct {
	text      string
	line, col int
	key       string
}

// rule is one row of the host-escalation table. Key is the service attribute
// it reads; Leaves extracts the values to check from that attribute; Bad says
// whether one value is a host-escalation feature. A leaf holding an
// interpolation is reported as "unresolvable" before Bad is consulted.
type rule struct {
	Name   string
	Key    string
	Why    string
	Leaves func(c *checker, v val) []leaf
	Bad    func(text string) bool
}

// serviceRules is the table of service attributes that escalate to the host
// (D-13). Rules run in this order; findings are re-sorted by position. Rules
// that only matter under Swarm (a stack deploy may ignore privileged, devices
// or cap_add) are flagged regardless: over-flagging is the safe direction.
var serviceRules = []rule{
	{
		Name:   "privileged",
		Key:    "privileged",
		Why:    "a privileged container has every capability and every host device",
		Leaves: scalarLeaves,
		Bad:    func(t string) bool { return !clearlyFalse(t) },
	},
	{
		Name:   "cap_add",
		Key:    "cap_add",
		Why:    "adds Linux capabilities to the container (ALL is root on the host)",
		Leaves: listLeaves,
		Bad:    always,
	},
	{
		Name:   "devices",
		Key:    "devices",
		Why:    "gives the container access to a host device",
		Leaves: listLeaves,
		Bad:    always,
	},
	{
		Name:   "device_cgroup_rules",
		Key:    "device_cgroup_rules",
		Why:    "allows access to host devices by major and minor number",
		Leaves: listLeaves,
		Bad:    always,
	},
	{
		Name:   "pid",
		Key:    "pid",
		Why:    "shares the host or another container's process namespace",
		Leaves: scalarLeaves,
		Bad:    hostOrContainer,
	},
	{
		Name:   "ipc",
		Key:    "ipc",
		Why:    "shares the host or another container's IPC namespace",
		Leaves: scalarLeaves,
		Bad:    hostOrContainer,
	},
	{
		Name:   "uts",
		Key:    "uts",
		Why:    "shares the host's hostname namespace",
		Leaves: scalarLeaves,
		Bad:    isHost,
	},
	{
		Name:   "userns_mode",
		Key:    "userns_mode",
		Why:    "opts out of user namespace remapping",
		Leaves: scalarLeaves,
		Bad:    isHost,
	},
	{
		Name:   "cgroup",
		Key:    "cgroup",
		Why:    "uses the host's cgroup namespace",
		Leaves: scalarLeaves,
		Bad:    isHost,
	},
	{
		Name:   "cgroup_parent",
		Key:    "cgroup_parent",
		Why:    "places the container under an arbitrary host cgroup",
		Leaves: scalarLeaves,
		Bad:    func(t string) bool { return strings.TrimSpace(t) != "" },
	},
	{
		Name:   "network_mode",
		Key:    "network_mode",
		Why:    "uses the host network or another container's network namespace",
		Leaves: scalarLeaves,
		Bad:    hostOrContainer,
	},
	{
		Name:   "security_opt",
		Key:    "security_opt",
		Why:    "turns off or replaces the container's confinement (seccomp, AppArmor, SELinux, masked paths, no-new-privileges)",
		Leaves: listLeaves,
		Bad:    securityOptBad,
	},
	{
		Name:   "volumes",
		Key:    "volumes",
		Why:    "host takeover through the Docker socket, a system path or any host path outside the data directories (/srv, /mnt, /media)",
		Leaves: volumeLeaves,
		Bad:    hostPathBad,
	},
}

// volumeRule reads a top-level named volume (Key is unused for it): the local
// driver turns type none with o bind into a bind mount of device.
var volumeRule = rule{
	Name:   "volume_driver_opts",
	Why:    "the local volume driver turns these options into a bind mount of a host path",
	Leaves: driverOptsLeaves,
	Bad:    hostPathBad,
}

// at builds a leaf for a node at the position the walk reports for it.
func at(v val, text string) leaf {
	return leaf{text: text, line: v.line(), col: v.column()}
}

// scalarLeaves checks a single-valued attribute. A null or absent value has no
// leaf; a mapping or sequence where a scalar belongs is one leaf with a
// placeholder text, so a rule that fails closed still sees it.
func scalarLeaves(c *checker, v val) []leaf {
	v = c.resolve(v)
	switch v.n.Kind {
	case yaml.ScalarNode:
		if v.n.ShortTag() == "!!null" {
			return nil
		}
		return []leaf{at(v, v.n.Value)}
	case yaml.MappingNode:
		return []leaf{at(v, "<mapping>")}
	case yaml.SequenceNode:
		return []leaf{at(v, "<sequence>")}
	}
	return nil
}

// clearlyFalse reports whether a boolean field is a literal false. YAML 1.1 and
// 1.2 disagree about yes/no/on/off, so only the spellings of false that both
// agree are not "set" are accepted; anything else counts as set.
func clearlyFalse(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "false", "no", "off", "n", "0":
		return true
	}
	return false
}

// interpolated reports whether a value holds a compose interpolation: ${...}
// or a bare $NAME. It over-matches the escaped $$ forms on purpose.
func interpolated(s string) bool {
	if strings.Contains(s, "${") {
		return true
	}
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		n := s[i+1]
		if n == '_' || (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z') {
			return true
		}
	}
	return false
}

func always(string) bool { return true }

func isHost(t string) bool { return strings.ToLower(strings.TrimSpace(t)) == "host" }

// hostOrContainer matches host and container:<name>; none, bridge, shareable,
// private and service:<name> are not escalations.
func hostOrContainer(t string) bool {
	v := strings.ToLower(strings.TrimSpace(t))
	return v == "host" || strings.HasPrefix(v, "container:")
}

// listLeaves checks a list attribute: one leaf per entry. A long-syntax entry
// (a mapping) is checked by its source; a scalar where a list belongs is one
// entry. An empty or null attribute has no leaves.
func listLeaves(c *checker, v val) []leaf {
	v = c.resolve(v)
	if v.n.Kind != yaml.SequenceNode {
		return scalarLeaves(c, v)
	}
	var out []leaf
	for _, item := range v.n.Content {
		if !c.spend() {
			break
		}
		it := c.resolve(val{n: item, at: v.at})
		switch it.n.Kind {
		case yaml.ScalarNode:
			out = append(out, at(it, it.n.Value))
		case yaml.MappingNode:
			if src, ok := c.scalarField(c.entries(it, 0), "source"); ok {
				out = append(out, at(src, src.n.Value))
			} else {
				out = append(out, at(it, "<mapping>"))
			}
		default:
			out = append(out, at(it, "<sequence>"))
		}
	}
	return out
}

// scalarField returns the first non-null scalar entry called name.
func (c *checker) scalarField(fields []entry, name string) (val, bool) {
	for _, f := range fields {
		if f.name != name {
			continue
		}
		x := c.resolve(f.val)
		if x.n.Kind == yaml.ScalarNode && x.n.ShortTag() != "!!null" {
			return x, true
		}
	}
	return val{}, false
}

// securityOptBad reports whether one security_opt entry disables or replaces
// confinement. Docker accepts both key=value and key:value.
func securityOptBad(t string) bool {
	t = strings.TrimSpace(t)
	key, value := t, ""
	if i := strings.IndexAny(t, "=:"); i >= 0 {
		key, value = t[:i], t[i+1:]
	}
	key = strings.ToLower(strings.TrimSpace(key))
	value = strings.ToLower(strings.TrimSpace(value))
	switch key {
	case "seccomp":
		// unconfined, and also a custom profile: it is a profile the reviewer
		// cannot see from here.
		return true
	case "apparmor", "systempaths":
		return value == "unconfined"
	case "label":
		return value == "disable" || value == "type:spc_t"
	case "no-new-privileges":
		switch value {
		case "", "true", "1", "yes", "on":
			return false
		}
		return true
	}
	return false
}

// volumeLeaves checks the service volumes list in short and long syntax. Only
// host sources are leaves: a named volume, a relative path, a tmpfs, an image
// mount and an anonymous volume are never host paths here.
func volumeLeaves(c *checker, v val) []leaf {
	v = c.resolve(v)
	if v.n.Kind != yaml.SequenceNode {
		return nil
	}
	var out []leaf
	for _, item := range v.n.Content {
		if !c.spend() {
			break
		}
		it := c.resolve(val{n: item, at: v.at})
		switch it.n.Kind {
		case yaml.ScalarNode:
			s := it.n.Value
			src, ok := shortSource(s)
			switch {
			case !ok:
			case interpolated(src):
				out = append(out, at(it, s))
			default:
				out = append(out, at(it, src))
			}
		case yaml.MappingNode:
			fields := c.entries(it, 0)
			if t, ok := c.scalarField(fields, "type"); ok {
				switch strings.ToLower(strings.TrimSpace(t.n.Value)) {
				case "volume", "tmpfs", "image", "cluster":
					continue
				}
			}
			if src, ok := c.scalarField(fields, "source"); ok {
				out = append(out, at(src, src.n.Value))
			}
		}
	}
	return out
}

// shortSource returns the source of a short-syntax volume entry
// (source:target[:mode]); an entry with no colon is an anonymous volume and
// has none. A Windows drive letter's colon is not a separator.
func shortSource(s string) (string, bool) {
	start := 0
	if len(s) >= 3 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		start = 2
	}
	i := strings.IndexByte(s[start:], ':')
	if i < 0 {
		return "", false
	}
	src := s[:start+i]
	return src, src != ""
}

// Host paths a container may be given without env Admin. The rule for an
// absolute bind source is an allow-list, not a deny-list: every absolute host
// path is a host-escalation feature unless it sits under one of these data
// roots, so a system or runtime path nobody thought to list (the containerd
// state directory, /usr, /home, ...) fails closed. The roots are conventional
// places for data a stack is meant to bind. The Bayplan data directory and
// anything else a user keeps under one of them cannot be known to this pass, so
// an operator who stores secrets under /srv or /mnt should not rely on it.
var (
	// dataRoots are directories a bind may name, or sit under.
	dataRoots = []string{"/srv", "/mnt", "/media"}
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
)

// underTree reports whether the cleaned absolute path p is root or below it.
func underTree(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

// socketPath reports whether p names a container runtime socket or the Windows
// Docker named pipe.
func socketPath(p string) bool {
	if strings.Contains(strings.ToLower(strings.ReplaceAll(p, "\\", "/")), "pipe/docker_engine") {
		return true
	}
	return slices.Contains(socketNames, path.Base(path.Clean(p)))
}

// hostPathBad reports whether a bind source (or local-driver device) gives the
// container something it should not have without env Admin. The path is
// cleaned first, so .., repeated separators and a trailing slash do not hide
// it.
//
//   - A runtime socket name or the Docker pipe is bad wherever it is, absolute
//     or relative.
//   - An absolute path is bad unless it is under a data root (dataRoots) or
//     strictly below /var/log.
//   - A path starting with ~ is bad: compose expands it to a home directory.
//   - A named volume or a relative path inside the project is fine. A relative
//     path that climbs out of the project with .. cannot be placed without the
//     project directory, so what follows the climb is read as if it started at
//     the root: ../../../../var/run/docker.sock is bad, ../shared is not. Two
//     or more .. with nothing after them (../..) could be the root and are bad.
func hostPathBad(p string) bool {
	p = strings.TrimSpace(p)
	if socketPath(p) {
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
		for _, root := range dataRoots {
			if underTree(p, root) {
				return false
			}
		}
		return true
	}
	return climbingPathBad(p)
}

// climbingPathBad judges a relative path by where a climb out of the project
// directory could land.
func climbingPathBad(p string) bool {
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

// fileSourceBad reports whether the file: source of a top-level secret or
// config reaches outside the project directory: an absolute path (POSIX or a
// Windows drive or UNC path), a ~ path, or a relative path that climbs out with
// .. . The engine reads such a file on whichever host loads the compose file
// and hands its content to the container, so a below-Admin deployer could
// otherwise read the master key or the database. A relative path inside the
// project is fine; a symlink inside the project is not visible to this pass.
func fileSourceBad(p string) bool {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return true
	}
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) {
		return true
	}
	c := path.Clean(p)
	return c == ".." || strings.HasPrefix(c, "../")
}

// fileRules read the file: source of a top-level secret and of a top-level
// config (Key is unused for them).
var (
	secretFileRule = rule{
		Name:   "secret_file",
		Why:    "reads a file from outside the project directory on the host that loads the compose file",
		Leaves: fileSourceLeaves,
		Bad:    fileSourceBad,
	}
	configFileRule = rule{
		Name:   "config_file",
		Why:    "reads a file from outside the project directory on the host that loads the compose file",
		Leaves: fileSourceLeaves,
		Bad:    fileSourceBad,
	}
)

// fileSourceLeaves checks the file: key of one top-level secret or config.
func fileSourceLeaves(c *checker, v val) []leaf {
	var out []leaf
	for _, f := range c.entries(v, 0) {
		if f.name != "file" {
			continue
		}
		if x, ok := c.scalarField([]entry{f}, "file"); ok {
			out = append(out, leaf{text: x.n.Value, line: x.line(), col: x.column(), key: "file"})
		}
	}
	return out
}

// networkFSTypes are local-driver mount types whose device is a remote share
// (host:/export, //server/share), not a host path.
var networkFSTypes = []string{"nfs", "nfs4", "cifs", "smb3", "smbfs", "ceph", "glusterfs"}

// driverOptsLeaves checks one top-level volume definition. Only the local
// driver mounts a device, and it does so for type none with o bind (a bind of
// a host path) and for any filesystem type (a host block device such as
// /dev/sda1). So the device is checked when o contains bind, when type is none,
// when type is any filesystem other than a network one, and when o or type
// cannot be read (interpolation). With no type and no bind option nothing is
// mounted and the device is not checked.
func driverOptsLeaves(c *checker, v val) []leaf {
	fields := c.entries(v, 0)
	var out []leaf
	if d, ok := c.scalarField(fields, "driver"); ok {
		name := strings.ToLower(strings.TrimSpace(d.n.Value))
		switch {
		case interpolated(d.n.Value):
			out = append(out, leaf{text: d.n.Value, line: d.line(), col: d.column(), key: "driver"})
		case name != "local":
			return nil
		}
	}
	var opts []entry
	for _, f := range fields {
		if f.name == "driver_opts" {
			opts = append(opts, c.entries(f.val, 0)...)
		}
	}
	binds, unknown, fsType := false, false, false
	if o, ok := c.scalarField(opts, "o"); ok {
		if interpolated(o.n.Value) {
			unknown = true
			out = append(out, leaf{text: o.n.Value, line: o.line(), col: o.column(), key: "driver_opts.o"})
		} else if strings.Contains(strings.ToLower(o.n.Value), "bind") {
			binds = true
		}
	}
	if t, ok := c.scalarField(opts, "type"); ok {
		kind := strings.ToLower(strings.TrimSpace(t.n.Value))
		switch {
		case interpolated(t.n.Value):
			unknown = true
			out = append(out, leaf{text: t.n.Value, line: t.line(), col: t.column(), key: "driver_opts.type"})
		case kind == "none":
			binds = true
		case kind != "" && !slices.Contains(networkFSTypes, kind):
			fsType = true
		}
	}
	if d, ok := c.scalarField(opts, "device"); ok && (binds || unknown || fsType) {
		out = append(out, leaf{text: d.n.Value, line: d.line(), col: d.column(), key: "driver_opts.device"})
	}
	return out
}
