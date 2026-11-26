// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import audit from "@/i18n/en/audit.json";
import { actionLabel, ENV_SCOPED_ACTIONS, KNOWN_ACTIONS } from "./filters";

// The two actions the ingress pages of Phase 27 close the list with (D-21, D-22, UI-SPEC S9). The labels come from the
// real English catalogue, so a missing label reads back as the raw key and fails here.
describe("actionLabel (ingress pages)", () => {
  const real = (key: string) => {
    const parts = key.split(".");
    let node: unknown = audit;
    for (const part of parts) node = (node as Record<string, unknown>)?.[part];
    return typeof node === "string" ? node : key;
  };

  const PAGES_ACTIONS = ["ingress.claimed", "ingress.vip_moved"];

  it("lists both actions in the Action filter and labels them", () => {
    for (const key of PAGES_ACTIONS) {
      expect(KNOWN_ACTIONS, key).toContain(key);
      expect(actionLabel(key, real), key).not.toBe(key);
    }
  });

  it("scopes both actions to an environment", () => {
    for (const key of PAGES_ACTIONS) expect(ENV_SCOPED_ACTIONS, key).toContain(key);
  });

  it("reads the labels exactly", () => {
    expect(actionLabel("ingress.claimed", real)).toBe("Claimed an ingress hostname");
    expect(actionLabel("ingress.vip_moved", real)).toBe(
      "Ingress virtual address moved to another node",
    );
  });
});
