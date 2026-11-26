// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { keyExists } from "@/i18n/use-t";
import "@/i18n";

import { type RouteHeadlineState, routeHeadline } from "./route-state";

const STATES: RouteHeadlineState[] = [
  "serving",
  "pending",
  "waiting_certificate",
  "backend_not_running",
  "not_attached",
  "ingress_off",
  "unknown",
];

describe("routeHeadline", () => {
  it.each([
    ["serving", false, "online"],
    ["pending", false, "reconnecting"],
    ["waiting_certificate", false, "degraded"],
    ["not_attached", false, "degraded"],
    ["backend_not_running", true, "unknown"],
    ["backend_not_running", false, "offline"],
    ["ingress_off", false, "pending"],
    ["unknown", false, "unknown"],
  ] as const)("%s (stopped %s) draws the %s dot", (state, stopped, dot) => {
    expect(routeHeadline(state, stopped).dot).toBe(dot);
  });

  it("keeps a stopped stack's other states as they are", () => {
    expect(routeHeadline("serving", true).dot).toBe("online");
    expect(routeHeadline("unknown", true).dot).toBe("unknown");
  });

  it("never draws an unread state as a healthy colour", () => {
    expect(routeHeadline("unknown", false).dot).not.toBe("online");
  });

  it("names the unknown state's label key", () => {
    expect(routeHeadline("unknown", false).labelKey).toBe(
      "stackIngress.routes.state.unknown.label",
    );
  });

  it.each(STATES)("the %s label key resolves in the catalogue", (state) => {
    const { labelKey } = routeHeadline(state, false);
    expect(keyExists(labelKey)).toBe(true);
  });
});
