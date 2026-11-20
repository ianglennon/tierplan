// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package traefikproc runs Traefik as a child of the supervisor, which is PID 1 in the image (D-20,
// D-39). One reaper owns every wait: nothing here calls cmd.Wait, because a second waiter would race the
// reaper for the child's status and could signal a pid that has been reused.
package traefikproc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Binary is where the image puts Traefik.
const Binary = "/usr/local/bin/traefik"

// The restart delay bounds (D-20).
const (
	RestartMin = time.Second
	RestartMax = 30 * time.Second
)

const minimalPath = "PATH=/usr/local/bin:/usr/bin:/bin"

// DefaultArgs is Traefik's command line: its static configuration, which the supervisor writes (D-20).
func DefaultArgs() []string {
	return []string{"--configFile=" + ingressbundle.NodeRoot + "/static/traefik.yml"}
}

// Env is the whole environment Traefik gets. The supervisor's own environment may hold the join token, and
// Traefik reads every TRAEFIK_* variable as configuration, so it is handed PATH and nothing else (D-39).
func Env() []string { return []string{minimalPath} }

// Clock is the time source for the restart delay and the stop window.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

type process interface {
	Pid() int
	Signal(os.Signal) error
}

type starter func(binary string, args, env []string, stdout, stderr io.Writer) (process, error)

func realStart(string, []string, []string, io.Writer, io.Writer) (process, error) {
	return nil, errors.New("not implemented")
}

// Options configure New.
type Options struct {
	Binary         string
	Args, Env      []string
	Stdout, Stderr io.Writer
	Clock          Clock
	Log            *slog.Logger
	Rand           func() float64

	start starter
}

// Proc is the supervised Traefik child.
type Proc struct{ o Options }

// New returns a Proc.
func New(o Options) *Proc { return &Proc{o: o} }

// Start starts the child.
func (p *Proc) Start(context.Context) error { return errors.New("not implemented") }

// Stop stops the child.
func (p *Proc) Stop() error { return errors.New("not implemented") }

// Running reports whether the child is running.
func (p *Proc) Running() bool { return false }

// PID is the child's pid.
func (p *Proc) PID() int { return 0 }
