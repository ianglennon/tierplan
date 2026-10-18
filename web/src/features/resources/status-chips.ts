// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The class string of the filter-chip row of the resource lists (services, nodes, containers,
// volumes, images). The chips wrap onto further rows at every width, so each one stays on the
// screen at 375px. Phase 6 UAT gap G-06-2: the user rejected the earlier rule that turned the row
// into a strip that scrolls sideways under 768px, where the last chip could only be reached by
// swiping. The ToggleGroup's spacing keeps chips, and rows once they wrap, 8px apart, and each
// chip keeps its own 44px height under 768px (P1 D-64). The row is as wide as its chips when they
// fit on one line and as wide as the toolbar when they wrap, which changes nothing visible because
// the row has no background or border of its own.
export const STATUS_CHIP_ROW = "max-w-full flex-wrap";
