// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { FleetSummary } from "@/api/fleet";
import { TooltipProvider } from "@/components/ui/tooltip";
import { fleetSearchSchema } from "@/features/fleet/fleet-search";
import { FleetTiles } from "@/features/fleet/fleet-tiles";
import "@/i18n";

afterEach(cleanup);

const SUMMARY: FleetSummary = {
  environments: { total: 6, needAttention: 2 },
  failing: { count: 3, environments: 2 },
  updates: { count: 12, checksFailed: 0, allChecked: false, lastCheckedAt: "" },
  pools: { nearFull: 4, almostFull: 1 },
  notIncluded: [],
  readAt: "2026-11-04T10:00:00Z",
};

function mount(props: React.ComponentProps<typeof FleetTiles>, at = "/environments") {
  const root = createRootRoute();
  const page = createRoute({
    getParentRoute: () => root,
    path: "/environments",
    validateSearch: fleetSearchSchema,
    component: () => <FleetTiles {...props} />,
  });
  const router = createRouter({
    routeTree: root.addChildren([page]),
    history: createMemoryHistory({ initialEntries: [at] }),
  });
  render(
    <TooltipProvider>
      <RouterProvider router={router as never} />
    </TooltipProvider>,
  );
}

const tile = (label: string) =>
  screen.findByRole("link", { name: new RegExp(`^${label}`) }) as Promise<HTMLAnchorElement>;

describe("FleetTiles", () => {
  it("shows the four values and sub-lines, each tile a link to its tab", async () => {
    mount({ summary: SUMMARY });
    const environments = await tile("Environments");
    expect(environments.textContent).toBe("Environments62 need attention");
    expect(environments.getAttribute("href")).toContain("tab=environments");
    const failing = await tile("Failing services");
    expect(failing.textContent).toBe("Failing services3in 2 environments");
    expect(failing.getAttribute("href")).toContain("tab=failing");
    const updates = await tile("Updates available");
    expect(updates.textContent).toBe("Updates available12Not every environment is checked yet");
    expect(updates.getAttribute("href")).toContain("tab=updates");
    const pools = await tile("Pools near full");
    expect(pools.textContent).toBe("Pools near full41 almost full");
    expect(pools.getAttribute("href")).toContain("tab=pools");
  });

  it("marks the tile of the tab that is showing with aria-current, and only that one", async () => {
    mount({ summary: SUMMARY }, "/environments?tab=failing");
    const failing = await tile("Failing services");
    expect(failing.getAttribute("aria-current")).toBe("page");
    for (const other of ["Environments", "Updates available", "Pools near full"]) {
      expect((await tile(other)).getAttribute("aria-current")).toBeNull();
    }
  });

  it("marks the Environments tile when the address names no tab", async () => {
    mount({ summary: SUMMARY });
    expect((await tile("Environments")).getAttribute("aria-current")).toBe("page");
    expect((await tile("Failing services")).getAttribute("aria-current")).toBeNull();
  });

  it("keeps the environment filter in every link", async () => {
    const id = "0190a1b2-0000-7000-8000-000000000001";
    mount(
      { summary: SUMMARY, env: [id] },
      `/environments?tab=failing&env=${encodeURIComponent(JSON.stringify([id]))}`,
    );
    const pools = await tile("Pools near full");
    expect(decodeURIComponent(pools.getAttribute("href") ?? "")).toContain(id);
  });

  it("says None failing and All under 70% for a fleet in good health, with a 0 that is a 0", async () => {
    mount({
      summary: {
        ...SUMMARY,
        environments: { total: 2, needAttention: 0 },
        failing: { count: 0, environments: 0 },
        pools: { nearFull: 0, almostFull: 0 },
      },
    });
    expect((await tile("Environments")).textContent).toContain("All reporting");
    expect((await tile("Failing services")).textContent).toBe("Failing services0None failing");
    expect((await tile("Pools near full")).textContent).toBe("Pools near full0All under 70%");
  });

  it("names checks that failed, and says when every check succeeded", async () => {
    mount({ summary: { ...SUMMARY, updates: { ...SUMMARY.updates, checksFailed: 2 } } });
    expect((await tile("Updates available")).textContent).toContain("2 checks failed");
    cleanup();
    mount({
      summary: {
        ...SUMMARY,
        updates: {
          count: 0,
          checksFailed: 0,
          allChecked: true,
          lastCheckedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
        },
      },
    });
    expect((await tile("Updates available")).textContent).toContain("Checked 5 min ago");
  });

  it("shows a dash and Couldn't read this for a count that is null, never a 0", async () => {
    mount({
      summary: {
        ...SUMMARY,
        failing: { count: null, environments: null },
        updates: { ...SUMMARY.updates, count: null, checksFailed: null },
      },
    });
    const failing = await tile("Failing services");
    expect(failing.textContent).toBe("Failing services—Couldn't read this.");
    expect((await tile("Updates available")).textContent).toBe(
      "Updates available—Couldn't read this.",
    );
    // The tiles that were read stand.
    expect((await tile("Pools near full")).textContent).toContain("4");
  });

  it("does not take a 0 for a fact while an environment was not read", async () => {
    mount({
      summary: {
        ...SUMMARY,
        failing: { count: 0, environments: 0 },
        notIncluded: [{ envId: "x", envName: "prod", reason: "offline", nodes: 0 }],
      },
    });
    const failing = await tile("Failing services");
    expect(failing.textContent).toBe("Failing services—Some environments aren't included");
  });

  it("does not call pools none almost full, or say where failures are, while an environment was not read", async () => {
    mount({
      summary: {
        ...SUMMARY,
        pools: { nearFull: 4, almostFull: 0 },
        notIncluded: [{ envId: "x", envName: "prod", reason: "offline", nodes: 0 }],
      },
    });
    // The counts that were read stand, with the note that they are not the whole fleet.
    expect((await tile("Pools near full")).textContent).toBe(
      "Pools near full4Some environments aren't included",
    );
    expect((await tile("Failing services")).textContent).toBe(
      "Failing services3Some environments aren't included",
    );
  });

  it("still says how many pools are almost full while an environment was not read", async () => {
    mount({
      summary: {
        ...SUMMARY,
        notIncluded: [{ envId: "x", envName: "prod", reason: "offline", nodes: 0 }],
      },
    });
    expect((await tile("Pools near full")).textContent).toBe("Pools near full41 almost full");
  });

  it("shows a dash on every tile when the summary could not be read", async () => {
    mount({ failed: true });
    for (const label of [
      "Environments",
      "Failing services",
      "Updates available",
      "Pools near full",
    ]) {
      expect((await tile(label)).textContent).toBe(`${label}—Couldn't read this.`);
    }
  });

  it("shows skeleton values while the first answer is awaited", async () => {
    mount({ loading: true });
    const failing = await tile("Failing services");
    expect(failing.getAttribute("aria-busy")).toBe("true");
    expect(failing.querySelector("[data-slot=skeleton]")).not.toBeNull();
    expect(failing.textContent).toBe("Failing services");
  });
});
