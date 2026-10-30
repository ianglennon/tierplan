// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/ianglennon/coaming/internal/config"
)

// The two ways Coaming restarts after a restore from the web UI
// (COAMING_RESTORE_RESTART).
const (
	// RestartExec replaces the process image with a new Coaming, so the container's
	// PID 1 survives and no restart policy is needed.
	RestartExec = config.RestoreRestartExec
	// RestartExit exits 0 after the swap and leaves the start to the container's
	// restart policy. It is the fallback for an image where exec does not work.
	RestartExit = config.RestoreRestartExit
)

// ErrRestart is what Run returns when a restore asked for a restart: the server
// has stopped serving and closed its database, and still holds the data-directory
// lock. errors.As to *RestartError for the request, then call Restart.
var ErrRestart = errors.New("coaming: restart requested")

// RestartRequest is what a restore hands the process to finish after the server
// has stopped.
type RestartRequest struct {
	// Swap replaces the database while nothing has it open and nothing else can
	// take the data-directory lock. A nil Swap means there is nothing to do.
	Swap func(context.Context) error
}

// RestartError carries the request out of Run. errors.Is(err, ErrRestart) is true.
type RestartError struct{ Request RestartRequest }

func (e *RestartError) Error() string { return ErrRestart.Error() }

// Is makes errors.Is(err, ErrRestart) true.
func (e *RestartError) Is(target error) bool { return target == ErrRestart }

// restarter holds the one restart a server can be asked for.
type restarter struct {
	mu   sync.Mutex
	req  *RestartRequest
	done chan struct{}
}

func newRestarter() *restarter { return &restarter{done: make(chan struct{})} }

// request accepts the first request and refuses any later one.
func (r *restarter) request(req RestartRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.req != nil {
		return false
	}
	r.req = &req
	close(r.done)
	return true
}

// requested is closed once a restart has been requested.
func (r *restarter) requested() <-chan struct{} { return r.done }

// take returns the request, if there is one.
func (r *restarter) take() (RestartRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.req == nil {
		return RestartRequest{}, false
	}
	return *r.req, true
}

// RequestRestart asks Run to stop serving, close the database (keeping the
// data-directory lock) and return a *RestartError carrying req. Only the first
// request is accepted; it reports false for a later one.
func (s *Server) RequestRestart(req RestartRequest) bool { return s.restart.request(req) }

// execImage replaces the process image. Tests replace it; nothing else does.
var execImage = syscall.Exec

// Restart finishes a restore after Run returned ErrRestart. It runs the swap, then
// in mode "exec" replaces the process with a new Coaming (same arguments and
// environment, same PID), which opens the restored database from scratch, so no
// part of the old state survives. The data-directory lock is on a close-on-exec
// descriptor, so it is held until that moment and released by it. In mode "exit"
// it returns nil and the caller exits 0, leaving the start to a restart policy.
func Restart(mode string, req RestartRequest) error {
	if req.Swap != nil {
		// The process is going away either way, so a cancelled signal context must
		// not stop the swap half way.
		if err := req.Swap(context.Background()); err != nil {
			return fmt.Errorf("swap the restored database in: %w", err)
		}
	}
	if mode == RestartExit {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the Coaming executable to restart: %w", err)
	}
	if err := execImage(exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("restart Coaming (COAMING_RESTORE_RESTART=exit exits instead): %w", err)
	}
	return nil // not reached: a successful exec does not return
}
