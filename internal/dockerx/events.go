// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// EventRow is one Docker event as the feed shows it. It carries only what the
// feed needs: Docker puts every container label and some environment details
// into an event's attributes, and those can hold secrets (A12, T-01-88, D-61).
type EventRow struct {
	// ID is a stable hash of TimeNano, Type, Action and ActorID. It serves as
	// the SSE event id and as the key that drops an event the daemon sends twice.
	ID       string
	Time     time.Time
	TimeNano int64
	// Type is container, image, network, volume, service, node, secret or config;
	// Coaming's own rows have the type coaming.
	Type string
	// Scope is local for what happened on this node and swarm for what happened to
	// the cluster, which every manager reports; empty for a row of Coaming's own.
	Scope string
	// Action is what happened, for example start, die, pull or connect.
	Action string
	// ActorID is the ID of the container, image, network or volume.
	ActorID string
	// Name is the container, image or network name; for a volume, its name,
	// which Docker uses as the actor ID.
	Name string
	// Attributes holds only the allowlisted attributes (keptAttributes), when the
	// event had them.
	Attributes map[string]string
	// InvalidateOnly marks a row that tells the hub to drop its cached snapshot and
	// tells browsers to refetch, but is not shown in the feed and is not stored in
	// the ring. A container's health_status change is one: the list needs to show
	// Unhealthy at once, while a feed meant for people has no use for every check
	// result. Its attributes are always empty, so a health check's command line
	// never leaves the server.
	InvalidateOnly bool
}

// eventTypes are the event types the feed shows (D-57, D-48). The swarm types
// (service, node, secret, config) are only ever sent by a swarm manager; a
// standalone engine or a worker accepts the filter and simply never sends them,
// so one list serves every host.
var eventTypes = []string{"container", "image", "network", "volume", "service", "node", "secret", "config"}

// Actions that are noise in a feed meant for people (A12). exec_create,
// exec_start and exec_die fire for every health check command and every
// `docker exec`, so a busy host would bury everything else; the exec_ actions
// also carry the command line, which can hold secrets.
var (
	noiseActionPrefixes = []string{"exec_"}
	noiseActions        = map[string]bool{"attach": true, "detach": true, "resize": true, "top": true}
)

// invalidateOnlyActionPrefixes are the actions that change what a list shows but
// would bury the feed: a container's health_status fires for every check. They
// become rows with InvalidateOnly set and no attributes (RES-14).
var invalidateOnlyActionPrefixes = []string{"health_status"}

// keptAttributes is the allowlist of event attributes that reach a browser
// (D-61, T-01-88, T-04-98). Docker puts every container label, and for some
// events an environment value or a command line, into an event's attributes, and
// those can hold secrets, so nothing is passed unless it is named here. The set is
// the small, non-sensitive one the Events page and the dashboard need: what the
// object is called (name, image), how a container ended (exitCode, signal), what
// kind of network or volume it is and what it was attached to (type, container,
// driver, destination, read/write, reason), and the two labels that group
// containers by project (com.docker.compose.project, com.docker.stack.namespace).
// No other label, no environment value and no free-form attribute is on it, and
// nothing outside it is added without a new decision.
var keptAttributes = []string{
	"name", "image", "exitCode", "signal",
	"type", "container", "driver", "reason", "destination", "read/write",
	"com.docker.compose.project", "com.docker.stack.namespace",
}

// Events streams the engine's container, image, network, volume and (on a swarm
// manager) service, node, secret and config events as they happen. The first stream starts at since, which replays what the daemon
// still buffers (at most its last 256 events, and nothing after a daemon
// restart); the zero time means "from now". Resuming after a drop passes the
// time of the last event seen, and the daemon then sends that event again, so
// the caller dedupes by ID.
//
// The rows channel is closed when the stream ends, for whatever reason, and the
// error channel then holds the reason (the context's error when ctx ended).
// Cancelling ctx is how a caller stops the stream. A stream that ended is never
// reopened here: the caller decides when to try again.
//
// The stream keeps the moby client that was current when it began; a later
// Probe replaces the client but closes only its idle connections.
func (c *Client) Events(ctx context.Context, since time.Time) (<-chan EventRow, <-chan error) {
	cli := c.cur()

	rows := make(chan EventRow)
	errc := make(chan error, 1)
	go func() {
		err := c.streamEvents(ctx, cli, since, rows)
		errc <- err
		close(errc)
		close(rows)
	}()
	return rows, errc
}

// streamEvents runs one stream until it ends and returns why.
func (c *Client) streamEvents(ctx context.Context, cli *client.Client, since time.Time, rows chan<- EventRow) error {
	res := cli.Events(ctx, client.EventsListOptions{
		Since:   sinceParam(since),
		Filters: make(client.Filters).Add("type", eventTypes...),
	})
	for {
		select {
		case m := <-res.Messages:
			row, ok := eventRow(m)
			if !ok {
				continue
			}
			select {
			case rows <- row:
			case <-ctx.Done():
				return ctx.Err()
			}
		case err := <-res.Err:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				err = io.EOF
			}
			return fmt.Errorf("read the events of %s: %w", c.t.Describe(), err)
		}
	}
}

// sinceParam is how the Engine API wants a start time: unix seconds, or
// seconds.nanoseconds when the time has a fractional part, which is how a
// resume names the exact event it stopped at.
func sinceParam(t time.Time) string {
	switch {
	case t.IsZero():
		return ""
	case t.Nanosecond() == 0:
		return strconv.FormatInt(t.Unix(), 10)
	default:
		return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
	}
}

// eventRow converts one Docker event, reporting false for an event the feed
// does not show: a type it does not keep, or noise.
func eventRow(m events.Message) (EventRow, bool) {
	typ, action := string(m.Type), string(m.Action)
	if !isFeedType(typ) || isNoise(action) {
		return EventRow{}, false
	}
	nano := m.TimeNano
	if nano == 0 {
		nano = m.Time * int64(time.Second)
	}
	row := EventRow{
		ID:       eventID(nano, typ, action, m.Actor.ID),
		Time:     time.Unix(0, nano).UTC(),
		TimeNano: nano,
		Type:     typ,
		Scope:    m.Scope,
		Action:   action,
		ActorID:  m.Actor.ID,
		// Never nil, so a JSON consumer always sees an object.
		Attributes: map[string]string{},
	}
	if isInvalidateOnly(action) {
		// Nothing but the fact that this container changed leaves the server: the
		// attributes of a health_status event can name the check command.
		row.InvalidateOnly = true
		return row, true
	}
	for _, k := range keptAttributes {
		if v, ok := m.Actor.Attributes[k]; ok {
			row.Attributes[k] = v
		}
	}
	row.Name = row.Attributes["name"]
	if row.Name == "" && typ == "volume" {
		row.Name = m.Actor.ID
	}
	return row, true
}

func isFeedType(t string) bool {
	for _, k := range eventTypes {
		if t == k {
			return true
		}
	}
	return false
}

func isInvalidateOnly(action string) bool {
	for _, p := range invalidateOnlyActionPrefixes {
		if strings.HasPrefix(action, p) {
			return true
		}
	}
	return false
}

func isNoise(action string) bool {
	if noiseActions[action] {
		return true
	}
	for _, p := range noiseActionPrefixes {
		if strings.HasPrefix(action, p) {
			return true
		}
	}
	return false
}

// eventID hashes the fields that identify an event. Docker has no event ID, and
// two events of the same type, action and actor in the same nanosecond are one
// event for the feed's purposes.
func eventID(nano int64, typ, action, actor string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(nano, 10) + "\x00" + typ + "\x00" + action + "\x00" + actor))
	return hex.EncodeToString(sum[:12])
}
