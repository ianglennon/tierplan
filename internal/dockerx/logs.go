// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// ErrLogsUnreadable is returned by ContainerLogs when the engine answers that it cannot
// read the container's log back: its logging driver (none, and some others) keeps no
// log the engine can show. It is the engine's own word, an HTTP 501; nothing is guessed
// from the driver's name, because a driver such as syslog can keep a readable copy too.
var ErrLogsUnreadable = errors.New("the engine cannot read this container's logs back")

// LogOptions say which part of a container's log to read.
type LogOptions struct {
	// Since and Until bound the lines by time; the engine reads RFC 3339 times,
	// Unix times (with a fraction) and durations such as 10m. Empty means no bound.
	Since, Until string
	// Tail is how many of the last lines to read, as a number, or "all" or empty for
	// every line.
	Tail string
	// Follow keeps the stream open for lines written from now on.
	Follow bool
}

// ContainerLogs opens the log of a container, stdout and stderr together, with the
// engine's timestamp in front of every line. It reports whether the container has a
// TTY: the stream of a TTY container is raw bytes, and any other is multiplexed
// (stdcopy). The caller reads the stream and closes it; closing ends a follow.
//
// The container is inspected first, because the engine does not say in the answer
// which of the two formats it chose. A container that does not exist is an error
// IsNotFound recognises. A log driver the engine cannot read back is
// ErrLogsUnreadable; anything else the engine refuses comes back as its error.
func (c *Client) ContainerLogs(ctx context.Context, id string, o LogOptions) (rc io.ReadCloser, tty bool, err error) {
	cli := c.cur()
	res, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("inspect container on %s: %w", c.t.Describe(), err)
	}
	tty = res.Container.Config != nil && res.Container.Config.Tty
	out, err := cli.ContainerLogs(ctx, res.Container.ID, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Timestamps: true,
		Since: o.Since, Until: o.Until, Tail: o.Tail, Follow: o.Follow,
	})
	if err != nil {
		if cerrdefs.IsNotImplemented(err) {
			return nil, tty, fmt.Errorf("read logs of a container on %s: %w: %w", c.t.Describe(), ErrLogsUnreadable, err)
		}
		return nil, tty, fmt.Errorf("read logs of a container on %s: %w", c.t.Describe(), err)
	}
	return out, tty, nil
}
