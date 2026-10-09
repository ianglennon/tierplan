// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { HealthState } from "@/api/environments";
import type { ContainerRow } from "@/api/resources";
import { StatusDot } from "@/components/status-dot";
import type { PendingVerb } from "@/features/containers/pending";
import { useT } from "@/i18n/use-t";

// The container state colours (UI-SPEC "Color"): running is online; restarting,
// paused and removing are warning; dead is offline; exited and created are
// neutral. The dot is always paired with the state's text.
export const STATE_DOT: Record<ContainerRow["state"], HealthState> = {
  running: "online",
  restarting: "unsupported",
  paused: "unsupported",
  removing: "unsupported",
  dead: "offline",
  exited: "unknown",
  created: "unknown",
};

interface ContainerStateCellProps {
  state: ContainerRow["state"];
  health?: ContainerRow["health"];
  // The action the row is waiting on, when it is: its transitional label then
  // stands in for the state.
  pending?: PendingVerb;
}

// ContainerStateCell is the State column: the state's dot and word, and a second
// offline dot with "Unhealthy" while the health check fails. A container still
// starting its check, or healthy, shows nothing extra. A row waiting on an
// action shows "Stopping…" and its kind instead, until the change is seen.
export function ContainerStateCell({ state, health, pending }: ContainerStateCellProps) {
  const t = useT();
  if (pending) {
    // The pulsing warning dot of a change under way, with its word ("Stopping…").
    return (
      <span className="inline-flex items-center whitespace-nowrap" data-pending={pending}>
        <StatusDot state="reconnecting" label={t(`actions.pending.${pending}`)} />
      </span>
    );
  }
  return (
    <span className="inline-flex flex-wrap items-center gap-x-3 gap-y-0.5 whitespace-nowrap">
      <StatusDot
        state={STATE_DOT[state] ?? "unknown"}
        label={t(`dashboard.lists.states.${state}`)}
      />
      {health === "unhealthy" ? (
        <StatusDot state="offline" label={t("containers.list.unhealthy")} />
      ) : null}
    </span>
  );
}
