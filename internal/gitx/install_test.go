// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitx_test

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"

	"github.com/ianglennon/coaming/internal/gitx"
)

// noInstallEnv makes TestMain leave the transport alone, in the child process of the test below.
const noInstallEnv = "GITX_TEST_NO_INSTALL"

// A client built where nothing installed the transport must not fall back to go-git's own (IN-A01): that one has no
// egress policy, no size bound and speaks http, git and file. go-git keeps its protocols in a package variable, so the
// check runs in a process of its own where TestMain did not install anything.
func TestNewGoGitInstallsTheHardenedTransport(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNewGoGitInstallsTheHardenedTransportChild$", "-test.v") //nolint:gosec // G204: the test binary re-running itself
	cmd.Env = append(os.Environ(), noInstallEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the child process failed: %v\n%s", err, out)
	}
}

func TestNewGoGitInstallsTheHardenedTransportChild(t *testing.T) {
	if os.Getenv(noInstallEnv) == "" {
		t.Skip("runs in the child process of TestNewGoGitInstallsTheHardenedTransport")
	}
	gitx.NewGoGit(gitx.Options{})
	for _, raw := range []string{"http://127.0.0.1:1/x.git", "git://127.0.0.1:1/x.git", "file:///etc"} {
		ep, err := transport.NewEndpoint(raw)
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.NewClient(ep)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if _, err := c.NewUploadPackSession(ep, nil); !errors.Is(err, gitx.ErrProtocolRefused) {
			t.Errorf("%s: err = %v, want ErrProtocolRefused: NewGoGit left go-git's own transport in place", raw, err)
		}
	}
}
