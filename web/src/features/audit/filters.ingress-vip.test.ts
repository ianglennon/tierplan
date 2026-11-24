// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import audit from "@/i18n/en/audit.json";
import { actionLabel, ENV_SCOPED_ACTIONS, KNOWN_ACTIONS } from "./filters";

// The virtual IP actions of Phase 26 (A-14, UI-SPEC S2). The labels come from the real English catalogue, so a
// missing label reads back as the raw key and fails here.
describe("actionLabel (ingress virtual IP)", () => {
  const real = (key: string) => {
    const parts = key.split(".");
    let node: unknown = audit;
    for (const part of parts) node = (node as Record<string, unknown>)?.[part];
    return typeof node === "string" ? node : key;
  };

  const VIP_ACTIONS = [
    "ingress.vip_enabled",
    "ingress.vip_changed",
    "ingress.vip_disabled",
    "ingress.vip_handoff",
  ];

  it("lists every virtual IP action in the Action filter and labels it", () => {
    for (const key of VIP_ACTIONS) {
      expect(KNOWN_ACTIONS, key).toContain(key);
      expect(actionLabel(key, real), key).not.toBe(key);
    }
  });

  it("scopes every virtual IP action to an environment", () => {
    for (const key of VIP_ACTIONS) expect(ENV_SCOPED_ACTIONS, key).toContain(key);
  });

  it("reads the labels exactly", () => {
    expect(actionLabel("ingress.vip_enabled", real)).toBe("Turned on ingress virtual address");
    expect(actionLabel("ingress.vip_changed", real)).toBe("Changed ingress virtual address");
    expect(actionLabel("ingress.vip_disabled", real)).toBe("Turned off ingress virtual address");
    expect(actionLabel("ingress.vip_handoff", real)).toBe(
      "Moved ingress virtual address off a node before draining it",
    );
  });
});
