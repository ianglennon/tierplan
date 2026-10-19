// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { HealthState } from "@/api/environments";
import type { StackState } from "@/api/stacks";
import { StatusDot } from "@/components/status-dot";
import { useT } from "@/i18n/use-t";

// The status colour of each state this version's server returns (UI-SPEC "Color"). Later plans
// add the live states (Degraded, Stopped, Rolling back ...) to the same map.
const TONE: Record<StackState, HealthState> = {
  not_deployed: "unknown",
  deploying: "reconnecting",
  deploy_failed: "offline",
  deployed: "online",
};

interface StackStateCellProps {
  state: StackState;
}

// StackStateCell is a stack's state as a dot and a word, never colour alone. The words are text
// nodes, so nothing a stack is named can add markup.
export function StackStateCell({ state }: StackStateCellProps) {
  const t = useT();
  return <StatusDot state={TONE[state]} label={t(`stacks.state.${state}`)} />;
}
