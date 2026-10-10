// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import type { EventDTO } from "@/features/dashboard/events";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EventsPage } from "@/features/events/events-page";
import type { EventsSearch } from "@/features/events/filters";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

function ev(id: string, over: Partial<EventDTO> = {}): EventDTO {
  return {
    id,
    node: "node-1",
    time: new Date(Date.now() - 60_000).toISOString(),
    type: "container",
    action: "start",
    actorId: `0123456789abcdef-${id}`,
    name: `name-${id}`,
    attributes: {},
    ...over,
  };
}

let me: ReturnType<typeof makeMe>;
let recent: (url: URL) => Promise<Response>;
let eventUrls: URL[];
let swarm: boolean;

function mount(search: EventsSearch = {}) {
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <TestRouter>
        <TooltipProvider>
          <EventsPage envId={ENV} search={search} onSearchChange={() => {}} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const stream = () => FakeEventSource.instances[0] as FakeEventSource;
const bodyRows = () => within(screen.getByRole("table")).getAllByRole("row").slice(1);

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  me = makeMe({ envs: { [ENV]: "viewer" } });
  swarm = false;
  eventUrls = [];
  recent = async () => Response.json({ items: [] });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: [{ id: ENV, name: "Lab swarm", envType: swarm ? "swarm" : "standalone" }],
        });
      }
      if (url.pathname === `/api/env/${ENV}/events`) {
        eventUrls.push(url);
        return recent(url);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("EventsPage", () => {
  it("shows the title, the caption with the environment's name and the info line", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Events" })).toBeTruthy();
    expect(
      await screen.findByText("What Docker reports on Lab swarm, as it happens."),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "Docker keeps only a short history of events, so older ones may be missing.",
      ),
    ).toBeTruthy();
  });

  it("reads the last hour, up to 1000 events", async () => {
    mount();
    await screen.findByText("No events in this range");
    const url = eventUrls[0] as URL;
    expect(url.searchParams.get("limit")).toBe("1000");
    const since = Date.parse(url.searchParams.get("since") ?? "");
    expect(Math.abs(Date.now() - 60 * 60_000 - since)).toBeLessThan(5_000);
  });

  it("lists the events newest first, then puts a new one at the top, with a Live chip", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("2", {
            name: "web",
            action: "die",
            attributes: { exitCode: "137", image: "nginx:1" },
          }),
          ev("1", { name: "db", action: "start" }),
        ],
      });
    mount();
    expect(await screen.findByText("web")).toBeTruthy();
    const rows = bodyRows().filter((r) => within(r).queryByText(/^(web|db)$/));
    expect(rows.map((r) => within(r).getByText(/^(web|db)$/).textContent)).toEqual(["web", "db"]);
    const first = rows[0] as HTMLElement;
    expect(within(first).getByText("Container")).toBeTruthy();
    expect(within(first).getByText("die")).toBeTruthy();
    expect(within(first).getByText("nginx:1")).toBeTruthy();
    expect(within(first).getByText("exit code 137")).toBeTruthy();

    act(() => stream().open());
    expect(await screen.findByText("Live")).toBeTruthy();
    act(() =>
      stream().send(ev("3", { name: "cache", action: "start", time: new Date().toISOString() })),
    );
    const after = bodyRows().filter((r) => within(r).queryByText(/^(web|db|cache)$/));
    expect(within(after[0] as HTMLElement).getByText("cache")).toBeTruthy();
    expect(after).toHaveLength(3);
  });

  it("shows the Node column on a swarm and not otherwise", async () => {
    recent = async () => Response.json({ items: [ev("1", { node: "worker-7" })] });
    mount();
    await screen.findByText("name-1");
    expect(screen.queryByRole("columnheader", { name: "Node" })).toBeNull();
    cleanup();
    swarm = true;
    mount();
    expect(await screen.findByRole("columnheader", { name: "Node" })).toBeTruthy();
    expect(await screen.findByText("worker-7")).toBeTruthy();
  });

  it("shows skeleton rows with the title at once, and an error with Retry", async () => {
    recent = () => new Promise<Response>(() => {});
    mount();
    expect(screen.getByRole("heading", { name: "Events" })).toBeTruthy();
    expect(document.querySelectorAll('[data-slot="skeleton"]')).toHaveLength(8);
    cleanup();

    recent = async () => Response.json({}, { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't load events.", {}, { timeout: 4000 })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("opens nothing for someone who may not read the environment", async () => {
    me = makeMe({ envs: { other: "viewer" } });
    mount();
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(eventUrls).toHaveLength(0);
    expect(FakeEventSource.instances).toHaveLength(0);
  });
});
