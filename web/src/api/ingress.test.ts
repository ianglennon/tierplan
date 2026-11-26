// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import {
  INGRESS_SETTLING_REFETCH_MS,
  type IngressRoutes,
  ingressRefetchInterval,
  routesSettling,
} from "@/api/ingress";
import { LIST_REFETCH_MS } from "@/api/resources";

// The Ingress page's cadence (UI-SPEC S1, D-24/A-09): a pure function of what the page holds and whether it is
// visible, tested with fixed inputs and no timers.

function routes(...rows: { state: string; stopped?: boolean; kind?: string }[]): IngressRoutes {
  return {
    enabled: true,
    routes: rows.map((r, i) => ({
      kind: r.kind ?? "route",
      hostname: `h${i}.example.com`,
      path: "/",
      ownerKind: "stack",
      port: 8080,
      reason: "",
      scheme: "http",
      service: "web",
      stackId: "s1",
      stackName: "shop",
      state: r.state,
      stopped: r.stopped ?? false,
      strip: false,
      conditions: [],
    })),
  } as unknown as IngressRoutes;
}

describe("ingressRefetchInterval", () => {
  it("is off while the page is hidden, however much is settling", () => {
    expect(ingressRefetchInterval(true, false)).toBe(false);
    expect(ingressRefetchInterval(false, false)).toBe(false);
  });

  it("is 10 s while a route settles and 30 s otherwise, while visible", () => {
    expect(ingressRefetchInterval(true, true)).toBe(10_000);
    expect(ingressRefetchInterval(false, true)).toBe(30_000);
    expect(INGRESS_SETTLING_REFETCH_MS).toBe(10_000);
    expect(LIST_REFETCH_MS).toBe(30_000);
  });
});

describe("routesSettling", () => {
  it.each(["pending", "waiting_certificate", "not_attached", "unknown"])(
    "is true for a %s route",
    (state) => {
      expect(routesSettling(routes({ state }))).toBe(true);
    },
  );

  it("is true for a running stack's route with no backend running", () => {
    expect(routesSettling(routes({ state: "backend_not_running", stopped: false }))).toBe(true);
  });

  it("is false for a serving route and for one switched off with ingress", () => {
    expect(routesSettling(routes({ state: "serving" }))).toBe(false);
    expect(routesSettling(routes({ state: "ingress_off" }))).toBe(false);
  });

  it("is false for a stopped stack's rows, which are stopped on purpose", () => {
    expect(
      routesSettling(
        routes(
          { state: "backend_not_running", stopped: true },
          { state: "not_attached", stopped: true },
        ),
      ),
    ).toBe(false);
  });

  it("is true when any one route settles among settled ones", () => {
    expect(routesSettling(routes({ state: "serving" }, { state: "pending" }))).toBe(true);
  });

  it("is false with no answer yet, no routes, and rows that are not routes", () => {
    expect(routesSettling(undefined)).toBe(false);
    expect(routesSettling(routes())).toBe(false);
    expect(
      routesSettling(routes({ kind: "claim", state: "" }, { kind: "reserved", state: "" })),
    ).toBe(false);
  });
});
