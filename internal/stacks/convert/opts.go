// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package convert

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"
)

// The two capability values docker uses: ALL adds or drops every capability, RESET is an update
// instruction that has no meaning on a stack deploy.
const (
	allCapabilities   = "ALL"
	resetCapabilities = "RESET"
)

// normalizeCapability upper-cases and trims a capability and gives it the CAP_ prefix, leaving
// the ALL and RESET magic values alone. Ported from NormalizeCapability in docker/cli's
// opts/capabilities.go (Apache-2.0).
func normalizeCapability(capability string) string {
	capability = strings.ToUpper(strings.TrimSpace(capability))
	if capability == allCapabilities || capability == resetCapabilities {
		return capability
	}
	if !strings.HasPrefix(capability, "CAP_") {
		capability = "CAP_" + capability
	}
	return capability
}

func capabilitySet(caps []string) map[string]bool {
	out := make(map[string]bool, len(caps))
	for _, c := range caps {
		out[normalizeCapability(c)] = true
	}
	return out
}

// effectiveCapAddCapDrop normalizes and sorts the capabilities to add and to drop and returns
// the effective lists. Adding takes precedence over dropping, so a capability in both lists is
// not dropped, and ALL trumps every other capability on its side. Duplicates are removed.
// Ported from EffectiveCapAddCapDrop in docker/cli's opts/capabilities.go (Apache-2.0).
func effectiveCapAddCapDrop(add, drop []string) (capAdd, capDrop []string) {
	addCaps, dropCaps := capabilitySet(add), capabilitySet(drop)
	if addCaps[allCapabilities] {
		addCaps = map[string]bool{allCapabilities: true}
	}
	if dropCaps[allCapabilities] {
		dropCaps = map[string]bool{allCapabilities: true}
	}
	for c := range dropCaps {
		if addCaps[c] {
			continue
		}
		if c != resetCapabilities {
			capDrop = append(capDrop, c)
		}
	}
	for c := range addCaps {
		if c != resetCapabilities {
			capAdd = append(capAdd, c)
		}
	}
	slices.Sort(capAdd)
	slices.Sort(capDrop)
	return capAdd, capDrop
}

// restartPolicyFromRestart converts the legacy restart key ("always", "unless-stopped",
// "on-failure[:N]", "no") to a swarm restart policy. "no" and an empty key give none. Ported from
// convertRestartPolicy and ParseRestartPolicy in docker/cli (cli/compose/convert/service.go and
// opts/parse.go, Apache-2.0).
func restartPolicyFromRestart(restart string) (*swarm.RestartPolicy, error) {
	if restart == "" {
		return nil, nil
	}
	name, count, hasCount := strings.Cut(restart, ":")
	if hasCount && name == "" {
		return nil, errors.New("invalid restart policy format: no policy provided before colon")
	}
	retries := 0
	if count != "" {
		n, err := strconv.Atoi(count)
		if err != nil {
			return nil, errors.New("invalid restart policy format: maximum retry count must be an integer")
		}
		retries = n
	}
	if retries < 0 {
		return nil, errors.New("invalid restart policy: maximum retry count cannot be negative")
	}
	var maxAttempts *uint64
	if retries > 0 {
		maxAttempts = new(uint64(retries))
	}
	switch name {
	case "no", "":
		return nil, nil
	case "always", "unless-stopped":
		return &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny, MaxAttempts: maxAttempts}, nil
	case "on-failure":
		return &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionOnFailure, MaxAttempts: maxAttempts}, nil
	}
	return nil, fmt.Errorf("invalid restart policy: unknown policy %q (must be no, always, on-failure or unless-stopped)", name)
}

// nanoCPUs converts a compose cpus value to nano-CPUs exactly: 0.1 is 100000000, not the
// float32 neighbour of it. A value with more than nine decimal places is refused, as docker
// does (ParseCPUs in docker/cli's opts/opts.go, Apache-2.0).
func nanoCPUs(cpus composetypes.NanoCPUs) (int64, error) {
	if cpus == 0 {
		return 0, nil
	}
	// The shortest decimal that parses back to the same float32 is the number the user wrote.
	cpu, ok := new(big.Rat).SetString(strconv.FormatFloat(float64(cpus), 'f', -1, 32))
	if !ok {
		return 0, fmt.Errorf("cpus %v is not a number", float32(cpus))
	}
	nano := cpu.Mul(cpu, big.NewRat(1e9, 1))
	if !nano.IsInt() {
		return 0, fmt.Errorf("cpus %v has more than nine decimal places", float32(cpus))
	}
	return nano.Num().Int64(), nil
}
