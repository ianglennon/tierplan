// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
)

// sdkContainerCreate is the container create the Compose SDK builds in getCreateConfigs for a
// service with a published port, an environment, a named volume, a healthcheck and two
// networks, with the zero values the moby client encodes beside them.
func sdkContainerCreate() container.CreateRequest {
	retries := 3
	stop := 10
	return container.CreateRequest{
		Config: &container.Config{
			Hostname:     "web",
			AttachStderr: true,
			AttachStdout: true,
			ExposedPorts: network.PortSet{network.MustParsePort("80/tcp"): {}},
			Env:          []string{"MODE=prod", "SECRET_REF=x"},
			Cmd:          []string{"nginx", "-g", "daemon off;"},
			Healthcheck: &container.HealthConfig{
				Test: []string{"CMD", "curl", "-f", "http://localhost/"}, Interval: 30 * time.Second,
				Timeout: 30 * time.Second, Retries: retries,
			},
			Image:       "nginx:1.27",
			WorkingDir:  "/app",
			StopTimeout: &stop,
			StopSignal:  "SIGQUIT",
			Labels: map[string]string{
				"com.docker.compose.project": "shop", "com.docker.compose.service": "web",
				"com.docker.compose.config-hash": "abc123", "com.docker.compose.container-number": "1",
				"com.docker.compose.depends_on": "",
			},
		},
		HostConfig: &container.HostConfig{
			Binds:         []string{"shop_data:/data:rw"},
			NetworkMode:   "shop_default",
			PortBindings:  network.PortMap{network.MustParsePort("80/tcp"): {{HostPort: "8080"}}},
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			CapDrop:       []string{"CAP_NET_RAW"},
			Sysctls:       map[string]string{"net.core.somaxconn": "1024"},
			Resources: container.Resources{
				Memory: 256 << 20, NanoCPUs: 500_000_000,
				Ulimits: []*container.Ulimit{{Name: "nofile", Soft: 1024, Hard: 2048}},
			},
			ExtraHosts: []string{"db:10.0.0.9"},
			DNS:        []netip.Addr{netip.MustParseAddr("1.1.1.1")},
			Tmpfs:      map[string]string{"/run": "size=16m"},
			Init:       new(true),
		},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			"shop_default": {Aliases: []string{"web"}, IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.5.0.4")}, IPAddress: netip.MustParseAddr("10.5.0.4")},
			"shop_backend": {Aliases: []string{"web", "api"}, GwPriority: 5, DriverOpts: map[string]string{"k": "v"}},
		}},
	}
}

// enc encodes a value the way the server's Docker client does, every field
// present and zero values included.
func enc(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// postJSON is a request with a JSON body of the right length.
func postJSON(method, target, body string, headers ...string) string {
	headers = append([]string{"Content-Type: application/json\r\n", "Content-Length: " + strconv.Itoa(len(body)) + "\r\n"}, headers...)
	return request(method, target, headers...) + body
}

// roundTrip validates a body and decodes what would be forwarded into out.
func roundTrip(t *testing.T, kind BodyKind, in string, out any) error {
	t.Helper()
	got, err := validateBody(kind, strings.NewReader(in), bodyEnv{})
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(got)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("the rebuilt body does not decode: %v: %s", err, got)
	}
	return nil
}

func TestBodyExecCreateIsRebuiltAndDecodesToTheSameRequest(t *testing.T) {
	want := container.ExecCreateRequest{
		AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: true,
		Cmd: []string{"/bin/sh"}, User: "root", WorkingDir: "/app", ConsoleSize: &[2]uint{24, 80},
	}
	var got container.ExecCreateRequest
	if err := roundTrip(t, BodyExecCreate, enc(t, want), &got); err != nil {
		t.Fatalf("a shell request was refused: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the forwarded request is %+v, want %+v", got, want)
	}
}

func TestBodyExecCreateRefusals(t *testing.T) {
	ok := container.ExecCreateRequest{Tty: true, AttachStdout: true, Cmd: []string{"/bin/sh"}}
	with := func(f func(*container.ExecCreateRequest)) string {
		r := ok
		f(&r)
		return enc(t, r)
	}
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	for _, tc := range []struct{ name, body string }{
		{"privileged", with(func(r *container.ExecCreateRequest) { r.Privileged = true })},
		{"detach keys", with(func(r *container.ExecCreateRequest) { r.DetachKeys = "ctrl-p" })},
		{"an environment entry", with(func(r *container.ExecCreateRequest) { r.Env = []string{"A=1"} })},
		{"no terminal", with(func(r *container.ExecCreateRequest) { r.Tty = false })},
		{"an empty command", with(func(r *container.ExecCreateRequest) { r.Cmd = nil })},
		{"sixty-five command entries", with(func(r *container.ExecCreateRequest) { r.Cmd = tooMany })},
		{"a command entry over 4096 bytes", with(func(r *container.ExecCreateRequest) { r.Cmd = []string{strings.Repeat("a", 4097)} })},
		{"a console over 1000", with(func(r *container.ExecCreateRequest) { r.ConsoleSize = &[2]uint{1001, 80} })},
		{"a user over 64 characters", with(func(r *container.ExecCreateRequest) { r.User = strings.Repeat("u", 65) })},
		{"an unknown key", `{"Tty":true,"Cmd":["sh"],"Foo":1}`},
		{"privileged under another case", `{"Tty":true,"Cmd":["sh"],"privileged":true}`},
		{"data after the object", `{"Tty":true,"Cmd":["sh"]}{"Privileged":true}`},
		{"not an object", `["sh"]`},
		{"over 64 KiB", `{"Tty":true,"Cmd":["` + strings.Repeat("a", 70<<10) + `"]}`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateBody(BodyExecCreate, strings.NewReader(tc.body), bodyEnv{}); err == nil {
				t.Fatal("the body was accepted")
			}
		})
	}
}

func TestBodyDuplicateKeysCannotSmuggleAField(t *testing.T) {
	// Go keeps the last of two equal keys. Whatever it keeps, the body that is
	// forwarded is the rebuilt one, with Privileged absent.
	got, err := validateBody(BodyExecCreate, strings.NewReader(`{"Tty":true,"Cmd":["sh"],"Privileged":true,"Privileged":false}`), bodyEnv{})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if strings.Contains(string(got), `"Privileged":true`) {
		t.Fatalf("the forwarded body carries Privileged true: %s", got)
	}
}

func TestBodyExecStart(t *testing.T) {
	var got container.ExecStartRequest
	want := container.ExecStartRequest{Tty: true, ConsoleSize: &[2]uint{24, 80}}
	if err := roundTrip(t, BodyExecStart, enc(t, want), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted = %+v, %v", got, err)
	}
	for name, body := range map[string]string{
		"detached":        enc(t, container.ExecStartRequest{Detach: true, Tty: true}),
		"no terminal":     enc(t, container.ExecStartRequest{}),
		"a console over":  enc(t, container.ExecStartRequest{Tty: true, ConsoleSize: &[2]uint{24, 5000}}),
		"an unknown key":  `{"Tty":true,"Foo":1}`,
		"data after json": `{"Tty":true} x`,
	} {
		if _, err := validateBody(BodyExecStart, strings.NewReader(body), bodyEnv{}); err == nil {
			t.Errorf("%s: the body was accepted", name)
		}
	}
}

func TestBodyNetworkCreate(t *testing.T) {
	want := network.CreateRequest{
		Name: "app-net", Driver: "overlay", Attachable: true, Internal: true,
		Labels:  map[string]string{"team": "a"},
		Options: map[string]string{"com.docker.network.driver.mtu": "1400"},
		IPAM: &network.IPAM{Driver: "default", Config: []network.IPAMConfig{{
			Subnet: netip.MustParsePrefix("10.20.0.0/24"), IPRange: netip.MustParsePrefix("10.20.0.0/25"), Gateway: netip.MustParseAddr("10.20.0.1"),
		}}},
	}
	var got network.CreateRequest
	if err := roundTrip(t, BodyNetworkCreate, enc(t, want), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted = %+v, %v", got, err)
	}
	ok := network.CreateRequest{Name: "n1", Driver: "bridge"}
	if err := roundTrip(t, BodyNetworkCreate, enc(t, ok), &got); err != nil {
		t.Fatalf("a plain bridge network was refused: %v", err)
	}
	with := func(f func(*network.CreateRequest)) string {
		r := ok
		f(&r)
		return enc(t, r)
	}
	for _, tc := range []struct{ name, body string }{
		{"ingress", with(func(r *network.CreateRequest) { r.Ingress = true })},
		{"config only", with(func(r *network.CreateRequest) { r.ConfigOnly = true })},
		{"config from", with(func(r *network.CreateRequest) { r.ConfigFrom = &network.ConfigReference{Network: "x"} })},
		{"the host driver", with(func(r *network.CreateRequest) { r.Driver = "host" })},
		{"a plugin driver", with(func(r *network.CreateRequest) { r.Driver = "evil/driver" })},
		{"a null scope driver", with(func(r *network.CreateRequest) { r.Scope = "global" })},
		{"IPAM options", with(func(r *network.CreateRequest) { r.IPAM = &network.IPAM{Options: map[string]string{"a": "b"}} })},
		{"an IPAM driver", with(func(r *network.CreateRequest) { r.IPAM = &network.IPAM{Driver: "other"} })},
		{"auxiliary addresses", with(func(r *network.CreateRequest) {
			r.IPAM = &network.IPAM{Config: []network.IPAMConfig{{AuxAddress: map[string]netip.Addr{"h": netip.MustParseAddr("10.0.0.9")}}}}
		})},
		{"no name", with(func(r *network.CreateRequest) { r.Name = "" })},
		{"a name with a slash", with(func(r *network.CreateRequest) { r.Name = "a/b" })},
		{"too many labels", with(func(r *network.CreateRequest) {
			r.Labels = map[string]string{}
			for i := range 65 {
				r.Labels["k"+strconv.Itoa(i)] = "v"
			}
		})},
		{"a label value over 4096", with(func(r *network.CreateRequest) { r.Labels = map[string]string{"k": strings.Repeat("v", 4097)} })},
		{"an unknown key", `{"Name":"n","Driver":"bridge","Foo":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateBody(BodyNetworkCreate, strings.NewReader(tc.body), bodyEnv{}); err == nil {
				t.Fatal("the body was accepted")
			}
		})
	}
}

func TestBodyNetworkConnectAndDisconnect(t *testing.T) {
	want := network.ConnectRequest{
		Container: "web-1",
		EndpointConfig: &network.EndpointSettings{
			IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.0.0.5")},
			Aliases:    []string{"web"},
		},
	}
	var got network.ConnectRequest
	if err := roundTrip(t, BodyNetworkConnect, enc(t, want), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted = %+v, %v", got, err)
	}
	if err := roundTrip(t, BodyNetworkConnect, enc(t, network.ConnectRequest{Container: "web-1"}), &got); err != nil {
		t.Fatalf("a connect with no endpoint settings was refused: %v", err)
	}
	with := func(f func(*network.EndpointSettings)) string {
		ep := &network.EndpointSettings{Aliases: []string{"web"}}
		f(ep)
		return enc(t, network.ConnectRequest{Container: "web-1", EndpointConfig: ep})
	}
	for _, tc := range []struct{ name, body string }{
		{"links", with(func(e *network.EndpointSettings) { e.Links = []string{"db:db"} })},
		{"driver options", with(func(e *network.EndpointSettings) { e.DriverOpts = map[string]string{"a": "b"} })},
		{"a MAC address", with(func(e *network.EndpointSettings) { e.MacAddress = network.HardwareAddr{2, 0, 0, 0, 0, 1} })},
		{"a gateway priority", with(func(e *network.EndpointSettings) { e.GwPriority = 5 })},
		{"a network ID", with(func(e *network.EndpointSettings) { e.NetworkID = "x" })},
		{"link-local IPs", with(func(e *network.EndpointSettings) {
			e.IPAMConfig = &network.EndpointIPAMConfig{LinkLocalIPs: []netip.Addr{netip.MustParseAddr("169.254.1.1")}}
		})},
		{"seventeen aliases", with(func(e *network.EndpointSettings) { e.Aliases = make([]string, 17) })},
		{"an alias over 253 characters", with(func(e *network.EndpointSettings) { e.Aliases = []string{strings.Repeat("a", 254)} })},
		{"a container that is not an ID", enc(t, network.ConnectRequest{Container: "../x"})},
		{"no container", enc(t, network.ConnectRequest{})},
		{"an unknown key", `{"Container":"a","Foo":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateBody(BodyNetworkConnect, strings.NewReader(tc.body), bodyEnv{}); err == nil {
				t.Fatal("the body was accepted")
			}
		})
	}
	var dis network.DisconnectRequest
	if err := roundTrip(t, BodyNetworkDisconnect, enc(t, network.DisconnectRequest{Container: "web-1"}), &dis); err != nil || dis.Container != "web-1" || dis.Force {
		t.Fatalf("disconnect accepted = %+v, %v", dis, err)
	}
	for name, body := range map[string]string{
		"a forced disconnect": enc(t, network.DisconnectRequest{Container: "web-1", Force: true}),
		"no container":        enc(t, network.DisconnectRequest{}),
		"an unknown key":      `{"Container":"a","Foo":1}`,
	} {
		if _, err := validateBody(BodyNetworkDisconnect, strings.NewReader(body), bodyEnv{}); err == nil {
			t.Errorf("%s: the body was accepted", name)
		}
	}
}

func TestBodyVolumeCreate(t *testing.T) {
	want := volume.CreateRequest{
		Name: "data", Driver: "local",
		DriverOpts: map[string]string{"type": "nfs", "o": "addr=10.0.0.2,rw", "device": ":/export"},
		Labels:     map[string]string{"app": "db"},
	}
	var got volume.CreateRequest
	if err := roundTrip(t, BodyVolumeCreate, enc(t, want), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted = %+v, %v", got, err)
	}
	if err := roundTrip(t, BodyVolumeCreate, enc(t, volume.CreateRequest{}), &got); err != nil {
		t.Fatalf("an anonymous volume was refused: %v", err)
	}
	for name, body := range map[string]string{
		"a cluster volume":    enc(t, volume.CreateRequest{Name: "v", ClusterVolumeSpec: &volume.ClusterVolumeSpec{Group: "g"}}),
		"a name with a slash": enc(t, volume.CreateRequest{Name: "a/b"}),
		"a leading dash":      enc(t, volume.CreateRequest{Name: "-a"}),
		"a bad driver":        enc(t, volume.CreateRequest{Name: "v", Driver: "../x"}),
		"too many options": enc(t, volume.CreateRequest{Name: "v", DriverOpts: func() map[string]string {
			m := map[string]string{}
			for i := range 65 {
				m[strconv.Itoa(i)] = "v"
			}
			return m
		}()}),
		"an unknown key": `{"Name":"v","Foo":1}`,
	} {
		if _, err := validateBody(BodyVolumeCreate, strings.NewReader(body), bodyEnv{}); err == nil {
			t.Errorf("%s: the body was accepted", name)
		}
	}
}

func TestBodyVolumeCreateHostPolicy(t *testing.T) {
	local := func(driver string, opts map[string]string) volume.CreateRequest {
		return volume.CreateRequest{Name: "v", Driver: driver, DriverOpts: opts}
	}
	bind := func(device string) map[string]string {
		return map[string]string{"type": "none", "o": "bind", "device": device}
	}
	optIn := bodyEnv{policy: Policy{AllowPrivilegedServices: true}}

	refused := map[string]volume.CreateRequest{
		"a bind of /etc":                       local("local", bind("/etc")),
		"a bind of / with the driver left out": local("", bind("/")),
		"a block device":                       local("local", map[string]string{"type": "ext4", "device": "/dev/sda1"}),
		"a bind among other flags":             local("local", map[string]string{"o": "bind,ro", "device": "/var/lib/docker"}),
		"a climbing relative device":           local("local", map[string]string{"type": "btrfs", "device": "../../etc"}),
		"the docker socket":                    local("local", bind("/var/run/docker.sock")),
		"the driver in upper case":             local("LOCAL", bind("/")),
		"a volume plugin":                      local("rexray/ebs", map[string]string{"size": "10"}),

		"a relative bind of etc":                       local("local", bind("etc")),
		"a relative bind of var/lib/docker":            local("local", bind("var/lib/docker")),
		"a relative bind of var/run":                   local("local", bind("var/run")),
		"a relative bind of var/run/docker.sock":       local("local", bind("var/run/docker.sock")),
		"a device with a leading space":                local("local", bind(" /srv/data")),
		"a device with a trailing space":               local("local", bind("/srv ")),
		"an overlay with the host root as a lower dir": local("local", map[string]string{"type": "overlay", "device": "overlay", "o": "lowerdir=/:/srv/empty"}),
		"a proc filesystem":                            local("local", map[string]string{"type": "proc", "device": "proc"}),
		"a sysfs filesystem":                           local("local", map[string]string{"type": "sysfs", "device": "sysfs"}),
		"devtmpfs":                                     local("local", map[string]string{"type": "devtmpfs", "device": "devtmpfs"}),
		"a bind with a network type":                   local("local", map[string]string{"type": "nfs", "o": "bind", "device": "etc"}),
	}
	for name, req := range refused {
		t.Run("refused: "+name, func(t *testing.T) {
			if _, err := validateBody(BodyVolumeCreate, strings.NewReader(enc(t, req)), bodyEnv{}); !errors.Is(err, errHostPolicy) {
				t.Fatalf("err = %v, want errHostPolicy", err)
			}
			var got volume.CreateRequest
			clean, err := validateBody(BodyVolumeCreate, strings.NewReader(enc(t, req)), optIn)
			if err != nil {
				t.Fatalf("the opt-in refused it: %v", err)
			}
			if err := json.Unmarshal(clean, &got); err != nil || !reflect.DeepEqual(got, req) {
				t.Fatalf("the opt-in forwarded %+v (%v), want %+v", got, err, req)
			}
		})
	}

	forwarded := map[string]volume.CreateRequest{
		"an nfs share":                   local("local", map[string]string{"type": "nfs", "o": "addr=10.0.0.2,rw", "device": ":/export"}),
		"a cifs share":                   local("local", map[string]string{"type": "cifs", "device": "//server/share"}),
		"a bind under /srv":              local("local", bind("/srv/data")),
		"a bind under /mnt":              local("local", bind("/mnt/backups")),
		"tmpfs":                          local("local", map[string]string{"type": "tmpfs", "device": "tmpfs"}),
		"tmpfs with a size":              local("local", map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "size=64m,uid=1000"}),
		"an nfs4 share":                  local("local", map[string]string{"type": "nfs4", "o": "addr=10.0.0.2", "device": ":/export"}),
		"a bind under /media":            local("local", bind("/media/disk")),
		"a bind strictly below /var/log": local("local", bind("/var/log/app")),
		"an anonymous volume":            {},
		"a local volume, no opts":        local("local", nil),
	}
	for name, req := range forwarded {
		t.Run("forwarded: "+name, func(t *testing.T) {
			var got volume.CreateRequest
			if err := roundTrip(t, BodyVolumeCreate, enc(t, req), &got); err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !reflect.DeepEqual(got, req) {
				t.Fatalf("rebuilt %+v, want %+v", got, req)
			}
		})
	}

	t.Run("a driver with spaces is refused by the name rule under either env", func(t *testing.T) {
		body := enc(t, local(" local ", bind("/")))
		for name, env := range map[string]bodyEnv{"without the opt-in": {}, "with the opt-in": optIn} {
			_, err := validateBody(BodyVolumeCreate, strings.NewReader(body), env)
			if !errors.Is(err, errBodyRefused) || errors.Is(err, errHostPolicy) {
				t.Errorf("%s: err = %v, want the driver-name refusal", name, err)
			}
		}
	})
}

func TestBodyNodeSpec(t *testing.T) {
	want := swarm.NodeSpec{
		Annotations:  swarm.Annotations{Name: "alpha", Labels: map[string]string{"zone": "a", "disk": "ssd"}},
		Role:         swarm.NodeRoleManager,
		Availability: swarm.NodeAvailabilityDrain,
	}
	var got swarm.NodeSpec
	if err := roundTrip(t, BodyNodeSpec, enc(t, want), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted = %+v, %v; want %+v", got, err, want)
	}
	for _, role := range []swarm.NodeRole{swarm.NodeRoleManager, swarm.NodeRoleWorker} {
		for _, avail := range []swarm.NodeAvailability{swarm.NodeAvailabilityActive, swarm.NodeAvailabilityPause, swarm.NodeAvailabilityDrain} {
			if err := roundTrip(t, BodyNodeSpec, enc(t, swarm.NodeSpec{Role: role, Availability: avail}), &got); err != nil {
				t.Errorf("role %s availability %s was refused: %v", role, avail, err)
			}
		}
	}
	// The client encodes unset labels as null and no name as nothing; both are the spec as read.
	if err := roundTrip(t, BodyNodeSpec, `{"Labels":null,"Role":"worker","Availability":"active"}`, &got); err != nil {
		t.Errorf("a spec with null labels was refused: %v", err)
	}
	if err := roundTrip(t, BodyNodeSpec, `{"Labels":{},"Role":"worker","Availability":"pause"}`, &got); err != nil {
		t.Errorf("a spec with empty labels was refused: %v", err)
	}
}

func TestBodyNodeSpecRefusals(t *testing.T) {
	manyLabels := map[string]string{}
	for i := range 65 {
		manyLabels[strconv.Itoa(i)] = "v"
	}
	for name, body := range map[string]string{
		"no role":                 `{"Availability":"active"}`,
		"no availability":         `{"Role":"worker"}`,
		"an unknown role":         `{"Role":"leader","Availability":"active"}`,
		"an unknown availability": `{"Role":"worker","Availability":"down"}`,
		"a role in other case":    `{"Role":"Manager","Availability":"active"}`,
		"an unknown key":          `{"Role":"worker","Availability":"active","Foo":1}`,
		"an ID":                   `{"ID":"x","Role":"worker","Availability":"active"}`,
		"a version":               `{"Version":{"Index":3},"Role":"worker","Availability":"active"}`,
		"a description":           `{"Description":{"Hostname":"x"},"Role":"worker","Availability":"active"}`,
		"a status":                `{"Status":{"State":"ready"},"Role":"worker","Availability":"active"}`,
		"data after the value":    `{"Role":"worker","Availability":"active"}{}`,
		"not an object":           `["worker"]`,
		"an empty body":           ``,
		"too many labels":         enc(t, swarm.NodeSpec{Annotations: swarm.Annotations{Labels: manyLabels}, Role: "worker", Availability: "active"}),
		"an over-long label key":  enc(t, swarm.NodeSpec{Annotations: swarm.Annotations{Labels: map[string]string{strings.Repeat("k", 257): "v"}}, Role: "worker", Availability: "active"}),
		"an over-long label":      enc(t, swarm.NodeSpec{Annotations: swarm.Annotations{Labels: map[string]string{"k": strings.Repeat("v", 4097)}}, Role: "worker", Availability: "active"}),
		"an over-long name":       enc(t, swarm.NodeSpec{Annotations: swarm.Annotations{Name: strings.Repeat("n", 254)}, Role: "worker", Availability: "active"}),
	} {
		if _, err := validateBody(BodyNodeSpec, strings.NewReader(body), bodyEnv{}); err == nil {
			t.Errorf("%s: the body was accepted", name)
		}
	}
	if bodyCap(BodyNodeSpec) != 64<<10 {
		t.Errorf("the node spec cap = %d, want 64 KiB", bodyCap(BodyNodeSpec))
	}
}

func TestBodySecretCreate(t *testing.T) {
	want := swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: "db_password.v2", Labels: map[string]string{"app": "db"}},
		Data:        []byte("hunter2\n"),
	}
	var got swarm.SecretSpec
	if err := roundTrip(t, BodySecretCreate, enc(t, want), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted = %+v, %v; want %+v", got, err, want)
	}
	// The client encodes unset labels as null.
	if err := roundTrip(t, BodySecretCreate, `{"Name":"a","Labels":null,"Data":"eA=="}`, &got); err != nil {
		t.Errorf("a secret with null labels was refused: %v", err)
	}
	// 511,999 bytes is the largest value the swarm takes, and its base64 is well inside the cap.
	big := swarm.SecretSpec{Annotations: swarm.Annotations{Name: "big"}, Data: bytes.Repeat([]byte("x"), 511999)}
	if err := roundTrip(t, BodySecretCreate, enc(t, big), &got); err != nil || len(got.Data) != 511999 {
		t.Errorf("a 511,999 byte secret = %d bytes, %v; want accepted", len(got.Data), err)
	}
}

func TestBodySecretCreateRefusals(t *testing.T) {
	many := map[string]string{}
	for i := range 65 {
		many[strconv.Itoa(i)] = "v"
	}
	for name, body := range map[string]string{
		"a driver":               enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a"}, Data: []byte("x"), Driver: &swarm.Driver{Name: "vault"}}),
		"templating":             enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a"}, Data: []byte("x"), Templating: &swarm.Driver{Name: "golang"}}),
		"an unknown key":         `{"Name":"a","Data":"eA==","Foo":1}`,
		"an ID":                  `{"ID":"x","Name":"a","Data":"eA=="}`,
		"a version":              `{"Version":{"Index":3},"Name":"a","Data":"eA=="}`,
		"no name":                enc(t, swarm.SecretSpec{Data: []byte("x")}),
		"a name with a slash":    enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a/b"}, Data: []byte("x")}),
		"a leading dash":         enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "-a"}, Data: []byte("x")}),
		"a trailing dot":         enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a."}, Data: []byte("x")}),
		"a 65 character name":    enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: strings.Repeat("a", 65)}, Data: []byte("x")}),
		"no value":               enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a"}}),
		"a 512,000 byte value":   enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a"}, Data: bytes.Repeat([]byte("x"), 512000)}),
		"too many labels":        enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a", Labels: many}, Data: []byte("x")}),
		"an over-long label":     enc(t, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a", Labels: map[string]string{"k": strings.Repeat("v", 4097)}}, Data: []byte("x")}),
		"data after the value":   `{"Name":"a","Data":"eA=="}{}`,
		"not an object":          `["a"]`,
		"an empty body":          ``,
		"a value that is no b64": `{"Name":"a","Data":"!!!"}`,
	} {
		if _, err := validateBody(BodySecretCreate, strings.NewReader(body), bodyEnv{}); err == nil {
			t.Errorf("%s: the body was accepted", name)
		}
	}
}

func TestBodySecretCreateCapIs1MiBAndRefusesAnythingOverIt(t *testing.T) {
	if bodyCap(BodySecretCreate) != 1<<20 {
		t.Fatalf("the secret create cap = %d, want 1 MiB", bodyCap(BodySecretCreate))
	}
	over := `{"Name":"a","Data":"` + strings.Repeat("A", 1<<20) + `"}`
	if _, err := validateBody(BodySecretCreate, strings.NewReader(over), bodyEnv{}); err == nil {
		t.Error("a secret create body over 1 MiB was accepted")
	}
}

func TestBodyOnlyTheRebuiltBytesAndTheirHeadersReachTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	body := enc(t, container.ExecCreateRequest{Tty: true, AttachStdout: true, Cmd: []string{"/bin/sh"}})
	// A form content type would make the engine read the body as query keys.
	resp := do(t, h, postJSON("POST", "/v1.47/containers/abc/exec", body,
		"Content-Type: application/x-www-form-urlencoded\r\n", "Content-Encoding: gzip\r\n"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	bodies := sock.bodiesSeen()
	if len(bodies) != 1 {
		t.Fatalf("the engine saw %d bodies", len(bodies))
	}
	var sent, got container.ExecCreateRequest
	_ = json.Unmarshal([]byte(body), &sent)
	if err := json.Unmarshal(bodies[0], &got); err != nil || !reflect.DeepEqual(got, sent) {
		t.Fatalf("the engine received %s, want the same request as %s", bodies[0], body)
	}
	sock.mu.Lock()
	defer sock.mu.Unlock()
	if ct := sock.headers[0].Get("Content-Type"); ct != "application/json" {
		t.Errorf("the engine saw the content type %q", ct)
	}
	if ce := sock.headers[0].Get("Content-Encoding"); ce != "" {
		t.Errorf("the engine saw the content encoding %q", ce)
	}
}

func TestBodyRefusedBodiesNeverReachTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	var reasons []string
	h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
	big := `{"Tty":true,"Cmd":["` + strings.Repeat("a", 70<<10) + `"]}`
	for _, tc := range []struct{ name, raw string }{
		{"a privileged exec", postJSON("POST", "/containers/abc/exec", enc(t, container.ExecCreateRequest{Tty: true, Privileged: true, Cmd: []string{"sh"}}))},
		{"an exec with an environment", postJSON("POST", "/containers/abc/exec", enc(t, container.ExecCreateRequest{Tty: true, Env: []string{"A=1"}, Cmd: []string{"sh"}}))},
		{"an exec over 64 KiB", postJSON("POST", "/containers/abc/exec", big)},
		{"a detached exec start", postJSON("POST", "/exec/x/start", enc(t, container.ExecStartRequest{Detach: true, Tty: true}))},
		{"an ingress network", postJSON("POST", "/networks/create", enc(t, network.CreateRequest{Name: "n", Driver: "overlay", Ingress: true}))},
		{"a forced disconnect", postJSON("POST", "/networks/n1/disconnect", enc(t, network.DisconnectRequest{Container: "c", Force: true}))},
		{"a cluster volume", postJSON("POST", "/volumes/create", enc(t, volume.CreateRequest{Name: "v", ClusterVolumeSpec: &volume.ClusterVolumeSpec{}}))},
		{"an exec create with no body", request("POST", "/containers/abc/exec")},
		{"a volume create with no body", request("POST", "/volumes/create")},
		{"a chunked body of the wrong shape", request("POST", "/volumes/create", "Transfer-Encoding: chunked\r\n") + "6\r\n[1,2,3\r\n1\r\n]\r\n0\r\n\r\n"},
	} {
		resp := do(t, h, tc.raw)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", tc.name, resp.StatusCode)
		}
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("a refused request reached the engine: %v", got)
	}
	if len(reasons) != 10 {
		t.Errorf("onDenied ran %d times, want 10", len(reasons))
	}
}

func TestBodyEveryAcceptedShapeIsForwardedAsRebuiltBytes(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	cases := []struct{ target, body string }{
		{"/networks/create", enc(t, network.CreateRequest{Name: "n", Driver: "overlay", Attachable: true, Labels: map[string]string{"a": "b"}})},
		{"/networks/n1/connect", enc(t, network.ConnectRequest{Container: "c", EndpointConfig: &network.EndpointSettings{Aliases: []string{"x"}}})},
		{"/networks/n1/disconnect", enc(t, network.DisconnectRequest{Container: "c"})},
		{"/volumes/create", enc(t, volume.CreateRequest{Name: "v", Driver: "local", Labels: map[string]string{"a": "b"}})},
	}
	for _, tc := range cases {
		resp := do(t, h, postJSON("POST", tc.target, tc.body))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST %s: status %d, want 200", tc.target, resp.StatusCode)
		}
	}
	bodies := sock.bodiesSeen()
	if len(bodies) != len(cases) {
		t.Fatalf("the engine saw %d bodies, want %d", len(bodies), len(cases))
	}
	for i, tc := range cases {
		var a, b any
		_ = json.Unmarshal([]byte(tc.body), &a)
		_ = json.Unmarshal(bodies[i], &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("POST %s: the engine received %s, want the same as %s", tc.target, bodies[i], tc.body)
		}
	}
}

// openStream sends a raw request over a fresh stream and returns the parsed
// response head and the reader and writer the stream continues on.
func openStream(t *testing.T, h http.Handler, raw string) (*http.Response, *bufio.Reader, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	go ServeConn(server, h)
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(client, raw); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	return resp, br, client
}

func TestUpgradeIsForwardedOnExecStartAndBytesFlowBothWays(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	body := enc(t, container.ExecStartRequest{Tty: true, ConsoleSize: &[2]uint{24, 80}})
	resp, br, conn := openStream(t, h, postJSON("POST", "/exec/x1/start", body, "Connection: Upgrade\r\n", "Upgrade: tcp\r\n"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	if _, err := io.WriteString(conn, "echo hi\n"); err != nil {
		t.Fatal(err)
	}
	line, err := br.ReadString('\n')
	if err != nil || line != "echo hi\n" {
		t.Fatalf("the echo = %q, %v", line, err)
	}
	if got := sock.requests(); len(got) != 1 || got[0] != "POST /exec/x1/start" {
		t.Fatalf("the engine saw %v", got)
	}
}

func TestUpgradeIsRefusedEverywhereButExecStart(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	up := []string{"Connection: Upgrade\r\n", "Upgrade: tcp\r\n"}
	for _, tc := range []struct{ name, raw string }{
		{"stop", request("POST", "/containers/abc/stop", up...)},
		{"exec create", postJSON("POST", "/containers/abc/exec", enc(t, container.ExecCreateRequest{Tty: true, Cmd: []string{"sh"}}), up...)},
		{"exec resize", request("POST", "/exec/x/resize?h=24&w=80", up...)},
		{"logs", get("/containers/abc/logs", up...)},
		{"attach", request("POST", "/containers/abc/attach", up...)},
		{"a volume create", postJSON("POST", "/volumes/create", enc(t, volume.CreateRequest{Name: "v"}), up...)},
		{"events", get("/events", up...)},
	} {
		resp := do(t, h, tc.raw)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", tc.name, resp.StatusCode)
		}
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("an upgrade reached the engine: %v", got)
	}
}

func TestLogsAreForwardedAndFlushedAsWritten(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	target := "/v1.47/containers/abc/logs?follow=1&stdout=1&stderr=1&timestamps=1&tail=500&since=1700000000.5&until=2026-10-09T10:00:00Z"
	resp := do(t, h, get(target)) //nolint:bodyclose // closing would drain an endless stream; the helper closes the connection
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", target, resp.StatusCode)
	}
	// The handler is still open (it waits for release), so this line can only
	// have been delivered by flushing.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "container") {
		t.Fatalf("the first line did not arrive while the stream was open: %q, %v", line, err)
	}
	if got := sock.requests(); len(got) != 1 || got[0] != "GET "+target {
		t.Fatalf("the engine saw %v", got)
	}
}

func TestLogsRefuseWhatTheyDoNotTake(t *testing.T) {
	for _, q := range []string{"tail=abc", "tail=-1", "tail=1234567", "follow=maybe", "since=$(x)", "until=", "foo=1", "stdout=1&stdout=zz"} {
		if ok, _, _ := Allowed("GET", "/containers/abc/logs", q, false, false, Policy{}); ok {
			t.Errorf("logs?%s was allowed", q)
		}
	}
	for _, q := range []string{"", "tail=all", "tail=0", "details=true", "since=1700000000", "timestamps=0&follow=false"} {
		if ok, reason, _ := Allowed("GET", "/containers/abc/logs", q, false, false, Policy{}); !ok {
			t.Errorf("logs?%s was refused (%s)", q, reason)
		}
	}
}

func TestRouteNetworkVolumeAndExecReadsAndDeletes(t *testing.T) {
	sock := newFakeSocket(t)
	// The fake answers a volume it was not told of with 404, as an engine does.
	sock.answers = map[string]string{"/volumes/v1": `{"Name":"v1"}`}
	h := Handler(sock.path, Policy{}, nil)
	allowed := []struct{ method, target string }{
		{"DELETE", "/volumes/v1"},
		{"DELETE", "/networks/n1"},
		{"GET", "/volumes/v1"},
		{"GET", "/networks/n1?verbose=true&scope=swarm"},
		{"GET", "/system/df?type=volume&verbose=1"},
		{"POST", "/exec/x/resize?h=24&w=80"},
		{"GET", "/exec/x/json"},
	}
	for _, tc := range allowed {
		resp := do(t, h, request(tc.method, tc.target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: status %d, want 200", tc.method, tc.target, resp.StatusCode)
		}
	}
	if len(sock.requests()) != len(allowed) {
		t.Fatalf("the engine saw %v", sock.requests())
	}
	before := len(sock.requests())
	for _, tc := range []struct{ method, target string }{
		{"DELETE", "/volumes/v1?force=1"},
		{"DELETE", "/networks/n1?force=1"},
		{"GET", "/system/df?type=image"},
		{"GET", "/system/df?type=volume&type=container"},
		{"GET", "/networks/n1?scope=everything"},
		{"POST", "/exec/x/resize?h=24&w=80&extra=1"},
		{"POST", "/exec/x/resize?h=abc&w=80"},
		{"POST", "/volumes/prune"},
		{"POST", "/networks/prune"},
		{"GET", "/volumes/a/b"},
	} {
		resp := do(t, h, request(tc.method, tc.target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", tc.method, tc.target, resp.StatusCode)
		}
	}
	if got := sock.requests(); len(got) != before {
		t.Fatalf("a refused request reached the engine: %v", got[before:])
	}
}

func TestBodyConfigCreate(t *testing.T) {
	want := swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: "nginx.conf.v2", Labels: map[string]string{"app": "web"}},
		Data:        []byte("worker_processes 1;\n"),
	}
	var got swarm.ConfigSpec
	if err := roundTrip(t, BodyConfigCreate, enc(t, want), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted = %+v, %v; want %+v", got, err, want)
	}
	// The golang templating driver, named alone, is the one driver a config may carry.
	templated := want
	templated.Templating = &swarm.Driver{Name: "golang"}
	if err := roundTrip(t, BodyConfigCreate, enc(t, templated), &got); err != nil || !reflect.DeepEqual(got, templated) {
		t.Fatalf("templated = %+v, %v; want %+v", got, err, templated)
	}
	// The client encodes unset labels as null.
	if err := roundTrip(t, BodyConfigCreate, `{"Name":"a","Labels":null,"Data":"eA=="}`, &got); err != nil {
		t.Errorf("a config with null labels was refused: %v", err)
	}
	// 1,023,999 bytes is the largest content the swarm takes, and its base64 is well inside the cap.
	big := swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "big"}, Data: bytes.Repeat([]byte("x"), 1023999)}
	if err := roundTrip(t, BodyConfigCreate, enc(t, big), &got); err != nil || len(got.Data) != 1023999 {
		t.Errorf("a 1,023,999 byte config = %d bytes, %v; want accepted", len(got.Data), err)
	}
}

func TestBodyConfigCreateRefusals(t *testing.T) {
	many := map[string]string{}
	for i := range 65 {
		many[strconv.Itoa(i)] = "v"
	}
	spec := func(mod func(*swarm.ConfigSpec)) string {
		s := swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "a"}, Data: []byte("x")}
		mod(&s)
		return enc(t, s)
	}
	for name, body := range map[string]string{
		"another templating driver": spec(func(s *swarm.ConfigSpec) { s.Templating = &swarm.Driver{Name: "vault"} }),
		"an unnamed templating":     spec(func(s *swarm.ConfigSpec) { s.Templating = &swarm.Driver{} }),
		"templating options": spec(func(s *swarm.ConfigSpec) {
			s.Templating = &swarm.Driver{Name: "golang", Options: map[string]string{"a": "b"}}
		}),
		"an unknown key":           `{"Name":"a","Data":"eA==","Foo":1}`,
		"a driver key":             `{"Name":"a","Data":"eA==","Driver":{"Name":"vault"}}`,
		"an ID":                    `{"ID":"x","Name":"a","Data":"eA=="}`,
		"a version":                `{"Version":{"Index":3},"Name":"a","Data":"eA=="}`,
		"no name":                  enc(t, swarm.ConfigSpec{Data: []byte("x")}),
		"a name with a slash":      spec(func(s *swarm.ConfigSpec) { s.Name = "a/b" }),
		"a leading dash":           spec(func(s *swarm.ConfigSpec) { s.Name = "-a" }),
		"a trailing dot":           spec(func(s *swarm.ConfigSpec) { s.Name = "a." }),
		"a 65 character name":      spec(func(s *swarm.ConfigSpec) { s.Name = strings.Repeat("a", 65) }),
		"no content":               spec(func(s *swarm.ConfigSpec) { s.Data = nil }),
		"a 1,024,000 byte content": spec(func(s *swarm.ConfigSpec) { s.Data = bytes.Repeat([]byte("x"), 1024000) }),
		"too many labels":          spec(func(s *swarm.ConfigSpec) { s.Labels = many }),
		"an over-long label":       spec(func(s *swarm.ConfigSpec) { s.Labels = map[string]string{"k": strings.Repeat("v", 4097)} }),
		"data after the value":     `{"Name":"a","Data":"eA=="}{}`,
		"not an object":            `["a"]`,
		"an empty body":            ``,
		"content that is no b64":   `{"Name":"a","Data":"!!!"}`,
	} {
		if _, err := validateBody(BodyConfigCreate, strings.NewReader(body), bodyEnv{}); err == nil {
			t.Errorf("%s: the body was accepted", name)
		}
	}
}

func TestBodyConfigCreateCapIs2MiBAndRefusesAnythingOverIt(t *testing.T) {
	if bodyCap(BodyConfigCreate) != 2<<20 {
		t.Fatalf("the config create cap = %d, want 2 MiB", bodyCap(BodyConfigCreate))
	}
	over := `{"Name":"a","Data":"` + strings.Repeat("A", 2<<20) + `"}`
	if _, err := validateBody(BodyConfigCreate, strings.NewReader(over), bodyEnv{}); err == nil {
		t.Error("a config create body over 2 MiB was accepted")
	}
}

// The Compose SDK's container create is forwarded as the same bytes the SDK sent: the rebuild
// copies every field it sets, so nothing it needs is lost on the way to the engine.
func TestContainerCreateTheSDKSendsIsRebuiltByteForByte(t *testing.T) {
	in := enc(t, sdkContainerCreate())
	// The allowlisted policy is judged elsewhere; this bytes check runs with the host's opt-in so
	// it stands on the closed shape alone.
	out, err := validateBody(BodyContainerCreate, strings.NewReader(in), bodyEnv{policy: Policy{AllowPrivilegedServices: true}})
	if err != nil {
		t.Fatalf("the SDK's container create was refused: %v", err)
	}
	if string(out) != in {
		t.Fatalf("the rebuilt body differs from the SDK's:\n got %s\nwant %s", out, in)
	}
}

func TestContainerCreateRefusesAnUnknownField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*container.CreateRequest)
	}{
		{"ContainerIDFile", func(r *container.CreateRequest) { r.HostConfig.ContainerIDFile = "/tmp/cid" }},
		{"VolumeDriver", func(r *container.CreateRequest) { r.HostConfig.VolumeDriver = "rexray/ebs" }},
		{"PublishAllPorts", func(r *container.CreateRequest) { r.HostConfig.PublishAllPorts = true }},
		{"Cgroup", func(r *container.CreateRequest) { r.HostConfig.Cgroup = "container:other" }},
		{"Umask", func(r *container.CreateRequest) { r.HostConfig.Umask = new(uint32) }},
		{"ConsoleSize", func(r *container.CreateRequest) { r.HostConfig.ConsoleSize = [2]uint{24, 80} }},
		{"OnBuild", func(r *container.CreateRequest) { r.OnBuild = []string{"RUN x"} }},
		{"ArgsEscaped", func(r *container.CreateRequest) { r.ArgsEscaped = true }},
		{"NetworkDisabled", func(r *container.CreateRequest) { r.NetworkDisabled = true }},
		{"an endpoint's NetworkID", func(r *container.CreateRequest) { r.NetworkingConfig.EndpointsConfig["shop_default"].NetworkID = "n1" }},
		{"an endpoint's EndpointID", func(r *container.CreateRequest) { r.NetworkingConfig.EndpointsConfig["shop_default"].EndpointID = "e1" }},
		{"an endpoint's DNSNames", func(r *container.CreateRequest) {
			r.NetworkingConfig.EndpointsConfig["shop_default"].DNSNames = []string{"x"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sdkContainerCreate()
			tc.build(&req)
			_, err := validateBody(BodyContainerCreate, strings.NewReader(enc(t, req)), bodyEnv{policy: Policy{AllowPrivilegedServices: true}})
			if !errors.Is(err, errBodyRefused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
		})
	}
	t.Run("a key that is no field at all", func(t *testing.T) {
		body := strings.Replace(enc(t, sdkContainerCreate()), `{"Hostname"`, `{"Bogus":1,"Hostname"`, 1)
		if _, err := validateBody(BodyContainerCreate, strings.NewReader(body), bodyEnv{}); !errors.Is(err, errBodyRefused) {
			t.Fatalf("err = %v, want a refusal", err)
		}
	})
}

func TestContainerCreateNeedsAnImage(t *testing.T) {
	noImage := sdkContainerCreate()
	noImage.Image = ""
	noConfig := container.CreateRequest{HostConfig: &container.HostConfig{}}
	for name, body := range map[string]string{
		"an empty image":  enc(t, noImage),
		"no config":       enc(t, noConfig),
		"an empty object": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateBody(BodyContainerCreate, strings.NewReader(body), bodyEnv{}); !errors.Is(err, errBodyRefused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
		})
	}
}

func TestContainerCreateOverTheCapIsRefused(t *testing.T) {
	if got := bodyCap(BodyContainerCreate); got != 256<<10 {
		t.Fatalf("the container create cap is %d, want 256 KiB", got)
	}
	req := sdkContainerCreate()
	req.Env = []string{"BIG=" + strings.Repeat("a", 250<<10)}
	underCap := enc(t, req)
	if len(underCap) >= 256<<10 {
		t.Fatalf("test body is %d bytes, want it under the cap", len(underCap))
	}
	if _, err := validateBody(BodyContainerCreate, strings.NewReader(underCap), bodyEnv{policy: Policy{AllowPrivilegedServices: true}}); err != nil {
		t.Fatalf("a body under the cap was refused: %v", err)
	}
	req.Env = []string{"BIG=" + strings.Repeat("a", 256<<10)}
	if _, err := validateBody(BodyContainerCreate, strings.NewReader(enc(t, req)), bodyEnv{policy: Policy{AllowPrivilegedServices: true}}); !errors.Is(err, errBodyRefused) {
		t.Fatalf("err = %v, want the cap refusal", err)
	}
}
