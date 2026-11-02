// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"
)

// SystemTime reads the engine's own clock from its info: the time the engine says it is now, in
// UTC. The zero time with a nil error means the engine did not report one (or reported one that is
// not a time), which a caller treats as unknown. It is read-only.
func (c *Client) SystemTime(ctx context.Context) (time.Time, error) {
	res, err := c.cur().Info(ctx, client.InfoOptions{})
	if err != nil {
		return time.Time{}, fmt.Errorf("read the engine clock of %s: %w", c.t.Describe(), err)
	}
	if res.Info.SystemTime == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, res.Info.SystemTime)
	if err != nil {
		return time.Time{}, nil //nolint:nilerr // an engine time that cannot be read is an unknown time, which the caller judges as no skew was found
	}
	return at.UTC(), nil
}
