// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { activeShellsQuery, SHELLS_POLL_MS } from "@/api/shells";
import { Toaster } from "@/components/ui/sonner";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The Shells tab of an environment's settings page (plan 04-25, OBS-03, D-37, D-51).

const ENV = {
  id: "e1",
  name: "prod",
  kind: "agent",
  enabled: true,
  isSelfHost: false,
  isLocalSocket: false,
  createdAt: "2026-10-01T10:00:00Z",
  engine: { name: "Docker Engine", version: "28.5.1", apiVersion: "1.50" },
  status: { state: "online" },
  envType: "swarm",
  agentVersion: null,
  agents: null,
  engineVersion: null,
  failingServices: null,
  lastSeen: null,
  nodes: null,
  updateAvailable: false,
};

// Fixture dates count from one fixed instant, and beforeEach freezes Date at it, so a
// minute-granularity "N min ago" is exact however late the run starts. Only Date is faked:
// findBy and waitFor and the app's timers keep running in real time.
const NOW = new Date("2026-10-03T12:00:00Z");
function shell(over: Record<string, unknown> = {}) {
  return {
    id: "s-olive",
    user: "olive",
    containerId: "c1".padEnd(64, "0"),
    container: "web",
    node: "worker-1",
    startedAt: new Date(NOW.getTime() - 12 * 60_000).toISOString(),
    idleSeconds: 400,
    ...over,
  };
}

let items: Array<Record<string, unknown>>;
let current: Record<string, unknown>;
let env: Record<string, unknown>;
let listStatus: number;
let listPending: boolean;
let endStatus: number;
let ended: string[];

const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  items = [
    shell(),
    shell({ id: "s-pat", user: "pat", container: "db", node: "worker-2", idleSeconds: 20 }),
  ];
  current = makeMe({ username: "ada", id: "u-ada", envs: { e1: "admin" } });
  env = ENV;
  listStatus = 200;
  listPending = false;
  endStatus = 204;
  ended = [];
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(current);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/environments") return Response.json({ items: [env], total: 1 });
      if (path === "/api/env/e1/active-shells" && req.method === "GET") {
        if (listPending) return new Promise<Response>(() => {});
        return listStatus === 200
          ? Response.json({ items })
          : Response.json({ title: "boom" }, { status: listStatus });
      }
      if (path.startsWith("/api/env/e1/active-shells/") && req.method === "DELETE") {
        const id = path.split("/").pop() as string;
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
  document.title = "";
});

function renderAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <Toaster />
    </QueryClientProvider>,
  );
  return router;
}

const SHELLS = "/settings/environments/e1/shells";

async function tab() {
  const heading = await screen.findByRole("heading", { level: 3, name: "Open shells on prod" });
  return heading.closest("section") as HTMLElement;
}

async function openMenu(table: HTMLElement, user: string) {
  fireEvent.pointerDown(
    within(table).getByRole("button", { name: `Actions for ${user}'s shell` }),
    {
      button: 0,
      ctrlKey: false,
    },
  );
  return screen.findByRole("menuitem", { name: "End session…" });
}

describe("the Shells tab in the environment page's tab bar", () => {
  it("shows Settings, Access and Shells to an env Admin, with Shells selected from the URL", async () => {
    renderAt(SHELLS);
    await tab();
    const tabs = within(screen.getByRole("tablist", { name: "Environment sections" }))
      .getAllByRole("tab")
      .map((t) => t.textContent);
    expect(tabs).toEqual(["Settings", "Access", "Shells"]);
    const shellsTab = screen.getByRole("tab", { name: "Shells" });
    expect(shellsTab.getAttribute("href")).toBe("/settings/environments/e1/shells");
    expect(shellsTab.getAttribute("data-active")).toBe("true");
  });

  it("hides the Shells tab from someone who may give access but not manage shells", async () => {
    current = makeMe({
      username: "carol",
      id: "u-carol",
      permissions: {
        global: ["settings.read"],
        environments: { e1: ["environments.configure", "environments.access"] },
      },
    });
    renderAt("/settings/environments/e1/access");
    await screen.findByRole("tab", { name: "Access" });
    expect(screen.queryByRole("tab", { name: "Shells" })).toBeNull();
  });

  it("shows no page at the Shells address to someone without shells.manage", async () => {
    current = makeMe({ username: "olive", id: "u-olive", envs: { e1: "operator" } });
    renderAt(SHELLS);
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Open shells on prod" })).toBeNull();
  });
});

describe("the open shells table", () => {
  it("is titled Open shells on {name}, explains itself and lists User, Container, Node, Started and Idle", async () => {
    renderAt(SHELLS);
    const c = await tab();
    expect(
      within(c).getByText(
        "Terminals people currently have open. Ending one closes it straight away.",
      ),
    ).toBeTruthy();
    const table = await within(c).findByRole("table", { name: "Open shells on prod" });
    const heads = within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(heads).toEqual(["User", "Container", "Node", "Started", "Idle", "Actions"]);
    const olive = within(table).getByText("olive").closest("tr") as HTMLElement;
    expect(within(olive).getByText("web")).toBeTruthy();
    expect(within(olive).getByText("worker-1")).toBeTruthy();
    expect(within(olive).getByText("12 min ago")).toBeTruthy();
    expect(within(olive).getByText("6 min")).toBeTruthy();
    // Under a minute of silence reads as Active.
    const pat = within(table).getByText("pat").closest("tr") as HTMLElement;
    expect(within(pat).getByText("Active")).toBeTruthy();
  });

  it("leaves out the Node column on a standalone host", async () => {
    env = { ...ENV, envType: "standalone" };
    renderAt(SHELLS);
    const c = await tab();
    const table = await within(c).findByRole("table", { name: "Open shells on prod" });
    expect(within(table).queryByRole("columnheader", { name: "Node" })).toBeNull();
    expect(within(table).queryByText("worker-1")).toBeNull();
  });

  it("says No open shells, and what shows up here, when there are none", async () => {
    items = [];
    renderAt(SHELLS);
    const c = await tab();
    expect(await within(c).findByText("No open shells")).toBeTruthy();
    expect(
      within(c).getByText(
        "Shells that people open in containers on prod show up here while they run.",
      ),
    ).toBeTruthy();
    expect(within(c).queryByRole("table")).toBeNull();
  });

  it("holds the place of the table with skeleton rows while the first poll loads", async () => {
    listPending = true;
    renderAt(SHELLS);
    const c = await tab();
    expect(c.querySelectorAll('[aria-busy="true"] tr').length).toBe(6);
    expect(within(c).queryByText("No open shells")).toBeNull();
  });

  it("says Couldn't load shells. with a Retry that asks again", async () => {
    listStatus = 500;
    renderAt(SHELLS);
    const c = await tab();
    expect(await within(c).findByText(/Couldn't load shells\./)).toBeTruthy();
    listStatus = 200;
    fireEvent.click(within(c).getByRole("button", { name: "Retry" }));
    expect(await within(c).findByText("olive")).toBeTruthy();
  });

  it("is read again every 15 seconds and when the window is focused", () => {
    const q = activeShellsQuery("e1");
    expect(SHELLS_POLL_MS).toBe(15_000);
    expect(q.refetchInterval).toBe(15_000);
    expect(q.refetchOnWindowFocus).toBe(true);
  });

  it("lets a long name be cut with the whole of it kept for a tooltip and for screen readers", async () => {
    const long = "a-very-long-username-that-goes-on-and-on-and-on-forever-and-ever";
    items = [shell({ user: long })];
    renderAt(SHELLS);
    const c = await tab();
    const cell = (await within(c).findByText(long)) as HTMLElement;
    expect(cell.className).toContain("truncate");
  });
});

describe("ending someone's shell", () => {
  it("asks End {user}'s shell? with Cancel first, ends it on confirm, toasts and drops the row", async () => {
    renderAt(SHELLS);
    const c = await tab();
    const table = await within(c).findByRole("table", { name: "Open shells on prod" });
    fireEvent.click(await openMenu(table, "olive"));

    const dialog = await screen.findByRole("alertdialog", { name: "End olive's shell?" });
    expect(
      within(dialog).getByText("The shell in web closes now and anything running in it stops."),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "End session" }));

    await waitFor(() => expect(ended).toEqual(["s-olive"]));
    expect(
      (await screen.findAllByText("Ended olive's shell.", { selector: "[data-sonner-toast] *" }))
        .length,
    ).toBeGreaterThan(0);
    await waitFor(() => expect(within(table).queryByText("olive")).toBeNull());
    expect(within(table).getByText("pat")).toBeTruthy();
  });

  it("ends nothing when the dialog is cancelled", async () => {
    renderAt(SHELLS);
    const c = await tab();
    const table = await within(c).findByRole("table", { name: "Open shells on prod" });
    fireEvent.click(await openMenu(table, "pat"));
    const dialog = await screen.findByRole("alertdialog", { name: "End pat's shell?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(ended).toEqual([]);
    expect(within(table).getByText("pat")).toBeTruthy();
  });

  it("says Shell already closed. when the shell closed on its own first, and the row goes", async () => {
    endStatus = 404;
    renderAt(SHELLS);
    const c = await tab();
    const table = await within(c).findByRole("table", { name: "Open shells on prod" });
    fireEvent.click(await openMenu(table, "olive"));
    const dialog = await screen.findByRole("alertdialog", { name: "End olive's shell?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "End session" }));

    expect(
      (await screen.findAllByText("Shell already closed.", { selector: "[data-sonner-toast] *" }))
        .length,
    ).toBeGreaterThan(0);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() => expect(within(table).queryByText("olive")).toBeNull());
  });

  it("keeps the dialog open with a message when the server fails", async () => {
    endStatus = 500;
    renderAt(SHELLS);
    const c = await tab();
    const table = await within(c).findByRole("table", { name: "Open shells on prod" });
    fireEvent.click(await openMenu(table, "olive"));
    const dialog = await screen.findByRole("alertdialog", { name: "End olive's shell?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "End session" }));
    expect(await within(dialog).findByText("Couldn't end the shell. Try again.")).toBeTruthy();
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });
});
