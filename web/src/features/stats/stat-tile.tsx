// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

interface StatTileProps {
  label: string;
  hint?: string;
  lines: ReactNode[];
  subline?: string;
  trend?: ReadonlyArray<number | null>;
  loading?: boolean;
}

export function StatTile(_props: StatTileProps) {
  return null;
}
