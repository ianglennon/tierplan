// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// ErrNoShell says the container has no such shell or executable: the engine
// could not start it (D-34). It is how the caller tells "pick another shell" from
// a failure of the connection.
var ErrNoShell = errors.New("the container has no such shell")

// Limits on a custom shell command (T-04-19).
const (
	maxCustomArgs    = 64
	maxCustomArgSize = 4096
	// maxTerminal is the largest rows or columns value a terminal is given; a
	// terminal this wide is already absurd, and the engine takes a 32 bit number.
	maxTerminal = 65535
	// tailSize is how much of the end of a session's output is kept to recognise
	// the runtime's "no such file" message.
	tailSize = 4096
	// waitPoll is how often Wait asks the engine whether the process has ended.
	waitPoll = 50 * time.Millisecond
)

// autoShell is the one command behind the "auto" shell (D-26): bash when the image
// has it, sh otherwise, in a single exec and so a single audit row.
const autoShell = "if [ -x /bin/bash ]; then exec /bin/bash; else exec /bin/sh; fi"

// ExecOptions describe the interactive process to start in a container.
type ExecOptions struct {
	// Cmd is the argv. It is never wrapped in a shell here; ShellCommand builds it.
	Cmd []string
	// User and WorkingDir are optional; empty means the container's own.
	User, WorkingDir string
	// Cols and Rows are the initial terminal size; zero lets the engine choose.
	Cols, Rows uint
}

// ExecState is what the engine says about the process of a session.
type ExecState struct {
	Running  bool
	ExitCode int
}

// ShellCommand returns the argv for the shell kind the user chose (D-26): "auto"
// (bash if the image has it, else sh), "bash", "sh", "ash" or "custom". A custom
// command is split into words the way a shell would read quotes and backslashes,
// but it is never given to sh -c, so nothing in it is expanded or run by a shell
// (T-04-19).
func ShellCommand(kind, custom string) ([]string, error) {
	switch kind {
	case "auto":
		return []string{"/bin/sh", "-c", autoShell}, nil
	case "bash":
		return []string{"/bin/bash"}, nil
	case "sh":
		return []string{"/bin/sh"}, nil
	case "ash":
		return []string{"/bin/ash"}, nil
	case "custom":
		return splitCommand(custom)
	default:
		return nil, fmt.Errorf("unknown shell %q", kind)
	}
}

// splitCommand splits s into arguments. Single quotes keep everything literal;
// inside double quotes a backslash escapes the next character; outside quotes a
// backslash escapes the next character and white space separates arguments. An
// unbalanced quote or a trailing backslash is an error.
func splitCommand(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inArg   bool
		single  bool
		double  bool
		escaped bool
	)
	flush := func() error {
		if !inArg {
			return nil
		}
		if cur.Len() > maxCustomArgSize {
			return fmt.Errorf("an argument is longer than %d bytes", maxCustomArgSize)
		}
		if len(args) >= maxCustomArgs {
			return fmt.Errorf("more than %d arguments", maxCustomArgs)
		}
		args = append(args, cur.String())
		cur.Reset()
		inArg = false
		return nil
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == 0 {
			return nil, errors.New("the command contains a NUL byte")
		}
		switch {
		case escaped:
			cur.WriteByte(ch)
			escaped = false
		case single:
			if ch == '\'' {
				single = false
			} else {
				cur.WriteByte(ch)
			}
		case double:
			switch ch {
			case '"':
				double = false
			case '\\':
				escaped = true
			default:
				cur.WriteByte(ch)
			}
		case ch == '\'':
			single, inArg = true, true
		case ch == '"':
			double, inArg = true, true
		case ch == '\\':
			escaped, inArg = true, true
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			cur.WriteByte(ch)
			inArg = true
		}
		if cur.Len() > maxCustomArgSize {
			return nil, fmt.Errorf("an argument is longer than %d bytes", maxCustomArgSize)
		}
	}
	if single || double {
		return nil, errors.New("the command has an unbalanced quote")
	}
	if escaped {
		return nil, errors.New("the command ends with a backslash")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, errors.New("the command is empty")
	}
	return args, nil
}

// ExecSession is one interactive TTY process in a container. Read and Write carry
// the raw terminal bytes both ways: with a TTY the engine does not multiplex, so
// there is nothing to demultiplex. The hijacked connection stays inside the
// session; callers see only this type.
type ExecSession struct {
	cli  *client.Client
	desc string
	id   string
	hr   client.HijackedResponse

	closeOnce sync.Once
	closeErr  error

	mu    sync.Mutex
	tail  []byte
	ended bool
}

// StartExec creates a TTY exec in the running container and attaches to it. It sets
// the TTY, the three attach flags, the console size, the user, the working
// directory and the command, and nothing else: no environment, no privileged mode,
// no detach keys (T-04-18). The agent refuses a request with any of those.
func (c *Client) StartExec(ctx context.Context, containerID string, o ExecOptions) (*ExecSession, error) {
	if containerID == "" {
		return nil, fmt.Errorf("start exec on %s: no container", c.t.Describe())
	}
	if len(o.Cmd) == 0 {
		return nil, fmt.Errorf("start exec on %s: no command", c.t.Describe())
	}
	if o.Cols > maxTerminal || o.Rows > maxTerminal {
		return nil, fmt.Errorf("start exec on %s: terminal size %dx%d is out of range", c.t.Describe(), o.Cols, o.Rows)
	}
	cli := c.cur()
	size := client.ConsoleSize{Height: o.Rows, Width: o.Cols}
	created, err := cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		User:         o.User,
		TTY:          true,
		ConsoleSize:  size,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   o.WorkingDir,
		Cmd:          o.Cmd,
	})
	if err != nil {
		return nil, c.execErr("create exec", err)
	}
	att, err := cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if err != nil {
		return nil, c.execErr("attach exec", err)
	}
	return &ExecSession{cli: cli, desc: c.t.Describe(), id: created.ID, hr: att.HijackedResponse}, nil
}

// execErr wraps an engine error in the house form, and says ErrNoShell when the
// engine refused to start a command it could not find.
func (c *Client) execErr(what string, err error) error {
	if looksLikeNoShell(err.Error()) {
		return fmt.Errorf("%s on %s: %w", what, c.t.Describe(), errors.Join(ErrNoShell, err))
	}
	return fmt.Errorf("%s on %s: %w", what, c.t.Describe(), err)
}

// looksLikeNoShell reports whether text is the runtime's message for a command that
// is not in the image.
func looksLikeNoShell(text string) bool {
	if !strings.Contains(text, "OCI runtime exec failed") && !strings.Contains(text, "OCI runtime create failed") {
		return false
	}
	return strings.Contains(text, "no such file or directory") || strings.Contains(text, "executable file not found")
}

// ID is the engine's ID of the exec.
func (s *ExecSession) ID() string { return s.id }

// Read reads terminal output. It returns an error when the process has ended and
// the output is drained, or after Close. The end of the output is kept so Wait can
// recognise a missing executable.
func (s *ExecSession) Read(p []byte) (int, error) {
	n, err := s.hr.Reader.Read(p)
	s.mu.Lock()
	if n > 0 {
		s.tail = append(s.tail, p[:n]...)
		if len(s.tail) > tailSize {
			s.tail = append(s.tail[:0], s.tail[len(s.tail)-tailSize:]...)
		}
	}
	if err != nil {
		s.ended = true
	}
	s.mu.Unlock()
	return n, err
}

// Write sends bytes to the terminal's input.
func (s *ExecSession) Write(p []byte) (int, error) { return s.hr.Conn.Write(p) }

// Resize changes the size of the terminal.
func (s *ExecSession) Resize(ctx context.Context, cols, rows uint) error {
	if cols == 0 || rows == 0 || cols > maxTerminal || rows > maxTerminal {
		return fmt.Errorf("resize exec on %s: terminal size %dx%d is out of range", s.desc, cols, rows)
	}
	if _, err := s.cli.ExecResize(ctx, s.id, client.ExecResizeOptions{Height: rows, Width: cols}); err != nil {
		return fmt.Errorf("resize exec on %s: %w", s.desc, err)
	}
	return nil
}

// Inspect asks the engine whether the process still runs and how it ended.
func (s *ExecSession) Inspect(ctx context.Context) (ExecState, error) {
	st, _, err := s.inspect(ctx)
	return st, err
}

// inspect also returns whether the process was ever started (it has a PID, or an
// exit code that only a started or failed process has).
func (s *ExecSession) inspect(ctx context.Context) (ExecState, bool, error) {
	res, err := s.cli.ExecInspect(ctx, s.id, client.ExecInspectOptions{})
	if err != nil {
		return ExecState{}, false, fmt.Errorf("inspect exec on %s: %w", s.desc, err)
	}
	return ExecState{Running: res.Running, ExitCode: res.ExitCode}, res.PID != 0 || res.ExitCode != 0, nil
}

// Wait returns the state of the process once it has ended, polling the engine until
// it has or ctx is done. It returns ErrNoShell, with the state, when the process
// could not be started because the executable is not in the image: exit code 126 or
// 127, or the runtime's "no such file or directory" message at the end of the output
// that Read returned. A caller that reads the output should read it to its end first
// so a process that has not started yet is not mistaken for one that ended with 0.
func (s *ExecSession) Wait(ctx context.Context) (ExecState, error) {
	t := time.NewTicker(waitPoll)
	defer t.Stop()
	for {
		st, started, err := s.inspect(ctx)
		if err != nil {
			return ExecState{}, err
		}
		s.mu.Lock()
		ended := s.ended
		s.mu.Unlock()
		if !st.Running && (started || ended) {
			if st.ExitCode == 126 || st.ExitCode == 127 || looksLikeNoShell(s.output()) {
				return st, ErrNoShell
			}
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, fmt.Errorf("wait for exec on %s: %w", s.desc, ctx.Err())
		case <-t.C:
		}
	}
}

// output is the end of the output read so far.
func (s *ExecSession) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.tail)
}

// Close closes the hijacked connection, once; a second call does nothing and
// returns the first call's result. That is the whole teardown (D-30, user decision
// 2026-10-09): nothing here runs a cleanup exec or kills the process, so a process
// started from the shell, a background job or one that ignores the hangup, may keep
// running in the container after the session ends. This is an accepted limitation,
// recorded in the documentation.
func (s *ExecSession) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.hr.Conn.Close() })
	return s.closeErr
}
