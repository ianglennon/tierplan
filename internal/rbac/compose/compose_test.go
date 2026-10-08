// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package compose_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/rbac/compose"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // a fixed testdata name
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPrivilegedServiceYieldsOneFindingWithItsLine(t *testing.T) {
	fs, err := compose.Check(fixture(t, "privileged.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(fs), fs)
	}
	f := fs[0]
	if f.Service != "web" || f.Key != "privileged" || f.Value != "true" || f.Line != 4 || f.Rule != "privileged" {
		t.Fatalf("unexpected finding %+v", f)
	}
	if f.Why == "" {
		t.Fatal("a finding must say why")
	}
	if !compose.NeedsEnvAdmin(fs) {
		t.Fatal("a finding must require env Admin")
	}
	if got, want := compose.Format(f), "services.web.privileged (line 4): true"; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

func TestCleanComposeFileHasNoFindings(t *testing.T) {
	fs, err := compose.Check(fixture(t, "clean.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("want no findings, got %+v", fs)
	}
	if compose.NeedsEnvAdmin(fs) {
		t.Fatal("no findings must not require env Admin")
	}
}

func TestEmptyInputHasNoFindingsAndNoError(t *testing.T) {
	for _, src := range []string{"", "   \n", "# only a comment\n", "services: {}", "services:\n", "name: x\n"} {
		fs, err := compose.Check([]byte(src))
		if err != nil || len(fs) != 0 {
			t.Fatalf("%q: findings %+v err %v", src, fs, err)
		}
	}
}

func TestTooLargeInputIsRefusedAndMalformedYAMLIsAnError(t *testing.T) {
	big := bytes.Repeat([]byte("#"), compose.MaxSize+1)
	if _, err := compose.Check(big); !errors.Is(err, compose.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	edge := bytes.Repeat([]byte("#"), compose.MaxSize)
	if _, err := compose.Check(edge); err != nil {
		t.Fatalf("exactly 1 MiB must be accepted, got %v", err)
	}
	if _, err := compose.Check([]byte("services: [")); err == nil {
		t.Fatal("malformed YAML must be an error")
	}
}

// ruleValues renders findings as "rule:value" in the order Check returned them.
func ruleValues(t *testing.T, src string) []string {
	t.Helper()
	fs, err := compose.Check([]byte(src))
	if err != nil {
		t.Fatalf("Check: %v\n%s", err, src)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule+":"+f.Value)
	}
	return out
}

// service wraps a service body so a case only has to state the attributes.
func service(body string) string {
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: example/web:1\n")
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		b.WriteString("    " + l + "\n")
	}
	return b.String()
}

func sameStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return slices.Equal(a, b)
}

func TestHostEscalationKeysAreFlaggedAndOrdinaryValuesAreNot(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"cap_add", "cap_add: [NET_ADMIN]", []string{"cap_add:NET_ADMIN"}},
		{"cap_add every entry", "cap_add:\n  - NET_ADMIN\n  - SYS_PTRACE", []string{"cap_add:NET_ADMIN", "cap_add:SYS_PTRACE"}},
		{"cap_add empty list", "cap_add: []", nil},
		{"cap_drop is fine", "cap_drop: [ALL]", nil},
		{"devices", `devices: ["/dev/fuse"]`, []string{"devices:/dev/fuse"}},
		{"devices long syntax", "devices:\n  - source: /dev/fuse\n    target: /dev/fuse", []string{"devices:/dev/fuse"}},
		{"device_cgroup_rules", `device_cgroup_rules: ["c 1:3 mr"]`, []string{"device_cgroup_rules:c 1:3 mr"}},
		{"pid host", "pid: host", []string{"pid:host"}},
		{"pid container", `pid: "container:abc"`, []string{"pid:container:abc"}},
		{"pid service is fine", `pid: "service:db"`, nil},
		{"ipc host", "ipc: host", []string{"ipc:host"}},
		{"ipc container", `ipc: "container:x"`, []string{"ipc:container:x"}},
		{"ipc shareable is fine", "ipc: shareable", nil},
		{"ipc service is fine", `ipc: "service:db"`, nil},
		{"uts host", "uts: host", []string{"uts:host"}},
		{"userns_mode host", "userns_mode: host", []string{"userns_mode:host"}},
		{"cgroup host", "cgroup: host", []string{"cgroup:host"}},
		{"cgroup private is fine", "cgroup: private", nil},
		{"cgroup_parent", "cgroup_parent: /system.slice", []string{"cgroup_parent:/system.slice"}},
		{"network_mode host", "network_mode: host", []string{"network_mode:host"}},
		{"network_mode container", `network_mode: "container:x"`, []string{"network_mode:container:x"}},
		{"network_mode none is fine", "network_mode: none", nil},
		{"network_mode bridge is fine", "network_mode: bridge", nil},
		{"network_mode service is fine", `network_mode: "service:db"`, nil},
		{"deploy devices reservation is not the devices key", "deploy:\n  resources:\n    reservations:\n      devices:\n        - capabilities: [gpu]", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ruleValues(t, service(tc.body)); !sameStrings(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSecurityOptFlagsEveryConfinementDisablingOption(t *testing.T) {
	flagged := []string{
		"seccomp=unconfined", "seccomp:unconfined", "apparmor=unconfined", "apparmor:unconfined",
		"label=disable", "label:disable", "label=type:spc_t", "systempaths=unconfined",
		"no-new-privileges=false", "no-new-privileges:false", "seccomp=/opt/p.json",
	}
	for _, opt := range flagged {
		t.Run(opt, func(t *testing.T) {
			want := []string{"security_opt:" + opt}
			if got := ruleValues(t, service("security_opt:\n  - "+opt)); !sameStrings(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
	for _, opt := range []string{"no-new-privileges=true", "no-new-privileges:true", "no-new-privileges", "apparmor=docker-default", "label=level:s0:c100,c200"} {
		t.Run("ok "+opt, func(t *testing.T) {
			if got := ruleValues(t, service("security_opt:\n  - "+opt)); len(got) != 0 {
				t.Fatalf("%s must not be flagged, got %v", opt, got)
			}
		})
	}
}

func TestBindsOfHostPathsAreFlaggedAndDataBindsAreNot(t *testing.T) {
	fs, err := compose.Check(fixture(t, "binds.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		if f.Service != "app" || f.Key != "volumes" || f.Rule != "volumes" {
			t.Fatalf("unexpected finding %+v", f)
		}
		got = append(got, f.Value)
	}
	want := []string{
		"/var/run/docker.sock", "/run/docker.sock", "//./pipe/docker_engine", "/", "/var", "/var/run", "/run",
		"/etc/ssl", "/proc", "/sys/fs", "/dev", "/boot", "/root/.ssh", "/var/lib/docker/volumes",
		"/etc/../etc/passwd", "/var/run/", "/home/me/docker.sock",
		"/var/run/docker.sock", "/etc",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("flagged binds\n got %q\nwant %q", got, want)
	}
}

func TestDriverOptsThatBindAHostPathAreFlagged(t *testing.T) {
	src := `volumes:
  a: {driver_opts: {type: none, o: bind, device: /etc}}
  b: {driver_opts: {type: none, o: bind, device: /srv/data}}
  c: {driver_opts: {device: /etc}}
  d: {driver: local, driver_opts: {type: none, o: "bind,ro", device: /var/run/docker.sock}}
  e: {driver: nfs, driver_opts: {device: ":/export"}}
  f: {driver: other, driver_opts: {type: none, o: bind, device: /etc}}
  g: {driver_opts: {type: nfs, o: "addr=10.0.0.1", device: ":/export"}}
`
	fs, err := compose.Check([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 {
		t.Fatalf("want 2 findings, got %+v", fs)
	}
	for i, want := range []struct{ key, value string }{
		{"volumes.a.driver_opts.device", "/etc"},
		{"volumes.d.driver_opts.device", "/var/run/docker.sock"},
	} {
		f := fs[i]
		if f.Service != "" || f.Key != want.key || f.Value != want.value || f.Rule != "volume_driver_opts" {
			t.Fatalf("finding %d = %+v, want %+v", i, f, want)
		}
	}
	if got, want := compose.Format(fs[0]), "volumes.a.driver_opts.device (line 2): /etc"; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
	if got := ruleValues(t, "volumes:\n  v: {driver_opts: {type: none, o: \"${OPTS}\", device: /etc}}\n"); !slices.Equal(got, []string{"unresolvable:${OPTS}", "volume_driver_opts:/etc"}) {
		t.Fatalf("an unresolvable o must still check the device, got %v", got)
	}
}

func TestAnchorsAliasesAndMergeKeysAreResolvedAndReportedAtTheAlias(t *testing.T) {
	fs, err := compose.Check(fixture(t, "anchors.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		service, rule, value string
		line                 int
	}
	var got []row
	for _, f := range fs {
		got = append(got, row{f.Service, f.Rule, f.Value, f.Line})
	}
	want := []row{
		{"merged", "cap_add", "ALL", 9},
		{"merged", "privileged", "true", 9},
		{"listed", "cap_add", "SYS_ADMIN", 12},
		{"inline", "network_mode", "host", 20},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if fs[0].Column != fs[1].Column {
		t.Fatalf("both merged values sit at the alias, got columns %d and %d", fs[0].Column, fs[1].Column)
	}
}

func TestExplicitKeysOverrideMergedOnesAndEarlierMergesWin(t *testing.T) {
	src := `x-a: &a {privileged: false}
x-b: &b {privileged: true}
services:
  first:
    <<: [*a, *b]
  second:
    <<: [*b, *a]
  third:
    <<: *b
    privileged: "no"
`
	got := ruleValues(t, src)
	if !slices.Equal(got, []string{"privileged:true"}) {
		t.Fatalf("only the service whose first merge source is privileged is flagged, got %v", got)
	}
}

func TestEveryDocumentOfAMultiDocumentFileIsChecked(t *testing.T) {
	got := ruleValues(t, "services:\n  a:\n    image: x\n---\nservices:\n  b:\n    privileged: true\n")
	if !slices.Equal(got, []string{"privileged:true"}) {
		t.Fatalf("got %v", got)
	}
}

func TestARepeatedKeyIsStillSeen(t *testing.T) {
	got := ruleValues(t, service("privileged: false\nprivileged: true"))
	if !slices.Equal(got, []string{"privileged:true"}) {
		t.Fatalf("got %v", got)
	}
}

func TestBooleanFieldsFailClosed(t *testing.T) {
	for _, v := range []string{"true", "True", "yes", "Yes", "on", "ON", `"true"`, "1", `"anything"`, `""`, "[]", "{}"} {
		t.Run("set "+v, func(t *testing.T) {
			if got := ruleValues(t, service("privileged: "+v)); len(got) != 1 || !strings.HasPrefix(got[0], "privileged:") {
				t.Fatalf("privileged: %s must be flagged, got %v", v, got)
			}
		})
	}
	for _, v := range []string{"false", "False", "no", "No", "off", "OFF", `"false"`, "0", `"0"`, "null", "~", ""} {
		t.Run("clear "+v, func(t *testing.T) {
			if got := ruleValues(t, service("privileged: "+v)); len(got) != 0 {
				t.Fatalf("privileged: %s must not be flagged, got %v", v, got)
			}
		})
	}
	if got := ruleValues(t, service("image: x")); len(got) != 0 {
		t.Fatalf("an absent privileged must not be flagged, got %v", got)
	}
}

func TestInterpolationInACheckedFieldFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"privileged", `privileged: "${PRIV}"`, []string{"unresolvable:${PRIV}"}},
		{"cap_add", `cap_add: ["${CAP}"]`, []string{"unresolvable:${CAP}"}},
		{"bare variable", "network_mode: $MODE", []string{"unresolvable:$MODE"}},
		{"default syntax", `pid: "${PID_MODE:-private}"`, []string{"unresolvable:${PID_MODE:-private}"}},
		{"security_opt", `security_opt: ["seccomp=${PROFILE}"]`, []string{"unresolvable:seccomp=${PROFILE}"}},
		{"bind source", `volumes: ["${DATA_DIR}:/data"]`, []string{"unresolvable:${DATA_DIR}:/data"}},
		{"bind source with a default", `volumes: ["${DATA_DIR:-/srv}:/data"]`, []string{"unresolvable:${DATA_DIR:-/srv}:/data"}},
		{"bind target only is harmless", `volumes: ["./data:${DEST}"]`, nil},
		{"long syntax source", "volumes:\n  - type: bind\n    source: ${SRC}\n    target: /x", []string{"unresolvable:${SRC}"}},
		{"an unchecked field is not examined", `image: "${IMAGE}"`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ruleValues(t, service(tc.body)); !sameStrings(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOrderingByLineColumnThenRuleIsStable(t *testing.T) {
	src := `services:
  web: {privileged: true, cap_add: [ALL], network_mode: host}
  api:
    privileged: true
    cap_add: [NET_ADMIN]
`
	var first []compose.Finding
	for i := range 100 {
		fs, err := compose.Check([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = fs
			continue
		}
		if !slices.Equal(fs, first) {
			t.Fatalf("run %d differs:\n%+v\n%+v", i, fs, first)
		}
	}
	var got []string
	for _, f := range first {
		got = append(got, fmt.Sprintf("%d:%s", f.Line, f.Rule))
	}
	want := []string{"2:privileged", "2:cap_add", "2:network_mode", "4:privileged", "5:cap_add"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := 1; i < len(first); i++ {
		a, b := first[i-1], first[i]
		if a.Line == b.Line && a.Column > b.Column {
			t.Fatalf("columns out of order: %+v then %+v", a, b)
		}
	}
	// Two findings at the same position (a merge) fall back to the rule name.
	fs, err := compose.Check(fixture(t, "anchors.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fs[0].Rule != "cap_add" || fs[1].Rule != "privileged" {
		t.Fatalf("equal positions must order by rule, got %s then %s", fs[0].Rule, fs[1].Rule)
	}
}

func TestHostileAliasesAreRefusedNotExpanded(t *testing.T) {
	var b strings.Builder
	b.WriteString("x0: &x0 {privileged: true}\n")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "x%d: &x%d {<<: [*x%d, *x%d]}\n", i, i, i-1, i-1)
	}
	b.WriteString("services:\n  s:\n    <<: *x60\n")
	done := make(chan error, 1)
	go func() {
		_, err := compose.Check([]byte(b.String()))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, compose.ErrInvalid) {
			t.Fatalf("want ErrInvalid, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an exponential alias file must be refused promptly")
	}
	for _, cyc := range []string{"services:\n  a: &a\n    <<: *a\n", "services:\n  a:\n    cap_add: &c [*c]\n"} {
		fs, err := compose.Check([]byte(cyc))
		if err != nil && !errors.Is(err, compose.ErrInvalid) {
			t.Fatalf("%q: %v", cyc, err)
		}
		_ = fs
	}
}
