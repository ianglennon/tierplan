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

// DeriveState is the stack's state, its running services and its services.
func DeriveState(in StateInput) (StackState, int, int) {
	return "", 0, 0
}
