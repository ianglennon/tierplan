// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package facts reads what an agent reports about its own host from the Docker
// socket: who the engine is and what part it plays in a swarm. The server binds
// an environment to these facts, so they come from the engine itself and never
// from configuration, and each string is checked here before it is sent (the
// server checks again).
//
// It speaks plain HTTP over the Unix socket and does not pull in the Docker
// client: the agent stays small.
package facts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/bayplan/proto/wire"
)

const (
	// timeout bounds each of the two reads.
	timeout = 3 * time.Second
	// maxBody bounds a /info answer, which is a few KiB on a real engine.
	maxBody = 1 << 20
	// maxHostname and maxVersion cap text that is only shown.
	maxHostname = 128
	maxVersion  = 64
	// maxManagers is how many manager node IDs are reported.
	maxManagers = 64
)

// Collect describes the engine behind socketPath. An engine that does not answer
// leaves EngineReachable false; it is not an error, because the server shows it as
// engine_unreachable. The policy flags are the caller's to fill in.
func Collect(ctx context.Context, socketPath string) wire.Facts {
	var f wire.Facts
	hc := &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		}},
		Timeout: timeout,
	}
	defer hc.CloseIdleConnections()

	var info struct {
		ID, Name      string
		ServerVersion string
		Swarm         struct {
			LocalNodeState   string
			NodeID           string
			ControlAvailable bool
			Cluster          *struct{ ID string }
			RemoteManagers   []struct{ NodeID string }
		}
	}
	if err := getJSON(ctx, hc, "/info", &info); err != nil {
		return f
	}
	f.EngineReachable = true
	f.EngineID = identity(info.ID)
	f.Hostname = text(info.Name, maxHostname)
	f.EngineVersion = text(info.ServerVersion, maxVersion)
	if st := info.Swarm.LocalNodeState; st != "" && st != "inactive" {
		sf := &wire.SwarmFacts{State: text(st, 16), NodeID: identity(info.Swarm.NodeID), Control: info.Swarm.ControlAvailable}
		// Only a manager knows the cluster ID. A worker reports none, whatever its
		// engine answered, so a worker can never claim to be the cluster's manager.
		if info.Swarm.Cluster != nil && info.Swarm.ControlAvailable {
			sf.ClusterID = identity(info.Swarm.Cluster.ID)
		}
		for _, m := range info.Swarm.RemoteManagers {
			if id := identity(m.NodeID); id != "" && len(sf.ManagerIDs) < maxManagers {
				sf.ManagerIDs = append(sf.ManagerIDs, id)
			}
		}
		f.Swarm = sf
	}

	var ver struct {
		Version    string
		APIVersion string `json:"ApiVersion"`
	}
	if err := getJSON(ctx, hc, "/version", &ver); err != nil {
		return f
	}
	f.APIVersion = text(ver.APIVersion, maxVersion)
	if v := text(ver.Version, maxVersion); v != "" {
		f.EngineVersion = v
	}
	return f
}

// identity is s if it is a valid ID, else empty: an engine that reports one the
// server would refuse is better reported as having none, with its own message.
func identity(s string) string {
	if wire.ValidIdentity(s) {
		return s
	}
	return ""
}

// text makes engine-supplied text safe to send and show: valid UTF-8, no control
// characters, at most max bytes.
func text(s string, max int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

func getJSON(ctx context.Context, hc *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v)
}
