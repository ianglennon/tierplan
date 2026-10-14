// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export type HeatStep = 0 | 1 | 2 | 3 | 4;

export const HEAT_CLASS: Record<HeatStep, string> = { 0: "", 1: "", 2: "", 3: "", 4: "" };

export function heatStep(_count: number, _rowMax: number): HeatStep {
  return 0;
}
