// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackDriftService } from "@/api/stack-drift";

interface DriftServiceListProps {
  services: StackDriftService[];
}

// DriftServiceList is filled in by the green step of plan 08-19, task 2.
export function DriftServiceList(_props: DriftServiceListProps) {
  return null;
}
