// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package shells

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The shape of the socket (RESEARCH Pattern 5). Binary frames are the raw terminal
// bytes both ways. Text frames are JSON: {"type":"resize","cols":C,"rows":R} from
// the browser, and from the server {"type":"exit","code":N} when the process ends
// and {"type":"closed","reason":R} when the session is ended for another reason.
const (
	// ReadLimit is the largest frame the browser may send.
	ReadLimit = 64 << 10
	// copyBuffer is the size of the buffer output crosses in. It is the only buffer
	// between the container and the browser, so a browser that stops reading blocks
	// the shell's output instead of growing memory.
	copyBuffer = 32 << 10
	// DefaultWriteTimeout is how long one write to the browser may take.
	DefaultWriteTimeout = 30 * time.Second
	// DefaultPingInterval is how often the server pings an attached browser.
	DefaultPingInterval = 20 * time.Second

	// MaxTerminal is the largest size a resize may ask for.
	MaxTerminal = 1000

	// DefaultTickInterval is how often the idle limit and the caller's access are
	// checked (D-29, P3 D-10): a lost permission ends a shell within about this long.
	DefaultTickInterval = 5 * time.Second
	// IdleWarning is how long before the idle limit the browser is warned.
	IdleWarning = time.Minute

	resizeTimeout  = 5 * time.Second
	waitTimeout    = 10 * time.Second
	inspectTimeout = time.Second
)

// The reasons Bridge returns.
const (
	ReasonExited       = "exited"
	ReasonDisconnected = "disconnected"
	ReasonNoShell      = "no_shell"
	ReasonEnded        = "ended"
	ReasonError        = "error"
	// ReasonIdle is a shell closed for sitting unused for the instance's idle limit.
	ReasonIdle = "idle"
	// ReasonAccessLost is a shell closed because its user no longer holds the right
	// to have it: the permission, the environment, or the sign-in is gone.
	ReasonAccessLost = "access_lost"
	// ReasonEndedByAdmin and ReasonServerShutdown are given to Registry.End and
	// CloseAll by whoever ends the session from outside.
	ReasonEndedByAdmin = "ended_by_admin"
	// ReasonEndedByUser is a shell its own user ended from the list of their shells.
	ReasonEndedByUser    = "ended_by_user"
	ReasonServerShutdown = "server_shutdown"
)

// Control is a text frame.
type Control struct {
	Type    string `json:"type"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Code    *int   `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
	// Minutes is the idle limit, sent with the closed frame whose reason is idle, so
	// the page can say how long the shell was left alone.
	Minutes int `json:"minutes,omitempty"`
}

// Controls tune a bridge. The zero value is the production setting.
type Controls struct {
	// WriteTimeout bounds each write to the browser; a browser that takes longer
	// is gone. Zero means 30 seconds.
	WriteTimeout time.Duration
	// PingInterval is how often the browser is pinged. Zero means 20 seconds.
	PingInterval time.Duration
	// Idle is the idle limit, read on every tick so a change applies to a shell that
	// is open. Nil or a limit of zero or less means shells never go idle.
	Idle func() time.Duration
	// StillAllowed says whether the shell's user may still have it; false ends the
	// shell with the reason access_lost. Nil means always.
	StillAllowed func() bool
	// Clock is where activity and idleness are measured. Nil is the wall clock. It
	// must be the clock the session's registry runs on, which stamps its creation.
	Clock Clock
	// TickInterval is how often Idle and StillAllowed are consulted. Zero means 5
	// seconds.
	TickInterval time.Duration
}

func (c Controls) withDefaults() Controls {
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.PingInterval <= 0 {
		c.PingInterval = DefaultPingInterval
	}
	if c.Clock == nil {
		c.Clock = wallClock{}
	}
	if c.TickInterval <= 0 {
		c.TickInterval = DefaultTickInterval
	}
	return c
}

// Bridge joins the WebSocket and the session's process until either ends, and
// returns why: "exited" when the process ended (the browser was sent its exit
// code), "disconnected" when the browser went away or stopped reading, "no_shell"
// when the engine could not start the command, "ended" when ctx was cancelled, or
// the registry's reason when the session was ended from outside (the browser is
// then told). The process is closed and every goroutine has stopped when it
// returns. It never reads, logs or stores the bytes it carries.
//
// One goroutine copies output to the browser, one copies input to the process and
// one pings; the first to finish decides the reason, and the others are stopped
// through one cancel.
func Bridge(ctx context.Context, conn *websocket.Conn, s *Session, ctl Controls) string {
	ctl = ctl.withDefaults()
	conn.SetReadLimit(ReadLimit)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	first := make(chan string, 1)
	finish := func(reason string) {
		if reason == "" {
			return
		}
		select {
		case first <- reason:
		default:
		}
	}
	// stopping is set before the process is closed, so the output goroutine, whose
	// read fails the moment that happens, knows it is being stopped and sends nothing.
	var stopping atomic.Bool
	var wg sync.WaitGroup
	for _, run := range []func(context.Context) string{
		func(c context.Context) string { return pumpOutput(c, conn, s, ctl, &stopping) },
		func(c context.Context) string { return pumpInput(c, conn, s, ctl) },
		func(c context.Context) string { return pinger(c, conn, ctl) },
		func(c context.Context) string { return watcher(c, conn, s, ctl) },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finish(run(ctx))
		}()
	}

	var reason string
	select {
	case reason = <-first:
	case <-s.Done():
		reason = s.Reason()
		if reason == "" {
			reason = ReasonEnded
		}
		writeControl(ctx, conn, ctl, Control{Type: "closed", Reason: reason})
	case <-ctx.Done():
		reason = ReasonEnded
		if r := s.Reason(); r != "" {
			reason = r
		}
		writeControl(context.WithoutCancel(ctx), conn, ctl, Control{Type: "closed", Reason: reason})
	}

	stopping.Store(true)
	if reason == ReasonDisconnected {
		_ = conn.CloseNow()
	} else {
		// The close handshake needs the input goroutine to go on reading, to see
		// the browser's own close frame; it waits for it up to five seconds.
		_ = conn.Close(closeStatus(reason), "")
	}
	cancel()
	s.closeExec.Do(func() { _ = s.exec.Close() })
	wg.Wait()
	_ = conn.CloseNow()
	if reason != ReasonExited && reason != ReasonNoShell {
		noteState(s)
	}
	return reason
}

func closeStatus(reason string) websocket.StatusCode {
	switch reason {
	case ReasonExited, ReasonNoShell:
		return websocket.StatusNormalClosure
	}
	return websocket.StatusGoingAway
}

// noteState asks the engine whether the process has ended, briefly, so the end of
// a session that was cut short can say whether the process still runs. A shell that
// was just hung up on may need a moment to go, so "still running" can be a little
// pessimistic; it is never optimistic.
func noteState(s *Session) {
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()
	st, err := s.exec.Inspect(ctx)
	if err == nil && !st.Running {
		s.NoteExit(st.ExitCode)
	}
}

// writeControl sends a text frame, bounded by the write timeout.
func writeControl(ctx context.Context, conn *websocket.Conn, ctl Controls, c Control) bool {
	b, err := json.Marshal(c)
	if err != nil {
		return false
	}
	wctx, cancel := context.WithTimeout(ctx, ctl.WriteTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b) == nil
}

// pumpOutput copies the process's output to the browser in binary frames. When the
// output ends it waits for the process's state and sends the exit code.
func pumpOutput(ctx context.Context, conn *websocket.Conn, s *Session, ctl Controls, stopping *atomic.Bool) string {
	buf := make([]byte, copyBuffer)
	for {
		n, err := s.exec.Read(buf)
		if n > 0 {
			s.Touch(ctl.Clock.Now())
			wctx, cancel := context.WithTimeout(ctx, ctl.WriteTimeout)
			werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
			cancel()
			if werr != nil {
				return ReasonDisconnected
			}
		}
		if err != nil {
			break
		}
	}
	// The output ended: the process did, or the session is being stopped from
	// outside, which has its own say.
	if ctx.Err() != nil || stopping.Load() {
		return ""
	}
	select {
	case <-s.Done():
		return ""
	default:
	}
	wctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	st, err := s.exec.Wait(wctx)
	switch {
	case errors.Is(err, dockerx.ErrNoShell):
		s.NoteExit(st.ExitCode)
		writeControl(ctx, conn, ctl, Control{Type: "closed", Reason: ReasonNoShell})
		return ReasonNoShell
	case err != nil:
		if ctx.Err() != nil || stopping.Load() {
			return ""
		}
		writeControl(ctx, conn, ctl, Control{Type: "closed", Reason: ReasonError})
		return ReasonError
	}
	s.NoteExit(st.ExitCode)
	code := st.ExitCode
	writeControl(ctx, conn, ctl, Control{Type: "exit", Code: &code})
	return ReasonExited
}

// pumpInput copies the browser's binary frames to the process and acts on its
// resize frames. Any other text frame, and a resize outside 1 to 1000 in either
// direction, is ignored.
func pumpInput(ctx context.Context, conn *websocket.Conn, s *Session, ctl Controls) string {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ""
			}
			return ReasonDisconnected
		}
		switch typ {
		case websocket.MessageBinary:
			s.Touch(ctl.Clock.Now())
			if _, err := s.exec.Write(data); err != nil {
				// The process is gone or going; the output side says how it ended.
				<-ctx.Done()
				return ""
			}
		case websocket.MessageText:
			var c Control
			if json.Unmarshal(data, &c) != nil || c.Type != "resize" {
				continue
			}
			if c.Cols < 1 || c.Cols > MaxTerminal || c.Rows < 1 || c.Rows > MaxTerminal {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, resizeTimeout)
			_ = s.exec.Resize(rctx, uint(c.Cols), uint(c.Rows))
			cancel()
		}
	}
}

// pinger pings the browser on an interval; a ping that is not answered in the
// write timeout means the browser is gone. A ping needs the input goroutine to be
// reading, which it always is.
func pinger(ctx context.Context, conn *websocket.Conn, ctl Controls) string {
	t := time.NewTicker(ctl.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, ctl.WriteTimeout)
		err := conn.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ""
			}
			return ReasonDisconnected
		}
	}
}

// watcher checks on a tick that the shell's user may still have it and that the shell
// has not sat unused for the idle limit. A shell with no input or output for the limit
// minus a minute is sent {"type":"idle_warning","seconds":60} once for that stretch of
// silence; input or output starts the stretch afresh. At the limit it is closed with
// reason idle, and when the user's access is gone with reason access_lost; the
// closing frame is sent before the reason is returned. Both the limit and the access
// are read on every tick, never captured when the shell opened.
func watcher(ctx context.Context, conn *websocket.Conn, s *Session, ctl Controls) string {
	t := time.NewTicker(ctl.TickInterval)
	defer t.Stop()
	var warnedFor time.Time // the activity the last warning was sent for
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-t.C:
		}
		if ctl.StillAllowed != nil && !ctl.StillAllowed() {
			writeControl(ctx, conn, ctl, Control{Type: "closed", Reason: ReasonAccessLost})
			return ReasonAccessLost
		}
		if ctl.Idle == nil {
			continue
		}
		limit := ctl.Idle()
		if limit <= 0 {
			continue
		}
		last := s.LastActivity()
		quiet := ctl.Clock.Now().Sub(last)
		switch {
		case quiet >= limit:
			writeControl(ctx, conn, ctl, Control{Type: "closed", Reason: ReasonIdle, Minutes: int(limit / time.Minute)})
			return ReasonIdle
		case quiet >= limit-IdleWarning && !last.Equal(warnedFor):
			warnedFor = last
			writeControl(ctx, conn, ctl, Control{Type: "idle_warning", Seconds: int(IdleWarning / time.Second)})
		}
	}
}
