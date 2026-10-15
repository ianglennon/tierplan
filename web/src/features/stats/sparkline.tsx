// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export const SPARKLINE_WIDTH = 96;
export const SPARKLINE_HEIGHT = 24;

export function sparklinePath(
  _values: ReadonlyArray<number | null>,
  _width = SPARKLINE_WIDTH,
  _height = SPARKLINE_HEIGHT,
): string {
  return "";
}

export function Sparkline(_props: { values: ReadonlyArray<number | null> }) {
  return null;
}
