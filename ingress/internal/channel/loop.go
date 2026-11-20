// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package channel

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/ingress/internal/backoff"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

const (
	// BackoffMin is the first reconnect delay's step (D-19).
	BackoffMin = time.Second
	// BackoffMax caps the reconnect delay (D-19). A refusal an administrator has to
	// act on waits exactly this long, so the node keeps knocking gently (D-35).
	BackoffMax = 60 * time.Second
	// MaxRetryAfter caps a server's retry-after hint (D-19): the hint replaces the
	// computed delay, but a server cannot park a node for longer than this.
	MaxRetryAfter = 5 * time.Minute
	// RevokedLogInterval is how often a revoked node repeats its error line, so an
	// operator who finds the node frozen can see why (D-35).
	RevokedLogInterval = 5 * time.Minute
)

// revokedMessage is the line a revoked node logs, now and every RevokedLogInterval.
const revokedMessage = "ingress node revoked: no longer connecting to TierPlan; serving the cached bundle until the service is removed"

// Clock times the loop's waits. A test passes one it controls.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

// LoopOptions is how Run keeps the channel up.
type LoopOptions struct {
	// Dial makes one connection attempt. The supervisor builds it over Dial with
	// fresh credentials each call, so a node that enrolled in one attempt connects
	// with its key in the next.
	Dial func(ctx context.Context) (*Conn, error)
	// Session is how each connection is served.
	Session SessionOptions
	// Clock times the waits. Nil means the real clock.
	Clock Clock
	// Rand is the jitter source of backoff.Delay. Nil means the process's.
	Rand func() float64
	// Log receives the loop's lines. Nil discards them.
	Log *slog.Logger
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// administratorRefusals are the refusals a retry cannot cure by itself: someone
// has to fix a token, a key, a duplicate enrolment or a transport (D-35). The
// node still retries, at the cap, in case they do.
var administratorRefusals = map[string]bool{
	ingresswire.RefuseAuthRejected:        true,
	ingresswire.RefuseWrongTokenKind:      true,
	ingresswire.RefuseNodeAlreadyEnrolled: true,
	ingresswire.RefuseInsecureTransport:   true,
}

// Run keeps a session with TierPlan going and returns only when ctx ends, with
// nil (D-19). An ordinary failure waits a jittered, doubling delay between
// BackoffMin and BackoffMax; a session that reached the welcome resets the count.
// A refusal that carries a retry-after hint waits exactly min(hint,
// MaxRetryAfter) instead. A refusal an administrator must act on, and a welcome
// from an instance other than the pinned one, wait BackoffMax and say so in the
// log. A revoked node (a refusal revoked or node_revoked, or a close with reason
// revoked) never dials again: it logs one error line now and again every
// RevokedLogInterval until ctx ends (D-35).
//
// While Run backs off, or after it stops dialing, the supervisor keeps serving
// the bundle it has: the loop never touches Traefik or readiness (PRX-03).
func Run(ctx context.Context, o LoopOptions) error {
	logger := o.Log
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	clock := o.Clock
	if clock == nil {
		clock = realClock{}
	}

	failures := 0
	for ctx.Err() == nil {
		conn, err := o.Dial(ctx)
		if err == nil {
			if conn == nil {
				err = errors.New("ingress channel: the dial made no connection")
			} else {
				failures = 0
				err = conn.Serve(ctx, o.Session)
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		failures++

		if isRevoked(err) {
			return holdRevoked(ctx, clock, logger)
		}
		delay := decideDelay(err, failures, o.Rand, logger)
		select {
		case <-ctx.Done():
			return nil
		case <-clock.After(delay):
		}
	}
	return nil
}

// isRevoked is whether err says TierPlan has revoked this node.
func isRevoked(err error) bool {
	var re *RefusedError
	if errors.As(err, &re) {
		return re.Code == ingresswire.RefuseRevoked || re.Code == ingresswire.RefuseNodeRevoked
	}
	var ce *ClosedError
	return errors.As(err, &ce) && ce.Reason == ingresswire.CloseRevoked
}

// decideDelay logs the failure once and returns how long to wait before the next
// attempt. A server hint replaces the computed delay, capped at MaxRetryAfter.
func decideDelay(err error, failures int, rnd func() float64, logger *slog.Logger) time.Duration {
	var re *RefusedError
	var hint time.Duration
	delay := backoff.Delay(failures, BackoffMin, BackoffMax, rnd)

	switch {
	case errors.As(err, &re):
		hint = re.RetryAfter
		if administratorRefusals[re.Code] {
			delay = BackoffMax
			logger.Error("ingress channel: refused by TierPlan; an administrator must act (the node is retrying at the slowest pace)",
				"code", re.Code, "detail", re.Detail, "delay", wait(delay, hint))
		} else {
			logger.Warn("ingress channel: refused by TierPlan; retrying",
				"code", re.Code, "detail", re.Detail, "delay", wait(delay, hint))
		}
	case errors.Is(err, ingresswire.ErrInstanceMismatch):
		delay = BackoffMax
		logger.Error("ingress channel: the server is not the TierPlan this node pinned; the address may point at a misdirected TierPlan, and an administrator must act",
			"delay", delay)
	default:
		logger.Warn("ingress channel: the session ended; reconnecting", "error", err, "delay", delay)
	}
	return wait(delay, hint)
}

// wait is the delay actually waited: the server's hint, capped, when it gave one.
func wait(computed, hint time.Duration) time.Duration {
	if hint > 0 {
		return min(hint, MaxRetryAfter)
	}
	return computed
}

// holdRevoked is the revoked state: no more dialing, the same error line every
// RevokedLogInterval, and a return only when ctx ends.
func holdRevoked(ctx context.Context, clock Clock, logger *slog.Logger) error {
	for {
		logger.Error(revokedMessage)
		select {
		case <-ctx.Done():
			return nil
		case <-clock.After(RevokedLogInterval):
		}
	}
}
