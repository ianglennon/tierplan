// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package compose_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/rbac/compose"
)

// CR-A01: a bind source is allowed only under the data roots, so a system or
// runtime path that no deny-list named still needs env Admin.
func TestBindSourcesOutsideTheDataRootsAreFlagged(t *testing.T) {
	flagged := []string{
		// container runtime sockets and the directories that hold them
		"/run/containerd", "/var/run/containerd", "/run/podman", "/var/lib/containerd",
		"/run/containerd/containerd.sock", "/var/run/crio/crio.sock", "/run/user/1000",
		// system trees a writable bind turns into host takeover
		"/usr", "/usr/bin", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/home", "/home/me/.ssh",
		"/var/spool/cron", "/var/log", "/var/lib", "/tmp", "/mnt/../etc", "/srv/../root",
		"//usr", "/usr/",
		// a socket name is bad even under a data root
		"/srv/app/docker.sock", "/mnt/x/containerd.sock",
		// relative paths that climb to a system path, and home expansion
		"../../../../var/run/docker.sock", "../../../../etc/passwd", "./a/../../../usr/bin",
		"~", "~/.ssh", "~root/.ssh", "../..", "../../..", "../var", "docker.sock", "./run/docker.sock",
	}
	for _, p := range flagged {
		t.Run("short "+p, func(t *testing.T) {
			if got := ruleValues(t, service("volumes: [\""+p+":/x\"]")); !slices.Equal(got, []string{"volumes:" + p}) {
				t.Fatalf("%s must be flagged, got %v", p, got)
			}
		})
		t.Run("long "+p, func(t *testing.T) {
			if got := ruleValues(t, service("volumes:\n  - type: bind\n    source: \""+p+"\"\n    target: /x")); !slices.Equal(got, []string{"volumes:" + p}) {
				t.Fatalf("%s must be flagged in long syntax, got %v", p, got)
			}
		})
	}
	allowed := []string{
		"/srv", "/srv/app", "/mnt/disk1/media", "/media/usb", "/var/log/app", "/var/log/app/", "/srv//a/./b",
		"./data", "data", "../shared", "../../shared", "..", "./a/../b", "named_volume", "subdir/x.conf",
	}
	for _, p := range allowed {
		t.Run("ok "+p, func(t *testing.T) {
			if got := ruleValues(t, service("volumes: [\""+p+":/x\"]")); len(got) != 0 {
				t.Fatalf("%s must not be flagged, got %v", p, got)
			}
		})
	}
}

// CR-A01: the local driver mounts a block device for any filesystem type, not
// only for type none, and the device is checked the same way as a bind.
func TestLocalVolumeDevicesAreFlaggedForEveryMountType(t *testing.T) {
	cases := []struct {
		name string
		vol  string
		want []string
	}{
		{"ext4 block device", "{driver_opts: {type: ext4, device: /dev/sda1}}", []string{"volume_driver_opts:/dev/sda1"}},
		{"xfs block device with options", `{driver: local, driver_opts: {type: xfs, o: "ro", device: /dev/mapper/vg-root}}`, []string{"volume_driver_opts:/dev/mapper/vg-root"}},
		{"btrfs in data root is fine", "{driver_opts: {type: btrfs, device: /srv/img}}", nil},
		{"bind of containerd state", "{driver_opts: {type: none, o: bind, device: /var/lib/containerd}}", []string{"volume_driver_opts:/var/lib/containerd"}},
		{"bind of usr", `{driver_opts: {type: none, o: "bind,ro", device: /usr}}`, []string{"volume_driver_opts:/usr"}},
		{"bind of home", "{driver_opts: {type: none, o: bind, device: /home}}", []string{"volume_driver_opts:/home"}},
		{"bind of data root is fine", "{driver_opts: {type: none, o: bind, device: /srv/data}}", nil},
		{"relative climb to the socket", "{driver_opts: {type: none, o: bind, device: ../../../../var/run/docker.sock}}", []string{"volume_driver_opts:../../../../var/run/docker.sock"}},
		{"nfs share", `{driver_opts: {type: nfs, o: "addr=10.0.0.1", device: ":/export"}}`, nil},
		{"nfs4 share", `{driver_opts: {type: nfs4, o: "addr=10.0.0.1", device: "10.0.0.1:/export"}}`, nil},
		{"cifs share", `{driver_opts: {type: cifs, o: "username=u", device: "//10.0.0.1/share"}}`, nil},
		{"tmpfs", "{driver_opts: {type: tmpfs, device: tmpfs, o: size=100m}}", nil},
		{"device with no type and no bind mounts nothing", "{driver_opts: {device: /dev/sda1}}", nil},
		{"another driver is not the local driver", "{driver: other, driver_opts: {type: ext4, device: /dev/sda1}}", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ruleValues(t, "volumes:\n  v: "+tc.vol+"\n"); !sameStrings(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// WR-A03: a top-level secret or config file: source is read by the host that
// loads the compose file and handed to the container.
func TestTopLevelSecretAndConfigFileSourcesOutsideTheProjectAreFlagged(t *testing.T) {
	for _, section := range []string{"secrets", "configs"} {
		rule := map[string]string{"secrets": "secret_file", "configs": "config_file"}[section]
		flagged := []string{
			"/data/bayplan.key", "/data/bayplan.db", "/etc/shadow", "/srv/app/token", "../../../../data/bayplan.key",
			"../x", "..", "./a/../../x", "~/.ssh/id_ed25519", `C:\Users\me\key`, `\\server\share\key`, "//server/share",
		}
		for _, p := range flagged {
			t.Run(section+" "+p, func(t *testing.T) {
				fs, err := compose.Check([]byte(section + ":\n  k:\n    file: '" + p + "'\n"))
				if err != nil {
					t.Fatal(err)
				}
				if len(fs) != 1 {
					t.Fatalf("%s file %q must give one finding, got %+v", section, p, fs)
				}
				f := fs[0]
				if f.Rule != rule || f.Service != "" || f.Key != section+".k.file" || f.Value != p || f.Line != 3 || f.Why == "" {
					t.Fatalf("unexpected finding %+v", f)
				}
				if !compose.NeedsEnvAdmin(fs) {
					t.Fatal("a finding must require env Admin")
				}
			})
		}
		for _, p := range []string{"./secret.txt", "secret.txt", "secrets/db_password", "./a/../b.txt", "a..b"} {
			t.Run(section+" ok "+p, func(t *testing.T) {
				fs, err := compose.Check([]byte(section + ":\n  k:\n    file: " + p + "\n"))
				if err != nil || len(fs) != 0 {
					t.Fatalf("%q inside the project must not be flagged, got %+v err %v", p, fs, err)
				}
			})
		}
	}
}

func TestSecretAndConfigSourcesThatAreNotFilesAreNotFlagged(t *testing.T) {
	src := `secrets:
  a: {environment: HOME_DIR}
  b: {external: true}
  c: {content: "/etc/shadow"}
  d: {file: ~}
configs:
  e: {content: "/etc/shadow"}
  f: {name: x, external: true}
services:
  web:
    image: x
    secrets: [a, {source: b, target: /run/secrets/b}]
    configs: [e]
`
	fs, err := compose.Check([]byte(src))
	if err != nil || len(fs) != 0 {
		t.Fatalf("got %+v err %v", fs, err)
	}
}

func TestInterpolatedSecretAndConfigFileSourcesFailClosed(t *testing.T) {
	got := ruleValues(t, "secrets:\n  k: {file: \"${KEYFILE}\"}\nconfigs:\n  c: {file: $CFG}\n")
	if !slices.Equal(got, []string{"unresolvable:${KEYFILE}", "unresolvable:$CFG"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSecretFileSourcesAreResolvedThroughAnchorsAndMerges(t *testing.T) {
	src := `x-bad: &bad {file: /data/bayplan.key}
secrets:
  k:
    <<: *bad
`
	if got := ruleValues(t, src); !slices.Equal(got, []string{"secret_file:/data/bayplan.key"}) {
		t.Fatalf("got %v", got)
	}
}
