// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import audit from "@/i18n/en/audit.json";
import { actionLabel, KNOWN_ACTIONS } from "./filters";

// The ingress node lifecycle and join-token actions of Phase 22 (D-27). The labels come from the real English
// catalogue, so a missing label reads back as the raw key and fails here.
describe("actionLabel (ingress nodes and join token)", () => {
  const real = (key: string) => {
    const parts = key.split(".");
    let node: unknown = audit;
    for (const part of parts) node = (node as Record<string, unknown>)?.[part];
    return typeof node === "string" ? node : key;
  };

  const PHASE_22_ACTIONS = [
    "ingress.node_revoked",
    "ingress.join_token_created",
    "ingress.join_token_rotated",
    "ingress.join_token_disabled",
    "ingress.node_enrol_refused",
  ];

  it("lists every phase-22 action in the Action filter and labels it", () => {
    for (const key of PHASE_22_ACTIONS) {
      expect(KNOWN_ACTIONS, key).toContain(key);
      expect(actionLabel(key, real), key).not.toBe(key);
    }
  });

  it("reads the revoked and join-token labels exactly", () => {
    expect(actionLabel("ingress.node_revoked", real)).toBe("Revoked ingress node");
    expect(actionLabel("ingress.join_token_created", real)).toBe("Created ingress join token");
    expect(actionLabel("ingress.join_token_rotated", real)).toBe("Rotated ingress join token");
    expect(actionLabel("ingress.join_token_disabled", real)).toBe("Disabled ingress join token");
  });
});
