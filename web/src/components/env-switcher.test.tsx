// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

const READER = makeMe({ allEnvironments: "viewer" });
const ADMIN = makeMe({ instanceAdmin: true });

function env(id: string, name: string, state: EnvironmentItem["status"]["state"], extra = {}) {
  return {
    id,
    name,
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state },
    envType: "swarm",
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
    ...extra,
  };
}

// Names chosen so that a plain sort would put Zulu before alpha.
const ALL = [
  env("z", "Zulu", "online"),
  env("a", "alpha", "online"),
  env("p", "prod-swarm", "degraded"),
  env("b", "Beta", "offline", {
    envType: "standalone",
    status: { state: "offline", since: "2026-10-03T09:00:00Z" },
  }),
];

let account: Record<string, unknown>;
let list: () => Response | Promise<Response>;
let recent: string[];

beforeEach(() => {
  account = ADMIN;
  list = () => Response.json({ items: ALL });
  recent = ["z", "p", "gone"];
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
      if (path === "/api/me") return Response.json(account);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/environments") return list();
      if (path === "/api/me/recent-environments") return Response.json({ environmentIds: recent });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
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
    </QueryClientProvider>,
  );
  return router;
}

// The sidebar's trigger, once the list has loaded (it shows the selected name).
async function trigger(name: string) {
  const button = await screen.findByRole("button", { name: "Switch environment" });
  await waitFor(() => expect(within(button).getByText(name)).toBeTruthy());
  return button;
}

async function openSwitcher(name = "alpha") {
  const button = await trigger(name);
  fireEvent.click(button);
  return await screen.findByRole("combobox", { name: "Find an environment…" });
}

const optionNames = (group: HTMLElement) =>
  within(group)
    .getAllByRole("option")
    .map((o) => o.querySelector("[title]")?.getAttribute("title") ?? o.textContent);

describe("EnvSwitcher (D-35)", () => {
  it("opens with focus in the filter, a Recent group and an Environments group", async () => {
    renderAt("/env/a");
    const filter = await openSwitcher();
    expect(document.activeElement).toBe(filter);
    expect(screen.getByRole("group", { name: "Recent" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Environments" })).toBeTruthy();
  });

  it("lists the server's recent environments in order, leaving out one that is gone", async () => {
    renderAt("/env/a");
    await openSwitcher();
    expect(optionNames(screen.getByRole("group", { name: "Recent" }))).toEqual([
      "Zulu",
      "prod-swarm",
    ]);
  });

  it("lists every environment alphabetically, ignoring case", async () => {
    renderAt("/env/a");
    await openSwitcher();
    expect(optionNames(screen.getByRole("group", { name: "Environments" }))).toEqual([
      "alpha",
      "Beta",
      "prod-swarm",
      "Zulu",
    ]);
  });

  it("checks the selected environment", async () => {
    renderAt("/env/b");
    await openSwitcher("Beta");
    const group = screen.getByRole("group", { name: "Environments" });
    const checked = within(group)
      .getAllByRole("option")
      .filter((o) => o.getAttribute("data-checked") === "true");
    expect(checked).toHaveLength(1);
    expect(checked[0]?.getAttribute("aria-current")).toBe("page");
    expect(within(checked[0] as HTMLElement).getByText("Beta")).toBeTruthy();
  });

  it("shows each item as a link with the name, the type and a dot with an accessible name", async () => {
    renderAt("/env/a/containers");
    await openSwitcher();
    const group = screen.getByRole("group", { name: "Environments" });
    const beta = within(group).getByRole("option", { name: /Beta/ });
    expect(beta.tagName).toBe("A");
    expect(beta.getAttribute("href")).toBe("/env/b/containers");
    expect(within(beta).getByText("Standalone")).toBeTruthy();
    expect(within(beta).getByRole("img", { name: "Offline" })).toBeTruthy();
    expect(within(beta).getByText("Beta").getAttribute("title")).toBe("Beta");
    // Dot only: the health text is not repeated beside it.
    expect(within(beta).queryByText(/Offline since/)).toBeNull();
  });

  it("filters by what is typed, in either group", async () => {
    renderAt("/env/a");
    const filter = await openSwitcher();
    fireEvent.change(filter, { target: { value: "pro" } });
    await waitFor(() => expect(screen.queryByRole("option", { name: /alpha/ })).toBeNull());
    const shown = screen.getAllByRole("option").map((o) => o.textContent ?? "");
    expect(shown.some((s) => s.includes("prod-swarm"))).toBe(true);
    expect(shown.some((s) => s.includes("Zulu"))).toBe(false);
  });

  it("matches the type as well as the name", async () => {
    renderAt("/env/a");
    const filter = await openSwitcher();
    fireEvent.change(filter, { target: { value: "standalone" } });
    await waitFor(() => expect(screen.queryByRole("option", { name: /alpha/ })).toBeNull());
    expect(screen.getByRole("option", { name: /Beta/ })).toBeTruthy();
  });

  it("does not find a pending environment by a type it does not show yet", async () => {
    list = () =>
      Response.json({
        items: [env("n", "new-host", "pending", { envType: undefined }), ...ALL],
      });
    renderAt("/env/a");
    const filter = await openSwitcher();
    fireEvent.change(filter, { target: { value: "swarm" } });
    await waitFor(() => expect(screen.queryByRole("option", { name: /Beta/ })).toBeNull());
    expect(screen.queryByRole("option", { name: /new-host/ })).toBeNull();
    expect(screen.getAllByRole("option", { name: /alpha/ }).length).toBeGreaterThan(0);
  });

  it("says nothing matches and still offers All environments", async () => {
    renderAt("/env/a");
    const filter = await openSwitcher();
    fireEvent.change(filter, { target: { value: "nope" } });
    expect(await screen.findByText("No environments match “nope”.")).toBeTruthy();
    const all = screen.getByRole("option", { name: "All environments" });
    expect(all.getAttribute("href")).toBe("/environments");
  });

  it("goes to the same section of the other environment, and closes", async () => {
    const router = renderAt("/env/a/containers");
    await openSwitcher();
    const group = screen.getByRole("group", { name: "Environments" });
    fireEvent.click(within(group).getByRole("option", { name: /Zulu/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/z/containers"));
    await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  });

  it("chooses with the keyboard", async () => {
    const router = renderAt("/env/a/images");
    const filter = await openSwitcher();
    fireEvent.change(filter, { target: { value: "zulu" } });
    await waitFor(() => expect(screen.queryByRole("option", { name: /alpha/ })).toBeNull());
    fireEvent.keyDown(filter, { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/z/images"));
  });

  it("goes to the dashboard from a page outside an environment", async () => {
    const router = renderAt("/environments");
    const button = await screen.findByRole("button", { name: "Switch environment" });
    await waitFor(() => expect(within(button).getByText("alpha")).toBeTruthy());
    fireEvent.click(button);
    await screen.findByRole("combobox");
    const group = screen.getByRole("group", { name: "Environments" });
    fireEvent.click(within(group).getByRole("option", { name: /Zulu/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/z"));
  });

  it("does not refuse an environment that is down", async () => {
    const router = renderAt("/env/a/containers");
    await openSwitcher();
    const group = screen.getByRole("group", { name: "Environments" });
    fireEvent.click(within(group).getByRole("option", { name: /Beta/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/b/containers"));
  });

  it("leaves a click that asks for a new tab to the browser", async () => {
    const router = renderAt("/env/a/containers");
    await openSwitcher();
    const group = screen.getByRole("group", { name: "Environments" });
    fireEvent.click(within(group).getByRole("option", { name: /Zulu/ }), { ctrlKey: true });
    await act(async () => {});
    expect(router.state.location.pathname).toBe("/env/a/containers");
  });

  it("follows the health stream: a dot changes without reopening", async () => {
    renderAt("/env/a");
    await openSwitcher();
    const group = screen.getByRole("group", { name: "Environments" });
    expect(
      within(within(group).getByRole("option", { name: /Zulu/ })).getByRole("img", {
        name: "Online",
      }),
    ).toBeTruthy();
    const stream = FakeEventSource.instances[0];
    act(() => {
      stream?.sendFrame(
        "env.status",
        { envId: "z", at: "2026-10-03T10:00:00Z", environment: env("z", "Zulu", "offline") },
        "5",
      );
    });
    await waitFor(() =>
      expect(
        within(within(group).getByRole("option", { name: /Zulu/ })).getByRole("img", {
          name: "Offline",
        }),
      ).toBeTruthy(),
    );
  });

  it("shows one skeleton item until the first load finishes", async () => {
    let release: (r: Response) => void = () => {};
    list = () => new Promise<Response>((r) => (release = r));
    renderAt("/env/a");
    const button = await screen.findByRole("button", { name: "Switch environment" });
    fireEvent.click(button);
    const box = await screen.findByRole("combobox", { name: "Find an environment…" });
    expect(box).toBeTruthy();
    expect(document.querySelectorAll("[data-slot=skeleton]")).toHaveLength(1);
    expect(screen.queryByText(/No environments match/)).toBeNull();
    release(Response.json({ items: ALL }));
    await waitFor(() => expect(document.querySelectorAll("[data-slot=skeleton]")).toHaveLength(0));
    expect(screen.getByRole("option", { name: /alpha/ })).toBeTruthy();
  });

  it("keeps the last known name and adds Couldn't load environments when a reload fails", async () => {
    renderAt("/env/a");
    const button = await trigger("alpha");
    list = () => new Response("boom", { status: 500 });
    void queryClient.invalidateQueries({ queryKey: ["environments"] });
    fireEvent.click(button);
    const item = await screen.findByText("Couldn't load environments", {}, { timeout: 4000 });
    expect(item.closest("[aria-disabled=true]")).toBeTruthy();
    expect(within(button).getByText("alpha")).toBeTruthy();
    // The last list is still there to choose from.
    const group = screen.getByRole("group", { name: "Environments" });
    expect(within(group).getByRole("option", { name: /Zulu/ })).toBeTruthy();
  }, 8000);

  it("shows only the Local socket badge, in the trigger and the list, so the name keeps its room", async () => {
    list = () =>
      Response.json({
        items: [env("l", "this-host", "online", { isLocalSocket: true, envType: "standalone" })],
      });
    renderAt("/env/l");
    const button = await trigger("this-host");
    expect(within(button).getByText("Local socket")).toBeTruthy();
    expect(within(button).queryByText("Standalone")).toBeNull();
    fireEvent.click(button);
    await screen.findByRole("combobox");
    const group = screen.getByRole("group", { name: "Environments" });
    const row = within(group).getByRole("option", { name: /this-host/ });
    expect(within(row).queryByText("Standalone")).toBeNull();
    expect(within(row).getByText("Local socket")).toBeTruthy();
    // It is still found by its type.
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "standalone" } });
    await waitFor(() =>
      expect(screen.queryByText("No environments match “standalone”.")).toBeNull(),
    );
    expect(screen.getAllByRole("option", { name: /this-host/ }).length).toBeGreaterThan(0);
  });

  describe("footer", () => {
    it("offers All environments and Manage environments to a settings holder", async () => {
      renderAt("/env/a");
      await openSwitcher();
      expect(screen.getByRole("option", { name: "All environments" })).toBeTruthy();
      const manage = screen.getByRole("option", { name: "Manage environments" });
      expect(manage.getAttribute("href")).toBe("/settings/environments");
    });

    it("leaves Manage environments out for someone who may only read", async () => {
      account = READER;
      renderAt("/env/a");
      await openSwitcher();
      expect(screen.getByRole("option", { name: "All environments" })).toBeTruthy();
      expect(screen.queryByRole("option", { name: "Manage environments" })).toBeNull();
    });

    it("offers Manage environments to an env Admin of one environment, who holds no installation-wide permission", async () => {
      account = makeMe({ envs: { a: "admin" } });
      renderAt("/env/a");
      await openSwitcher();
      expect(screen.getByRole("option", { name: "Manage environments" })).toBeTruthy();
    });

    it("leaves Manage environments out for an Operator or a Viewer of every environment", async () => {
      for (const role of ["operator", "viewer"] as const) {
        account = makeMe({ allEnvironments: role });
        renderAt("/env/a");
        await openSwitcher();
        expect(screen.getByRole("option", { name: "All environments" })).toBeTruthy();
        expect(screen.queryByRole("option", { name: "Manage environments" })).toBeNull();
        cleanup();
        queryClient.clear();
      }
    });

    const originalUA = navigator.userAgent;
    afterEach(() => {
      Object.defineProperty(navigator, "userAgent", { value: originalUA, configurable: true });
    });

    it("shows the shortcut as Ctrl K, or ⌘K on a Mac", async () => {
      Object.defineProperty(navigator, "userAgent", {
        value: "Mozilla/5.0 (X11; Linux x86_64) Chrome/150.0",
        configurable: true,
      });
      renderAt("/env/a");
      await openSwitcher();
      const hint = document.querySelector("kbd[data-slot=kbd]");
      expect(hint?.textContent).toBe("Ctrl K");
      cleanup();
      queryClient.clear();

      Object.defineProperty(navigator, "userAgent", {
        value: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15",
        configurable: true,
      });
      renderAt("/env/a");
      await openSwitcher();
      expect(document.querySelector("kbd[data-slot=kbd]")?.textContent).toBe("⌘K");
    });
  });

  it("makes the trigger and every item at least 44px high under 768px", async () => {
    renderAt("/env/a");
    const button = await trigger("alpha");
    expect(button.className).toContain("max-md:h-11");
    fireEvent.click(button);
    await screen.findByRole("combobox");
    for (const option of screen.getAllByRole("option")) {
      expect(option.className).toContain("max-md:min-h-11");
    }
  });
});
