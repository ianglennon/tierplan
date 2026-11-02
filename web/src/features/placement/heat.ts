// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The shading of the placement grid (RES-12, D-18, UI-SPEC "Placement heat"). A cell is tinted by
// its task count relative to the largest count in its row, in four steps. The shade only
// decorates: the count is always printed in the cell.
export type HeatStep = 0 | 1 | 2 | 3 | 4;

// The classes are written out whole so the stylesheet generator finds them. Each is that share
// of the foreground colour over the card; zero is not tinted.
export const HEAT_CLASS: Record<HeatStep, string> = {
  0: "",
  1: "bg-foreground/6",
  2: "bg-foreground/12",
  3: "bg-foreground/18",
  4: "bg-foreground/24",
};

// heatStep is 0 for a count of zero, otherwise 1 to 4 for the quarter of the row's maximum the
// count falls in: the top step is the maximum itself.
export function heatStep(count: number, rowMax: number): HeatStep {
  if (count <= 0) return 0;
  if (rowMax <= 0) return 4;
  const step = Math.ceil((count / rowMax) * 4);
  return Math.min(4, Math.max(1, step)) as HeatStep;
}
