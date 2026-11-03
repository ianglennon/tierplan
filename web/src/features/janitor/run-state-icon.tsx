// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CircleCheck, CircleX, Info, Lock, TriangleAlert } from "lucide-react";

import type { JanitorRun } from "@/api/janitor";
import { Spinner } from "@/components/ui/spinner";

// The icon of a run's state: always beside a word, never colour alone (UI-SPEC "Color"). The run
// page, the Status card, the run history and the dashboard card all draw it.
export function StateIcon({ state }: { state: JanitorRun["state"] }) {
  switch (state) {
    case "running":
      return <Spinner aria-hidden="true" />;
    case "waiting":
      return <Lock aria-hidden="true" className="size-4 text-status-warning-text" />;
    case "completed":
      return <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />;
    case "partial":
      return <TriangleAlert aria-hidden="true" className="size-4 text-status-warning-text" />;
    case "skipped":
      return <Info aria-hidden="true" className="size-4 text-muted-foreground" />;
    default:
      return <CircleX aria-hidden="true" className="size-4 text-status-offline-text" />;
  }
}
