// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
)

// The state of a stack (plan 07-23, STK-10, UI-SPEC Defaults 11): one priority order, so the state never
// hides a running operation or a failed last deploy behind Degraded or Running.

func services(states ...string) []stackops.ServiceRow {
	rows := make([]stackops.ServiceRow, len(states))
	for i, s := range states {
		rows[i] = stackops.ServiceRow{Name: "web_" + string(rune('a'+i)), State: s}
	}
	return rows
}

func op(kind, state string) *stackops.DeployRow {
	return &stackops.DeployRow{Kind: kind, State: state}
}

func TestDeriveStateFollowsTheOnePriorityOrder(t *testing.T) {
	t.Parallel()
	run, deg, down, stop := stackops.ServiceRunning, stackops.ServiceDegraded, stackops.ServiceNotRunning, stackops.ServiceStopped
	for name, tc := range map[string]struct {
		in      stackops.StateInput
		want    stackops.StackState
		running int
		total   int
	}{
		// Removing, then Deploying, Rolling back or Starting.
		"a removal under way":                                   {stackops.StateInput{Latest: op("remove", "running"), Deployed: true, Services: services(run)}, stackops.StateRemoving, 1, 1},
		"a removal outranks a stopped stack":                    {stackops.StateInput{Latest: op("remove", "running"), Stopped: true, Deployed: true, Services: services(stop)}, stackops.StateRemoving, 0, 1},
		"a deploy under way":                                    {stackops.StateInput{Latest: op("deploy", "running"), Deployed: true, Services: services(run, deg)}, stackops.StateDeploying, 1, 2},
		"an update under way":                                   {stackops.StateInput{Latest: op("update", "running"), Deployed: true, Services: services(run)}, stackops.StateDeploying, 1, 1},
		"the first deploy under way":                            {stackops.StateInput{Latest: op("deploy", "running")}, stackops.StateDeploying, 0, 0},
		"a roll back under way":                                 {stackops.StateInput{Latest: op("rollback", "running"), Deployed: true, Services: services(run)}, stackops.StateRollingBack, 1, 1},
		"a start under way":                                     {stackops.StateInput{Latest: op("start", "running"), Stopped: true, Deployed: true, Services: services(stop)}, stackops.StateStarting, 0, 1},
		"a stopped stack with a running deploy reads Deploying": {stackops.StateInput{Latest: op("deploy", "running"), Stopped: true, Deployed: true, Services: services(stop)}, stackops.StateDeploying, 0, 1},
		"a deploy under way outranks a failed one before it":    {stackops.StateInput{Latest: op("deploy", "running"), Deployed: true, Services: services(down)}, stackops.StateDeploying, 0, 1},
		// Stopped.
		"a stopped stack":                             {stackops.StateInput{Latest: op("stop", "done"), Stopped: true, Deployed: true, Services: services(stop, stop)}, stackops.StateStopped, 0, 2},
		"a stopped stack outranks a failed deploy":    {stackops.StateInput{Latest: op("deploy", "failed"), Stopped: true, Deployed: true, Services: services(stop)}, stackops.StateStopped, 0, 1},
		"a stop under way is not yet a stopped stack": {stackops.StateInput{Latest: op("stop", "running"), Deployed: true, Services: services(run, run)}, stackops.StateStopping, 2, 2},
		"a stop under way on a stopped stack":         {stackops.StateInput{Latest: op("stop", "running"), Stopped: true, Deployed: true, Services: services(stop)}, stackops.StateStopping, 0, 1},
		// Deploy failed.
		"a failed deploy":                        {stackops.StateInput{Latest: op("deploy", "failed"), Deployed: true, Services: services(run)}, stackops.StateDeployFailed, 1, 1},
		"a failed deploy outranks Degraded":      {stackops.StateInput{Latest: op("deploy", "failed"), Deployed: true, Services: services(run, deg)}, stackops.StateDeployFailed, 1, 2},
		"a failed deploy outranks Not running":   {stackops.StateInput{Latest: op("update", "failed"), Deployed: true, Services: services(down)}, stackops.StateDeployFailed, 0, 1},
		"a cancelled deploy":                     {stackops.StateInput{Latest: op("deploy", "cancelled"), Deployed: true, Services: services(run)}, stackops.StateDeployFailed, 1, 1},
		"an interrupted deploy":                  {stackops.StateInput{Latest: op("deploy", "interrupted"), Deployed: true, Services: services(run)}, stackops.StateDeployFailed, 1, 1},
		"a failed roll back":                     {stackops.StateInput{Latest: op("rollback", "failed"), Deployed: true, Services: services(run)}, stackops.StateDeployFailed, 1, 1},
		"a first deploy that failed":             {stackops.StateInput{Latest: op("deploy", "failed")}, stackops.StateDeployFailed, 0, 0},
		"a failed deploy with the engine unread": {stackops.StateInput{Latest: op("deploy", "failed"), Deployed: true, Unknown: true}, stackops.StateDeployFailed, 0, 0},
		// A stop or a start after a deploy that failed does not hide the failure: it is read from the newest deploy.
		"a start after a failed deploy keeps Deploy failed":     {stackops.StateInput{Latest: op("start", "done"), LatestDeploy: op("deploy", "failed"), Deployed: true, Services: services(run)}, stackops.StateDeployFailed, 1, 1},
		"a stop and a start after a failed update keep it":      {stackops.StateInput{Latest: op("stop", "failed"), LatestDeploy: op("update", "cancelled"), Deployed: true, Services: services(run)}, stackops.StateDeployFailed, 1, 1},
		"a start after a deploy that worked reads the services": {stackops.StateInput{Latest: op("start", "done"), LatestDeploy: op("deploy", "done"), Deployed: true, Services: services(run, deg)}, stackops.StateDegraded, 1, 2},
		"a stopped stack still outranks the failed deploy":      {stackops.StateInput{Latest: op("stop", "done"), LatestDeploy: op("deploy", "failed"), Stopped: true, Deployed: true, Services: services(stop)}, stackops.StateStopped, 0, 1},
		// A failed stop, start or removal is no failed deploy.
		"a failed stop reads from the services":    {stackops.StateInput{Latest: op("stop", "failed"), Deployed: true, Services: services(run)}, stackops.StateRunning, 1, 1},
		"a failed removal reads from the services": {stackops.StateInput{Latest: op("remove", "failed"), Deployed: true, Services: services(run, deg)}, stackops.StateDegraded, 1, 2},
		// Not running, Degraded, Running.
		"nothing running":                                     {stackops.StateInput{Latest: op("deploy", "done"), Deployed: true, Services: services(down, down)}, stackops.StateNotRunning, 0, 2},
		"services scaled to zero without a stop":              {stackops.StateInput{Latest: op("deploy", "done"), Deployed: true, Services: services(stop)}, stackops.StateNotRunning, 0, 1},
		"deployed with no service at all":                     {stackops.StateInput{Latest: op("deploy", "done"), Deployed: true}, stackops.StateNotRunning, 0, 0},
		"some of the services running":                        {stackops.StateInput{Latest: op("deploy", "done"), Deployed: true, Services: services(run, down, run)}, stackops.StateDegraded, 2, 3},
		"a service that is degraded itself":                   {stackops.StateInput{Latest: op("deploy", "done"), Deployed: true, Services: services(run, deg)}, stackops.StateDegraded, 1, 2},
		"every service running":                               {stackops.StateInput{Latest: op("deploy", "done"), Deployed: true, Services: services(run, run, run)}, stackops.StateRunning, 3, 3},
		"a deployed stack after a later stop that was undone": {stackops.StateInput{Latest: op("start", "done"), Deployed: true, Services: services(run)}, stackops.StateRunning, 1, 1},
		// Not deployed.
		"a stack with a draft and no deploy":                      {stackops.StateInput{}, stackops.StateNotDeployed, 0, 0},
		"a stack whose removal kept the definition":               {stackops.StateInput{Latest: op("remove", "done")}, stackops.StateNotDeployed, 0, 0},
		"a stack never deployed on an engine that cannot be read": {stackops.StateInput{Unknown: true}, stackops.StateNotDeployed, 0, 0},
		// Unknown.
		"a deployed stack on an engine that cannot be read": {stackops.StateInput{Latest: op("deploy", "done"), Deployed: true, Unknown: true}, stackops.StateUnknown, 0, 0},
		"a running deploy outranks an unread engine":        {stackops.StateInput{Latest: op("deploy", "running"), Deployed: true, Unknown: true}, stackops.StateDeploying, 0, 0},
		"a stopped stack on an engine that cannot be read":  {stackops.StateInput{Latest: op("stop", "done"), Stopped: true, Deployed: true, Unknown: true}, stackops.StateStopped, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, running, total := stackops.DeriveState(tc.in)
			if got != tc.want || running != tc.running || total != tc.total {
				t.Errorf("DeriveState = %s %d of %d, want %s %d of %d", got, running, total, tc.want, tc.running, tc.total)
			}
		})
	}
}

func TestDeriveStateReachesEveryState(t *testing.T) {
	t.Parallel()
	seen := map[stackops.StackState]bool{}
	for _, in := range []stackops.StateInput{
		{Latest: op("remove", "running")},
		{Latest: op("deploy", "running")},
		{Latest: op("rollback", "running")},
		{Latest: op("start", "running")},
		{Latest: op("stop", "running")},
		{Stopped: true},
		{Latest: op("deploy", "failed")},
		{Deployed: true},
		{Deployed: true, Services: services(stackops.ServiceRunning, stackops.ServiceNotRunning)},
		{Deployed: true, Services: services(stackops.ServiceRunning)},
		{},
		{Deployed: true, Unknown: true},
	} {
		s, _, _ := stackops.DeriveState(in)
		seen[s] = true
	}
	for _, want := range []stackops.StackState{
		stackops.StateRemoving, stackops.StateDeploying, stackops.StateRollingBack, stackops.StateStarting,
		stackops.StateStopped, stackops.StateDeployFailed, stackops.StateNotRunning, stackops.StateDegraded,
		stackops.StateRunning, stackops.StateNotDeployed, stackops.StateUnknown, stackops.StateStopping,
	} {
		if !seen[want] {
			t.Errorf("no input reaches %s", want)
		}
	}
}
