// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/server"
)

// The image HEALTHCHECK runs `tierplan healthcheck` as root every 30 seconds.
// That command reads no data, so it must not make the privilege drop walk and
// chown /data (WR-08).
func TestHealthcheckNeverPreparesTheDataDirectory(t *testing.T) {
	t.Setenv("TIERPLAN_DATA_DIR", "/srv/tierplan-data")
	root := newRoot()

	for _, tc := range []struct {
		command string
		want    string
	}{
		{"healthcheck", ""},
		{"version", ""},
		{"serve", "/srv/tierplan-data"},
	} {
		cmd, _, err := root.Find([]string{tc.command})
		if err != nil || cmd.Name() != tc.command {
			t.Fatalf("find %s: %v", tc.command, err)
		}
		if err := cmd.ParseFlags(nil); err != nil {
			t.Fatal(err)
		}
		if got := dropOptions(cmd).DataDir; got != tc.want {
			t.Errorf("%s: DataDir = %q, want %q", tc.command, got, tc.want)
		}
	}
}

// TierPlan always serves HTTPS with its own certificate, so there is no plain mode
// to recover to and the recovery command is gone (D-14, TLS-12). This keeps it
// from coming back unnoticed.
func TestTheRootCommandHasNoHTTPSCommand(t *testing.T) {
	root := newRoot()
	for _, c := range root.Commands() {
		if c.Name() == "https" {
			t.Fatalf("the root command still has an %q subcommand", c.Name())
		}
	}

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"https", "disable"})
	err := root.Execute()
	if err == nil {
		t.Fatal("the removed https command succeeded, want an unknown-command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want it to say unknown command", err)
	}
}

// A restore from the web UI makes Run return a restart request. In exit mode the
// swap runs and the process ends with status 0 (no error), leaving the start to the
// container's restart policy (plan 03-25, RESEARCH A5).
func TestFinishRunInExitModeRunsTheSwapAndEndsWithoutAnError(t *testing.T) {
	swapped := 0
	err := finishRun(&server.RestartError{Request: server.RestartRequest{Swap: func(context.Context) error {
		swapped++
		return nil
	}}}, server.RestartExit)
	if err != nil {
		t.Fatalf("finishRun in exit mode = %v, want nil", err)
	}
	if swapped != 1 {
		t.Fatalf("the swap ran %d times, want 1", swapped)
	}
}

func TestFinishRunReportsAFailedSwapAndPassesOtherErrorsThrough(t *testing.T) {
	boom := errors.New("disk full")
	if err := finishRun(&server.RestartError{Request: server.RestartRequest{Swap: func(context.Context) error { return boom }}}, server.RestartExit); !errors.Is(err, boom) {
		t.Fatalf("finishRun with a failing swap = %v, want it to wrap the swap's error", err)
	}
	other := errors.New("listen: address in use")
	if err := finishRun(other, server.RestartExit); !errors.Is(err, other) {
		t.Fatalf("finishRun of an ordinary error = %v", err)
	}
	if err := finishRun(nil, server.RestartExec); err != nil {
		t.Fatalf("finishRun(nil) = %v", err)
	}
}
