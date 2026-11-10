// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"slices"

	"github.com/ianglennon/tierplan/internal/jobs"
)

// The two steps a DNS-01 run has beyond the six every run has. Like the others they are the item keys
// of the live job the UI shows, so they are a contract.
const (
	StepPropagation = "propagation"
	StepCleanup     = "cleanup"
)

// stepItemsDNS01 are the steps of a DNS-01 run, in the order the run reports them. Clean-up comes after
// Stored because the order code withdraws its answers when the order ends, after the certificate was
// stored (order.go defers cleanUp), and a failed order reports its own end the same way.
var stepItemsDNS01 = []jobs.ItemSpec{
	{ID: StepAccount, Label: "Account", Kind: "step"},
	{ID: StepOrder, Label: "Order", Kind: "step"},
	{ID: StepChallenge, Label: "Challenge", Kind: "step"},
	{ID: StepPropagation, Label: "Propagation", Kind: "step"},
	{ID: StepValidate, Label: "Validation", Kind: "step"},
	{ID: StepFinalize, Label: "Finalize", Kind: "step"},
	{ID: StepStored, Label: "Stored", Kind: "step"},
	{ID: StepCleanup, Label: "Clean-up", Kind: "step"},
}

// itemsFor are the job items of a run that uses challenge: the six steps of HTTP-01, or the eight of
// DNS-01. Each call returns its own copy.
func itemsFor(challenge string) []jobs.ItemSpec {
	if challenge == ChallengeDNS01 {
		return slices.Clone(stepItemsDNS01)
	}
	return slices.Clone(stepItems)
}
