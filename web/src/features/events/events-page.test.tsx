// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import type { EventDTO } from "@/features/dashboard/events";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EventsPage } from "@/features/events/events-page";
import type { EventsSearch, EventsSearchPatch } from "@/features/events/filters";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

// A higher id is a second newer, so the order of events never rests on the clock
// ticking between two calls.
function ev(id: string, over: Partial<EventDTO> = {}): EventDTO {
  return {
    id,
    node: "node-1",
    time: new Date(Date.now() - 60_000 + (Number.parseInt(id, 10) || 0) * 1000).toISOString(),
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

// The route keeps the search in the URL; this keeps it in state and drops what a
// patch sets to undefined, as the route does.
function Harness({
  initial,
  spy,
}: {
  initial: EventsSearch;
  spy?: (p: EventsSearchPatch) => void;
}) {
  const [search, setSearch] = useState(initial);
  return (
    <EventsPage
      envId={ENV}
      search={search}
      onSearchChange={(patch) => {
        spy?.(patch);
        setSearch((prev) => {
          const next: Record<string, unknown> = { ...prev, ...patch };
          for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
          return next as EventsSearch;
        });
      }}
    />
  );
}

function mount(search: EventsSearch = {}, spy?: (p: EventsSearchPatch) => void) {
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <TestRouter>
        <TooltipProvider>
          <Harness initial={search} spy={spy} />
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
    // The request's since is bounded by the clock read before mounting and after the request,
    // so the check holds however slowly the page renders.
    const before = Date.now();
    mount();
    await screen.findByText("No events in this range");
    const after = Date.now();
    const url = eventUrls[0] as URL;
    expect(url.searchParams.get("limit")).toBe("1000");
    const since = Date.parse(url.searchParams.get("since") ?? "");
    expect(since).toBeGreaterThanOrEqual(before - 60 * 60_000);
    expect(since).toBeLessThanOrEqual(after - 60 * 60_000);
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
    expect(await screen.findByText("Couldn't load events.")).toBeTruthy();
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

// A page of events to filter: three containers, a network and a volume.
function seed() {
  recent = async () =>
    Response.json({
      items: [
        ev("5", { type: "volume", name: "data", actorId: "data", action: "create", node: "n2" }),
        ev("4", {
          type: "network",
          name: "app-net",
          actorId: "net1",
          action: "connect",
          node: "n2",
          attributes: { container: "web" },
        }),
        ev("3", {
          name: "db",
          action: "die",
          attributes: { image: "postgres:16", exitCode: "137" },
        }),
        ev("2", { name: "cache", action: "start", attributes: { image: "redis:7" } }),
        ev("1", { name: "web", action: "start", attributes: { image: "nginx:1.27" } }),
      ],
    });
}

const hasRow = (name: string) => screen.queryByText(name) !== null;

describe("EventsPage range", () => {
  it("offers last 5 minutes, last hour and last 24 hours, and reads again for a new range", async () => {
    const spy = vi.fn();
    mount({}, spy);
    await screen.findByText("No events in this range");
    const trigger = screen.getByRole("combobox", { name: "Show events from" });
    expect(trigger.textContent).toBe("Last hour");
    fireEvent.keyDown(trigger, { key: "Enter" });
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
      "Last 5 minutes",
      "Last hour",
      "Last 24 hours",
    ]);
    const before = Date.now();
    fireEvent.click(screen.getByRole("option", { name: "Last 24 hours" }));
    expect(spy).toHaveBeenCalledWith({ since: "24h" });
    await waitFor(() => expect(eventUrls).toHaveLength(2));
    const after = Date.now();
    const since = Date.parse((eventUrls[1] as URL).searchParams.get("since") ?? "");
    expect(since).toBeGreaterThanOrEqual(before - 24 * 60 * 60_000);
    expect(since).toBeLessThanOrEqual(after - 24 * 60 * 60_000);
    // The old stream is closed and a new one opened.
    expect(FakeEventSource.instances).toHaveLength(2);
    expect((FakeEventSource.instances[0] as FakeEventSource).closed).toBe(true);
  });
});

describe("EventsPage filters", () => {
  it("offers the four Docker types, the swarm ones only on a swarm, and none selected means all", async () => {
    seed();
    mount();
    await screen.findByText("web");
    const group = screen.getByRole("toolbar", { name: "Filter events by type" });
    expect(
      within(group)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Container", "Image", "Network", "Volume"]);
    expect(hasRow("app-net") && hasRow("data") && hasRow("web")).toBe(true);
    fireEvent.click(within(group).getByRole("button", { name: "Network" }));
    expect(hasRow("web")).toBe(false);
    expect(hasRow("app-net")).toBe(true);
    fireEvent.click(within(group).getByRole("button", { name: "Network" }));
    expect(hasRow("web")).toBe(true);

    cleanup();
    swarm = true;
    mount();
    await screen.findByText("web");
    const swarmGroup = screen.getByRole("toolbar", { name: "Filter events by type" });
    expect(
      within(swarmGroup)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Container", "Image", "Network", "Volume", "Service", "Node", "Secret", "Config"]);
  });

  it("lists the actions seen, filters by the ones ticked and shows how many", async () => {
    seed();
    mount();
    await screen.findByText("web");
    fireEvent.click(screen.getByRole("button", { name: "Action" }));
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual(["connect", "create", "die", "start"]);
    fireEvent.click(screen.getByRole("option", { name: "die" }));
    expect(hasRow("db")).toBe(true);
    expect(hasRow("web")).toBe(false);
    expect(screen.getByRole("button", { name: "Action (1)" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "die" }).getAttribute("data-checked")).toBe("true");
    fireEvent.click(screen.getByRole("option", { name: "create" }));
    expect(screen.getByRole("button", { name: "Action (2)" })).toBeTruthy();
    expect(hasRow("data")).toBe(true);
  });

  it("filters by node on a swarm, over the nodes the events came from", async () => {
    seed();
    swarm = true;
    mount();
    await screen.findByText("web");
    const trigger = screen.getByRole("combobox", { name: "Node" });
    expect(trigger.textContent).toBe("All nodes");
    fireEvent.keyDown(trigger, { key: "Enter" });
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
      "All nodes",
      "n2",
      "node-1",
    ]);
    fireEvent.click(screen.getByRole("option", { name: "n2" }));
    expect(hasRow("app-net")).toBe(true);
    expect(hasRow("web")).toBe(false);
  });

  it("has no Node filter off a swarm", async () => {
    seed();
    mount();
    await screen.findByText("web");
    expect(screen.queryByRole("combobox", { name: "Node" })).toBeNull();
  });

  it("searches name, image and attributes", async () => {
    seed();
    mount();
    await screen.findByText("web");
    const box = screen.getByRole("searchbox", { name: "Search events" });
    expect(box.getAttribute("placeholder")).toBe("Search by name, image or attribute");
    fireEvent.change(box, { target: { value: "redis" } });
    await waitFor(() => expect(hasRow("web")).toBe(false));
    expect(hasRow("cache")).toBe(true);
    fireEvent.change(box, { target: { value: "WEB" } });
    await waitFor(() => expect(hasRow("cache")).toBe(false));
    // The name, and the network that has the container as an attribute.
    expect(hasRow("web")).toBe(true);
    expect(hasRow("app-net")).toBe(true);
  });

  it("filters by name from a row's menu, as a chip that can be removed", async () => {
    seed();
    mount();
    await screen.findByText("web");
    fireEvent.keyDown(screen.getByRole("button", { name: "Actions for web" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Filter by this name" }));
    expect(screen.getByText("Name: web")).toBeTruthy();
    expect(hasRow("cache")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Remove name filter" }));
    expect(screen.queryByText("Name: web")).toBeNull();
    expect(hasRow("cache")).toBe(true);
  });

  it("says no event matches, and Clear filters brings them back", async () => {
    seed();
    mount({ q: "nothing like this" });
    expect(await screen.findByText("No events match these filters.")).toBeTruthy();
    expect(screen.queryByText("No events in this range")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(await screen.findByText("web")).toBeTruthy();
  });

  it("applies the filters from the URL, and ignores a swarm-only type off a swarm", async () => {
    seed();
    mount({ type: ["network", "service"] });
    await screen.findByText("app-net");
    expect(hasRow("web")).toBe(false);
  });
});

describe("EventsPage pause and the pill", () => {
  const live = async () => {
    seed();
    mount();
    await screen.findByText("web");
    act(() => stream().open());
    await screen.findByText("Live");
  };
  const arrive = (id: string, name: string) =>
    act(() => stream().send(ev(id, { name, time: new Date().toISOString() })));

  it("holds new events while paused, counts them, and applies them on Resume", async () => {
    await live();
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    expect(screen.getByText("Paused")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
    arrive("10", "first-new");
    expect(hasRow("first-new")).toBe(false);
    expect(screen.getByRole("button", { name: "1 new event" })).toBeTruthy();
    arrive("11", "second-new");
    expect(screen.getByRole("button", { name: "2 new events" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    expect(await screen.findByText("first-new")).toBeTruthy();
    expect(hasRow("second-new")).toBe(true);
    expect(screen.queryByText("Paused")).toBeNull();
    expect(screen.getByText("Live")).toBeTruthy();
  });

  it("applies the held events from the pill", async () => {
    await live();
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    arrive("10", "first-new");
    fireEvent.click(screen.getByRole("button", { name: "1 new event" }));
    expect(await screen.findByText("first-new")).toBeTruthy();
  });

  it("keeps applying the filters to what is held", async () => {
    await live();
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    arrive("10", "first-new");
    fireEvent.click(screen.getByRole("button", { name: "Volume" }));
    expect(hasRow("data")).toBe(true);
    expect(hasRow("web")).toBe(false);
    // The new container event is held, and does not match the filter either.
    expect(screen.queryByText(/new event/)).toBeNull();
  });

  it("holds back a new event while the table is scrolled down, and offers Jump to latest", async () => {
    await live();
    const scroller = document.querySelector(".overflow-auto") as HTMLElement;
    scroller.scrollTop = 200;
    fireEvent.scroll(scroller);
    arrive("10", "first-new");
    expect(hasRow("first-new")).toBe(false);
    const jump = screen.getByRole("button", { name: "Jump to latest (1 new)" });
    // Still live: the chip stays, nothing is paused.
    expect(screen.getByText("Live")).toBeTruthy();
    expect(screen.queryByText("Paused")).toBeNull();
    fireEvent.click(jump);
    expect(await screen.findByText("first-new")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Jump to latest/ })).toBeNull();
  });

  it("puts a new event at the top at once when the table is at the top", async () => {
    await live();
    arrive("10", "first-new");
    expect(await screen.findByText("first-new")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Jump to latest/ })).toBeNull();
  });
});

describe("EventsPage rows", () => {
  it("links names to the resource pages, except for what was destroyed", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("3", { name: "gone", actorId: "g1", action: "destroy" }),
          ev("2", { name: "app-net", type: "network", actorId: "net1", action: "connect" }),
          ev("1", { name: "web", actorId: "c1" }),
        ],
      });
    mount();
    expect((await screen.findByRole("link", { name: "web" })).getAttribute("href")).toBe(
      "/env/e1/containers/c1",
    );
    expect(screen.getByRole("link", { name: "app-net" }).getAttribute("href")).toBe(
      "/env/e1/networks/net1",
    );
    expect(screen.queryByRole("link", { name: "gone" })).toBeNull();
    expect(screen.getByText("gone")).toBeTruthy();
  });

  it("does not link a page the person may not read", async () => {
    me = makeMe({
      permissions: { global: [], environments: { [ENV]: ["environments.read", "networks.read"] } },
    });
    recent = async () =>
      Response.json({
        items: [
          ev("2", { name: "app-net", type: "network", actorId: "net1" }),
          ev("1", { name: "web" }),
        ],
      });
    mount();
    await screen.findByRole("link", { name: "app-net" });
    expect(screen.queryByRole("link", { name: "web" })).toBeNull();
    expect(screen.getByText("web")).toBeTruthy();
  });

  it("opens a row to its attributes", async () => {
    recent = async () =>
      Response.json({
        items: [ev("1", { name: "web", attributes: { image: "nginx:1", signal: "15" } })],
      });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Show attributes for web" }));
    const attrs = screen.getByRole("table", { name: "Attributes of web" });
    expect(within(attrs).getByText("signal")).toBeTruthy();
    expect(within(attrs).getByText("15")).toBeTruthy();
  });

  it("finds the node of a swarm object when its name is clicked, and says nothing of one that is gone", async () => {
    swarm = true;
    const listed: string[] = [];
    recent = async () =>
      Response.json({ items: [ev("1", { name: "web", actorId: "c1", node: "worker-2" })] });
    const base = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const url = new URL(req.url);
        if (url.pathname === `/api/env/${ENV}/containers`) {
          listed.push(url.search);
          return Response.json({
            items: [],
            total: 0,
            truncated: false,
            nodes: [],
            notReporting: [],
          });
        }
        return base(req);
      }),
    );
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "web" }));
    await waitFor(() => expect(listed).toHaveLength(1));
    expect(new URLSearchParams(listed[0]).get("q")).toBe("web");
    // It is no longer listed: the name turns to plain text.
    await waitFor(() => expect(screen.queryByRole("button", { name: "web" })).toBeNull());
    expect(screen.getByText("web")).toBeTruthy();
  });

  it("notes the 1000-event buffer when it is full", async () => {
    recent = async () =>
      Response.json({
        items: Array.from({ length: 1000 }, (_, i) =>
          ev(`e${i}`, { time: new Date(Date.now() - i * 1000).toISOString() }),
        ),
      });
    mount();
    expect(await screen.findByText("Showing the newest 1000 events.")).toBeTruthy();
    cleanup();
    recent = async () => Response.json({ items: [ev("1")] });
    mount();
    await screen.findByText("name-1");
    expect(screen.queryByText("Showing the newest 1000 events.")).toBeNull();
  });
});

// jsdom has no layout, so the narrow-width rules are checked as the classes that
// carry them; the 375px look is the held-out visual test (UI-SPEC backstop).
describe("EventsPage under 768px", () => {
  it("wraps the filters, scrolls the table sideways in its card and makes every target 44px", async () => {
    recent = async () => Response.json({ items: [ev("1", { name: "web" })] });
    mount();
    await screen.findByText("web");
    act(() => stream().open());
    const classes = (el: HTMLElement) => el.className;

    const toolbar = screen.getByRole("toolbar", { name: "Filter events by type" })
      .parentElement as HTMLElement;
    expect(classes(toolbar)).toContain("flex-wrap");
    expect(classes(screen.getByRole("toolbar", { name: "Filter events by type" }))).toContain(
      "flex-wrap",
    );
    expect(classes(screen.getByRole("table", { name: "Docker events, newest first" }))).toContain(
      "min-w-[48rem]",
    );
    expect((document.querySelector(".overflow-auto") as HTMLElement).className).toContain(
      "overflow-auto",
    );

    const targets = [
      screen.getByRole("button", { name: "Show attributes for web" }),
      screen.getByRole("button", { name: "Actions for web" }),
    ];
    for (const el of targets) expect(classes(el)).toContain("max-md:size-11");
    for (const name of ["Pause", "Action", "Container"]) {
      expect(classes(screen.getByRole("button", { name }))).toMatch(/max-md:(h|min-h)-11/);
    }
    expect(classes(screen.getByRole("combobox", { name: "Show events from" }))).toContain(
      "max-md:h-11",
    );
    expect(
      classes(
        screen.getByRole("searchbox", { name: "Search events" }).parentElement as HTMLElement,
      ),
    ).toContain("max-md:h-11");
  });

  it("keeps the pill 44px tall and sticky over the table", async () => {
    recent = async () => Response.json({ items: [ev("1", { name: "web" })] });
    mount();
    await screen.findByText("web");
    act(() => stream().open());
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    act(() => stream().send(ev("2", { name: "later", time: new Date().toISOString() })));
    const pill = screen.getByRole("button", { name: "1 new event" });
    expect(pill.className).toContain("max-md:h-11");
    expect(((pill.parentElement as HTMLElement).parentElement as HTMLElement).className).toContain(
      "sticky",
    );
  });
});
