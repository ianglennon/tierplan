// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

// StackState is the one word a stack is listed in, everywhere it shows.
type StackState string

// The states of a stack, in the order DeriveState decides them (UI-SPEC Defaults 11).
const (
	StateRemoving     StackState = "removing"
	StateDeploying    StackState = "deploying"
	StateRollingBack  StackState = "rolling_back"
	StateStarting     StackState = "starting"
	StateStopping     StackState = "stopping"
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
	// LatestDeploy is the newest deploy, update or roll back, whatever came after it, nil when the stack has had
	// none. A stop or a start after a failed deploy must not hide the failure (UI-SPEC Defaults 11), so the
	// failed-deploy state is read from it. Nil reads the failure from Latest, as before stops and starts existed.
	LatestDeploy *DeployRow
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

// The kinds of operation the later plans add (07-24 update); a stop, a start and a remove are
// DeployKindStop, DeployKindStart and DeployKindRemove.
const (
	kindUpdate = "update"
	kindStart  = DeployKindStart
	kindStop   = DeployKindStop
	kindRemove = DeployKindRemove
)

// DeriveState is the stack's state, the services of it that run and the services it has: the one
// priority order of UI-SPEC Defaults 11, so no state hides a running operation or a failed last deploy.
//
//	Removing, then Deploying, Rolling back, Starting or Stopping (an operation under way), then Not deployed (the newest
//	operation is a removal that finished), then Stopped, then
//	Deploy failed (the newest deploy, update or roll back did not finish), then Not deployed, then
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
		case op.Kind == kindStop:
			return StateStopping, running, total
		case deploying(op.Kind):
			return StateDeploying, running, total
		}
	}
	// A removal that kept the definition leaves a stack with nothing on the target: it reads Not deployed, whatever
	// it was before, until a deploy puts something there again.
	if op := in.Latest; op != nil && op.Kind == kindRemove && op.State == DeployDone {
		return StateNotDeployed, 0, 0
	}
	if in.Stopped {
		return StateStopped, running, total
	}
	failing := in.LatestDeploy
	if failing == nil {
		failing = in.Latest
	}
	if op := failing; op != nil && deploying(op.Kind) && op.State != DeployRunning && op.State != DeployDone {
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
