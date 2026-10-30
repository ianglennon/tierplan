// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package facts

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEngine serves h on a Unix socket and returns its path.
func fakeEngine(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cmfa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return path
}

// engine answers /info with info and /version with a fixed engine version.
func engine(t *testing.T, info string) string {
	t.Helper()
	return fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			_, _ = w.Write([]byte(info))
		case "/version":
			_, _ = w.Write([]byte(`{"Version":"29.1.0","ApiVersion":"1.52"}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func TestCollectDescribesAStandaloneEngineWithNoSwarmFacts(t *testing.T) {
	sock := engine(t, `{"ID":"ENG:1","Name":"node-1","ServerVersion":"29.1.0","Swarm":{"LocalNodeState":"inactive"}}`)
	f := Collect(context.Background(), sock)
	if !f.EngineReachable || f.EngineID != "ENG:1" || f.Hostname != "node-1" || f.EngineVersion != "29.1.0" || f.APIVersion != "1.52" {
		t.Fatalf("facts = %+v", f)
	}
	if f.Swarm != nil {
		t.Fatalf("an engine outside a swarm reported swarm facts: %+v", f.Swarm)
	}
}

func TestCollectDescribesASwarmManagerWithItsClusterIDAndControl(t *testing.T) {
	sock := engine(t, `{"ID":"E","Name":"m1","ServerVersion":"29.1.0","Swarm":{"LocalNodeState":"active","NodeID":"n1","ControlAvailable":true,"Cluster":{"ID":"c1"},"RemoteManagers":[{"NodeID":"n2"}]}}`)
	f := Collect(context.Background(), sock)
	if f.Swarm == nil || f.Swarm.State != "active" || f.Swarm.NodeID != "n1" || !f.Swarm.Control || f.Swarm.ClusterID != "c1" ||
		len(f.Swarm.ManagerIDs) != 1 || f.Swarm.ManagerIDs[0] != "n2" {
		t.Fatalf("swarm facts = %+v", f.Swarm)
	}
}

func TestCollectDescribesAWorkerWithItsNodeIDAndManagersButNoClusterID(t *testing.T) {
	// A worker's /info has no Cluster, and the manager list names the managers it
	// knows. A cluster ID that appeared anyway is not reported: only a manager knows it.
	sock := engine(t, `{"ID":"E","Name":"w1","Swarm":{"LocalNodeState":"active","NodeID":"w1n","ControlAvailable":false,"Cluster":{"ID":"c1"},"RemoteManagers":[{"NodeID":"m1"},{"NodeID":"m2"}]}}`)
	f := Collect(context.Background(), sock)
	if f.Swarm == nil || f.Swarm.NodeID != "w1n" || f.Swarm.Control || f.Swarm.ClusterID != "" {
		t.Fatalf("worker facts = %+v", f.Swarm)
	}
	if len(f.Swarm.ManagerIDs) != 2 || f.Swarm.ManagerIDs[0] != "m1" || f.Swarm.ManagerIDs[1] != "m2" {
		t.Fatalf("manager IDs = %v", f.Swarm.ManagerIDs)
	}
}

func TestCollectReportsAnEngineThatDoesNotAnswerAsUnreachableWithoutAnError(t *testing.T) {
	if f := Collect(context.Background(), filepath.Join(t.TempDir(), "none.sock")); f.EngineReachable || f.EngineID != "" {
		t.Fatalf("facts = %+v, want the engine unreachable", f)
	}
	sock := fakeEngine(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if f := Collect(context.Background(), sock); f.EngineReachable {
		t.Fatalf("an engine answering 500 was reported reachable: %+v", f)
	}
}

func TestCollectDropsAnIdentityOutsideTheAlphabetAndCapsLongText(t *testing.T) {
	long := strings.Repeat("h", 400)
	sock := engine(t, `{"ID":"x'; DROP TABLE agents;--","Name":"`+long+`\u0007bell","ServerVersion":"`+strings.Repeat("9", 200)+`","Swarm":{"LocalNodeState":"active","NodeID":"has space","ControlAvailable":true,"Cluster":{"ID":"`+strings.Repeat("c", 129)+`"},"RemoteManagers":[{"NodeID":"ok-1"},{"NodeID":"bad id"}]}}`)
	f := Collect(context.Background(), sock)
	if f.EngineID != "" {
		t.Errorf("engine ID %q was reported though it is outside the alphabet", f.EngineID)
	}
	if len(f.Hostname) > 128 || strings.ContainsRune(f.Hostname, 7) {
		t.Errorf("hostname = %q (%d bytes), want at most 128 bytes and no control characters", f.Hostname, len(f.Hostname))
	}
	if len(f.EngineVersion) > 64 {
		t.Errorf("engine version has %d bytes, want at most 64", len(f.EngineVersion))
	}
	if f.Swarm == nil || f.Swarm.NodeID != "" || f.Swarm.ClusterID != "" {
		t.Errorf("swarm identities = %+v, want the invalid node and cluster IDs dropped", f.Swarm)
	}
	if f.Swarm != nil && (len(f.Swarm.ManagerIDs) != 1 || f.Swarm.ManagerIDs[0] != "ok-1") {
		t.Errorf("manager IDs = %v, want only the valid one", f.Swarm.ManagerIDs)
	}
}
