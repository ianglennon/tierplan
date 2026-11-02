// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

// ReplicasSpec is replicasSpec for the tests of package stackops_test.
var ReplicasSpec = replicasSpec

// ConvertLogText is convertLogText for the tests of package stackops_test.
var ConvertLogText = convertLogText

// SetSubnetPlanHook returns d with the hook a swarm deploy calls after planning the subnets of its new networks.
func SetSubnetPlanHook(d Deps, hook func()) Deps {
	d.subnetPlanHook = hook
	return d
}
