// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { HealthState } from "@/api/environments";
import type { ContainerRow } from "@/api/resources";
import { StatusDot } from "@/components/status-dot";
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
}

// ContainerStateCell is the State column: the state's dot and word, and a second
// offline dot with "Unhealthy" while the health check fails. A container still
// starting its check, or healthy, shows nothing extra.
export function ContainerStateCell({ state, health }: ContainerStateCellProps) {
  const t = useT();
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
