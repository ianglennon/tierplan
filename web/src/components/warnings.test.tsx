// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { ENV_WARNINGS_KEY } from "@/api/warnings";
import { Toaster } from "@/components/ui/sonner";
import { DataNotPersistedBanner, HealthBell } from "@/components/warnings";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EnvHealthStreamProvider } from "@/features/environments/health-stream";
import { meQueryOptions } from "@/hooks/use-me";
import { formatClock } from "@/lib/format";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";

// Whether the page counts as "plain HTTP from another host" is decided by
// window.location, which jsdom does not let a test change.
let plainHttp = false;
vi.mock("@/components/auth-layout", () => ({ isPlainHttpRemote: () => plainHttp }));

const ADMIN = makeMe({
  instanceAdmin: true,
  permissions: { global: ["settings.read", "settings.write"], environments: {} },
});
const VIEWER = { ...ADMIN, permissions: { global: [], environments: {} } };
// Reads every environment and may finish setting one up, but has no say over the instance.
const OPERATOR = {
  ...ADMIN,
  permissions: splitPermissions(["environments.read", "environments.token"]),
};
const ADMIN_AND_READER = {
  ...ADMIN,
  permissions: { global: ["settings.read"], environments: { "*": ["environments.read"] } },
};
const READER = {
  ...ADMIN,
  permissions: { global: [], environments: { "*": ["environments.read"] } },
};

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
}

let sent: Sent[] = [];
let me = ADMIN;
let items: Array<Record<string, string | number>> = [];
let ackStatus = 204;
// What GET /api/environments/warnings answers, how, and when (a gate holds the
// answer back to show the first-load state).
let envItems: Array<Record<string, unknown>> = [];
let envStatus = 200;
let envGate: Promise<void> | null = null;
// What GET /api/registries/alerts answers.
let registryItems: Array<Record<string, unknown>> = [];

beforeEach(() => {
  sent = [];
  me = ADMIN;
  items = [];
  ackStatus = 204;
  envItems = [];
  envStatus = 200;
  envGate = null;
  registryItems = [];
  plainHttp = false;
  setCsrfToken("csrf-1");
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/warnings") return Response.json({ items });
      if (path === "/api/environments/warnings") {
        await envGate;
        if (envStatus !== 200) return Response.json({}, { status: envStatus });
        return Response.json({ items: envItems });
      }
      if (path === "/api/registries/alerts") return Response.json({ items: registryItems });
      if (path === "/api/warnings/key-backup/ack") {
        if (ackStatus === 204) {
          items = items.filter((w) => w.id !== "key_backup");
          return new Response(null, { status: 204 });
        }
        return Response.json({ status: ackStatus }, { status: ackStatus });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

let router: ReturnType<typeof makeRouter>;

// A router with the pages the bell links to, so a click can be followed.
function makeRouter() {
  const root = createRootRoute({
    component: () => (
      <>
        <DataNotPersistedBanner />
        <HealthBell />
        <Toaster />
        <Outlet />
      </>
    ),
  });
  const page = (path: string) =>
    createRoute({
      getParentRoute: () => root,
      path,
      component: () => <p data-testid="page">{path}</p>,
    });
  return createRouter({
    routeTree: root.addChildren([
      page("/"),
      page("/environments"),
      page("/audit"),
      page("/registries"),
      page("/settings/backup"),
      page("/env/$envId"),
      page("/settings/environments"),
      page("/settings/environments/$envId"),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
}

function renderWarnings() {
  queryClient.setQueryData(meQueryOptions.queryKey, me);
  router = makeRouter();
  render(
    <QueryClientProvider client={queryClient}>
      <EnvHealthStreamProvider>
        <RouterProvider router={router} />
      </EnvHealthStreamProvider>
    </QueryClientProvider>,
  );
}

const warningsCalls = () => sent.filter((s) => s.path === "/api/warnings");
const registryAlertCalls = () => sent.filter((s) => s.path === "/api/registries/alerts");
const envWarningsCalls = () => sent.filter((s) => s.path === "/api/environments/warnings");

async function openBell(name: string | RegExp = "Warnings") {
  fireEvent.click(await screen.findByRole("button", { name }));
}

describe("HealthBell", () => {
  it("says there are no warnings, with no count badge, when nothing applies", async () => {
    renderWarnings();
    await waitFor(() => expect(warningsCalls()).toHaveLength(1));
    const bell = screen.getByRole("button", { name: "Warnings" });
    expect(bell.textContent).toBe("");
    await openBell();
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });

  it("counts and lists the key backup reminder", async () => {
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    renderWarnings();
    const bell = await screen.findByRole("button", { name: "Warnings, 1 item" });
    expect(bell.textContent).toBe("1");
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("Back up your encryption key.")).toBeTruthy();
    expect(
      screen.getByText("Stored secrets can't be recovered without /data/bayplan.key."),
    ).toBeTruthy();
    expect(screen.queryByText("No warnings")).toBeNull();
  });

  it("lists the plain HTTP warning from a non-localhost http page, with its fix link", async () => {
    plainHttp = true;
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("You're using Bayplan over plain HTTP.")).toBeTruthy();
    const fix = screen.getByRole("link", {
      name: "Serve Bayplan over HTTPS or put it behind a reverse proxy",
    });
    expect(fix.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("counts both items together", async () => {
    plainHttp = true;
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    renderWarnings();
    expect(await screen.findByRole("button", { name: "Warnings, 2 items" })).toBeTruthy();
  });

  it("acknowledges the key backup once and the item disappears", async () => {
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    fireEvent.click(await screen.findByRole("button", { name: "I've backed it up" }));

    await waitFor(() => expect(screen.queryByText("Back up your encryption key.")).toBeNull());
    const acks = sent.filter((s) => s.path === "/api/warnings/key-backup/ack");
    expect(acks).toHaveLength(1);
    expect(acks[0]?.method).toBe("POST");
    expect(acks[0]?.csrf).toBe("csrf-1");
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });

  it("toasts a failure and keeps the reminder when the acknowledgement fails", async () => {
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    ackStatus = 500;
    renderWarnings();
    await openBell("Warnings, 1 item");
    fireEvent.click(await screen.findByRole("button", { name: "I've backed it up" }));
    expect(await screen.findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(screen.getByText("Back up your encryption key.")).toBeTruthy();
  });

  it("never asks the server for warnings without settings.read", async () => {
    me = VIEWER;
    renderWarnings();
    await screen.findByRole("button", { name: "Warnings" });
    expect(warningsCalls()).toHaveLength(0);
  });
});

describe("HealthBell reverse proxy items", () => {
  const DOCS = "https://github.com/ianglennon/bayplan/blob/main/docs/reverse-proxy.md";

  it("lists untrusted forwarded headers with the address and a docs link that opens in a new tab", async () => {
    items = [{ id: "proxy_untrusted_forwarded", ip: "10.0.0.5" }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("Forwarded headers from an untrusted address")).toBeTruthy();
    expect(
      screen.getByText(
        "Bayplan got X-Forwarded headers from 10.0.0.5, which isn't in BAYPLAN_TRUSTED_PROXIES. They were ignored.",
      ),
    ).toBeTruthy();
    const link = screen.getByRole("link", { name: "Reverse proxy docs" });
    expect(link.getAttribute("href")).toBe(DOCS);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("lists a URL mismatch with the host, the scheme and the public URL", async () => {
    items = [
      {
        id: "proxy_url_mismatch",
        host: "bayplan:7480",
        scheme: "http",
        url: "https://bayplan.example.com",
      },
    ];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("Requests don't match the public URL")).toBeTruthy();
    expect(
      screen.getByText(
        "Requests arrive for bayplan:7480 over http, but the public URL is https://bayplan.example.com. Sign-in and agent connections may fail.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Reverse proxy docs" }).getAttribute("href")).toBe(
      DOCS,
    );
  });

  it("lists failing WebSocket connections", async () => {
    items = [{ id: "proxy_websocket_failing", ip: "203.0.113.9" }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("WebSocket connections are failing")).toBeTruthy();
    expect(
      screen.getByText(
        "Agents or terminals can't connect through your proxy. Check that it passes the Upgrade headers.",
      ),
    ).toBeTruthy();
  });

  it("counts every proxy item with the others and lists them all", async () => {
    plainHttp = true;
    items = [
      { id: "key_backup", keyPath: "/data/bayplan.key" },
      { id: "proxy_untrusted_forwarded", ip: "10.0.0.5" },
      { id: "proxy_websocket_failing", ip: "203.0.113.9" },
    ];
    renderWarnings();
    await openBell("Warnings, 4 items");
    expect(screen.getAllByRole("link", { name: "Reverse proxy docs" })).toHaveLength(2);
  });

  it("never shows a proxy item to a user without settings.read", async () => {
    me = VIEWER;
    items = [{ id: "proxy_websocket_failing", ip: "203.0.113.9" }];
    renderWarnings();
    await openBell("Warnings");
    expect(await screen.findByText("No warnings")).toBeTruthy();
    expect(screen.queryByText("WebSocket connections are failing")).toBeNull();
  });

  it("drops the item when the server stops listing it", async () => {
    items = [{ id: "proxy_websocket_failing", ip: "203.0.113.9" }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("WebSocket connections are failing")).toBeTruthy();
    items = [];
    await queryClient.invalidateQueries({ queryKey: ["warnings"] });
    await waitFor(() => expect(screen.queryByText("WebSocket connections are failing")).toBeNull());
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });
});

describe("DataNotPersistedBanner", () => {
  it("shows the UI-SPEC copy when the data directory is not persisted, with no way to dismiss it", async () => {
    items = [{ id: "data_not_persisted", reason: "not a mount point" }];
    renderWarnings();
    const banner = await screen.findByRole("status");
    expect(banner.textContent).toBe(
      "Data isn't being persisted. /data isn't a mounted volume, so users, sessions and the audit log are lost when this container is removed. Mount a volume at /data and restart.",
    );
    expect(banner.querySelector("button")).toBeNull();
  });

  it("is absent when the data directory is persisted", async () => {
    renderWarnings();
    await waitFor(() => expect(warningsCalls()).toHaveLength(1));
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("is not part of the bell's count", async () => {
    items = [{ id: "data_not_persisted", reason: "tmpfs" }];
    renderWarnings();
    await screen.findByRole("status");
    expect(screen.getByRole("button", { name: "Warnings" })).toBeTruthy();
  });
});

// The environment problems of the bell (plan 02-31, ENV-06, ENV-08, D-37, D-39).

const SINCE = "2026-10-04T14:05:00Z";
const offline = (id: string, name: string, since = SINCE, reason = "timeout") => ({
  id: "env_offline",
  severity: "error",
  envId: id,
  envName: name,
  reason,
  since,
});

// The bell's list, top to bottom: each item's kind and title.
function listed(): Array<[string | null, string]> {
  return Array.from(document.querySelectorAll("[data-warning]")).map((el) => [
    el.getAttribute("data-warning"),
    el.querySelector("[data-slot=item-title]")?.textContent ?? "",
  ]);
}

function badge(): string | null {
  const bell = screen.getByRole("button", { name: /^Warnings/ });
  return bell.textContent === "" ? null : bell.textContent;
}

describe("HealthBell environment items", () => {
  beforeEach(() => {
    me = READER;
  });

  it("lists each offline environment with its time and reason and a View link to it", async () => {
    envItems = [
      offline("e1", "Edge"),
      offline("e2", "Beta", "2026-10-04T13:00:00Z", "agent_offline"),
    ];
    renderWarnings();
    await openBell("Warnings, 2 items");
    expect(await screen.findByText("Edge is offline")).toBeTruthy();
    expect(screen.getByText("Beta is offline")).toBeTruthy();

    const edge = document.querySelector('[data-warning="env_offline"]') as HTMLElement;
    expect(edge.textContent).toContain(`Offline since ${formatClock(SINCE)}.`);
    expect(edge.textContent).toContain("A node stopped answering.");
    const links = screen.getAllByRole("link", { name: "View" });
    expect(links.map((l) => l.getAttribute("href"))).toEqual(["/env/e1", "/env/e2"]);
  });

  it("collapses more than five offline environments into one item that links to the overview", async () => {
    envItems = ["a", "b", "c", "d", "e", "f"].map((id) => offline(id, `Env ${id}`));
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("6 environments are offline")).toBeTruthy();
    expect(screen.getByText("Open the overview to see which.")).toBeTruthy();
    expect(screen.queryByText("Env a is offline")).toBeNull();
    expect(listed()).toHaveLength(1);
    expect(screen.getByRole("link", { name: "View environments" }).getAttribute("href")).toBe(
      "/environments",
    );
  });

  it("keeps five offline environments as five items", async () => {
    envItems = ["a", "b", "c", "d", "e"].map((id) => offline(id, `Env ${id}`));
    renderWarnings();
    await openBell("Warnings, 5 items");
    expect(await screen.findByText("Env a is offline")).toBeTruthy();
    expect(listed()).toHaveLength(5);
    expect(screen.queryByText(/environments are offline/)).toBeNull();
  });

  it("orders errors before warnings and then newest first, with the instance items after the dated ones", async () => {
    plainHttp = true;
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    envItems = [
      { id: "fold_offered", severity: "warning", envId: "s1", envName: "Prod swarm" },
      offline("old", "Older", "2026-10-04T10:00:00Z"),
      {
        id: "clock_skew",
        severity: "warning",
        envId: "c1",
        envName: "Edge",
        node: "node-a",
        offsetSeconds: 45,
        since: "2026-10-04T11:00:00Z",
      },
      offline("new", "Newer", "2026-10-04T12:00:00Z"),
    ];
    me = ADMIN_AND_READER;
    renderWarnings();
    await openBell("Warnings, 6 items");
    await screen.findByText("Newer is offline");
    expect(listed().map(([, title]) => title)).toEqual([
      "Newer is offline",
      "Older is offline",
      "The clock is off on node-a",
      "This host is part of Prod swarm",
      "You're using Bayplan over plain HTTP.",
      "Back up your encryption key.",
    ]);
  });

  it("counts the items on the badge, caps it at 9+ and hides it at none", async () => {
    envItems = Array.from({ length: 9 }, (_, i) => ({
      id: "fold_offered",
      severity: "warning",
      envId: `s${i}`,
      envName: `Swarm ${i}`,
    }));
    renderWarnings();
    await screen.findByRole("button", { name: "Warnings, 9 items" });
    expect(badge()).toBe("9");
    cleanup();

    envItems = [
      ...envItems,
      { id: "fold_offered", severity: "warning", envId: "s9", envName: "S9" },
    ];
    queryClient.clear();
    renderWarnings();
    await screen.findByRole("button", { name: "Warnings, 10 items" });
    expect(badge()).toBe("9+");
    cleanup();

    envItems = [];
    queryClient.clear();
    renderWarnings();
    await waitFor(() => expect(envWarningsCalls().length).toBeGreaterThan(0));
    expect(badge()).toBeNull();
    await openBell();
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });

  it("words a certificate that is not renewing with its node, environment and expiry", async () => {
    const expires = new Date(Date.now() + 6.5 * 86_400_000).toISOString();
    envItems = [
      {
        id: "cert_not_renewing",
        severity: "warning",
        envId: "e1",
        envName: "Edge",
        node: "node-a",
        expiresAt: expires,
        since: new Date().toISOString(),
      },
    ];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("An agent certificate isn't renewing")).toBeTruthy();
    expect(
      screen.getByText(
        "node-a on Edge: the certificate expires in 6 days. Check that the agent can reach Bayplan.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "View nodes" }).getAttribute("href")).toBe(
      "/settings/environments/e1",
    );
  });

  it("words an expired certificate as expired", async () => {
    envItems = [
      {
        id: "cert_not_renewing",
        severity: "error",
        envId: "e1",
        envName: "Edge",
        node: "node-a",
        expiresAt: new Date(Date.now() - 3 * 3_600_000).toISOString(),
        since: new Date().toISOString(),
      },
    ];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(
      await screen.findByText(/node-a on Edge: the certificate expired .+\. Check/),
    ).toBeTruthy();
  });

  it("counts the agents that need a manual update", async () => {
    envItems = [{ id: "manual_update_needed", severity: "warning", count: 2 }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("2 agents need a manual update")).toBeTruthy();
    expect(
      screen.getByText("Auto-update is off or the agent is outside the compatibility window."),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "View" }).getAttribute("href")).toBe(
      "/settings/environments",
    );
    cleanup();

    envItems = [
      { id: "manual_update_needed", severity: "warning", count: 1, envId: "e1", envName: "Edge" },
    ];
    queryClient.clear();
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("1 agent needs a manual update")).toBeTruthy();
    expect(screen.getByRole("link", { name: "View" }).getAttribute("href")).toBe(
      "/settings/environments/e1",
    );
  });

  it("names the node and the offset when a clock is out of step", async () => {
    envItems = [
      {
        id: "clock_skew",
        severity: "warning",
        envId: "e1",
        envName: "Edge",
        node: "node-a",
        offsetSeconds: -45,
      },
    ];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("The clock is off on node-a")).toBeTruthy();
    expect(
      screen.getByText(
        "node-a on Edge is 45 s out of step. Bayplan refuses agents beyond 5 minutes.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "View nodes" }).getAttribute("href")).toBe(
      "/settings/environments/e1",
    );
  });

  it("offers Finish setup for an environment that has had no agent for days, and it opens the install dialog", async () => {
    envItems = [
      {
        id: "pending_long",
        severity: "warning",
        envId: "e1",
        envName: "Waiting",
        days: 8,
        since: SINCE,
      },
    ];
    me = OPERATOR;
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("Waiting still has no agent")).toBeTruthy();
    expect(
      screen.getByText("It was added 8 days ago. Finish installing the agent or remove it."),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Finish setup" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: "Show install command" })).toBeTruthy();
    // The popover made way for the dialog.
    expect(screen.queryByText("Waiting still has no agent")).toBeNull();
  });

  it("puts focus back on the bell when the install dialog it opened is closed", async () => {
    envItems = [
      {
        id: "pending_long",
        severity: "warning",
        envId: "e1",
        envName: "Waiting",
        days: 8,
        since: SINCE,
      },
    ];
    me = OPERATOR;
    renderWarnings();
    await openBell("Warnings, 1 item");
    fireEvent.click(await screen.findByRole("button", { name: "Finish setup" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Warnings, 1 item" })),
    );
  });

  it("links a pending environment to its page when the caller may not make tokens", async () => {
    envItems = [
      {
        id: "pending_long",
        severity: "warning",
        envId: "e1",
        envName: "Waiting",
        days: 1,
        since: SINCE,
      },
    ];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(
      await screen.findByText("It was added 1 day ago. Finish installing the agent or remove it."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Finish setup" })).toBeNull();
    expect(screen.getByRole("link", { name: "View" }).getAttribute("href")).toBe("/env/e1");
  });

  it("words a re-initialised swarm and the fold offer, each with its link", async () => {
    envItems = [
      {
        id: "cluster_changed",
        severity: "error",
        envId: "e1",
        envName: "Swarm",
        reason: "cluster_changed",
        since: SINCE,
      },
      { id: "fold_offered", severity: "warning", envId: "s1", envName: "Prod swarm" },
    ];
    renderWarnings();
    await openBell("Warnings, 2 items");
    expect(await screen.findByText("Swarm was re-initialised")).toBeTruthy();
    expect(screen.getByText("Adopt the new cluster or remove the environment.")).toBeTruthy();
    expect(screen.getByText("This host is part of Prod swarm")).toBeTruthy();
    expect(
      screen.getByText("Fold it into the swarm environment so it isn't listed twice."),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "View" }).getAttribute("href")).toBe("/env/e1");
    expect(screen.getByRole("link", { name: "Review" }).getAttribute("href")).toBe(
      "/settings/environments/s1#fold",
    );
  });

  it("opens with focus on the popover, so Tab reaches the first item rather than the first button in the list", async () => {
    envItems = [
      offline("e1", "Edge"),
      {
        id: "pending_long",
        severity: "warning",
        envId: "p1",
        envName: "Waiting",
        days: 8,
        since: SINCE,
      },
    ];
    me = OPERATOR;
    renderWarnings();
    await openBell("Warnings, 2 items");
    await screen.findByText("Edge is offline");
    await waitFor(() =>
      expect(document.activeElement?.getAttribute("data-slot")).toBe("popover-content"),
    );
  });

  it("closes the popover and follows the link when one is used", async () => {
    envItems = [offline("e1", "Edge")];
    renderWarnings();
    await openBell("Warnings, 1 item");
    fireEvent.click(await screen.findByRole("link", { name: "View" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/e1"));
    expect(screen.queryByText("Edge is offline")).toBeNull();
  });

  it("shows a name an agent supplied as text, and a long name with its whole title on hover", async () => {
    const hostile = "<img src=x onerror=alert(1)>";
    const long = "a-very-long-environment-name-".repeat(3);
    envItems = [
      {
        id: "clock_skew",
        severity: "warning",
        envId: "e1",
        envName: hostile,
        node: "<b>node</b>",
        offsetSeconds: 60,
      },
      offline("e2", long),
    ];
    renderWarnings();
    await openBell("Warnings, 2 items");
    await screen.findByText("The clock is off on <b>node</b>");
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("[data-warning=clock_skew] b")).toBeNull();
    const title = screen.getByText(`${long} is offline`);
    expect(title.getAttribute("title")).toBe(`${long} is offline`);
  });

  it("asks for environment problems only when the caller may read an environment", async () => {
    me = ADMIN;
    renderWarnings();
    await waitFor(() => expect(warningsCalls()).toHaveLength(1));
    expect(envWarningsCalls()).toHaveLength(0);
  });

  it("shows no badge and one skeleton item until the first answer, and never No warnings before it", async () => {
    let release: () => void = () => {};
    envGate = new Promise<void>((resolve) => {
      release = resolve;
    });
    renderWarnings();
    await openBell();
    expect(badge()).toBeNull();
    await waitFor(() =>
      expect(document.querySelectorAll("[data-slot=skeleton]").length).toBeGreaterThan(0),
    );
    expect(document.querySelectorAll("[data-warning]")).toHaveLength(0);
    expect(screen.queryByText("No warnings")).toBeNull();

    envItems = [offline("e1", "Edge")];
    await act(async () => release());
    expect(await screen.findByText("Edge is offline")).toBeTruthy();
    expect(document.querySelectorAll("[data-slot=skeleton]")).toHaveLength(0);
  });

  it("keeps the last items when a refresh fails, and polls every 30 seconds", async () => {
    envItems = [offline("e1", "Edge")];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("Edge is offline")).toBeTruthy();

    const query = queryClient.getQueryCache().find({ queryKey: ENV_WARNINGS_KEY });
    expect(query?.observers[0]?.options.refetchInterval).toBe(30_000);

    envStatus = 500;
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ENV_WARNINGS_KEY });
    });
    await waitFor(() => expect(query?.state.fetchStatus).toBe("idle"));
    expect(screen.getByText("Edge is offline")).toBeTruthy();
    expect(screen.queryByText("No warnings")).toBeNull();
  });

  it("reads the environment problems again when the health stream reports a change", async () => {
    envItems = [];
    renderWarnings();
    await waitFor(() => expect(envWarningsCalls()).toHaveLength(1));
    const stream = FakeEventSource.instances[0];
    if (!stream) throw new Error("the health stream was not opened");
    await act(async () => stream.open());

    envItems = [offline("e1", "Edge")];
    await act(async () => {
      stream.sendFrame("env.status", { envId: "e1", at: "2026-10-04T14:05:00Z" }, "7");
    });
    await waitFor(() => expect(envWarningsCalls().length).toBeGreaterThan(1));
    expect(await screen.findByRole("button", { name: "Warnings, 1 item" })).toBeTruthy();
  });

  it("reads them again on a failover frame, which the stream now delivers", async () => {
    renderWarnings();
    await waitFor(() => expect(envWarningsCalls()).toHaveLength(1));
    const stream = FakeEventSource.instances[0];
    if (!stream) throw new Error("the health stream was not opened");
    await act(async () => stream.open());
    await act(async () => {
      stream.sendFrame(
        "failover",
        { envId: "e1", at: "2026-10-04T14:06:00Z", fromNode: "mgr-1", toNode: "mgr-2" },
        "8",
      );
    });
    await waitFor(() => expect(envWarningsCalls().length).toBeGreaterThan(1));
  });
});

describe("HealthBell refusal bursts", () => {
  it("lists a burst for an instance admin with the user, the count and a link to the audit log", async () => {
    items = [{ id: "refusal_burst", subject: "mallory", count: 12 }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("12 requests were refused")).toBeTruthy();
    expect(
      screen.getByText("mallory was refused 12 times in the last minute. Look at the audit log."),
    ).toBeTruthy();
    const link = screen.getByRole("link", { name: "View audit log" });
    expect(link.getAttribute("href")).toContain("/audit");
    expect(link.getAttribute("href")).toContain("action=permission.denied");
    // Derived from live state: there is nothing to dismiss.
    expect(screen.queryByRole("button", { name: /dismiss|acknowledge/i })).toBeNull();
  });

  it("says a single refusal in the singular", async () => {
    items = [{ id: "refusal_burst", subject: "mallory", count: 1 }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("1 request was refused")).toBeTruthy();
  });

  it("shows the unchanged empty bell when no burst or other condition holds", async () => {
    renderWarnings();
    await openBell();
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });
});

describe("HealthBell failed scheduled backups", () => {
  const TWO_DAYS_AGO = new Date(Date.now() - 2 * 86_400_000).toISOString();

  it("lists the failure for an instance admin with the reason, when the last good backup was, and a link to the backups", async () => {
    items = [{ id: "backup_failed", subject: "no space left on device", since: TWO_DAYS_AGO }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("The scheduled backup failed")).toBeTruthy();
    expect(
      screen.getByText("no space left on device. The last good backup was 2 d ago."),
    ).toBeTruthy();
    const link = screen.getByRole("link", { name: "Open backups" });
    expect(link.getAttribute("href")).toContain("/settings/backup");
    // Derived from live state: there is nothing to dismiss.
    expect(screen.queryByRole("button", { name: /dismiss|acknowledge/i })).toBeNull();
  });

  it("gives only the reason when there has never been a good backup", async () => {
    items = [{ id: "backup_failed", subject: "no space left on device" }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("no space left on device.")).toBeTruthy();
  });

  it("follows the link to the backup page and closes the list", async () => {
    items = [{ id: "backup_failed", subject: "boom", since: TWO_DAYS_AGO }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    fireEvent.click(await screen.findByRole("link", { name: "Open backups" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/backup"));
    await waitFor(() => expect(screen.queryByText("The scheduled backup failed")).toBeNull());
  });

  it("counts with the other items", async () => {
    items = [
      { id: "backup_failed", subject: "boom" },
      { id: "key_backup", keyPath: "/data/bayplan.key" },
    ];
    renderWarnings();
    expect(await screen.findByRole("button", { name: "Warnings, 2 items" })).toBeTruthy();
  });

  it("shows nothing for someone who is not an instance admin, and nothing when no failure is current", async () => {
    me = VIEWER;
    items = [{ id: "backup_failed", subject: "boom" }];
    renderWarnings();
    await openBell();
    expect(await screen.findByText("No warnings")).toBeTruthy();
    expect(screen.queryByText("The scheduled backup failed")).toBeNull();
  });
});

describe("HealthBell agents not known after a restore", () => {
  it("lists how many agents are not known with the reason and a link to Settings > Environments", async () => {
    items = [{ id: "unknown_agents", count: 3 }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("3 agents aren't known to this Bayplan")).toBeTruthy();
    expect(screen.getByText("They were installed after the backup you restored.")).toBeTruthy();
    const link = screen.getByRole("link", { name: "View" });
    expect(link.getAttribute("href")).toContain("/settings/environments");
    // Derived from what agents tried lately: there is nothing to dismiss.
    expect(screen.queryByRole("button", { name: /dismiss|acknowledge/i })).toBeNull();
  });

  it("says it in the singular for one agent", async () => {
    items = [{ id: "unknown_agents", count: 1 }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("1 agent isn't known to this Bayplan")).toBeTruthy();
    expect(screen.getByText("It was installed after the backup you restored.")).toBeTruthy();
  });

  it("follows the link to the environments settings and closes the list", async () => {
    items = [{ id: "unknown_agents", count: 2 }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    fireEvent.click(await screen.findByRole("link", { name: "View" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/environments"));
    await waitFor(() =>
      expect(screen.queryByText("2 agents aren't known to this Bayplan")).toBeNull(),
    );
  });

  it("shows nothing for someone who is not an instance admin", async () => {
    me = VIEWER;
    items = [{ id: "unknown_agents", count: 2 }];
    renderWarnings();
    await openBell();
    expect(await screen.findByText("No warnings")).toBeTruthy();
    expect(screen.queryByText(/aren't known to this Bayplan/)).toBeNull();
  });
});

// The credential items of the bell (plan 05-21, IMG-01, D-07).

// An instance administrator, and an Admin of one environment, who can fix credentials; and
// Operators and Viewers, who cannot.
const REGISTRY_ADMIN = makeMe({ instanceAdmin: true });
const ENV_ADMIN = makeMe({ envs: { "env-a": "admin" } });
const ENV_OPERATOR = makeMe({ envs: { "env-a": "operator" } });
const ENV_VIEWER = makeMe({ envs: { "env-a": "viewer" } });

const alertOf = (kind: string, id: string, name: string, extra: Record<string, unknown> = {}) => ({
  kind,
  credentialId: id,
  name,
  host: "ghcr.io",
  prefix: "",
  scope: { kind: "instance", envId: null, envName: null },
  days: 3,
  at: "2026-10-15T23:59:59Z",
  ...extra,
});
const expiring = (id: string, name: string, days: number, extra: Record<string, unknown> = {}) =>
  alertOf("expiring", id, name, { days, ...extra });

describe("HealthBell registry credential items", () => {
  it("lists an instance credential that expires in 3 days and opens its sheet", async () => {
    me = REGISTRY_ADMIN;
    registryItems = [expiring("cred-1", "ghcr-acme", 3)];
    renderWarnings();
    await openBell("Warnings, 1 item");

    expect(await screen.findByText("“ghcr-acme” expires in 3 days")).toBeTruthy();
    expect(
      screen.getByText("Instance credential for ghcr.io. Pulls that use it fail after that."),
    ).toBeTruthy();
    const link = screen.getByRole("link", { name: "Open registries" });
    expect(link.getAttribute("href")).toBe("/registries?credential=cred-1");
    fireEvent.click(link);
    await waitFor(() => expect(router.state.location.pathname).toBe("/registries"));
    expect(router.state.location.search).toEqual({ credential: "cred-1" });
  });

  it("says one day in the singular and names the environment of an environment credential", async () => {
    me = ENV_ADMIN;
    registryItems = [
      expiring("cred-2", "team", 1, {
        scope: { kind: "environment", envId: "env-a", envName: "Production" },
      }),
    ];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("“team” expires in 1 day")).toBeTruthy();
    expect(
      screen.getByText("Production credential for ghcr.io. Pulls that use it fail after that."),
    ).toBeTruthy();
  });

  it("asks only an instance administrator or an Admin of an environment", async () => {
    // Both read their environment, so the bell does ask about those problems; the credential
    // alerts are the part they are not for.
    for (const who of [ENV_OPERATOR, ENV_VIEWER]) {
      sent = [];
      queryClient.clear();
      me = who;
      registryItems = [expiring("cred-1", "ghcr-acme", 3)];
      renderWarnings();
      await waitFor(() => expect(envWarningsCalls()).toHaveLength(1));
      await openBell();
      expect(await screen.findByText("No warnings")).toBeTruthy();
      expect(registryAlertCalls()).toHaveLength(0);
      cleanup();
    }
    sent = [];
    me = ENV_ADMIN;
    renderWarnings();
    await waitFor(() => expect(registryAlertCalls()).toHaveLength(1));
  });

  it("adds no item and no count when no credential needs attention", async () => {
    me = REGISTRY_ADMIN;
    renderWarnings();
    await waitFor(() => expect(registryAlertCalls()).toHaveLength(1));
    await openBell();
    expect(await screen.findByText("No warnings")).toBeTruthy();
    expect(badge()).toBeNull();
  });

  it("drops the item once the server stops listing it", async () => {
    me = REGISTRY_ADMIN;
    registryItems = [expiring("cred-1", "ghcr-acme", 3)];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("“ghcr-acme” expires in 3 days")).toBeTruthy();
    registryItems = [];
    await queryClient.invalidateQueries({ queryKey: ["registries"] });
    await waitFor(() => expect(screen.queryByText("“ghcr-acme” expires in 3 days")).toBeNull());
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });

  it("lists an expired credential as an error and links to it", async () => {
    me = REGISTRY_ADMIN;
    registryItems = [
      alertOf("expired", "cred-3", "old-token", { at: "2026-10-01T23:59:59Z", days: undefined }),
    ];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("“old-token” has expired")).toBeTruthy();
    expect(
      screen.getByText("Instance credential for ghcr.io. Pulls that use it are being refused."),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open registries" }).getAttribute("href")).toBe(
      "/registries?credential=cred-3",
    );
  });

  it("lists a refused credential with when it failed and a Test credential link", async () => {
    me = REGISTRY_ADMIN;
    const failed = new Date(Date.now() - 2 * 3_600_000).toISOString();
    registryItems = [alertOf("auth_failed", "cred-4", "ghcr-acme", { at: failed })];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("“ghcr-acme” was refused by ghcr.io")).toBeTruthy();
    expect(
      screen.getByText(/^Its last use failed .+\. Check its token or test it\.$/),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Test credential" }).getAttribute("href")).toBe(
      "/registries?credential=cred-4",
    );
  });

  it("collapses several of a kind into one item that links to the filtered table", async () => {
    me = REGISTRY_ADMIN;
    registryItems = [
      alertOf("expiring", "cred-1", "one", { days: 2 }),
      alertOf("expiring", "cred-2", "two", { days: 5 }),
      alertOf("expired", "cred-3", "three"),
      alertOf("expired", "cred-4", "four"),
      alertOf("auth_failed", "cred-5", "five"),
      alertOf("auth_failed", "cred-6", "six"),
    ];
    renderWarnings();
    await openBell("Warnings, 3 items");
    expect(await screen.findByText("2 registry credentials have expired")).toBeTruthy();
    expect(screen.getByText("2 registry credentials were refused")).toBeTruthy();
    expect(screen.getByText("2 registry credentials expire within 7 days")).toBeTruthy();
    expect(screen.getByText("Pulls that use them are being refused.")).toBeTruthy();
    expect(screen.getByText("Pulls that use them fail after that.")).toBeTruthy();
    expect(
      screen.getByText("Their last use failed. Check their tokens or test them."),
    ).toBeTruthy();
    expect(screen.queryByText(/“one”/)).toBeNull();
    // Most severe first: expired, then refused, then the expiry to come.
    expect(listed().map(([id]) => id)).toEqual([
      "registry_expired",
      "registry_auth_failed",
      "registry_expiring",
    ]);
    expect(
      screen.getAllByRole("link", { name: "Open registries" }).map((l) => l.getAttribute("href")),
    ).toEqual([
      "/registries?alert=expired",
      "/registries?alert=auth_failed",
      "/registries?alert=expiring",
    ]);
  });

  it("puts an expired credential among the errors, before the warnings", async () => {
    me = REGISTRY_ADMIN;
    plainHttp = true;
    registryItems = [alertOf("expiring", "cred-1", "soon"), alertOf("expired", "cred-2", "gone")];
    renderWarnings();
    await openBell("Warnings, 3 items");
    await screen.findByText("“gone” has expired");
    expect(listed().map(([id]) => id)).toEqual([
      "registry_expired",
      "plain_http",
      "registry_expiring",
    ]);
  });

  it("truncates a long name with its whole text on hover and wraps a long host", async () => {
    me = REGISTRY_ADMIN;
    const name = "a-very-long-credential-name-".repeat(6);
    const host = `${"registry-".repeat(12)}example.org`;
    registryItems = [alertOf("expired", "cred-7", name, { host })];
    renderWarnings();
    await openBell("Warnings, 1 item");
    const title = await screen.findByText(`“${name}” has expired`);
    expect(title.className).toContain("truncate");
    expect(title.getAttribute("title")).toBe(`“${name}” has expired`);
    const description = screen.getByText(new RegExp(host));
    expect(description.className).toContain("wrap-anywhere");
  });

  it("renders a name that holds markup as text", async () => {
    me = REGISTRY_ADMIN;
    registryItems = [alertOf("expired", "cred-8", "<img src=x onerror=alert(1)>")];
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("“<img src=x onerror=alert(1)>” has expired")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });
});
