// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UnknownAgentsCard } from "@/features/environments/unknown-agents-card";
import { meQueryOptions } from "@/hooks/use-me";
import "@/i18n";
import { makeMe } from "@/test-me";

const NOW = "2026-10-03T12:00:00Z";

function item(extra: Record<string, unknown> = {}) {
  return {
    envId: "e1",
    envName: "Production",
    node: "0198a1b2-agent-1",
    ip: "198.51.100.7",
    lastAttempt: "2026-10-03T11:55:00Z",
    ...extra,
  };
}

let me = makeMe({ instanceAdmin: true });
let items: ReturnType<typeof item>[];
let listCalls: number;
let posts: string[];

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(NOW));
  me = makeMe({ instanceAdmin: true });
  items = [item()];
  listCalls = 0;
  posts = [];
  setCsrfToken("csrf-1");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (req.method === "POST") posts.push(path);
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments/unknown-agents") {
        listCalls++;
        return Response.json({ items });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function mount() {
  queryClient.setQueryData(meQueryOptions.queryKey, me);
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <UnknownAgentsCard />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

describe("UnknownAgentsCard", () => {
  it("lists each agent with its node, environment, last attempt and address, and a Show install command action", async () => {
    mount();
    expect(
      await screen.findByRole("heading", { name: "Agents TierPlan doesn't know" }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "These agents were installed after the backup was taken, so this TierPlan refused them. Reinstall each one with its environment's install command.",
      ),
    ).toBeTruthy();
    const table = screen.getByRole("table", { name: "Agents TierPlan doesn't know" });
    for (const name of ["Node", "Environment", "Last attempt", "IP address"]) {
      expect(within(table).getByRole("columnheader", { name })).toBeTruthy();
    }
    const row = within(table).getByRole("row", { name: /0198a1b2-agent-1/ });
    expect(within(row).getByText("Production")).toBeTruthy();
    expect(within(row).getByText("5 min ago")).toBeTruthy();
    expect(within(row).getByText("198.51.100.7")).toBeTruthy();
    expect(within(row).getByRole("button", { name: /^Show install command/ })).toBeTruthy();
  });

  it("renders nothing while the list is empty", async () => {
    items = [];
    const { container } = mount();
    await waitFor(() => expect(listCalls).toBe(1));
    expect(container.textContent).toBe("");
  });

  it("opens the install dialog of that agent's environment without making a token", async () => {
    mount();
    fireEvent.click(await screen.findByRole("button", { name: /^Show install command/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("button", { name: "Generate new token" })).toBeTruthy();
    expect(posts).toEqual([]);
  });

  it("does not read the list for anyone who is not an instance admin", async () => {
    me = makeMe({ envs: { e1: "admin" } });
    const { container } = mount();
    await Promise.resolve();
    await Promise.resolve();
    expect(listCalls).toBe(0);
    expect(container.textContent).toBe("");
  });

  it("cuts a long node name to its column with the whole name in a tooltip", async () => {
    const long = `0198a1b2-${"agent".repeat(30)}`;
    items = [item({ node: long })];
    mount();
    const cell = await screen.findByText(long);
    expect(cell.className).toContain("truncate");
    fireEvent.focus(cell);
    expect((await screen.findAllByText(long)).length).toBeGreaterThan(1);
  });

  it("keeps the table inside its card, scrolling sideways instead of widening the page", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Agents TierPlan doesn't know" });
    const container = table.closest('[data-slot="table-container"]');
    expect(container?.className).toContain("overflow-x-auto");
    expect(container?.closest('[data-slot="card-content"]')?.className).toContain("min-w-0");
  });

  it("hides Show install command from a caller without environments.token", async () => {
    // A user who can reach the card's data but may not make tokens: the list is
    // the server's to refuse, so this is only about the action.
    me = makeMe({ instanceAdmin: true, permissions: { global: [], environments: {} } });
    mount();
    await screen.findByRole("heading", { name: "Agents TierPlan doesn't know" });
    expect(screen.queryByRole("button", { name: /Show install command/ })).toBeNull();
  });
});
