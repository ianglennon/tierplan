// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

// StackState is the one word a stack is listed in, everywhere it shows.
type StackState string

// The states of a stack, in the order DeriveState decides them (UI-SPEC Defaults 11).
const (
	StateRemoving     StackState = "removing"
	StateDeploying    StackState = "deploying"
	StateRollingBack  StackState = "rolling_back"
	StateStarting     StackState = "starting"
	StateStopped      StackState = "stopped"
	StateDeployFailed StackState = "deploy_failed"
	StateNotRunning   StackState = "not_running"
	StateDegraded     StackState = "degraded"
	StateRunning      StackState = "running"
	StateNotDeployed  StackState = "not_deployed"
	// StateUnknown is a stack that was deployed whose engine could not be read.
	StateUnknown StackState = "unknown"
)

// StateInput is what a stack's state is read from.
type StateInput struct {
	// Latest is the newest operation on the stack, nil when it has had none.
	Latest *DeployRow
	// Stopped is the stack's own record that a person stopped it.
	Stopped bool
	// Deployed is true when a deploy, update or roll back of the stack has succeeded.
	Deployed bool
	// Unknown is true when what the engine runs could not be read.
	Unknown bool
	// Services are the services the stack owns on the engine.
	Services []ServiceRow
}

// The operation kinds that put a definition on the target. A failed one is the last deploy failing.
func deploying(kind string) bool {
	return kind == DeployKindDeploy || kind == DeployKindRollback || kind == kindUpdate
}

// The kinds of operation the later plans add (07-24 update, 07-26 stop and start, 07-27 remove).
const (
	kindUpdate = "update"
	kindStart  = "start"
	kindRemove = "remove"
)

// DeriveState is the stack's state, the services of it that run and the services it has: the one
// priority order of UI-SPEC Defaults 11, so no state hides a running operation or a failed last deploy.
//
//	Removing, then Deploying, Rolling back or Starting (an operation under way), then Stopped, then Deploy
//	failed (the newest operation put a definition on the target and did not finish), then Not deployed, then
//	Unknown (deployed, and the engine could not be read), then Not running, Degraded and Running by how many
//	services run.
//
// The counts are the engine's own and are zero when the services were not read.
func DeriveState(in StateInput) (StackState, int, int) {
	total, running := len(in.Services), 0
	for _, s := range in.Services {
		if s.State == ServiceRunning {
			running++
		}
	}
	if op := in.Latest; op != nil && op.State == DeployRunning {
		switch {
		case op.Kind == kindRemove:
			return StateRemoving, running, total
		case op.Kind == DeployKindRollback:
			return StateRollingBack, running, total
		case op.Kind == kindStart:
			return StateStarting, running, total
		case deploying(op.Kind):
			return StateDeploying, running, total
		}
	}
	if in.Stopped {
		return StateStopped, running, total
	}
	if op := in.Latest; op != nil && deploying(op.Kind) && op.State != DeployRunning && op.State != DeployDone {
		return StateDeployFailed, running, total
	}
	switch {
	case !in.Deployed && total == 0:
		return StateNotDeployed, 0, 0
	case in.Unknown:
		return StateUnknown, 0, 0
	case total == 0 || running == 0:
		return StateNotRunning, running, total
	case running < total:
		return StateDegraded, running, total
	}
	return StateRunning, running, total
}
