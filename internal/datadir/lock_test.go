// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package datadir_test

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/datadir"
)

func TestLockRefusesASecondLockUntilTheFirstIsReleased(t *testing.T) {
	dir := t.TempDir()
	release, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = datadir.Lock(dir)
	if !errors.Is(err, datadir.ErrLocked) || !strings.Contains(err.Error(), datadir.LockFile) {
		t.Fatalf("second Lock = %v, want ErrLocked naming %s", err, datadir.LockFile)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatalf("a second release = %v, want a no-op", err)
	}
	again, err := datadir.Lock(dir)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	_ = again()
}

func TestLockFileIsOwnerOnlyEmptyAndSurvivesRelease(t *testing.T) {
	dir := t.TempDir()
	release, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, datadir.LockFile))
	if err != nil {
		t.Fatalf("the lock file is gone after release: %v", err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Fatalf("lock file mode %v size %d, want 0600 and empty", info.Mode().Perm(), info.Size())
	}
}

func TestLockCreatesAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "yet")
	release, err := datadir.Lock(dir)
	if err != nil {
		t.Fatalf("Lock on a missing directory: %v", err)
	}
	_ = release()
}

// TestLockHelperProcess is the other process of TestLockIsHeldAcrossProcesses:
// it takes the lock, says so, and holds it until its standard input closes.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv("BAYPLAN_LOCK_HELPER_DIR")
	if dir == "" {
		t.Skip("only runs as the helper process of TestLockIsHeldAcrossProcesses")
	}
	release, err := datadir.Lock(dir)
	if err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(3)
	}
	os.Stdout.WriteString("locked\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	_ = release()
}

func TestLockIsHeldAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "BAYPLAN_LOCK_HELPER_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper said %q, %v", line, err)
	}

	if _, err := datadir.Lock(dir); !errors.Is(err, datadir.ErrLocked) {
		t.Fatalf("Lock while another process holds it = %v, want ErrLocked", err)
	}

	// The helper exiting (its descriptor closing with it) frees the lock.
	_ = stdin.Close()
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		release, err := datadir.Lock(dir)
		if err == nil {
			_ = release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lock was not released when the holder exited: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
