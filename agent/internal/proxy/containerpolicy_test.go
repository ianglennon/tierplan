// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/tierplan/proto/hostpath"
)

// containerFindingsPrefix starts the paths in the text of a host-policy refusal.
const containerFindingsPrefix = "gives the container host access: "

// containerVectorEnv answers the lookups a container create makes the way the vector's engine would: the
// named volumes it holds and the network names or IDs that are the host network. A network the
// vector does not list is an ordinary one.
func containerVectorEnv(t *testing.T, v hostpath.ContainerVector, policy Policy) bodyEnv {
	t.Helper()
	return bodyEnv{
		policy: policy,
		ctx:    t.Context(),
		volumes: func(_ context.Context, name string) (string, map[string]string, bool, error) {
			vv, ok := v.Volumes[name]
			if !ok {
				return "", nil, false, nil
			}
			return vv.Driver, vv.Options, true, nil
		},
		lookup: func(_ context.Context, target string) (string, string, error) {
			if slices.Contains(v.HostNetworkIDs, target) {
				return "host", "host", nil
			}
			return target, "bridge", nil
		},
	}
}

// decodeContainerVector reads a vector's body as the engine's own type.
func decodeContainerVector(t *testing.T, raw []byte) container.CreateRequest {
	t.Helper()
	var req container.CreateRequest
	if err := decodeStrict(raw, &req); err != nil {
		t.Fatalf("the vector body does not decode: %v: %s", err, raw)
	}
	return req
}

// containerFindingsOf is the finding paths named in a host-policy refusal.
func containerFindingsOf(t *testing.T, err error) []string {
	t.Helper()
	if !errors.Is(err, errBodyRefused) || !errors.Is(err, errHostPolicy) {
		t.Fatalf("err = %v, want a host-policy refusal", err)
	}
	_, tail, ok := strings.Cut(err.Error(), containerFindingsPrefix)
	if !ok {
		t.Fatalf("the refusal %q does not name its findings", err)
	}
	return strings.Split(tail, ", ")
}

// Every container-create vector is refused with the paths it lists on a host without the
// opt-in and forwarded, as the same request, on a host with it.
func TestContainerVectorsAreRefusedByDefaultAndForwardedWithTheOptIn(t *testing.T) {
	for _, v := range hostpath.ContainerVectors() {
		t.Run(v.Name, func(t *testing.T) {
			want := decodeContainerVector(t, v.Body)

			got, err := validateContainerCreate(v.Body, containerVectorEnv(t, v, Policy{}))
			if len(v.Refused) == 0 {
				if err != nil {
					t.Fatalf("the default policy refused a forwarded vector: %v", err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("the default policy rebuilt %+v, want %+v", got, want)
				}
			} else if gotPaths := containerFindingsOf(t, err); !slices.Equal(gotPaths, v.Refused) {
				t.Fatalf("findings = %v, want %v", gotPaths, v.Refused)
			}

			got, err = validateContainerCreate(v.Body, containerVectorEnv(t, v, Policy{AllowPrivilegedServices: true}))
			if err != nil {
				t.Fatalf("the opt-in refused it: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("the opt-in rebuilt %+v, want %+v", got, want)
			}
		})
	}
}

// With the zero environment (no lookups) a named volume cannot be shown to be free of host access, so
// it is refused, as a service's is; an anonymous volume and a path under a data root are not.
func TestContainerCreateWithNoLookupRefusesANamedVolume(t *testing.T) {
	byName := map[string]hostpath.ContainerVector{}
	for _, v := range hostpath.ContainerVectors() {
		byName[v.Name] = v
	}
	_, err := validateContainerCreate(byName["named volume the engine does not have"].Body, bodyEnv{})
	if got := containerFindingsOf(t, err); !slices.Equal(got, []string{"HostConfig.Binds[0]"}) {
		t.Fatalf("findings = %v", got)
	}
	for _, name := range []string{"anonymous volume in binds", "bind below /srv", "anonymous volume mount", "tmpfs mount", "plain container"} {
		if _, err := validateContainerCreate(byName[name].Body, bodyEnv{}); err != nil {
			t.Errorf("%s: refused with no lookup: %v", name, err)
		}
	}
}

// A volume the engine did not answer for is an engine fault, not host access, unless a real finding
// is present; and each distinct name is asked once.
func TestContainerCreateAVolumeLookupThatFailsIsAnEngineFaultNotHostAccess(t *testing.T) {
	body := enc(t, container.CreateRequest{
		Config:     &container.Config{Image: "x"},
		HostConfig: &container.HostConfig{Binds: []string{"a:/a", "a:/b", "b:/c"}},
	})
	var asked atomic.Int32
	env := bodyEnv{ctx: t.Context(), volumes: func(_ context.Context, name string) (string, map[string]string, bool, error) {
		asked.Add(1)
		if name == "b" {
			return "", nil, false, errors.New("engine down")
		}
		return "local", nil, true, nil
	}}
	_, err := validateContainerCreate([]byte(body), env)
	if !errors.Is(err, errVolumeLookup) || errors.Is(err, errHostPolicy) {
		t.Fatalf("err = %v, want errVolumeLookup and not host access", err)
	}
	if got := asked.Load(); got != 2 {
		t.Errorf("the engine was asked %d times, want 2 (a once, b once)", got)
	}

	withFinding := enc(t, container.CreateRequest{
		Config:     &container.Config{Image: "x"},
		HostConfig: &container.HostConfig{Privileged: true, Binds: []string{"b:/c"}},
	})
	if got := containerFindingsOf(t, func() error { _, err := validateContainerCreate([]byte(withFinding), env); return err }()); !slices.Equal(got, []string{"HostConfig.Privileged"}) {
		t.Fatalf("findings = %v, want a real finding named even when a lookup also failed", got)
	}
}

// Through the handler: a refusal is a 403 with the host_policy answer and never reaches the engine,
// and the same body goes through on a host with the opt-in.
func TestContainerCreateHostPolicyThroughTheHandler(t *testing.T) {
	privileged := enc(t, container.CreateRequest{
		Config:     &container.Config{Image: "x"},
		HostConfig: &container.HostConfig{Privileged: true, Binds: []string{"/var/run/docker.sock:/s"}},
	})
	t.Run("refused", func(t *testing.T) {
		sock := newFakeSocket(t)
		var reasons []string
		h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
		resp := do(t, h, postJSON("POST", "/containers/create?name=web", privileged))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status %d, want 403", resp.StatusCode)
		}
		var msg struct{ Message string }
		if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil || msg.Message != "agent_policy_denied: host_policy: POST /containers/create" {
			t.Fatalf("message = %q (%v)", msg.Message, err)
		}
		if !slices.Equal(reasons, []string{"host_policy"}) {
			t.Errorf("onDenied reasons = %v, want [host_policy]", reasons)
		}
		if got := sock.writesSeen(); len(got) != 0 {
			t.Fatalf("a refused container create reached the engine: %v", got)
		}
	})
	t.Run("opt-in", func(t *testing.T) {
		sock := newFakeSocket(t)
		h := Handler(sock.path, Policy{AllowPrivilegedServices: true}, nil)
		resp := do(t, h, postJSON("POST", "/containers/create?name=web", privileged))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
		if got := sock.writesSeen(); !slices.Equal(got, []string{"POST /containers/create?name=web"}) {
			t.Fatalf("the engine saw %v", got)
		}
	})
	t.Run("an existing volume that mounts the host is found on the agent's own engine", func(t *testing.T) {
		sock := newFakeSocket(t)
		sock.answers = map[string]string{"/volumes/rootvol": `{"Name":"rootvol","Driver":"local","Options":{"type":"none","o":"bind","device":"/"}}`}
		var reasons []string
		h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
		body := enc(t, container.CreateRequest{
			Config:     &container.Config{Image: "x"},
			HostConfig: &container.HostConfig{Binds: []string{"rootvol:/host"}},
		})
		resp := do(t, h, postJSON("POST", "/containers/create", body))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !slices.Equal(reasons, []string{"host_policy"}) {
			t.Fatalf("status %d, reasons %v, want 403 host_policy", resp.StatusCode, reasons)
		}
		if got := sock.writesSeen(); len(got) != 0 {
			t.Fatalf("the engine saw %v", got)
		}
	})
	t.Run("the host network by id is found on the agent's own engine", func(t *testing.T) {
		sock := newFakeSocket(t)
		sock.answers = map[string]string{"/networks/f1e2d3c4b5a6": `{"Name":"host","Driver":"host"}`}
		h := Handler(sock.path, Policy{}, nil)
		body := enc(t, container.CreateRequest{
			Config:     &container.Config{Image: "x"},
			HostConfig: &container.HostConfig{NetworkMode: "f1e2d3c4b5a6"},
		})
		resp := do(t, h, postJSON("POST", "/containers/create", body))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status %d, want 403", resp.StatusCode)
		}
		if got := sock.writesSeen(); len(got) != 0 {
			t.Fatalf("the engine saw %v", got)
		}
	})
	t.Run("a volume lookup the engine cannot answer is a 502 and not host access", func(t *testing.T) {
		sock := newFakeSocket(t)
		sock.answers = map[string]string{"/volumes/data": `{"message":"boom"}`}
		sock.status = map[string]int{"/volumes/data": http.StatusInternalServerError}
		var reasons []string
		h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
		body := enc(t, container.CreateRequest{
			Config:     &container.Config{Image: "x"},
			HostConfig: &container.HostConfig{Binds: []string{"data:/data"}},
		})
		resp := do(t, h, postJSON("POST", "/containers/create", body))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || len(reasons) != 0 {
			t.Fatalf("status %d, reasons %v, want 502 and nothing reported", resp.StatusCode, reasons)
		}
		if got := sock.writesSeen(); len(got) != 0 {
			t.Fatalf("the engine saw %v", got)
		}
	})
}
