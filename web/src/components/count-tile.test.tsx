// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { CountTile } from "@/components/count-tile";
import { TooltipProvider } from "@/components/ui/tooltip";
import "@/i18n";

function renderTile(tile: React.ReactNode) {
  const router = createRouter({
    routeTree: createRootRoute({ component: () => tile }),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  // The tile's `to` is typed against the app's routes; the test router is bare.
  render(
    <TooltipProvider>
      <RouterProvider router={router as never} />
    </TooltipProvider>,
  );
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

// The per-node breakdown of a swarm tile (plan 22, D-46): a desktop tooltip, up
// to 8 lines, and nothing added for touch.
describe("CountTile breakdown", () => {
  const nodes = (n: number) =>
    Array.from({ length: n }, (_, i) => ({ node: `n${i + 1}`, value: i + 1 }));

  async function open(label: RegExp) {
    const link = await screen.findByRole("link", { name: label });
    fireEvent.focus(link);
    return screen.findByRole("tooltip");
  }

  it("lists '{node}: {n}' for each node of a small swarm", async () => {
    renderTile(<CountTile label="Containers" value={6} to="/" breakdown={nodes(3)} />);
    const tip = await open(/Containers/);
    expect(tip.textContent).toBe("n1: 1n2: 2n3: 3");
    expect(tip.textContent).not.toContain("more");
  });

  it("lists eight nodes and counts the rest: 'and 2 more nodes' for ten", async () => {
    renderTile(<CountTile label="Containers" value={55} to="/" breakdown={nodes(10)} />);
    const tip = await open(/Containers/);
    expect(tip.textContent).toContain("n8: 8");
    expect(tip.textContent).not.toContain("n9: 9");
    expect(tip.textContent).toContain("and 2 more nodes");
  });

  it("says 'and 1 more node' for nine", async () => {
    renderTile(<CountTile label="Containers" value={45} to="/" breakdown={nodes(9)} />);
    const tip = await open(/Containers/);
    expect(tip.textContent).toContain("and 1 more node");
    expect(tip.textContent).not.toContain("more nodes");
  });

  it("does not make the tile a second tab stop or add a control inside it", async () => {
    renderTile(
      <>
        <CountTile label="Containers" value={6} to="/" breakdown={nodes(3)} />
        <CountTile label="Images" value={6} to="/" />
      </>,
    );
    const withTip = await screen.findByRole("link", { name: /Containers/ });
    const without = screen.getByRole("link", { name: /Images/ });
    expect(screen.getAllByRole("link")).toHaveLength(2);
    expect(withTip.getAttribute("tabindex")).toBe(without.getAttribute("tabindex"));
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("adds no tooltip without a breakdown or with an empty one", async () => {
    renderTile(
      <>
        <CountTile label="Containers" value={6} to="/" />
        <CountTile label="Images" value={6} to="/" breakdown={[]} />
      </>,
    );
    fireEvent.focus(await screen.findByRole("link", { name: /Containers/ }));
    fireEvent.focus(screen.getByRole("link", { name: /Images/ }));
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("shows a hostile node name as text (T-02-75)", async () => {
    const evil = "<img src=x onerror=alert(1)>";
    renderTile(
      <CountTile label="Containers" value={1} to="/" breakdown={[{ node: evil, value: 1 }]} />,
    );
    const tip = await open(/Containers/);
    expect(tip.textContent).toContain(`${evil}: 1`);
    expect(document.querySelector("img[src='x']")).toBeNull();
  });

  it("keeps showing '—' with its line when there is no value", async () => {
    renderTile(<CountTile label="Nodes" subline="No manager connected" />);
    await screen.findByText("Nodes");
    expect(screen.getByText("—")).toBeTruthy();
    expect(screen.getByText("No manager connected")).toBeTruthy();
  });
});

// The restart-loop tile's dot (plan 06-35, RES-13, D-15): an offline dot before the label while the
// value is above zero. The numerals stay the foreground colour.
describe("CountTile dot", () => {
  const dotOf = (label: string) =>
    (screen.getByText(label).closest("[aria-busy], div, a") as HTMLElement).querySelector(
      ".bg-status-offline",
    );

  it("renders an offline dot before the label when the value is above zero", async () => {
    renderTile(<CountTile label="Restart loops" value={2} dot="offline" />);
    await screen.findByText("Restart loops");
    const dot = dotOf("Restart loops");
    expect(dot).not.toBeNull();
    expect(dot?.getAttribute("aria-hidden")).toBe("true");
    const label = screen.getByText("Restart loops");
    expect(dot?.compareDocumentPosition(label)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(screen.getByText("2").className).not.toContain("status-offline");
  });

  it("has no dot at zero, while loading or when the count errored", async () => {
    renderTile(
      <>
        <CountTile label="Zero" value={0} dot="offline" />
        <CountTile label="Loading" value={3} dot="offline" loading />
        <CountTile label="Errored" value={3} dot="offline" error />
        <CountTile label="Plain" value={3} />
      </>,
    );
    await screen.findByText("Zero");
    for (const label of ["Zero", "Loading", "Errored", "Plain"]) {
      expect(dotOf(label), label).toBeNull();
    }
  });

  it("passes a search to its link", async () => {
    renderTile(<CountTile label="Services" value={1} to="/" search={{ status: "loop" }} />);
    const link = await screen.findByRole("link", { name: /Services/ });
    expect(link.getAttribute("href")).toBe("/?status=loop");
  });
});
