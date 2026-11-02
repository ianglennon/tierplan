// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ContainerRow } from "@/api/resources";

// Every action the list offers on a container.
export type Verb = "start" | "stop" | "restart" | "kill" | "pause" | "unpause" | "remove";

// The states each action applies to (UI-SPEC "Eligibility"). A container being
// removed offers nothing, remove included: it is already on its way out.
const STATES: Record<Verb, ReadonlySet<string>> = {
  start: new Set(["exited", "created"]),
  stop: new Set(["running", "restarting", "paused"]),
  kill: new Set(["running", "restarting", "paused"]),
  restart: new Set(["running", "exited", "paused", "restarting"]),
  pause: new Set(["running"]),
  unpause: new Set(["paused"]),
  remove: new Set(["created", "running", "paused", "restarting", "exited", "dead"]),
};

type Subject = Pick<ContainerRow, "state" | "selfKind" | "isSelf">;

// ownKind says whether a container is one TierPlan itself depends on: the server
// it runs in, or an agent. Acting on one asks for its name to be typed, one at
// a time (D-11), and a bulk never includes one (D-12).
export function ownKind(row: Pick<ContainerRow, "selfKind" | "isSelf">): "server" | "agent" | "" {
  if (row.selfKind === "server" || row.selfKind === "agent") return row.selfKind;
  return row.isSelf ? "server" : "";
}

// eligibleFor says whether the action applies to the container in its state.
// With bulk set, one of TierPlan's own containers never does.
export function eligibleFor(verb: Verb, row: Subject, opts: { bulk?: boolean } = {}): boolean {
  if (opts.bulk && ownKind(row) !== "") return false;
  return STATES[verb].has(row.state);
}

// needsForce says whether removing the container needs Force: it is running (or
// stopping, or paused), which the engine refuses to remove otherwise.
export function needsForce(row: Pick<ContainerRow, "state">): boolean {
  return STATES.stop.has(row.state);
}
