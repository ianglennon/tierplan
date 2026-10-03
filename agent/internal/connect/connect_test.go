// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeEngine(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bpag")
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

func TestReadFactsDescribesAStandaloneEngine(t *testing.T) {
	sock := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			_, _ = w.Write([]byte(`{"ID":"ENG:1","Name":"node-1","ServerVersion":"29.1.0","Swarm":{"LocalNodeState":"inactive"}}`))
		case "/version":
			_, _ = w.Write([]byte(`{"Version":"29.1.0","ApiVersion":"1.52"}`))
		default:
			http.NotFound(w, r)
		}
	})
	f := readFacts(context.Background(), sock, slog.New(slog.DiscardHandler))
	if !f.EngineReachable || f.EngineID != "ENG:1" || f.Hostname != "node-1" || f.EngineVersion != "29.1.0" || f.APIVersion != "1.52" {
		t.Fatalf("facts = %+v", f)
	}
	if f.Swarm != nil {
		t.Fatalf("an engine outside a swarm reported swarm facts: %+v", f.Swarm)
	}
}

func TestReadFactsDescribesASwarmManager(t *testing.T) {
	sock := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			_, _ = w.Write([]byte(`{"ID":"E","Name":"m1","ServerVersion":"29.1.0","Swarm":{"LocalNodeState":"active","NodeID":"n1","ControlAvailable":true,"Cluster":{"ID":"c1"},"RemoteManagers":[{"NodeID":"n2"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"Version":"29.1.0","ApiVersion":"1.52"}`))
	})
	f := readFacts(context.Background(), sock, slog.New(slog.DiscardHandler))
	if f.Swarm == nil || f.Swarm.State != "active" || f.Swarm.NodeID != "n1" || !f.Swarm.Control || f.Swarm.ClusterID != "c1" ||
		len(f.Swarm.ManagerIDs) != 1 || f.Swarm.ManagerIDs[0] != "n2" {
		t.Fatalf("swarm facts = %+v", f.Swarm)
	}
}

func TestReadFactsReportsAnEngineThatDoesNotAnswerAsUnreachable(t *testing.T) {
	f := readFacts(context.Background(), filepath.Join(t.TempDir(), "none.sock"), slog.New(slog.DiscardHandler))
	if f.EngineReachable {
		t.Fatalf("facts = %+v, want the engine unreachable", f)
	}
	sock := fakeEngine(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if f := readFacts(context.Background(), sock, slog.New(slog.DiscardHandler)); f.EngineReachable {
		t.Fatalf("an engine answering 500 was reported reachable: %+v", f)
	}
}

func TestRunRefusesAnAddressThatIsNotAWebSocketURL(t *testing.T) {
	for _, bad := range []string{"", "https://bayplan.example/agent/connect", "not a url", "wss://"} {
		err := Run(context.Background(), Config{URL: bad, JoinToken: "x"})
		if err == nil || strings.Contains(err.Error(), "bpjt") {
			t.Errorf("Run(%q) = %v, want an error about the address", bad, err)
		}
	}
}

func TestARefusalNamesItsCode(t *testing.T) {
	err := error(&RefusedError{Code: "auth_rejected", Detail: "the join token was not accepted"})
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "auth_rejected") || !strings.Contains(err.Error(), "join token") {
		t.Fatalf("error = %v", err)
	}
	if got := (&RefusedError{Code: "revoked"}).Error(); !strings.Contains(got, "revoked") {
		t.Errorf("error = %q", got)
	}
}
