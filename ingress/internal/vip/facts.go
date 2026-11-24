// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"io"
	"time"
)

// DiscoveryRescan is how often the helper looks for the interface again while it has none (D-09).
const DiscoveryRescan = 10 * time.Second

// FaultReasonGrace is how long a FAULT state waits for the check's failure line.
const FaultReasonGrace = 250 * time.Millisecond

// scriptName is the vrrp_script of Render.
const scriptName = "chk_proxy"

// probeFacts is a stub until the scanner is written.
type probeFacts struct{}

func (*probeFacts) writer() io.Writer { return io.Discard }
func (*probeFacts) failing() bool     { return false }
func (*probeFacts) reset()            {}
