// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { CountTile } from "@/components/count-tile";
import "@/i18n";

function renderTile(tile: React.ReactNode) {
  const router = createRouter({
    routeTree: createRootRoute({ component: () => tile }),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  // The tile's `to` is typed against the app's routes; the test router is bare.
  render(<RouterProvider router={router as never} />);
}

afterEach(cleanup);

describe("CountTile", () => {
  it("is a focusable link with a hover background when given a destination", async () => {
    renderTile(<CountTile label="Images" value={12} to="/" />);
    const link = await screen.findByRole("link", { name: /Images/ });
    expect(link.className).toContain("hover:bg-muted");
    expect(link.className).toContain("focus-visible:ring-2");
  });

  it("is static without one: no link, no hover state", async () => {
    renderTile(<CountTile label="Nodes" value={3} />);
    const label = await screen.findByText("Nodes");
    const frame = label.parentElement as HTMLElement;
    expect(screen.queryByRole("link")).toBeNull();
    expect(frame.className).not.toContain("hover:");
    expect(frame.tabIndex).toBe(-1);
  });

  it("renders zero as 0, tabular, and a dash when it errored", async () => {
    renderTile(
      <>
        <CountTile label="Volumes" value={0} />
        <CountTile label="Networks" error />
      </>,
    );
    const zero = await screen.findByText("0");
    expect(zero.className).toContain("tabular-nums");
    expect(screen.getByText("—")).toBeTruthy();
  });
});
