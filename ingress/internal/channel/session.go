// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// BundleHandler is the supervisor's answer to one bundle frame: it decodes the
// frame against the pinned instance ID, checks it, applies it and returns the
// bundle's ID with a short reason code when it did not apply the bundle. An empty
// reason means the bundle is live; a reason means it is not. frame is the message
// exactly as it came off the socket (D-12, D-15).
type BundleHandler func(ctx context.Context, frame []byte, instanceID string) (id, reason string)

// SessionOptions is how Serve runs one session.
type SessionOptions struct {
	// Handle receives every bundle frame. Required.
	Handle BundleHandler
	// Health returns whether Traefik answers and the ID of the bundle the node
	// runs now, at the moment of each health tick. Required.
	Health func(ctx context.Context) (traefikOK bool, appliedID string)
	// Ticker returns the channel that fires every d and the function that stops
	// it. Nil means a time.Ticker. A test passes a channel it fires itself.
	Ticker func(d time.Duration) (<-chan time.Time, func())
	// Log receives the session's debug lines. Nil discards them. The session does
	// not log bundle outcomes: the supervisor's handler logs "bundle applied" and
	// "bundle rejected", so each bundle produces exactly one line.
	Log *slog.Logger
}

// ClosedError is the error of a session the server ended with a close frame.
// Reason is the close reason text (ingresswire.Close*), for the reconnect loop:
// "revoked" is the one that ends the connecting for good (D-35).
type ClosedError struct {
	// Status is the WebSocket close status code.
	Status int
	// Reason is the close reason the server sent.
	Reason string
}

func (e *ClosedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("ingress channel: the server closed the session (status %d)", e.Status)
	}
	return fmt.Sprintf("ingress channel: the server closed the session (status %d): %s", e.Status, e.Reason)
}

// errSessionEnded is the cause the reader gives the health writer when it is done.
var errSessionEnded = errors.New("ingress channel: the session ended")

// Serve runs the session until it ends and returns why. A binary frame goes to
// o.Handle and the answer is sent as applied (no reason) or rejected (a reason,
// cut to ingresswire.MaxReasonLen). Every health interval, the welcome's
// healthMs or ingresswire.HealthInterval when it is zero, the node sends health
// with what o.Health returns at that tick (D-23). The node sends nothing else:
// hello was Dial's, and applied, rejected and health are the whole of the rest of
// its vocabulary (D-13).
//
// A text frame is a protocol error: Serve closes the session with
// ingresswire.CloseProtocolError and returns an error. A close from the server
// returns a *ClosedError. A cancelled ctx closes the session with
// ingresswire.CloseShutdown and returns ctx.Err(). Serve always closes the
// connection before it returns, and it stops the ticker.
//
// Serve does not log bundle outcomes: the supervisor's handler logs `bundle
// applied` and `bundle rejected`, so each bundle produces one line (23-13).
func (c *Conn) Serve(ctx context.Context, o SessionOptions) error {
	if o.Handle == nil || o.Health == nil {
		return errors.New("ingress channel: a session needs a bundle handler and a health function")
	}
	newTicker := o.Ticker
	if newTicker == nil {
		newTicker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	interval := healthInterval(c.welcome.HealthMs)

	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer func() { _ = c.ws.CloseNow() }()

	ticks, stop := newTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.writeHealth(sctx, ticks, o.Health, cancel)
	}()
	err := c.readFrames(ctx, sctx, o.Handle)
	cancel(errSessionEnded)
	<-done
	stop()
	if o.Log != nil {
		o.Log.Debug("ingress channel session ended", "error", err)
	}
	return err
}

const (
	// minHealthInterval is the shortest health interval a server can ask for: a node does not send health
	// every millisecond because a welcome said healthMs 1.
	minHealthInterval = time.Second
	// maxHealthInterval is the longest: a gap past this reads as a dead node to the server, and the cap also
	// keeps a huge healthMs from overflowing a Duration.
	maxHealthInterval = time.Hour
	maxHealthMs       = int(maxHealthInterval / time.Millisecond)
)

// healthInterval is the health interval for the welcome's healthMs: ingresswire.HealthInterval when the
// server gave none, otherwise the value kept between minHealthInterval and maxHealthInterval.
func healthInterval(healthMs int) time.Duration {
	if healthMs <= 0 {
		return ingresswire.HealthInterval
	}
	ms := min(healthMs, maxHealthMs)
	return max(time.Duration(ms)*time.Millisecond, minHealthInterval)
}

// readFrames is the session's reader: it returns the error that ends the session.
// parent is the caller's context and sctx the session's, which the health writer
// also ends when a write fails.
func (c *Conn) readFrames(parent, sctx context.Context, handle BundleHandler) error {
	for {
		typ, data, err := c.ws.Read(sctx)
		if err != nil {
			return c.readError(parent, sctx, err)
		}
		if typ != websocket.MessageBinary {
			_ = c.ws.Close(websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
			return errors.New("ingress channel: protocol error: the server sent a text message after the welcome")
		}
		id, reason := handle(sctx, data, c.instanceID)
		var answer any = ingresswire.Applied{T: ingresswire.TypeApplied, ID: id}
		if reason != "" {
			answer = ingresswire.Rejected{
				T: ingresswire.TypeRejected, ID: id, Reason: cutUTF8(reason, ingresswire.MaxReasonLen),
			}
		}
		if err := c.send(sctx, answer); err != nil {
			return c.readError(parent, sctx, err)
		}
	}
}

// readError classifies why a read or a write failed. A health write that failed
// ended the session before the reader saw the failure itself, so its error is
// the one that counts; a close from the server is a *ClosedError either way.
func (c *Conn) readError(parent, sctx context.Context, err error) error {
	if parent.Err() != nil {
		_ = c.ws.Close(websocket.StatusGoingAway, ingresswire.CloseShutdown)
		return parent.Err()
	}
	what := "the session failed"
	if cause := context.Cause(sctx); sctx.Err() != nil && cause != nil && !errors.Is(cause, context.Canceled) {
		err, what = cause, "send health"
	}
	if status := websocket.CloseStatus(err); status != -1 {
		var ce websocket.CloseError
		reason := ""
		if errors.As(err, &ce) {
			reason = ce.Reason
		}
		return &ClosedError{Status: int(status), Reason: reason}
	}
	return fmt.Errorf("ingress channel: %s: %w", what, err)
}

// writeHealth sends one health message per tick until ctx ends. A failed write
// ends the session through cancel, with the write's error as the cause.
func (c *Conn) writeHealth(ctx context.Context, ticks <-chan time.Time, health func(context.Context) (bool, string), cancel context.CancelCauseFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			ok, applied := health(ctx)
			if err := c.send(ctx, ingresswire.Health{T: ingresswire.TypeHealth, TraefikOK: ok, AppliedID: applied}); err != nil {
				cancel(err)
				return
			}
		}
	}
}

// send writes v as one JSON text message. Applied, rejected and health all go
// through it; coder/websocket allows concurrent writes.
func (c *Conn) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return errors.New("ingress channel: encode a message")
	}
	return c.ws.Write(ctx, websocket.MessageText, b)
}
