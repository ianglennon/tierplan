// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { HealthState } from "@/api/environments";
import type { StackState } from "@/api/stacks";
import { StatusDot } from "@/components/status-dot";
import { useT } from "@/i18n/use-t";

// The status colour of each state (UI-SPEC "Color"). An operation under way is the pulsing warning
// dot, which StatusDot turns off under reduced motion; Degraded is the warning colour without the
// pulse; a failure and a stack that is not running are offline; a stack that is stopped, not
// deployed or not known is neutral.
const TONE: Record<StackState, HealthState> = {
  removing: "reconnecting",
  deploying: "reconnecting",
  rolling_back: "reconnecting",
  starting: "reconnecting",
  stopped: "unknown",
  deploy_failed: "offline",
  not_running: "offline",
  degraded: "degraded",
  running: "online",
  not_deployed: "unknown",
  unknown: "unknown",
};

interface StackStateCellProps {
  state: StackState;
}

// StackStateCell is a stack's state as a dot and a word, never colour alone. The words are text
// nodes, so nothing a stack is named can add markup.
export function StackStateCell({ state }: StackStateCellProps) {
  const t = useT();
  return <StatusDot state={TONE[state]} label={t(`stackOverview.state.${state}`)} />;
}
