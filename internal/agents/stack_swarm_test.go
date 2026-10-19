// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// The stack suites (plan 07-11, STK-02, STK-09) run a stack deploy through Bayplan's API and a real
// agent on a swarm of three managers and a worker, and hold the result against what the engine does
// with the same compose file when the docker CLI deploys it.

// stackEnvReady installs the agents and waits until the swarm environment is online with all four.
func (f *swarmFix) stackEnvReady(t *testing.T) {
	t.Helper()
	f.install(t)
	f.waitSwarmOnline(t, 4)
}

// parityCase is one file of the parity corpus.
type parityCase struct {
	// file is the name under testdata/stacks.
	file string
	// files are written beside the compose file by both deploys.
	files []stackFile
}

// parityCorpus is every file the oracle deploys twice.
var parityCorpus = []parityCase{
	{file: "parity-basic.yaml"},
}

// sharedStackDeployParity is the differential oracle for the converter (STK-02, RESEARCH "Parity
// oracle", Pitfall 3). Each file of the corpus is deployed once by the real docker stack deploy
// --resolve-image=never on a manager and once by Bayplan, through the API and the agents, and the
// service specs the engine holds are compared after normaliseSpec. The image is named by digest, so
// neither deploy has a lookup to make and both write the same text. The two stacks run one after the
// other (the first is removed and gone before the second starts), so a published port or a subnet is
// the same in both. Each case leaves nothing of either stack behind.
func sharedStackDeployParity(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/parity/app:1", nil)
	values := map[string]string{"IMAGE": addr + "/parity/app@" + digest}

	for _, c := range parityCorpus {
		t.Run(c.file, func(t *testing.T) {
			src := paritySource{Compose: readCorpus(t, c.file, values), Files: c.files}

			if err := deployWithCLI(execCtx(t, 5*time.Minute), mgr, parityCLIStack, src); err != nil {
				t.Fatal(err)
			}
			cli, err := snapshotStack(execCtx(t, time.Minute), mgr, parityCLIStack)
			if err != nil {
				t.Fatal(err)
			}
			removeStackCLI(t, f, parityCLIStack)

			deployWithBayplan(t, f, parityBayplanStack, src)
			bp, err := snapshotStack(execCtx(t, time.Minute), mgr, parityBayplanStack)
			if err != nil {
				t.Fatal(err)
			}
			removeStackCLI(t, f, parityBayplanStack)

			compareSnapshots(t, cli, bp)
		})
	}
}
