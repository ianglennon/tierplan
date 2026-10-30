// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { myShellsQuery, SHELLS_POLL_MS } from "@/api/shells";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { YourShellsPopover } from "@/features/terminal/your-shells-popover";
import "@/i18n";
import { makeMe } from "@/test-me";
import { settled } from "@/test-settle";

// "Your shells ({n})" on the Shell tab (plan 04-25, OBS-03, D-31, D-37).

const ENV = "e1";

// Fixture dates count from one fixed instant, and beforeEach freezes Date at it, so a
// minute-granularity "N min ago" is exact however late the run starts. Only Date is faked:
// findBy and waitFor and the app's timers keep running in real time.
const NOW = new Date("2026-10-03T12:00:00Z");
function mine(over: Record<string, unknown> = {}) {
  return {
    id: "s1",
    user: "olive",
    containerId: "w".repeat(64),
    container: "web",
    node: "node-1",
    startedAt: new Date(NOW.getTime() - 5 * 60_000).toISOString(),
    idleSeconds: 10,
    ...over,
  };
}

let items: Array<Record<string, unknown>>;
let me: Record<string, unknown>;
let listStatus: number;
let endStatus: number;
let asked: string[];
let ended: string[];
// How many times /api/me was answered: the shells list is only asked for once it says who may.
let meServed: number;

const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  items = [
    mine(),
    mine({ id: "s2", startedAt: new Date(NOW.getTime() - 90 * 60_000).toISOString() }),
  ];
  me = makeMe({ username: "olive", id: "u-olive", envs: { e1: "admin" } });
  listStatus = 200;
  endStatus = 204;
  asked = [];
  ended = [];
  meServed = 0;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") {
        meServed++;
        return Response.json(me);
      }
      if (url.pathname === `/api/env/${ENV}/shells` && req.method === "GET") {
        asked.push(url.search);
        return listStatus === 200
          ? Response.json({ items })
          : Response.json({ title: "no" }, { status: listStatus });
      }
      if (url.pathname.startsWith(`/api/env/${ENV}/shells/`) && req.method === "DELETE") {
        const id = url.pathname.split("/").pop() as string;
        if (endStatus === 204) {
          ended.push(id);
          items = items.filter((s) => s.id !== id);
          return new Response(null, { status: 204 });
        }
        if (endStatus === 404) items = items.filter((s) => s.id !== id);
        return Response.json(
          endStatus === 404
            ? { type: "shell_not_found", title: "Not Found", status: 404 }
            : { title: "boom", status: endStatus },
          { status: endStatus },
        );
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  toast.dismiss();
  queryClient.setDefaultOptions(defaultQueryOptions);
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderPopover() {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <div data-testid="host">
          <YourShellsPopover envId={ENV} containerId="web" />
        </div>
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

async function openPopover() {
  const trigger = await screen.findByRole("button", { name: /^Your shells/ });
  fireEvent.click(trigger);
  return screen.findByRole("list", { name: "Your open shells" });
}

describe("the Your shells popover", () => {
  it("shows Your shells (2) with the container, when it started and End session for each", async () => {
    renderPopover();
    const trigger = await screen.findByRole("button", { name: "Your shells (2)" });
    expect(trigger).toBeTruthy();
    const list = await openPopover();
    const rows = within(list).getAllByRole("listitem") as [HTMLElement, HTMLElement];
    expect(rows.length).toBe(2);
    expect(within(rows[0]).getByText("web")).toBeTruthy();
    expect(within(rows[0]).getByText("5 min ago")).toBeTruthy();
    expect(within(rows[1]).getByText(/ h ago$/)).toBeTruthy();
    expect(within(rows[0]).getByRole("button", { name: "End session in web" })).toBeTruthy();
    // Only this container's, asked for by name.
    expect(asked[0]).toBe("?containerId=web");
  });

  it("is w-80 with a height of at most 320px that scrolls inside", async () => {
    renderPopover();
    await openPopover();
    const content = document.querySelector('[data-slot="popover-content"]') as HTMLElement;
    expect(content.className).toContain("w-80");
    expect(content.className).toContain("max-h-80");
    expect(content.className).toContain("overflow-y-auto");
  });

  it("is not there at all when the person has no shell open", async () => {
    items = [];
    renderPopover();
    await waitFor(() => expect(asked.length).toBeGreaterThan(0));
    expect(screen.queryByRole("button", { name: /^Your shells/ })).toBeNull();
    expect(screen.getByTestId("host").childElementCount).toBe(0);
  });

  it("is not there when the list cannot be read", async () => {
    listStatus = 500;
    renderPopover();
    await waitFor(() => expect(asked.length).toBeGreaterThan(0));
    expect(screen.queryByRole("button", { name: /^Your shells/ })).toBeNull();
  });

  it("does not ask for the list of someone who may not open shells", async () => {
    me = makeMe({ username: "vera", id: "u-vera", envs: { e1: "viewer" } });
    renderPopover();
    // Who the person is has been answered, and nothing is still running, before "nothing asked".
    await waitFor(() => expect(meServed).toBeGreaterThan(0));
    await settled(queryClient);
    expect(asked).toEqual([]);
    expect(screen.queryByRole("button", { name: /^Your shells/ })).toBeNull();
  });

  it("ends the session on End session, says so, and the trigger counts one fewer", async () => {
    renderPopover();
    const list = await openPopover();
    fireEvent.click(
      within(list).getAllByRole("button", { name: "End session in web" })[0] as HTMLElement,
    );
    await waitFor(() => expect(ended).toEqual(["s1"]));
    expect(
      (await screen.findAllByText("Session ended.", { selector: "[data-sonner-toast] *" })).length,
    ).toBeGreaterThan(0);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Your shells (1)" })).toBeTruthy(),
    );
  });

  it("says Shell already closed. when it had closed already, and the row goes", async () => {
    endStatus = 404;
    renderPopover();
    const list = await openPopover();
    fireEvent.click(
      within(list).getAllByRole("button", { name: "End session in web" })[0] as HTMLElement,
    );
    expect(
      (await screen.findAllByText("Shell already closed.", { selector: "[data-sonner-toast] *" }))
        .length,
    ).toBeGreaterThan(0);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Your shells (1)" })).toBeTruthy(),
    );
  });

  it("is read again every 15 seconds and when the window is focused", () => {
    const q = myShellsQuery(ENV, "web");
    expect(SHELLS_POLL_MS).toBe(15_000);
    expect(q.refetchInterval).toBe(15_000);
    expect(q.refetchOnWindowFocus).toBe(true);
  });
});
