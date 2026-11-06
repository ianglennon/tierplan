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

import { TooltipProvider } from "@/components/ui/tooltip";
import { UpdatesTile } from "@/features/updates/updates-tile";
import "@/i18n";

afterEach(cleanup);

function show(tile: React.ReactNode) {
  const root = createRootRoute({ component: () => tile });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/environments" }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <TooltipProvider>
      <RouterProvider router={router as never} />
    </TooltipProvider>,
  );
}

describe("UpdatesTile", () => {
  it("shows the count and links to the Updates tab of this environment", async () => {
    show(<UpdatesTile envId="e1" value={3} />);
    const link = await screen.findByRole("link", { name: /Updates available/ });
    expect(link.textContent).toContain("3");
    expect(link.textContent).toContain("In this environment");
    const href = link.getAttribute("href") ?? "";
    expect(href).toContain("/environments");
    expect(href).toContain("tab=updates");
    // The route takes the environments as a list.
    expect(decodeURIComponent(href)).toContain('env=["e1"]');
  });

  it("shows a real zero as 0", async () => {
    show(<UpdatesTile envId="e1" value={0} />);
    const link = await screen.findByRole("link", { name: /Updates available/ });
    expect(link.textContent).toContain("0");
    expect(link.textContent).not.toContain("—");
  });

  it("shows a dash and says it could not be read when the count is absent, never 0", async () => {
    show(<UpdatesTile envId="e1" />);
    const link = await screen.findByRole("link", { name: /Updates available/ });
    expect(link.textContent).toContain("—");
    expect(link.textContent).toContain("Couldn't read this.");
    expect(link.textContent).not.toMatch(/\b0\b/);
  });

  it("shows no verdict while the summary loads", async () => {
    show(<UpdatesTile envId="e1" loading />);
    const link = await screen.findByRole("link", { name: /Updates available/ });
    expect(link.textContent).not.toContain("Couldn't read this.");
    expect(link.getAttribute("aria-busy")).toBe("true");
  });
});
