// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import type { EventDTO } from "@/features/dashboard/events";
import { type CanOpen, EventsTable } from "@/features/events/events-table";
import "@/i18n";
import { TestRouter } from "@/test-router";

function ev(id: string, over: Partial<EventDTO> = {}): EventDTO {
  return {
    id,
    node: "node-1",
    time: "2026-10-02T10:00:00Z",
    type: "container",
    action: "start",
    actorId: `actor-${id}`,
    name: `name-${id}`,
    attributes: {},
    ...over,
  };
}

const ALL: CanOpen = { container: true, network: true, volume: true };

function show(
  events: EventDTO[],
  over: { swarm?: boolean; canOpen?: CanOpen; onFilterName?: (n: string) => void } = {},
) {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <TestRouter>
        <TooltipProvider>
          <EventsTable
            envId="e1"
            events={events}
            swarm={over.swarm ?? false}
            canOpen={over.canOpen ?? ALL}
            onFilterName={over.onFilterName ?? (() => {})}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

afterEach(cleanup);

describe("EventsTable", () => {
  it("links container, network and volume names to their pages, and the others stay text", () => {
    show([
      ev("1", { name: "web", actorId: "c0ffee" }),
      ev("2", { type: "network", name: "app-net", actorId: "n3t", action: "connect" }),
      ev("3", { type: "volume", name: "data", actorId: "data", action: "mount" }),
      ev("4", { type: "image", name: "nginx:1.27", action: "pull" }),
      ev("5", { type: "service", name: "shop_web", action: "update" }),
    ]);
    expect(screen.getByRole("link", { name: "web" }).getAttribute("href")).toBe(
      "/env/e1/containers/c0ffee",
    );
    expect(screen.getByRole("link", { name: "app-net" }).getAttribute("href")).toBe(
      "/env/e1/networks/n3t",
    );
    expect(screen.getByRole("link", { name: "data" }).getAttribute("href")).toBe(
      "/env/e1/volumes/data",
    );
    expect(screen.queryByRole("link", { name: "nginx:1.27" })).toBeNull();
    expect(screen.queryByRole("link", { name: "shop_web" })).toBeNull();
    expect(screen.getByText("shop_web")).toBeTruthy();
  });

  it("shows the name as plain text once the resource is gone", () => {
    show([
      ev("1", { name: "old", action: "destroy" }),
      ev("2", { type: "network", name: "old-net", action: "destroy" }),
      ev("3", { type: "volume", name: "old-vol", action: "remove" }),
      ev("4", { type: "image", name: "old-img", action: "delete" }),
    ]);
    expect(screen.queryAllByRole("link")).toHaveLength(0);
    for (const n of ["old", "old-net", "old-vol", "old-img"])
      expect(screen.getByText(n)).toBeTruthy();
  });

  it("does not link a page the person may not read", () => {
    show([ev("1", { name: "web" }), ev("2", { type: "volume", name: "data" })], {
      canOpen: { container: false, network: true, volume: true },
    });
    expect(screen.queryByRole("link", { name: "web" })).toBeNull();
    expect(screen.getByRole("link", { name: "data" })).toBeTruthy();
  });

  it("builds a link from the route's own parameters, never from an attribute", () => {
    show([
      ev("1", {
        name: "web",
        actorId: "c0ffee",
        attributes: { image: "javascript:alert(1)", name: "https://evil.example/x" },
      }),
    ]);
    const hrefs = screen.getAllByRole("link").map((l) => l.getAttribute("href"));
    expect(hrefs).toEqual(["/env/e1/containers/c0ffee"]);
  });

  it("opens a row to every attribute it has, as text", () => {
    show([
      ev("1", {
        name: "web",
        attributes: { image: "nginx:1", exitCode: "137", note: "<img src=x onerror=alert(1)>" },
      }),
    ]);
    expect(screen.queryByRole("table", { name: "Attributes of web" })).toBeNull();
    const expander = screen.getByRole("button", { name: "Show attributes for web" });
    expect(expander.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(expander);
    expect(expander.getAttribute("aria-expanded")).toBe("true");
    const attrs = screen.getByRole("table", { name: "Attributes of web" });
    expect(within(attrs).getByText("exitCode")).toBeTruthy();
    expect(within(attrs).getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(attrs.querySelector("img")).toBeNull();
    fireEvent.click(expander);
    expect(screen.queryByRole("table", { name: "Attributes of web" })).toBeNull();
  });

  it("shows up to two details, the image and the exit code", () => {
    show([ev("1", { attributes: { image: "nginx:1", exitCode: "1", signal: "9" } })]);
    expect(screen.getByText("nginx:1")).toBeTruthy();
    expect(screen.getByText("exit code 1")).toBeTruthy();
    expect(screen.queryByText("9")).toBeNull();
  });

  it("cuts the rows into days with a divider where the date changes", () => {
    show([
      ev("1", { time: "2026-10-03T12:00:00Z" }),
      ev("2", { time: "2026-10-03T11:00:00Z" }),
      ev("3", { time: "2026-10-01T12:00:00Z" }),
    ]);
    const full = new Intl.DateTimeFormat(undefined, { dateStyle: "full" });
    expect(screen.getAllByText(full.format(new Date("2026-10-03T12:00:00Z")))).toHaveLength(1);
    expect(screen.getAllByText(full.format(new Date("2026-10-01T12:00:00Z")))).toHaveLength(1);
  });

  it("gives the row menu a filter by this name, which hands the name over", async () => {
    const onFilterName = vi.fn();
    show([ev("1", { name: "web" })], { onFilterName });
    const trigger = screen.getByRole("button", { name: "Actions for web" });
    fireEvent.keyDown(trigger, { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Filter by this name" }));
    expect(onFilterName).toHaveBeenCalledWith("web");
  });

  it("names a row of Bayplan's own by its message, with no link, details or menu", () => {
    show([ev("1", { type: "bayplan", name: "Manager moved to mgr-2", action: "", node: "mgr-2" })]);
    expect(screen.getByText("Bayplan")).toBeTruthy();
    expect(screen.getByText("Manager moved to mgr-2")).toBeTruthy();
    expect(screen.queryAllByRole("link")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: /Actions for/ })).toBeNull();
  });
});
