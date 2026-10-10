// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package shells holds the browser shells that are open (OBS-03): an in-memory
// registry of sessions, and the bridge between a WebSocket and one interactive
// process in a container.
//
// Nothing here logs or stores what is typed into a shell or printed by it. The
// registry is memory only: a restart ends every session, and a session cannot be
// re-attached once its WebSocket has closed (D-30). Closing the connection closes
// the TTY attach and nothing more, so a process started from the shell may keep
// running in the container; that is an accepted limitation, recorded in the
// documentation.
package shells

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
)

// PendingTTL is how long a session may wait for the browser to attach to it.
const PendingTTL = 30 * time.Second

// ErrNotFound is the one answer to every claim that cannot be honoured: the session
// is unknown, belongs to another user or another sign-in, was claimed already, or
// has expired. It never says which, so a caller learns nothing about sessions that
// are not theirs.
var ErrNotFound = errors.New("shells: no such session")

// State is where a session is in its life.
type State int

const (
	// StatePending is a session whose exec exists and whose browser has not
	// attached yet.
	StatePending State = iota
	// StateAttached is a session a browser has claimed.
	StateAttached
	// StateClosed is a session that has ended; it is no longer in the registry.
	StateClosed
)

// Exec is the interactive process of a session. *dockerx.ExecSession is the real
// one; the interface lets the registry and the bridge be tested without a daemon.
type Exec interface {
	io.ReadWriteCloser
	Resize(ctx context.Context, cols, rows uint) error
	Wait(ctx context.Context) (dockerx.ExecState, error)
	Inspect(ctx context.Context) (dockerx.ExecState, error)
}

var _ Exec = (*dockerx.ExecSession)(nil)

// Clock is the time source of the registry: the current time and a timer. Tests
// replace it with one they advance by hand.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f once after d on its own goroutine. The returned function
	// stops the timer and reports whether it did so before f ran.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }
func (wallClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Session is one browser shell. The exported fields are set by the creator before
// Create and never change; the rest is the registry's.
type Session struct {
	// ID is made by Create: 32 URL-safe characters from crypto/rand. It is the
	// capability the WebSocket claims, together with the user and the sign-in.
	ID string
	// UserID and AuthSession bind the session to the person and the sign-in
	// (the hash of the session cookie, hex) that opened it.
	UserID      string
	AuthSession string
	// Actor is who the audit rows name.
	Actor audit.Actor
	// IP and UserAgent are the caller's, for the audit row of the end.
	IP, UserAgent string

	EnvID, NodeID, Node        string
	ContainerID, ContainerName string
	Image                      string
	Command                    []string
	User, WorkingDir           string
	CreatedAt                  time.Time
	exec                       Exec
	opened                     atomic.Bool
	lastActivity               atomic.Int64
	mu                         sync.Mutex
	state                      State
	pendingStop                func() bool
	done                       chan struct{}
	endReason                  string
	result                     EndResult
	exitCode                   *int
	closeExec                  sync.Once
}

// EndResult is what a session's end reports to the audit log.
type EndResult struct {
	// Duration is the time from the creation of the session to its end.
	Duration time.Duration
	// ExitCode is the exit code of the process when it was seen to end.
	ExitCode *int
	// StillRunning is true when the process was not seen to end: the connection
	// was closed on it, and it may keep running in the container (D-30).
	StillRunning bool
}

// Info is a copy of a session's facts for lists.
type Info struct {
	ID, UserID                 string
	EnvID, NodeID, Node        string
	ContainerID, ContainerName string
	Image                      string
	User, WorkingDir           string
	State                      State
	CreatedAt, LastActivity    time.Time
}

// Filter narrows List. An empty field matches everything.
type Filter struct {
	UserID, EnvID, ContainerID string
}

// Exec returns the session's process.
func (s *Session) Exec() Exec { return s.exec }

// Done is closed when the session has ended, for whatever reason.
func (s *Session) Done() <-chan struct{} { return s.done }

// Reason is why the session ended, and "" while it has not.
func (s *Session) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endReason
}

// Result is the end's report; it is the zero value while the session has not ended.
func (s *Session) Result() EndResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// NoteExit records that the process ended with code. The bridge calls it once it
// has seen the end.
func (s *Session) NoteExit(code int) {
	s.mu.Lock()
	s.exitCode = &code
	s.mu.Unlock()
}

// MarkOpened records that the opening of the session is on record in the audit log.
// Only a session that is can have its end audited: a closed row without an opened
// one would say a shell ended that the log never said began.
func (s *Session) MarkOpened() { s.opened.Store(true) }

// Opened reports whether MarkOpened was called.
func (s *Session) Opened() bool { return s.opened.Load() }

// Touch records activity (input or output now), for the idle timer.
func (s *Session) Touch(now time.Time) { s.lastActivity.Store(now.UnixNano()) }

// Registry is the set of open sessions. It is safe for concurrent use.
type Registry struct {
	clock Clock

	mu     sync.Mutex
	byID   map[string]*Session
	closed bool
	onEnd  func(*Session, string, EndResult)
}

// NewRegistry returns an empty registry. A nil clock is the wall clock.
func NewRegistry(clock Clock) *Registry {
	if clock == nil {
		clock = wallClock{}
	}
	return &Registry{clock: clock, byID: map[string]*Session{}}
}

// Now is the registry's clock reading.
func (r *Registry) Now() time.Time { return r.clock.Now() }

// OnEnd sets the function called once for every session that ends, after it has
// left the registry and its process has been closed: the place the end is audited.
// It is called on the goroutine that ended the session, with no lock held.
func (r *Registry) OnEnd(f func(s *Session, reason string, res EndResult)) {
	r.mu.Lock()
	r.onEnd = f
	r.mu.Unlock()
}

// Create registers s as a pending session around exec and starts its 30 s clock:
// a session the browser has not attached to by then is ended with the reason
// "expired" and its process closed. It fills in s.ID, s.CreatedAt and the rest of
// the runtime state, and returns s. After CloseAll it returns nil and closes exec,
// since the server is stopping.
func (r *Registry) Create(s *Session, exec Exec, pendingTTL time.Duration) *Session {
	if pendingTTL <= 0 {
		pendingTTL = PendingTTL
	}
	s.exec = exec
	s.done = make(chan struct{})
	s.state = StatePending
	s.CreatedAt = r.clock.Now()
	s.Touch(s.CreatedAt)

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = exec.Close()
		return nil
	}
	for {
		s.ID = newID()
		if _, taken := r.byID[s.ID]; !taken {
			break
		}
	}
	r.byID[s.ID] = s
	id := s.ID
	s.pendingStop = r.clock.AfterFunc(pendingTTL, func() { r.expire(id) })
	r.mu.Unlock()
	return s
}

// newID makes a session ID.
func newID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("shells: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// expire ends a session that is still pending.
func (r *Registry) expire(id string) {
	r.mu.Lock()
	s := r.byID[id]
	pending := s != nil && s.state == StatePending
	r.mu.Unlock()
	if pending {
		r.End(id, "expired")
	}
}

// Check reports whether Claim would succeed for the same caller, and changes
// nothing: the WebSocket handler asks before it upgrades, so a caller that is
// refused gets a plain 404, and a request refused for its origin does not use up
// the session. envID is the environment of the request; a session belongs to one.
func (r *Registry) Check(id, envID, userID, authSession string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil || s.EnvID != envID || s.UserID != userID || s.AuthSession != authSession || s.state != StatePending {
		return ErrNotFound
	}
	return nil
}

// Claim hands the pending session to the browser that is attaching, once. It is
// ErrNotFound for an unknown ID, a session of another user or another sign-in, a
// session that is not pending any more, and one past its time: the same answer for
// all, so nothing is learned about a session that is not the caller's.
func (r *Registry) Claim(id, userID, authSession string) (*Session, error) {
	r.mu.Lock()
	s := r.byID[id]
	if s == nil || s.UserID != userID || s.AuthSession != authSession || s.state != StatePending {
		r.mu.Unlock()
		return nil, ErrNotFound
	}
	s.state = StateAttached
	stop := s.pendingStop
	s.pendingStop = nil
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
	s.Touch(r.clock.Now())
	return s, nil
}

// End ends the session once, for the first reason given: it leaves the registry,
// its process is closed, Done is closed and the OnEnd function is told. It reports
// false when the session was not there (already ended, or never existed), and then
// does nothing.
func (r *Registry) End(id, reason string) (EndResult, bool) {
	r.mu.Lock()
	s := r.byID[id]
	if s == nil {
		r.mu.Unlock()
		return EndResult{}, false
	}
	delete(r.byID, id)
	onEnd := r.onEnd
	stop := s.pendingStop
	s.pendingStop = nil
	r.mu.Unlock()
	if stop != nil {
		stop()
	}

	s.closeExec.Do(func() { _ = s.exec.Close() })
	now := r.clock.Now()
	s.mu.Lock()
	s.state = StateClosed
	s.endReason = reason
	s.result = EndResult{Duration: max(now.Sub(s.CreatedAt), 0), ExitCode: s.exitCode, StillRunning: s.exitCode == nil}
	res := s.result
	s.mu.Unlock()
	close(s.done)
	if onEnd != nil {
		onEnd(s, reason, res)
	}
	return res, true
}

// CloseAll ends every session, pending or attached, with reason, and refuses new
// ones: the server is stopping. http.Server.Shutdown does not track hijacked
// connections, so without this a WebSocket would outlive the shutdown.
func (r *Registry) CloseAll(reason string) int {
	r.mu.Lock()
	r.closed = true
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, ok := r.End(id, reason); ok {
			n++
		}
	}
	return n
}

// List returns a copy of the facts of the open sessions that match f, oldest first.
func (r *Registry) List(f Filter) []Info {
	r.mu.Lock()
	out := make([]Info, 0, len(r.byID))
	for _, s := range r.byID {
		if (f.UserID != "" && s.UserID != f.UserID) || (f.EnvID != "" && s.EnvID != f.EnvID) ||
			(f.ContainerID != "" && s.ContainerID != f.ContainerID) {
			continue
		}
		out = append(out, Info{
			ID: s.ID, UserID: s.UserID, EnvID: s.EnvID, NodeID: s.NodeID, Node: s.Node,
			ContainerID: s.ContainerID, ContainerName: s.ContainerName, Image: s.Image,
			User: s.User, WorkingDir: s.WorkingDir, State: s.state,
			CreatedAt: s.CreatedAt, LastActivity: time.Unix(0, s.lastActivity.Load()),
		})
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Info) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return compareStrings(a.ID, b.ID)
	})
	return out
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
