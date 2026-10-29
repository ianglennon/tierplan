// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import "time"

// The bounds of a call, set small so that a test reaches them without a large repository or a long wait.

// SetAdvertisementMax sets the most bytes a listing reads from the server.
func (g *GoGit) SetAdvertisementMax(n int64) { g.advertisementMax = n }

// SetPackMax sets the most bytes a fetch over ssh reads from the server.
func (g *GoGit) SetPackMax(n int64) { g.packMax = n }

// SetHandshakeTimeout sets how long an ssh connect and handshake may take.
func (g *GoGit) SetHandshakeTimeout(d time.Duration) { g.handshake = d }
