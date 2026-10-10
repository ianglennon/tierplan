// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  VolumeList,
  type VolumeSearch,
  type VolumeSearchPatch,
} from "@/features/volumes/volume-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let volumes: unknown[];
let swarmEnv: boolean;
let urls: string[];
let search: VolumeSearch;
let changes: VolumeSearchPatch[];

const volume = (name: string, extra: Record<string, unknown> = {}) => ({
  name,
  driver: "local",
  node: "node-1",
  nodeId: "node-1-id",
  created: "2026-10-01T10:00:00Z",
  usedByContainers: 0,
  usedByServices: 0,
  anonymous: false,
  project: "",
  stack: "",
  ...extra,
});

beforeEach(() => {
  me = makeMe({ envs: { [ENV]: "admin" } });
  swarmEnv = false;
  urls = [];
  search = {};
  changes = [];
  volumes = [
    volume("appdata", { usedByContainers: 2, project: "shop" }),
    volume("cache", { project: "shop" }),
    volume("logs", {
      usedByContainers: 1,
      usedByServices: 2,
      stack: "mon",
      node: "node-2",
      nodeId: "node-2-id",
    }),
    volume("loose"),
    volume("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", { anonymous: true }),
  ];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      urls.push(url.pathname + url.search);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/volumes`) {
        return Response.json({
          items: volumes,
          total: volumes.length,
          truncated: false,
          nodes: ["node-1", "node-2"],
          notReporting: [],
        });
      }
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: swarmEnv ? [{ id: ENV, name: "Swarm", envType: "swarm" }] : [],
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <VolumeList envId={ENV} search={search} onSearchChange={(patch) => changes.push(patch)} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const row = async (name: string) => (await screen.findByText(name)).closest("tr") as HTMLElement;

describe("VolumeList usage", () => {
  it("reads Used by as the containers using the volume, or Not in use", async () => {
    mount();
    expect(within(await row("appdata")).getByText("2 containers")).toBeTruthy();
    expect(within(await row("cache")).getByText("Not in use")).toBeTruthy();
  });

  it("adds the services on a swarm", async () => {
    swarmEnv = true;
    mount();
    expect(await screen.findByText("1 container, 2 services")).toBeTruthy();
  });

  it("marks a volume Docker made without a name with the Anonymous badge", async () => {
    mount();
    const anon = await row("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef");
    expect(within(anon).getByText("Anonymous")).toBeTruthy();
    expect(within(await row("cache")).queryByText("Anonymous")).toBeNull();
  });

  it("shows a dash for the size until sizes are calculated", async () => {
    mount();
    const cells = within(await row("cache")).getAllByRole("cell");
    expect(cells.some((c) => c.textContent === "—")).toBe(true);
  });
});

describe("VolumeList filters", () => {
  it("offers All, In use and Unused and puts the choice in the URL", async () => {
    mount();
    const group = await screen.findByRole("group", { name: "Filter by usage" });
    const all = within(group).getByRole("radio", { name: "All" });
    expect(all.getAttribute("aria-checked")).toBe("true");
    fireEvent.click(within(group).getByRole("radio", { name: "In use" }));
    expect(changes).toEqual([{ usage: "used" }]);
    fireEvent.click(within(group).getByRole("radio", { name: "Unused" }));
    expect(changes[1]).toEqual({ usage: "unused" });
  });

  it("asks the server for the usage and the search it is told, before any cap", async () => {
    search = { usage: "unused", q: "ca" };
    mount();
    await screen.findByText("appdata");
    expect(
      urls.some(
        (u) =>
          u.startsWith(`/api/env/${ENV}/volumes?`) &&
          u.includes("usage=unused") &&
          u.includes("q=ca"),
      ),
    ).toBe(true);
  });

  it("says No volumes match these filters. with Clear filters when a filter matches nothing", async () => {
    volumes = [];
    search = { usage: "used" };
    mount();
    expect(await screen.findByText("No volumes match these filters.")).toBeTruthy();
    // The toolbar has one too; the empty state's is the one this is about.
    const buttons = screen.getAllByRole("button", { name: "Clear filters" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    expect(changes.at(-1)).toEqual({ q: undefined, usage: undefined, node: undefined });
  });

  it("says No volumes with Create volume when there are none and nothing is filtered", async () => {
    volumes = [];
    mount();
    expect(await screen.findByText("No volumes")).toBeTruthy();
    expect(screen.queryByText("No volumes match these filters.")).toBeNull();
  });
});

describe("VolumeList grouping", () => {
  it("groups by stack or project by default, with Ungrouped last", async () => {
    mount();
    await screen.findByText("appdata");
    const headers = Array.from(document.querySelectorAll('[data-slot="group-header-row"]')).map(
      (h) => h.textContent ?? "",
    );
    expect(headers).toHaveLength(3);
    expect(headers[0]).toContain("mon");
    expect(headers[0]).toContain("Stack");
    expect(headers[1]).toContain("shop");
    expect(headers[1]).toContain("Project");
    expect(headers[2]).toContain("Ungrouped");
  });

  it("has no headers when grouped by None", async () => {
    search = { group: "none" };
    mount();
    await screen.findByText("appdata");
    expect(document.querySelectorAll('[data-slot="group-header-row"]')).toHaveLength(0);
  });

  it("groups by node on a swarm, and offers Node only there", async () => {
    swarmEnv = true;
    search = { group: "node" };
    mount();
    await screen.findByText("appdata");
    await waitFor(() => {
      const headers = Array.from(document.querySelectorAll('[data-slot="group-header-row"]')).map(
        (h) => h.textContent ?? "",
      );
      expect(headers[0]).toContain("node-1");
      expect(headers[1]).toContain("node-2");
    });
  });

  it("does not offer Node on a standalone host", async () => {
    mount();
    await screen.findByText("appdata");
    fireEvent.click(screen.getByRole("combobox", { name: "Group by" }));
    expect(await screen.findByRole("option", { name: "Stack or project" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "Node" })).toBeNull();
    expect(screen.getByRole("option", { name: "None" })).toBeTruthy();
  });
});

describe("VolumeList selection", () => {
  it("gives a volume in use a checkbox that cannot be ticked, and says why", async () => {
    mount();
    const box = await screen.findByRole("checkbox", { name: "Select appdata" });
    expect(box.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(box);
    expect((await screen.findAllByText("In use, so it can't be deleted.")).length).toBeGreaterThan(
      0,
    );
    expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull();
  });

  it("shows the bulk bar for unused volumes, with the count, Delete… and Clear selection", async () => {
    mount();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Select cache" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Select loose" }));
    const bar = await screen.findByRole("toolbar", { name: "Bulk actions" });
    expect(within(bar).getByText("2 selected")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Delete…" })).toBeTruthy();
    fireEvent.click(within(bar).getByRole("button", { name: "Clear selection" }));
    await waitFor(() => expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull());
  });

  it("selects only the volumes that are not in use with the header checkbox", async () => {
    mount();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Select all volumes" }));
    const bar = await screen.findByRole("toolbar", { name: "Bulk actions" });
    // cache, loose and the anonymous one; appdata and logs are in use.
    expect(within(bar).getByText("3 selected")).toBeTruthy();
  });

  it("disables the bulk Delete… with the reason for someone without the delete volumes permission", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Select cache" }));
    const button = await within(
      await screen.findByRole("toolbar", { name: "Bulk actions" }),
    ).findByRole("button", { name: "Delete…" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(
      (await screen.findAllByText("Needs the delete volumes permission")).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("disables Delete… on a row in use with who uses it", async () => {
    mount();
    const button = await screen.findByRole("button", { name: "Delete appdata" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(
      await screen.findByText("In use by 2 containers. Remove what uses it first."),
    ).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});
