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

type Subject = Pick<ContainerRow, "state" | "selfKind" | "isSelf"> &
  Partial<Pick<ContainerRow, "ingressOwned">>;

// readOnlyIngress says whether the container is the standalone ingress proxy TierPlan runs for
// a host (D-09): read-only, every write refused by the server, logs and inspect open. The
// server decides it from the component label, the reserved project and the service together;
// the browser only follows its ingressOwned field and never reads a label itself.
export function readOnlyIngress(row: { ingressOwned?: boolean }): boolean {
  return row.ingressOwned === true;
}

// appliesTo says whether the action fits the container's state, whoever owns it. A surface that
// shows a read-only container's actions disabled, with the reason, asks this; every surface that
// offers an action asks eligibleFor.
export function appliesTo(verb: Verb, row: Pick<ContainerRow, "state">): boolean {
  return STATES[verb].has(row.state);
}

// ownKind says whether a container is one TierPlan itself depends on: the server
// it runs in, an agent, or a task of its ingress proxy or its virtual IP service. Acting on one asks for its
// name to be typed, one at a time (D-11, D-46), and a bulk never includes one (D-12).
export function ownKind(
  row: Pick<ContainerRow, "selfKind" | "isSelf">,
): "server" | "agent" | "ingress" | "ingress_vip" | "" {
  if (
    row.selfKind === "server" ||
    row.selfKind === "agent" ||
    row.selfKind === "ingress" ||
    row.selfKind === "ingress_vip"
  ) {
    return row.selfKind;
  }
  return row.isSelf ? "server" : "";
}

// eligibleFor says whether the action applies to the container in its state.
// With bulk set, one of TierPlan's own containers never does, and the standalone ingress proxy
// never does for any action (D-09).
export function eligibleFor(verb: Verb, row: Subject, opts: { bulk?: boolean } = {}): boolean {
  if (readOnlyIngress(row)) return false;
  if (opts.bulk && ownKind(row) !== "") return false;
  return appliesTo(verb, row);
}

// needsForce says whether removing the container needs Force: it is running (or
// stopping, or paused), which the engine refuses to remove otherwise.
export function needsForce(row: Pick<ContainerRow, "state">): boolean {
  return STATES.stop.has(row.state);
}
