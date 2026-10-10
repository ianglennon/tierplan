// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { EnvEventsProvider, type EventDTO } from "@/features/dashboard/events";
import { EventsFeed } from "@/features/dashboard/events-feed";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import "@/i18n";
import { TestRouter } from "@/test-router";

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

let recent: () => Promise<Response>;

function show(showNode = false, envId?: string) {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <TestRouter>
        <TooltipProvider>
          <EnvEventsProvider envId="e1">
            <EventsFeed envId={envId} showNode={showNode} />
          </EnvEventsProvider>
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const stream = () => FakeEventSource.instances[0] as FakeEventSource;
const rows = () => screen.queryAllByRole("listitem");

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  recent = async () => Response.json({ items: [] });
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => recent()),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("EventsFeed", () => {
  it("shows skeleton rows while the first read runs, with no empty or error text", async () => {
    recent = () => new Promise<Response>(() => {});
    show();
    expect(screen.getByText("Recent events")).toBeTruthy();
    expect(screen.queryByText("No events in the last hour")).toBeNull();
    expect(screen.queryByText("Couldn't load recent events.")).toBeNull();
    expect(document.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0);
  });

  it("says there are no events when the host has been quiet", async () => {
    show();
    expect(await screen.findByText("No events in the last hour")).toBeTruthy();
    expect(screen.getByText("New events appear here as they happen.")).toBeTruthy();
  });

  it("shows a row with time, type and action, the name and up to two attributes", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("1", {
            action: "die",
            name: "web-1",
            attributes: { image: "nginx:1.27", exitCode: "137" },
          }),
        ],
      });
    show();
    const row = (await screen.findAllByRole("listitem"))[0] as HTMLElement;
    expect(within(row).getByText("Container die")).toBeTruthy();
    expect(within(row).getByText("web-1").className).toContain("font-mono");
    expect(within(row).getByText("nginx:1.27")).toBeTruthy();
    expect(within(row).getByText("exit code 137")).toBeTruthy();
    expect(within(row).getByText("1 min ago")).toBeTruthy();
  });

  it("labels the swarm types Service, Node, Secret and Config and shows each under All with its name", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("1", { type: "service", action: "update", name: "web", time: "2026-10-02T10:00:04Z" }),
          ev("2", {
            type: "node",
            action: "update",
            name: "worker-1",
            time: "2026-10-02T10:00:03Z",
          }),
          ev("3", {
            type: "secret",
            action: "create",
            name: "db-password",
            time: "2026-10-02T10:00:02Z",
          }),
          ev("4", {
            type: "config",
            action: "remove",
            name: "nginx.conf",
            time: "2026-10-02T10:00:01Z",
          }),
        ],
      });
    show();
    const items = await screen.findAllByRole("listitem");
    expect(items).toHaveLength(4);
    expect(within(items[0] as HTMLElement).getByText("Service update")).toBeTruthy();
    expect(within(items[1] as HTMLElement).getByText("Node update")).toBeTruthy();
    expect(within(items[2] as HTMLElement).getByText("Secret create")).toBeTruthy();
    expect(within(items[3] as HTMLElement).getByText("Config remove")).toBeTruthy();
    expect(within(items[2] as HTMLElement).getByText("db-password")).toBeTruthy();
  });

  it("keeps showing 50 events at most when swarm events arrive", async () => {
    show();
    await screen.findByText("No events in the last hour");
    act(() => {
      stream().open();
      for (let i = 0; i < 60; i++) {
        stream().send(
          ev(`s${i}`, {
            type: "service",
            action: "update",
            name: `svc-${i}`,
            time: new Date(Date.now() + i * 1000).toISOString(),
          }),
        );
      }
    });
    expect(rows()).toHaveLength(50);
    expect(screen.getByText("svc-59")).toBeTruthy();
    expect(screen.queryByText("svc-9")).toBeNull();
  });

  it("shows a row of Bayplan's own with a Bayplan label and its message, under All only", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("1", {
            type: "bayplan",
            action: "failover",
            name: "Manager mgr-2 unreachable, using mgr-1",
            actorId: "env-1",
            attributes: { from: "mgr-2", to: "mgr-1" },
            time: "2026-10-02T10:00:02Z",
          }),
          ev("2", { type: "container", name: "web", time: "2026-10-02T10:00:01Z" }),
        ],
      });
    show();
    const row = (await screen.findAllByRole("listitem"))[0] as HTMLElement;
    expect(within(row).getByText("Bayplan")).toBeTruthy();
    expect(within(row).getByText("Manager mgr-2 unreachable, using mgr-1")).toBeTruthy();
    // The label stands in for the type icon, and the row has no subject line of its own.
    expect(row.querySelector("svg")).toBeNull();
    expect(within(row).queryByText("env-1")).toBeNull();
    expect(within(row).queryByText("Bayplan failover")).toBeNull();

    // No fifth filter, and the row is not under a Docker type.
    expect(screen.queryByRole("radio", { name: "Bayplan" })).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "Container" }));
    expect(screen.queryByText("Manager mgr-2 unreachable, using mgr-1")).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "All" }));
    expect(screen.getByText("Manager mgr-2 unreachable, using mgr-1")).toBeTruthy();
  });

  it("sets the Bayplan label in the line of the message, so a word wider than the icon column never covers it", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("1", {
            type: "bayplan",
            action: "failover",
            name: "Manager m2 unreachable, using m1",
            actorId: "env-1",
          }),
        ],
      });
    show();
    const row = (await screen.findAllByRole("listitem"))[0] as HTMLElement;
    const label = within(row).getByText("Bayplan");
    const message = within(row).getByText("Manager m2 unreachable, using m1");
    expect(label.parentElement).toBe(message);
    expect(label.className).not.toMatch(/\bw-\d/);
  });

  it("falls back to the short ID when Docker gave no name", async () => {
    recent = async () => Response.json({ items: [ev("1", { name: "" })] });
    show();
    expect(await screen.findByText("0123456789ab")).toBeTruthy();
  });

  it("puts new events at the top and shows an event from the load and the stream once", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("b", { name: "second", time: "2026-10-02T10:00:02Z" }),
          ev("a", { name: "first", time: "2026-10-02T10:00:01Z" }),
        ],
      });
    show();
    await screen.findAllByRole("listitem");
    await act(async () => {
      stream().open();
      stream().send(ev("b", { name: "second", time: "2026-10-02T10:00:02Z" }));
      stream().send(ev("c", { name: "third", time: "2026-10-02T10:00:03Z" }));
    });
    const names = rows().map((r) => within(r).getAllByText(/first|second|third/)[0]?.textContent);
    expect(names).toEqual(["third", "second", "first"]);
  });

  it("keeps 50 rows and drops the oldest", async () => {
    show();
    await screen.findByText("No events in the last hour");
    await act(async () => {
      stream().open();
      for (let i = 0; i < 60; i++) {
        const sec = String(i).padStart(2, "0");
        stream().send(ev(`n${i}`, { name: `n${i}`, time: `2026-10-02T10:00:${sec}Z` }));
      }
    });
    expect(rows()).toHaveLength(50);
    expect(screen.getByText("n59")).toBeTruthy();
    expect(screen.queryByText("n9")).toBeNull();
  });

  it("filters by type on the client, keeps the filter when it is pressed again, and shows All again", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("1", { type: "volume", name: "t1", action: "create" }),
          ev("2", { type: "container", name: "web" }),
          ev("3", { type: "image", name: "alpine", action: "pull" }),
        ],
      });
    show();
    await screen.findAllByRole("listitem");
    expect(rows()).toHaveLength(3);

    const group = screen.getByRole("radiogroup", { name: "Filter events by type" });
    const volume = within(group).getByRole("radio", { name: "Volume" });
    fireEvent.click(volume);
    expect(rows()).toHaveLength(1);
    expect(screen.getByText("Volume create")).toBeTruthy();

    fireEvent.click(volume);
    expect(rows()).toHaveLength(1);

    fireEvent.click(within(group).getByRole("radio", { name: "Network" }));
    expect(rows()).toHaveLength(0);
    expect(screen.getByText("No matching events")).toBeTruthy();

    fireEvent.click(within(group).getByRole("radio", { name: "All" }));
    expect(rows()).toHaveLength(3);
  });

  it("shows Live while the stream is connected, Reconnecting… while it retries, and no chip once polling", async () => {
    show();
    await screen.findByText("No events in the last hour");
    expect(screen.queryByText("Live")).toBeNull();

    await act(async () => stream().open());
    expect(screen.getByText("Live")).toBeTruthy();

    await act(async () => stream().fail());
    expect(screen.getByText("Reconnecting…")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();

    // The browser gives up: the feed polls, and says nothing about being live.
    await act(async () => stream().fail(true));
    expect(screen.queryByText("Reconnecting…")).toBeNull();
    expect(screen.queryByText("Live")).toBeNull();
    expect(screen.getByText("No events in the last hour")).toBeTruthy();
  });

  it("pulses the Live dot only when motion is allowed", async () => {
    show();
    await screen.findByText("No events in the last hour");
    await act(async () => stream().open());
    const dot = screen.getByText("Live").previousElementSibling as HTMLElement;
    expect(dot.className).toContain("animate-pulse");
    expect(dot.className).toContain("motion-reduce:animate-none");
  });

  it("shows an error with Retry when the first read fails, then the events", async () => {
    let calls = 0;
    recent = async () => {
      calls++;
      if (calls === 1) return new Response(null, { status: 500 });
      return Response.json({ items: [ev("1", { name: "web-1" })] });
    };
    show();
    expect(await screen.findByText("Couldn't load recent events.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("web-1")).toBeTruthy();
    expect(screen.queryByText("Couldn't load recent events.")).toBeNull();
  });

  it("renders a name that looks like markup as text", async () => {
    recent = async () =>
      Response.json({ items: [ev("1", { name: "<img src=x onerror=alert(1)>" })] });
    show();
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });
});

// A swarm's feed merges every node's events, so each row says where it happened
// (plan 22, D-43); Bayplan's own row says it in its message instead.
describe("EventsFeed on a swarm", () => {
  it("shows each row's node in muted text", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("1", { name: "web.1", node: "worker-2", time: "2026-10-02T10:00:02Z" }),
          ev("2", { name: "db", node: "mgr-1", time: "2026-10-02T10:00:01Z" }),
        ],
      });
    show(true);
    const items = await screen.findAllByRole("listitem");
    const node = within(items[0] as HTMLElement).getByText("worker-2");
    expect(node.className).toContain("text-muted-foreground");
    expect(within(items[1] as HTMLElement).getByText("mgr-1")).toBeTruthy();
  });

  it("shows no node on a standalone host or the local socket, which have only one", async () => {
    recent = async () => Response.json({ items: [ev("1", { name: "web", node: "lab" })] });
    show(false);
    await screen.findByText("web");
    expect(screen.queryByText("lab")).toBeNull();
  });

  it("shows no node on Bayplan's own row, only its message and label, and it stays under All", async () => {
    recent = async () =>
      Response.json({
        items: [
          ev("1", {
            type: "bayplan",
            action: "failover",
            name: "Manager m2 unreachable, using m1",
            node: "m1",
            actorId: "env-1",
            time: "2026-10-02T10:00:02Z",
          }),
          ev("2", { type: "container", name: "web", node: "m2", time: "2026-10-02T10:00:01Z" }),
        ],
      });
    show(true);
    const items = await screen.findAllByRole("listitem");
    const bayplan = items[0] as HTMLElement;
    expect(within(bayplan).getByText("Bayplan")).toBeTruthy();
    expect(within(bayplan).getByText("Manager m2 unreachable, using m1")).toBeTruthy();
    expect(within(bayplan).queryByText("m1")).toBeNull();
    expect(within(items[1] as HTMLElement).getByText("m2")).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: "Container" }));
    expect(screen.queryByText("Manager m2 unreachable, using m1")).toBeNull();
    expect(screen.getByText("m2")).toBeTruthy();
  });

  it("cuts a long node name and keeps the whole name in the tooltip", async () => {
    const long = "worker-with-a-long-host-name.example.internal".repeat(3);
    recent = async () => Response.json({ items: [ev("1", { name: "web", node: long })] });
    show(true);
    const node = await screen.findByText(long);
    expect(node.className).toContain("truncate");
  });

  it("shows a hostile node name as text (T-02-75)", async () => {
    const evil = "<img src=x onerror=alert(1)>";
    recent = async () => Response.json({ items: [ev("1", { name: "web", node: evil })] });
    show(true);
    await screen.findByText(evil);
    expect(document.querySelector("img[src='x']")).toBeNull();
  });

  it("has a View all events link to the same environment's Events page, and changes nothing else", async () => {
    recent = async () => Response.json({ items: [ev("1", { name: "web-1" })] });
    show(false, "e1");
    const link = await screen.findByRole("link", { name: "View all events" });
    expect(link.getAttribute("href")).toBe("/env/e1/events");
    expect(screen.getByText("Recent events")).toBeTruthy();
    expect(screen.getByRole("radiogroup", { name: "Filter events by type" })).toBeTruthy();
    expect(await screen.findByText("web-1")).toBeTruthy();
    expect(rows()).toHaveLength(1);
  });

  it("shows the link while the feed is loading or empty, and not without an environment", async () => {
    recent = async () => Response.json({ items: [] });
    show(false, "e1");
    expect(screen.getByRole("link", { name: "View all events" })).toBeTruthy();
    await screen.findByText("No events in the last hour");
    expect(screen.getByRole("link", { name: "View all events" })).toBeTruthy();
    cleanup();
    show();
    await screen.findByText("No events in the last hour");
    expect(screen.queryByRole("link", { name: "View all events" })).toBeNull();
  });
});
