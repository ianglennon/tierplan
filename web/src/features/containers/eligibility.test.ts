// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { eligibleFor, needsForce, ownKind, type Verb } from "@/features/containers/eligibility";

type State = "created" | "running" | "paused" | "restarting" | "removing" | "exited" | "dead";

const STATES: State[] = [
  "created",
  "running",
  "paused",
  "restarting",
  "removing",
  "exited",
  "dead",
];

const subject = (
  state: State,
  extra: { selfKind?: "" | "server" | "agent"; isSelf?: boolean } = {},
) => ({
  state,
  selfKind: extra.selfKind ?? "",
  isSelf: extra.isSelf ?? false,
});

const statesFor = (verb: Verb) => STATES.filter((s) => eligibleFor(verb, subject(s)));

describe("eligibleFor", () => {
  it("starts exited and created containers", () => {
    expect(statesFor("start")).toEqual(["created", "exited"]);
  });

  it("stops and kills running, restarting and paused containers", () => {
    expect(statesFor("stop")).toEqual(["running", "paused", "restarting"]);
    expect(statesFor("kill")).toEqual(["running", "paused", "restarting"]);
  });

  it("restarts running, exited, paused and restarting containers", () => {
    expect(statesFor("restart")).toEqual(["running", "paused", "restarting", "exited"]);
  });

  it("pauses running containers and unpauses paused ones", () => {
    expect(statesFor("pause")).toEqual(["running"]);
    expect(statesFor("unpause")).toEqual(["paused"]);
  });

  it("removes a container in any state but the one that is already being removed", () => {
    expect(statesFor("remove")).toEqual([
      "created",
      "running",
      "paused",
      "restarting",
      "exited",
      "dead",
    ]);
  });

  it("never offers an action on a container that is being removed", () => {
    for (const verb of [
      "start",
      "stop",
      "kill",
      "restart",
      "pause",
      "unpause",
      "remove",
    ] as Verb[]) {
      expect(eligibleFor(verb, subject("removing"))).toBe(false);
    }
  });

  it("lets one of TierPlan's own containers take an action on its own, never in a bulk", () => {
    const server = subject("running", { selfKind: "server" });
    const agent = subject("running", { selfKind: "agent" });
    const legacy = subject("running", { isSelf: true });
    for (const own of [server, agent, legacy]) {
      expect(eligibleFor("stop", own)).toBe(true);
      expect(eligibleFor("stop", own, { bulk: true })).toBe(false);
      expect(eligibleFor("remove", own, { bulk: true })).toBe(false);
    }
    expect(eligibleFor("stop", subject("running"), { bulk: true })).toBe(true);
  });
});

describe("ownKind", () => {
  it("names the kind from selfKind, and treats the older isSelf flag as the server", () => {
    expect(ownKind({ selfKind: "agent", isSelf: false })).toBe("agent");
    expect(ownKind({ selfKind: "server", isSelf: false })).toBe("server");
    expect(ownKind({ selfKind: "", isSelf: true })).toBe("server");
    expect(ownKind({ selfKind: "", isSelf: false })).toBe("");
  });
});

describe("needsForce", () => {
  it("is true for a container the engine will not remove without Force", () => {
    expect(STATES.filter((s) => needsForce({ state: s }))).toEqual([
      "running",
      "paused",
      "restarting",
    ]);
  });
});
