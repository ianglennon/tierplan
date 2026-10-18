// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { nodeDetail } from "@/features/nodes/fixtures";
import { NodeList } from "@/features/nodes/node-list";
import { nodeActionsFor } from "@/features/nodes/row-actions";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The row menu of the Nodes list (plan 06-15, RES-05, UI-SPEC "Nodes"): the changes a node's state
// offers, who may make them, the dialogs they open and the pending words until the list shows them.

const ENV = "e1";

type NodeItem = components["schemas"]["NodeItem"];

const node = (extra: Partial<NodeItem> = {}): NodeItem => ({
  id: "n1",
  hostname: "mgr-1",
  role: "manager",
  leader: true,
  runsBayplan: true,
  state: "ready",
  availability: "active",
  reachability: "reachable",
  address: "10.0.0.1",
  engineVersion: "29.1.0",
  os: "linux",
  arch: "aarch64",
  cpus: 8,
  memoryBytes: 31 * 1024 ** 3,
  tasksRunning: 3,
  tasksNotRunning: 0,
  agentReporting: true,
  targetNodeId: "t1",
  ...extra,
});

const NODES: NodeItem[] = [
  node(),
  node({
    id: "n2",
    hostname: "wkr-active",
    role: "worker",
    leader: false,
    runsBayplan: false,
    reachability: "",
  }),
  node({
    id: "n3",
    hostname: "wkr-drained",
    role: "worker",
    leader: false,
    runsBayplan: false,
    availability: "drain",
    reachability: "",
    tasksRunning: 0,
  }),
  node({
    id: "n4",
    hostname: "wkr-paused",
    role: "worker",
    leader: false,
    runsBayplan: false,
    availability: "pause",
    reachability: "",
  }),
];

describe("nodeActionsFor", () => {
  it("offers what each state allows", () => {
    expect(nodeActionsFor({ role: "manager", availability: "active" })).toEqual({
      drain: true,
      pause: true,
      activate: false,
      promote: false,
      demote: true,
    });
    expect(nodeActionsFor({ role: "worker", availability: "active" })).toEqual({
      drain: true,
      pause: true,
      activate: false,
      promote: true,
      demote: false,
    });
    expect(nodeActionsFor({ role: "worker", availability: "pause" })).toEqual({
      drain: true,
      pause: false,
      activate: true,
      promote: true,
      demote: false,
    });
    expect(nodeActionsFor({ role: "manager", availability: "drain" })).toEqual({
      drain: false,
      pause: false,
      activate: true,
      promote: false,
      demote: true,
    });
  });
});

interface Sent {
  method: string;
  path: string;
  search: string;
  body?: unknown;
}

let sent: Sent[];
let items: NodeItem[];
let noManager: boolean;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;
let answerPost: () => Response;

beforeEach(() => {
  sent = [];
  items = NODES;
  noManager = false;
  answerPost = () => new Response(null, { status: 204 });
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 1 } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const body = req.method === "POST" ? await req.json() : undefined;
      sent.push({ method: req.method, path: url.pathname, search: url.search, body });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/nodes` && req.method === "GET") {
        return Response.json({
          items,
          total: items.length,
          unfilteredTotal: items.length,
          counts: { managers: 1, workers: 3, drained: 1, down: 0 },
          noManager,
          tasksKnown: true,
          fetchedAt: "2026-10-14T09:00:00Z",
        });
      }
      const detail = /^\/api\/env\/e1\/nodes\/(n\d)$/.exec(url.pathname);
      if (detail && req.method === "GET") {
        const item = items.find((n) => n.id === detail[1]);
        return Response.json(
          nodeDetail({
            id: item?.id,
            hostname: item?.hostname,
            role: item?.role as "manager" | "worker",
            availability: item?.availability as "active" | "pause" | "drain",
            version: 9,
          }),
        );
      }
      if (/\/impact$/.test(url.pathname)) {
        const hostname = items.find((n) => url.pathname.includes(`/${n.id}/`))?.hostname ?? "";
        return Response.json({
          action: url.searchParams.get("action"),
          hostname,
          version: 9,
          tasks: [],
          managers: { total: 3, reachable: 3, after: 3 },
          warnings: [],
          needsConfirm: false,
        });
      }
      if (req.method === "POST" || req.method === "DELETE") return answerPost();
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm", envType: "swarm" }] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <NodeList envId={ENV} search={{}} onSearchChange={() => {}} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function openMenu(name: string) {
  await screen.findByRole("link", { name });
  await waitFor(() => expect(sent.some((s) => s.path === "/api/me")).toBe(true));
  await act(async () => {});
  const trigger = screen.getByRole("button", { name: `Actions for ${name}` });
  await act(async () => {
    fireEvent.keyDown(trigger, { key: "Enter" });
  });
  return { menu: await screen.findByRole("menu") };
}

const choose = (menu: HTMLElement, name: string) =>
  fireEvent.click(within(menu).getByRole("menuitem", { name }));

const itemNames = (menu: HTMLElement) =>
  within(menu)
    .getAllByRole("menuitem")
    .map((i) => i.textContent);

describe("the node row menu", () => {
  it("has an Actions button for each row", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    for (const n of NODES) {
      expect(screen.getByRole("button", { name: `Actions for ${n.hostname}` })).toBeTruthy();
    }
    expect(screen.getByRole("columnheader", { name: "Actions" })).toBeTruthy();
  });

  it("offers a manager Open, Drain, Pause and Demote", async () => {
    mount();
    const { menu } = await openMenu("mgr-1");
    expect(itemNames(menu)).toEqual(["Open", "Drain…", "Pause…", "Demote…", "Remove…"]);
  });

  it("offers an active worker Open, Drain, Pause and Promote", async () => {
    mount();
    const { menu } = await openMenu("wkr-active");
    expect(itemNames(menu)).toEqual(["Open", "Drain…", "Pause…", "Promote…", "Remove…"]);
  });

  it("offers a drained worker Activate node and Promote only", async () => {
    mount();
    const { menu } = await openMenu("wkr-drained");
    expect(itemNames(menu)).toEqual(["Open", "Activate node", "Promote…", "Remove…"]);
  });

  it("offers a paused worker Drain, Activate node and Promote", async () => {
    mount();
    const { menu } = await openMenu("wkr-paused");
    expect(itemNames(menu)).toEqual(["Open", "Drain…", "Activate node", "Promote…", "Remove…"]);
  });

  it("opens the drain dialog from Drain…", async () => {
    mount();
    const { menu } = await openMenu("wkr-active");
    choose(menu, "Drain…");
    const dialog = await screen.findByRole("dialog", { name: "Drain wkr-active?" });
    await waitFor(() =>
      expect(within(dialog).queryByText("Checking what this will do…")).toBeNull(),
    );
    expect(
      sent.some((s) => s.path === "/api/env/e1/nodes/n2/impact" && s.search === "?action=drain"),
    ).toBe(true);
  });

  it("opens the promote dialog from Promote…", async () => {
    mount();
    const { menu } = await openMenu("wkr-active");
    choose(menu, "Promote…");
    expect(
      await screen.findByRole("dialog", { name: "Promote wkr-active to manager?" }),
    ).toBeTruthy();
  });

  it("activates a node with no dialog, reading it first, and says so", async () => {
    mount();
    const { menu } = await openMenu("wkr-paused");
    choose(menu, "Activate node");
    expect(await screen.findByText("Activated wkr-paused.")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    const post = sent.find((s) => s.method === "POST");
    expect(post?.path).toBe("/api/env/e1/nodes/n4/availability");
    expect(post?.body).toEqual({ availability: "active", version: 9 });
  });

  it("says what went wrong when Activate is refused", async () => {
    answerPost = () =>
      Response.json({ status: 409, type: "node_changed", title: "x" }, { status: 409 });
    mount();
    const { menu } = await openMenu("wkr-paused");
    choose(menu, "Activate node");
    expect(
      await screen.findByText(
        "That changed while you were looking at it. Reload the page and try again.",
      ),
    ).toBeTruthy();
  });

  it("shows the pending word until the list shows the change", async () => {
    mount();
    const { menu } = await openMenu("wkr-active");
    choose(menu, "Drain…");
    const dialog = await screen.findByRole("dialog", { name: "Drain wkr-active?" });
    await waitFor(() =>
      expect(within(dialog).queryByText("Checking what this will do…")).toBeNull(),
    );
    // The list still reads Active when the swarm has accepted the drain.
    fireEvent.click(within(dialog).getByRole("button", { name: "Drain node" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const row = screen.getByRole("link", { name: "wkr-active" }).closest("tr") as HTMLElement;
    expect(within(row).getByText("Draining…")).toBeTruthy();
    expect(within(row).queryByText("Active")).toBeNull();

    // Once the list shows it, the row speaks for itself.
    items = items.map((n) =>
      n.id === "n2" ? { ...n, availability: "drain", tasksRunning: 2 } : n,
    );
    await client.invalidateQueries();
    await waitFor(() => expect(within(row).queryByText("Draining…")).toBeNull());
    expect(within(row).getByText("Draining (2 tasks left)")).toBeTruthy();
  });

  it("shows Promoting… in the role cell until the list shows a manager", async () => {
    mount();
    const { menu } = await openMenu("wkr-active");
    choose(menu, "Promote…");
    const dialog = await screen.findByRole("dialog", { name: "Promote wkr-active to manager?" });
    await waitFor(() =>
      expect(within(dialog).queryByText("Checking what this will do…")).toBeNull(),
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Promote node" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const row = screen.getByRole("link", { name: "wkr-active" }).closest("tr") as HTMLElement;
    expect(within(row).getByText("Promoting…")).toBeTruthy();
    expect(within(row).queryByText("Worker")).toBeNull();
  });

  it("shows every item to an Operator, unavailable with the reason", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    const { menu } = await openMenu("wkr-active");
    expect(itemNames(menu)).toEqual(["Open", "Drain…", "Pause…", "Promote…", "Remove…"]);
    const drain = within(menu).getByRole("menuitem", { name: "Drain…" });
    expect(drain.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(drain);
    expect((await screen.findAllByText("Needs Admin")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(sent.some((s) => s.method === "POST")).toBe(false);
  });

  it("turns the changes off, with the reason, while no manager is connected", async () => {
    noManager = true;
    mount();
    const { menu } = await openMenu("wkr-active");
    const drain = within(menu).getByRole("menuitem", { name: "Drain…" });
    expect(drain.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(drain);
    expect(
      (await screen.findAllByText("No manager is connected, so swarm changes are off.")).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("offers Remove… last, in destructive text, after a separator", async () => {
    mount();
    const { menu } = await openMenu("wkr-active");
    const remove = within(menu).getByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("data-variant")).toBe("destructive");
    const items = within(menu).getAllByRole("menuitem");
    expect(items[items.length - 1]).toBe(remove);
    expect(within(menu).getAllByRole("separator").length).toBe(2);
  });

  it("disables Remove… for a node that is not down, with the reason", async () => {
    mount();
    const { menu } = await openMenu("wkr-active");
    const remove = within(menu).getByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(remove);
    expect(
      (await screen.findAllByText("Only a node that is down can be removed.")).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("disables Remove… for a manager, telling the person to demote it first", async () => {
    mount();
    const { menu } = await openMenu("mgr-1");
    const remove = within(menu).getByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(remove);
    expect((await screen.findAllByText("Demote mgr-1 before removing it.")).length).toBeGreaterThan(
      0,
    );
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("opens the remove dialog for a down worker and removes it by name", async () => {
    items = [
      ...NODES,
      node({
        id: "n5",
        hostname: "wkr-down",
        role: "worker",
        leader: false,
        runsBayplan: false,
        state: "down",
        reachability: "",
        tasksRunning: 0,
      }),
    ];
    mount();
    const { menu } = await openMenu("wkr-down");
    const remove = within(menu).getByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("aria-disabled")).not.toBe("true");
    choose(menu, "Remove…");
    const dialog = await screen.findByRole("alertdialog", { name: "Remove wkr-down?" });
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "wkr-down" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove node" }));
    expect(await screen.findByText("Removed wkr-down.")).toBeTruthy();
    expect(
      sent.some(
        (s) =>
          s.method === "DELETE" &&
          s.path === "/api/env/e1/nodes/n5" &&
          s.search === "?confirm=wkr-down",
      ),
    ).toBe(true);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("shows Remove… to an Operator unavailable with the reason", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    items = [
      ...NODES,
      node({
        id: "n5",
        hostname: "wkr-down",
        role: "worker",
        leader: false,
        runsBayplan: false,
        state: "down",
        reachability: "",
        tasksRunning: 0,
      }),
    ];
    mount();
    const { menu } = await openMenu("wkr-down");
    const remove = within(menu).getByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(remove);
    expect((await screen.findAllByText("Needs Admin")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});
