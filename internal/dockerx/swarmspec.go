// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

var (
	// ErrNotSwarmManager means the engine did not report a swarm cluster it manages: it is outside
	// a swarm, or a worker, or a manager that has not yet learned the cluster.
	ErrNotSwarmManager = errors.New("dockerx: the engine is not a swarm manager")
	// ErrSwarmChanged means the swarm was changed after its settings were read: the version sent
	// with an update is no longer the swarm's own (the engine's "update out of sequence"). Nothing
	// was changed. The caller reads the settings again and asks again; this package never does.
	ErrSwarmChanged = errors.New("dockerx: the swarm changed since its settings were read")
)

// SwarmSettings is what one manager says about the swarm's own settings: the version of the
// spec and the spec itself, which a swarm update must be sent against.
type SwarmSettings struct {
	// Version is the swarm's object version (Info.Swarm.Cluster.Version.Index), the one an update
	// is judged against.
	Version uint64
	// Spec is the swarm's spec as the engine reports it in /info. It carries no join token.
	Spec swarm.Spec
	// TaskHistoryLimit is Spec.Orchestration.TaskHistoryRetentionLimit as the engine reports it,
	// -1 included (it keeps every task), and DefaultTaskHistoryLimit when the cluster never set
	// it. Unlike TaskHistoryLimit on the Client it does not turn a negative value into the
	// default: the person changing the limit must see what the swarm really does.
	TaskHistoryLimit int64
	// Manager is true when the engine controls the swarm, which is the only engine that can
	// change it. SwarmSettings answers ErrNotSwarmManager for any other, so it is true in every
	// result.
	Manager bool
}

// SwarmSettings reads the settings of the swarm from the engine's /info, which carries the
// cluster without its join tokens. GET /swarm is never used: its answer holds the credentials to
// add a node, and an agent does not forward it. An engine that is not a manager of a swarm is
// ErrNotSwarmManager. It is read-only.
func (c *Client) SwarmSettings(ctx context.Context) (SwarmSettings, error) {
	in, err := c.cur().Info(ctx, client.InfoOptions{})
	if err != nil {
		return SwarmSettings{}, fmt.Errorf("read the engine info from %s: %w", c.t.Describe(), err)
	}
	sw := in.Info.Swarm
	if sw.Cluster == nil || !sw.ControlAvailable {
		return SwarmSettings{}, fmt.Errorf("read the swarm settings from %s: %w", c.t.Describe(), ErrNotSwarmManager)
	}
	out := SwarmSettings{
		Version: sw.Cluster.Version.Index, Spec: sw.Cluster.Spec, TaskHistoryLimit: DefaultTaskHistoryLimit, Manager: true,
	}
	if lim := sw.Cluster.Spec.Orchestration.TaskHistoryRetentionLimit; lim != nil {
		out.TaskHistoryLimit = *lim
	}
	return out, nil
}

// maxSwarmUpdateAnswer caps what is read of a refusal of a swarm update.
const maxSwarmUpdateAnswer = 4 << 10

// swarmUpdateTimeout bounds one swarm update.
const swarmUpdateTimeout = 30 * time.Second

// SetTaskHistoryLimit changes the swarm's task history retention limit and nothing else. It sends
// the spec s read, with only Orchestration.TaskHistoryRetentionLimit replaced, in one
// POST /swarm/update against the version s read. It sets no rotate flag, and the request does not
// even carry the rotate keys: the engine's client always sends them (as false), and an agent's
// route for this call takes the version as its only key, so the request is built by hand. A swarm
// whose version moved since s was read is ErrSwarmChanged and nothing is changed. The write is
// never retried: whoever asked for it asks again.
//
// An agent's refusal keeps its words in the error, so envs.ClassifyDockerError reads it.
func (c *Client) SetTaskHistoryLimit(ctx context.Context, s SwarmSettings, limit int64) error {
	spec := s.Spec
	spec.Orchestration.TaskHistoryRetentionLimit = &limit
	body, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("encode the swarm spec for %s: %w", c.t.Describe(), err)
	}
	// The version prefix is the one the client negotiated, so an engine of any age takes it.
	target := "http://docker/v" + c.cur().ClientVersion() + "/swarm/update?version=" + strconv.FormatUint(s.Version, 10)
	ctx, cancel := context.WithTimeout(ctx, swarmUpdateTimeout)
	defer cancel()
	// G704: the host is a placeholder; the transport dials the engine whatever the URL says.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body)) //nolint:gosec // G704, see above
	if err != nil {
		return fmt.Errorf("update the swarm on %s: %w", c.t.Describe(), err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return c.t.DialContext(ctx, "", "") },
		DisableKeepAlives: true,
	}}
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req) //nolint:gosec // G704, see above
	if err != nil {
		return fmt.Errorf("update the swarm on %s: %w", c.t.Describe(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxSwarmUpdateAnswer))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg := engineMessage(raw)
	if strings.Contains(msg, outOfSequence) {
		return fmt.Errorf("update the swarm on %s: %w", c.t.Describe(), ErrSwarmChanged)
	}
	return fmt.Errorf("update the swarm on %s: %s: %s", c.t.Describe(), resp.Status, msg)
}

// engineMessage is the "message" of an engine's JSON error body, or the body's own text when it is
// not that.
func engineMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	return strings.TrimSpace(string(raw))
}
