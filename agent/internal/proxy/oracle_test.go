// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package proxy

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/coaming/proto/hostpath"
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

// oracleHostPath returns "" when a host path may be bound into a container and otherwise a short
// reason. It is an allow-list: the path must be absolute and sit under a data root or strictly below
// /var/log, and must not name a runtime socket.
func oracleHostPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return p + " is not an absolute path"
	}
	c := path.Clean(p)
	if slices.Contains(oracleSocketNames, path.Base(c)) {
		return p + " is a runtime socket"
	}
	if oracleUnder(c, oracleLogRoot) {
		if c == oracleLogRoot {
			return p + " is the whole of " + oracleLogRoot
		}
		return ""
	}
	for _, root := range oracleDataRoots {
		if oracleUnder(c, root) {
			return ""
		}
	}
	return p + " is outside the data roots"
}

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
		if reason := oracleHostPath(device); reason != "" {
			return "the bind device " + reason
		}
		return ""
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

// containerOracle returns the reasons a container create that was forwarded to a host without the
// owner's opt-in and with no way to look anything up would give the container access to the host;
// it is empty for a safe one. Like volumeOracle it is written from the safety property alone ("the
// container gets nothing of the host") and shares no code with the rule it checks: it imports nothing
// from proto/hostpath and calls no function of containerpolicy.go. It is an allow-list where the
// fields allow one, and it reads the zero environment: a named volume cannot be judged and so is a
// finding, and a user-defined network is not looked up.
func containerOracle(req container.CreateRequest) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	shared := func(mode string) bool {
		m := strings.ToLower(strings.TrimSpace(mode))
		return m == "host" || strings.HasPrefix(m, "container:")
	}
	if hc := req.HostConfig; hc != nil {
		switch {
		case hc.Privileged:
			add("the container is privileged")
		case len(hc.CapAdd) > 0:
			add("capabilities %v are added", hc.CapAdd)
		case len(hc.Devices) > 0 || len(hc.DeviceCgroupRules) > 0 || len(hc.DeviceRequests) > 0:
			add("the container is given devices")
		case hc.CgroupParent != "":
			add("the container is placed under the cgroup %q", hc.CgroupParent)
		case len(hc.VolumesFrom) > 0:
			add("the container takes the volumes of %v", hc.VolumesFrom)
		case hc.MaskedPaths != nil || hc.ReadonlyPaths != nil:
			add("the masked or read-only paths are replaced")
		case hc.Runtime != "" && hc.Runtime != "runc":
			add("the runtime is %q", hc.Runtime)
		}
		for name, mode := range map[string]string{
			"network": string(hc.NetworkMode), "pid": string(hc.PidMode), "ipc": string(hc.IpcMode),
			"uts": string(hc.UTSMode), "user": string(hc.UsernsMode), "cgroup": string(hc.CgroupnsMode),
		} {
			if shared(mode) {
				add("the %s namespace is shared (%q)", name, mode)
			}
		}
		for _, opt := range hc.SecurityOpt {
			if reason := oracleSecurityOpt(opt); reason != "" {
				add("%s", reason)
			}
		}
		for _, b := range hc.Binds {
			// With no colon the entry is a volume at a path inside the container.
			src, _, ok := strings.Cut(b, ":")
			switch {
			case !ok:
			case !strings.HasPrefix(src, "/"):
				add("the named volume %q cannot be judged with no lookup", src)
			default:
				if reason := oracleHostPath(src); reason != "" {
					add("the bind %s", reason)
				}
			}
		}
		for _, m := range hc.Mounts {
			switch string(m.Type) {
			case "bind", "":
				if reason := oracleHostPath(m.Source); reason != "" {
					add("the bind mount %s", reason)
				}
			case "volume":
				if m.Source != "" {
					add("the named volume %q cannot be judged with no lookup", m.Source)
				}
				if vo := m.VolumeOptions; vo != nil && vo.DriverConfig != nil {
					if reason := volumeOracle(vo.DriverConfig.Name, vo.DriverConfig.Options); reason != "" {
						add("the inline volume: %s", reason)
					}
				}
			case "tmpfs", "image":
			default:
				add("the mount type %q is not forwarded", m.Type)
			}
		}
	}
	if nc := req.NetworkingConfig; nc != nil {
		for name := range nc.EndpointsConfig {
			if strings.ToLower(strings.TrimSpace(name)) == "host" {
				add("the container joins the host network as an endpoint")
			}
		}
	}
	return out
}

// oracleSecurityOpt returns a reason when a security option lowers the sandbox, and "" otherwise.
// White space is dropped from the whole entry, so a spelling with spaces is judged as without.
func oracleSecurityOpt(opt string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(opt), ""))
	for _, sep := range []string{"=", ":"} {
		switch norm {
		case "seccomp" + sep + "unconfined", "apparmor" + sep + "unconfined", "label" + sep + "disable", "label" + sep + "type:spc_t",
			"systempaths" + sep + "unconfined":
			return "the security option " + opt + " lowers the sandbox"
		}
		if rest, ok := strings.CutPrefix(norm, "seccomp"+sep); ok && rest != "builtin" && rest != "default" {
			return "the security option " + opt + " is not the built-in seccomp profile"
		}
	}
	return ""
}

func TestContainerOracleAgreesWithTheVectors(t *testing.T) {
	// The rule refuses a security option the engine would refuse itself (an unknown key, a seccomp with no
	// value); the oracle names only what lowers the sandbox, so it finds nothing in these.
	ruleOnly := map[string]bool{"a security option the agent does not know": true, "a security option with no value": true}
	for _, v := range hostpath.ContainerVectors() {
		t.Run(v.Name, func(t *testing.T) {
			var req container.CreateRequest
			if err := decodeStrict(v.Body, &req); err != nil {
				t.Fatal(err)
			}
			reasons := containerOracle(req)
			lookups := len(v.Volumes) > 0 || len(v.HostNetworkIDs) > 0
			switch {
			case len(v.Refused) > 0 && !lookups && !ruleOnly[v.Name] && len(reasons) == 0:
				t.Errorf("the rule refuses %v and the oracle found nothing", v.Refused)
			case len(v.Refused) == 0:
				// With no lookups a named volume is the one thing the oracle cannot clear.
				for _, r := range reasons {
					if !strings.Contains(r, "cannot be judged with no lookup") {
						t.Errorf("the rule forwards the body and the oracle found: %s", r)
					}
				}
			}
		})
	}
}

func TestContainerOracleAcceptsASafeContainer(t *testing.T) {
	req := sdkContainerCreate()
	req.HostConfig.Binds = []string{"/srv/data:/data", "/anon"}
	req.HostConfig.SecurityOpt = []string{"no-new-privileges", "seccomp=builtin", "label=type:container_t"}
	if reasons := containerOracle(req); len(reasons) != 0 {
		t.Fatalf("the oracle refused a safe container: %v", reasons)
	}
}
