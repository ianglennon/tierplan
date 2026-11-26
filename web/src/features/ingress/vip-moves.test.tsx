// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { VIPMoves } from "@/features/ingress/vip-moves";
import "@/i18n";

const ENV = "e1";

type Json = Record<string, unknown>;

let pages: (url: URL) => Response;
let requests: URL[];
let client: QueryClient;

function move(i: number, over: Json = {}): Json {
  return {
    at: `2026-11-20T10:${String(59 - i).padStart(2, "0")}:00Z`,
    fromNodeId: `sw-${i}`,
    fromNode: `from-${i}`,
    toNodeId: `sw-${i + 1}`,
    toNode: `to-${i}`,
    reason: "holder_failed",
    ...over,
  };
}

const range = (from: number, to: number, over: Json = {}) =>
  Array.from({ length: to - from }, (_, k) => move(from + k, over));

beforeEach(() => {
  setCsrfToken(null);
  requests = [];
  pages = () => Response.json({ items: [] });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === `/api/env/${ENV}/ingress/vip/moves`) {
        requests.push(url);
        return pages(url);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <VIPMoves envId={ENV} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const moves = () => screen.findByRole("table", { name: "Address moves" });
const bodyRows = (t: HTMLElement) => within(t).getAllByRole("row").slice(1);

describe("the virtual IP's history of moves", () => {
  it("reads the newest 10, newest first, with a Show more while there is a next page", async () => {
    pages = () => Response.json({ items: range(0, 10), next: "c1" });
    mount();
    const t = await moves();
    expect(screen.getByRole("heading", { name: "Address moves", level: 3 })).toBeTruthy();
    const rows = bodyRows(t);
    expect(rows).toHaveLength(10);
    expect(within(rows[0] as HTMLElement).getByText("from-0 to to-0")).toBeTruthy();
    expect(within(rows[9] as HTMLElement).getByText("from-9 to to-9")).toBeTruthy();
    expect(requests[0]?.searchParams.get("limit")).toBe("10");
    expect(requests[0]?.searchParams.get("before")).toBeNull();
    expect(screen.getByRole("button", { name: "Show more" })).toBeTruthy();
  });

  it("appends the next page from the cursor, and drops the button when there is no next", async () => {
    pages = (url) =>
      url.searchParams.get("before") === "c1"
        ? Response.json({ items: range(10, 13) })
        : Response.json({ items: range(0, 10), next: "c1" });
    mount();
    const t = await moves();
    fireEvent.click(screen.getByRole("button", { name: "Show more" }));
    await waitFor(() => expect(bodyRows(t)).toHaveLength(13));
    expect(requests[1]?.searchParams.get("before")).toBe("c1");
    expect(within(bodyRows(t)[12] as HTMLElement).getByText("from-12 to to-12")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
  });

  it("says the address hasn't moved when it hasn't, with no table", async () => {
    mount();
    expect(await screen.findByText("The address hasn't moved.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
  });

  it("writes an empty side of a move as no node", async () => {
    pages = () =>
      Response.json({
        items: [
          move(0, { fromNode: "", fromNodeId: "", toNode: "node-b" }),
          move(1, { fromNode: "node-a", toNode: "", toNodeId: "", reason: "disabled" }),
        ],
      });
    mount();
    const t = await moves();
    expect(within(t).getByText("no node to node-b")).toBeTruthy();
    expect(within(t).getByText("node-a to no node")).toBeTruthy();
  });

  it("gives each of the five reasons in words, and a code it doesn't know as the unknown words", async () => {
    pages = () =>
      Response.json({
        items: [
          move(0, { reason: "holder_failed" }),
          move(1, { reason: "probe_failing" }),
          move(2, { reason: "drain_handoff" }),
          move(3, { reason: "disabled" }),
          move(4, { reason: "unknown" }),
          move(5, { reason: "from_a_newer_server" }),
        ],
      });
    mount();
    const t = await moves();
    expect(within(t).getByText("The previous holder stopped answering.")).toBeTruthy();
    expect(within(t).getByText("The previous holder's proxy stopped answering.")).toBeTruthy();
    expect(within(t).getByText("The previous holder was drained.")).toBeTruthy();
    expect(within(t).getByText("The virtual IP was turned off.")).toBeTruthy();
    expect(within(t).getAllByText("TierPlan couldn't tell why.")).toHaveLength(2);
  });

  it("draws the time of each move as a time element", async () => {
    pages = () => Response.json({ items: [move(0)] });
    mount();
    const t = await moves();
    expect(t.querySelector("time[datetime='2026-11-20T10:59:00Z']")).not.toBeNull();
  });

  it("draws the error with its retry, and reads again when it is pressed", async () => {
    pages = () => Response.json({ title: "boom" }, { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't read the address moves.")).toBeTruthy();
    pages = () => Response.json({ items: [move(0)] });
    fireEvent.click(screen.getByRole("button", { name: "Retry reading the address moves" }));
    expect(await moves()).toBeTruthy();
  });
});
