// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"fmt"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// ContainerVerb is one thing that can be done to a running or stopped container.
// Each has its own typed call; there is no way to send any other request to the
// engine from here (P1 D-44).
type ContainerVerb string

// The container verbs of the actions endpoint (D-06). Remove has its own method,
// because it takes different options and removes the container.
const (
	VerbStart   ContainerVerb = "start"
	VerbStop    ContainerVerb = "stop"
	VerbRestart ContainerVerb = "restart"
	VerbKill    ContainerVerb = "kill"
	VerbPause   ContainerVerb = "pause"
	VerbUnpause ContainerVerb = "unpause"
)

// ActionOptions tune a stop, restart or kill. The zero value uses the container's
// own settings: its stop timeout and stop signal for a stop or restart, SIGKILL
// for a kill (D-09). Start, pause and unpause ignore both.
type ActionOptions struct {
	// Timeout is how many seconds a stop or restart waits before it kills the
	// container; nil means the container's own StopTimeout.
	Timeout *int
	// Signal is the signal a stop, restart or kill sends; empty means the
	// container's own StopSignal (or SIGKILL for a kill).
	Signal string
}

// ContainerAction does verb to the container id once. It never retries: a write
// whose answer was lost may or may not have happened (P2 D-42). Starting a
// container that already runs, or stopping one that already stopped, is not an
// error: the engine answers 304 and the client treats it as success.
func (c *Client) ContainerAction(ctx context.Context, id string, verb ContainerVerb, o ActionOptions) error {
	cli := c.cur()
	var err error
	switch verb {
	case VerbStart:
		_, err = cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	case VerbStop:
		_, err = cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: o.Timeout, Signal: o.Signal})
	case VerbRestart:
		_, err = cli.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: o.Timeout, Signal: o.Signal})
	case VerbKill:
		_, err = cli.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: o.Signal})
	case VerbPause:
		_, err = cli.ContainerPause(ctx, id, client.ContainerPauseOptions{})
	case VerbUnpause:
		_, err = cli.ContainerUnpause(ctx, id, client.ContainerUnpauseOptions{})
	default:
		return fmt.Errorf("%s container on %s: unknown action", verb, c.t.Describe())
	}
	if err != nil {
		return fmt.Errorf("%s container on %s: %w", verb, c.t.Describe(), err)
	}
	return nil
}

// RemoveOptions are the two choices of a container remove. The engine's third,
// removing links, is never set.
type RemoveOptions struct {
	// Force removes a running container (the engine kills it first).
	Force bool
	// Volumes also removes the container's anonymous volumes. Named volumes stay:
	// the engine only removes volumes the container created without a name.
	Volumes bool
}

// ContainerRemove removes the container id once and never retries. A container that
// runs is a conflict unless o.Force is set.
func (c *Client) ContainerRemove(ctx context.Context, id string, o RemoveOptions) error {
	_, err := c.cur().ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: o.Force, RemoveVolumes: o.Volumes})
	if err != nil {
		return fmt.Errorf("remove container on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// ContainerOwnership is what the server needs to know about a container before it
// acts on it: whose it is, and what it is doing now. It is read fresh from the
// engine, never from a cached list, because the list can be seconds old and the
// own-container rule must not depend on it (D-11).
type ContainerOwnership struct {
	ID   string
	Name string
	// Labels are all the labels of the container. They stay inside the server: the
	// caller reads the few it decides on and never puts them in a response.
	Labels  map[string]string
	Running bool
	Tty     bool
	// Image is the image reference the container was created with (Config.Image), for
	// example nginx:1.27. It is what a remembered shell choice is keyed by.
	Image string
	// StopTimeout is the container's own stop timeout in seconds, nil when it sets
	// none (the engine then waits 10 s); StopSignal is its own stop signal, empty for
	// SIGTERM. A stop deadline is derived from them (D-09).
	StopTimeout *int
	StopSignal  string
}

// ContainerOwnership inspects one container and returns the facts above.
func (c *Client) ContainerOwnership(ctx context.Context, id string) (ContainerOwnership, error) {
	res, err := c.cur().ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerOwnership{}, fmt.Errorf("inspect container on %s: %w", c.t.Describe(), err)
	}
	insp := res.Container
	out := ContainerOwnership{ID: insp.ID, Name: strings.TrimPrefix(insp.Name, "/"), Labels: map[string]string{}}
	if insp.Config != nil {
		for k, v := range insp.Config.Labels {
			out.Labels[k] = v
		}
		out.Tty = insp.Config.Tty
		out.Image = insp.Config.Image
		out.StopTimeout = insp.Config.StopTimeout
		out.StopSignal = insp.Config.StopSignal
	}
	if insp.State != nil {
		out.Running = insp.State.Running
	}
	return out, nil
}

// IsNotFound reports whether err says the engine has no such object.
func IsNotFound(err error) bool { return cerrdefs.IsNotFound(err) }

// IsConflict reports whether err says the object is in a state that does not
// allow the call, for example removing a container that is running.
func IsConflict(err error) bool { return cerrdefs.IsConflict(err) }
