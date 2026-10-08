// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

const ADMIN = makeMe({
  instanceAdmin: true,
  permissions: { global: ["audit.read", "settings.read"], environments: {} },
});
const VIEWER = makeMe({ permissions: { global: [], environments: {} } });

const LONG_AGENT = `Mozilla/5.0 ${"VeryLongUserAgentToken/1.0.0-".repeat(30)}`;

function entry(over: Record<string, unknown> = {}) {
  return {
    id: "01990000-0000-7000-8000-000000000001",
    seq: 10,
    time: new Date(Date.now() - 5 * 60_000).toISOString(),
    actorType: "user",
    actor: "alice",
    action: "auth.login",
    targetType: "user",
    target: "alice",
    environment: null,
    result: "success",
    reason: "",
    ip: "203.0.113.7",
    userAgent: "Mozilla/5.0 (X11; Linux x86_64)",
    details: {},
    ...over,
  };
}

interface Call {
  path: string;
  search: URLSearchParams;
}

let me: typeof ADMIN;
let calls: Call[];
let answer: (call: Call) => Response | Promise<Response>;

const listCalls = () => calls.filter((c) => c.path === "/api/audit");
const lastList = () => listCalls()[listCalls().length - 1]?.search as URLSearchParams;

beforeEach(() => {
  calls = [];
  me = ADMIN;
  answer = () => Response.json({ items: [entry()], nextCursor: null, retentionDays: 90 });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const call: Call = { path: url.pathname, search: url.searchParams };
      calls.push(call);
      if (url.pathname === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: [
            {
              id: "e1",
              name: "lab-1",
              kind: "local",
              enabled: true,
              isSelfHost: true,
              createdAt: "2026-10-01T10:00:00Z",
              type: "standalone",
              engine: { name: "Docker Engine", version: "29.8.2", apiVersion: "1.56" },
              status: { state: "online" },
            },
          ],
        });
      }
      if (url.pathname === "/api/audit/actors") return Response.json(["admin", "alice", "bob"]);
      if (url.pathname === "/api/audit") return answer(call);
      if (url.pathname === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
        });
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

describe("the audit log", () => {
  it("shows the title, the retention and integrity caption, and the columns", async () => {
    renderAt("/audit");
    expect(await screen.findByRole("heading", { level: 1, name: "Audit log" })).toBeTruthy();
    const caption = await screen.findByText(/Entries are kept for 90 days\./);
    expect(caption.textContent).toContain(
      "Verify integrity with bayplan audit verify inside the container.",
    );
    expect(caption.querySelector("code")?.textContent).toBe("bayplan audit verify");
    for (const name of [
      "Time",
      "User",
      "Action",
      "Target",
      "Environment",
      "Result",
      "IP address",
    ]) {
      expect(await screen.findByRole("columnheader", { name })).toBeTruthy();
    }
    await waitFor(() => expect(document.title).toBe("Audit log · Lab"));
  });

  it("says entries are kept forever when retention is off", async () => {
    answer = () => Response.json({ items: [entry()], nextCursor: null, retentionDays: 0 });
    renderAt("/audit");
    expect(await screen.findByText(/Entries are kept forever\./)).toBeTruthy();
  });

  it("asks for the last 24 hours with no other filter by default", async () => {
    renderAt("/audit");
    await screen.findByText("Signed in");
    const search = lastList();
    const from = new Date(search.get("from") ?? "").getTime();
    expect(Math.abs(Date.now() - 24 * 3_600_000 - from)).toBeLessThan(60_000);
    expect(search.has("to")).toBe(false);
    expect(search.has("user")).toBe(false);
    expect(search.has("action")).toBe(false);
    expect(search.has("cursor")).toBe(false);
  });

  it("shows each entry's human action, target, environment name, result text and IP", async () => {
    answer = () =>
      Response.json({
        items: [
          entry({
            id: "a",
            seq: 3,
            action: "environment.renamed",
            target: "lab-1",
            targetType: "environment",
            environment: "e1",
            ip: "198.51.100.2",
          }),
          entry({
            id: "b",
            seq: 2,
            action: "permission.denied",
            result: "denied",
            target: "/api/audit",
          }),
          entry({ id: "c", seq: 1, action: "something.new", result: "failure" }),
        ],
        nextCursor: null,
        retentionDays: 90,
      });
    renderAt("/audit");
    const renamed = (await screen.findByText("Renamed environment")).closest("tr") as HTMLElement;
    // The target and the environment are both lab-1, the second by name, not ID.
    expect(within(renamed).getAllByText("lab-1")).toHaveLength(2);
    expect(within(renamed).queryByText("e1")).toBeNull();
    expect(within(renamed).getByText("Success")).toBeTruthy();
    expect(within(renamed).getByText("198.51.100.2")).toBeTruthy();
    const denied = screen.getByText("Permission denied").closest("tr") as HTMLElement;
    expect(within(denied).getByText("Denied")).toBeTruthy();
    // An action this build has no label for shows as its raw key.
    const unknown = screen.getByText("something.new").closest("tr") as HTMLElement;
    expect(within(unknown).getByText("Failure")).toBeTruthy();
  });

  it("shows a dash for a global action's environment and for a missing target or IP", async () => {
    answer = () =>
      Response.json({
        items: [entry({ target: "", ip: "", environment: null })],
        nextCursor: null,
        retentionDays: 90,
      });
    renderAt("/audit");
    const row = (await screen.findByText("Signed in")).closest("tr") as HTMLElement;
    expect(within(row).getAllByText("—")).toHaveLength(3);
  });

  it("shows an unknown sign-in actor as Anonymous with the attempted name as the target, and a system principal in mono", async () => {
    answer = () =>
      Response.json({
        items: [
          entry({
            id: "a",
            seq: 2,
            actorType: "anonymous",
            actor: "anonymous",
            action: "auth.login_failed",
            target: "mallory",
            result: "failure",
          }),
          entry({
            id: "b",
            seq: 1,
            actorType: "system",
            actor: "system:janitor",
            action: "audit.pruned",
          }),
        ],
        nextCursor: null,
        retentionDays: 90,
      });
    renderAt("/audit");
    const failed = (await screen.findByText("Failed sign-in")).closest("tr") as HTMLElement;
    expect(within(failed).getByText("Anonymous")).toBeTruthy();
    expect(within(failed).getByText("mallory")).toBeTruthy();
    const system = screen.getByText("Pruned audit log").closest("tr") as HTMLElement;
    const actor = within(system).getByText("system:janitor");
    expect(actor.className).toContain("font-mono");
  });

  it("shows skeleton rows while the first page loads", async () => {
    answer = () => new Promise<Response>(() => {});
    renderAt("/audit");
    await screen.findByRole("heading", { level: 1, name: "Audit log" });
    await waitFor(() =>
      expect(document.querySelectorAll("[data-slot=skeleton]").length).toBeGreaterThan(5),
    );
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });
});

describe("expanding a row", () => {
  it("shows the UTC and local time, raw key, full target, user agent, IP, reason and entry ID", async () => {
    answer = () =>
      Response.json({
        items: [
          entry({
            time: "2026-10-01T04:30:05Z",
            userAgent: LONG_AGENT,
            reason: "bad password",
            result: "failure",
            action: "auth.login_failed",
            target: "mallory",
            targetType: "user",
            details: { attempts: "3" },
          }),
        ],
        nextCursor: null,
        retentionDays: 90,
      });
    renderAt("/audit");
    const toggle = await screen.findByRole("button", { name: "Show details for Failed sign-in" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);

    const hide = await screen.findByRole("button", { name: "Hide details for Failed sign-in" });
    expect(hide.getAttribute("aria-expanded")).toBe("true");
    const panel = document.getElementById(
      hide.getAttribute("aria-controls") as string,
    ) as HTMLElement;
    const view = within(panel);
    expect(view.getByText("Time (UTC)").nextElementSibling?.textContent).toContain("04:30:05");
    expect(view.getByText("Time (UTC)").nextElementSibling?.textContent).toContain("UTC");
    expect(view.getByText("Time (local)")).toBeTruthy();
    expect(view.getByText("auth.login_failed").className).toContain("font-mono");
    expect(view.getByText("user: mallory")).toBeTruthy();
    expect(view.getByText("bad password")).toBeTruthy();
    expect(view.getByText("attempts: 3")).toBeTruthy();
    expect(view.getByText("01990000-0000-7000-8000-000000000001")).toBeTruthy();
    // The long user agent wraps inside the panel instead of widening the table.
    const agent = view.getByText(LONG_AGENT);
    expect(agent.className).toContain("break-all");
    expect(agent.className).toContain("whitespace-pre-wrap");
    expect((panel.querySelector("td > div") as HTMLElement).className).toContain("min-w-full");

    fireEvent.click(hide);
    await waitFor(() => expect(document.getElementById(panel.id)).toBeNull());
  });

  it("renders attacker-controlled text as text, never as markup", async () => {
    answer = () =>
      Response.json({
        items: [
          entry({
            actorType: "anonymous",
            actor: "anonymous",
            action: "auth.login_failed",
            target: "<img src=x onerror=alert(1)>",
            userAgent: "<script>alert(1)</script>",
          }),
        ],
        nextCursor: null,
        retentionDays: 90,
      });
    renderAt("/audit");
    fireEvent.click(await screen.findByRole("button", { name: "Show details for Failed sign-in" }));
    await screen.findByText("<script>alert(1)</script>");
    expect(document.querySelector("#main img")).toBeNull();
    expect(document.querySelector("#main script")).toBeNull();
  });
});

describe("refused requests", () => {
  const FIRST = "2026-10-01T04:30:05Z";
  const LAST = "2026-10-01T04:30:58Z";

  function refusal(over: Record<string, unknown> = {}) {
    return entry({
      action: "permission.denied",
      result: "denied",
      actor: "mallory",
      targetType: "route",
      target: "GET /api/audit",
      reason: "forbidden",
      ...over,
    });
  }

  it("shows a coalesced refusal as one row with a x11 badge and the first time", async () => {
    answer = () =>
      Response.json({
        items: [
          refusal({
            time: "2026-10-01T04:31:10Z",
            details: { coalesced: "true", count: "11", first: FIRST, last: LAST },
          }),
        ],
        nextCursor: null,
        retentionDays: 90,
      });
    renderAt("/audit");
    const row = (await screen.findByText("Permission denied")).closest("tr") as HTMLElement;
    const badge = within(row).getByText("×11");
    expect(badge.className).toContain("tabular-nums");
    expect(badge.className).toContain("text-xs");
    // The Time column shows the first refusal's time, not the summary's.
    expect(within(row).getByRole("time").getAttribute("datetime")).toBe(FIRST);
  });

  it("expands to First refused, Last refused and Count without the raw coalesced keys", async () => {
    answer = () =>
      Response.json({
        items: [
          refusal({
            details: { coalesced: "true", count: "11", first: FIRST, last: LAST, other: "kept" },
          }),
        ],
        nextCursor: null,
        retentionDays: 90,
      });
    renderAt("/audit");
    fireEvent.click(
      await screen.findByRole("button", { name: "Show details for Permission denied" }),
    );
    const hide = await screen.findByRole("button", { name: "Hide details for Permission denied" });
    const view = within(
      document.getElementById(hide.getAttribute("aria-controls") as string) as HTMLElement,
    );
    expect(view.getByText("First refused").nextElementSibling?.textContent).toContain("04:30:05");
    expect(view.getByText("Last refused").nextElementSibling?.textContent).toContain("04:30:58");
    expect(view.getByText("Count").nextElementSibling?.textContent).toBe("11");
    expect(view.queryByText("coalesced: true")).toBeNull();
    expect(view.queryByText("count: 11")).toBeNull();
    expect(view.queryByText(`first: ${FIRST}`)).toBeNull();
    // A detail that is not part of the burst is still listed.
    expect(view.getByText("other: kept")).toBeTruthy();
  });

  it("explains an environment the user cannot see as answered as not found", async () => {
    answer = () =>
      Response.json({
        items: [refusal({ reason: "environment_not_visible", environment: "env-1" })],
        nextCursor: null,
        retentionDays: 90,
      });
    renderAt("/audit");
    fireEvent.click(
      await screen.findByRole("button", { name: "Show details for Permission denied" }),
    );
    expect(
      await screen.findByText("Environment not visible to this user (answered as not found)."),
    ).toBeTruthy();
  });

  it("shows an ordinary refusal with no badge and its reason as recorded", async () => {
    answer = () => Response.json({ items: [refusal()], nextCursor: null, retentionDays: 90 });
    renderAt("/audit");
    const row = (await screen.findByText("Permission denied")).closest("tr") as HTMLElement;
    expect(within(row).queryByText(/^×/)).toBeNull();
    fireEvent.click(
      await screen.findByRole("button", { name: "Show details for Permission denied" }),
    );
    expect(await screen.findByText("forbidden")).toBeTruthy();
  });
});

describe("Load more", () => {
  it("fetches the next 50 with the cursor and appends them until there is no cursor", async () => {
    answer = (call) =>
      call.search.get("cursor") === "5"
        ? Response.json({
            items: [entry({ id: "older", seq: 4, action: "auth.logout" })],
            nextCursor: null,
            retentionDays: 90,
          })
        : Response.json({
            items: [entry({ id: "newer", seq: 10 })],
            nextCursor: 5,
            retentionDays: 90,
          });
    renderAt("/audit");
    fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByText("Signed out")).toBeTruthy();
    expect(lastList().get("cursor")).toBe("5");
    // The first page is still there, and with no cursor left there is no button.
    expect(screen.getByText("Signed in")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("button", { name: "Load more" })).toBeNull());
  });

  it("shows an inline error with Retry when a later page fails and keeps the rows", async () => {
    let fail = true;
    answer = (call) => {
      if (call.search.get("cursor") === "5" && fail) return new Response("{}", { status: 500 });
      return Response.json({
        items: [entry({ id: call.search.get("cursor") ? "older" : "newer" })],
        nextCursor: call.search.get("cursor") ? null : 5,
        retentionDays: 90,
      });
    };
    renderAt("/audit");
    fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByText("Couldn't load the audit log.")).toBeTruthy();
    expect(screen.getByText("Signed in")).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByText("Couldn't load the audit log.")).toBeNull());
  });
});

describe("filters live in the URL", () => {
  it("applies user, repeated actions and a preset range from the address", async () => {
    renderAt("/audit?user=alice&action=auth.login&action=auth.logout&range=7d");
    await screen.findByText("Signed in");
    const search = lastList();
    expect(search.get("user")).toBe("alice");
    expect(search.getAll("action")).toEqual(["auth.login", "auth.logout"]);
    const from = new Date(search.get("from") ?? "").getTime();
    expect(Math.abs(Date.now() - 7 * 24 * 3_600_000 - from)).toBeLessThan(60_000);
    expect(screen.getByRole("button", { name: /alice/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /2 actions/ })).toBeTruthy();
  });

  it("sends a custom range as inclusive local days in UTC", async () => {
    renderAt("/audit?range=custom&from=2026-09-01&to=2026-09-03");
    await screen.findByText("Signed in");
    const search = lastList();
    expect(search.get("from")).toBe(new Date(2026, 8, 1, 0, 0, 0, 0).toISOString());
    expect(search.get("to")).toBe(new Date(2026, 8, 3, 23, 59, 59, 999).toISOString());
    expect(screen.getByRole("button", { name: "Custom range" })).toBeTruthy();
  });

  it("writes an action choice to the URL and asks again", async () => {
    const router = renderAt("/audit");
    await screen.findByText("Signed in");
    fireEvent.click(screen.getByRole("button", { name: /Any action/ }));
    fireEvent.click(await screen.findByRole("checkbox", { name: "Signed out" }));
    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({ action: ["auth.logout"] }),
    );
    await waitFor(() => expect(lastList().getAll("action")).toEqual(["auth.logout"]));
  });

  it("takes free text for the user, because a failed sign-in's name is not a known actor", async () => {
    const router = renderAt("/audit");
    await screen.findByText("Signed in");
    fireEvent.click(screen.getByRole("button", { name: /Any user/ }));
    const field = await screen.findByRole("textbox", { name: "Type a user name" });
    fireEvent.change(field, { target: { value: "mallory" } });
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(router.state.location.search).toMatchObject({ user: "mallory" }));
    await waitFor(() => expect(lastList().get("user")).toBe("mallory"));
  });

  it("suggests the actors the log has seen", async () => {
    const router = renderAt("/audit");
    await screen.findByText("Signed in");
    fireEvent.click(screen.getByRole("button", { name: /Any user/ }));
    fireEvent.click(await screen.findByRole("button", { name: "bob" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ user: "bob" }));
  });

  it("clears the filters back to the last 24 hours", async () => {
    const router = renderAt("/audit?user=alice&action=auth.login&range=30d");
    await screen.findByText("Signed in");
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    await waitFor(() => {
      const search = router.state.location.search as Record<string, unknown>;
      expect(search.user).toBeUndefined();
      expect(search.action).toBeUndefined();
      expect(search.range).toBe("24h");
    });
    await waitFor(() => expect(lastList().has("user")).toBe(false));
  });

  it("names each filter control by its label and its current value", async () => {
    renderAt("/audit?range=30d");
    await screen.findByText("Signed in");
    expect(screen.getByRole("button", { name: "User Any user" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Action Any action" })).toBeTruthy();
    expect(screen.getByRole("combobox", { name: /Time range/ }).textContent).toContain(
      "Last 30 days",
    );
  });

  it("disables Clear filters on the default view", async () => {
    renderAt("/audit");
    await screen.findByText("Signed in");
    expect(
      (screen.getByRole("button", { name: "Clear filters" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});

describe("export", () => {
  it("offers CSV and JSON for exactly the entries the filters match", async () => {
    renderAt("/audit?user=alice&action=auth.login&range=1h");
    await screen.findByText("Signed in");
    const trigger = screen.getByRole("button", { name: "Export" });
    fireEvent.keyDown(trigger, { key: "Enter" });
    expect(
      await screen.findByText("Exports the entries that match the current filters."),
    ).toBeTruthy();
    const csv = await screen.findByRole("menuitem", { name: "Export CSV" });
    const json = screen.getByRole("menuitem", { name: "Export JSON" });
    const url = new URL(csv.getAttribute("href") as string, "http://bayplan.test");
    expect(url.pathname).toBe("/api/audit/export");
    expect(url.searchParams.get("format")).toBe("csv");
    expect(url.searchParams.get("user")).toBe("alice");
    expect(url.searchParams.getAll("action")).toEqual(["auth.login"]);
    expect(url.searchParams.has("from")).toBe(true);
    expect(url.searchParams.has("cursor")).toBe(false);
    expect(
      new URL(json.getAttribute("href") as string, "http://bayplan.test").searchParams.get(
        "format",
      ),
    ).toBe("json");
    expect(csv.hasAttribute("download")).toBe(true);
  });
});

describe("states", () => {
  it("shows the empty state with Clear filters", async () => {
    answer = () => Response.json({ items: [], nextCursor: null, retentionDays: 90 });
    const router = renderAt("/audit?user=nobody&range=30d");
    expect(await screen.findByText("No audit entries match these filters.")).toBeTruthy();
    expect(screen.getByText("Try a wider time range or clear the filters.")).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Clear filters" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    await waitFor(() =>
      expect((router.state.location.search as Record<string, unknown>).range).toBe("24h"),
    );
  });

  it("shows an inline error with Retry, and Retry asks again", async () => {
    let fail = true;
    answer = () =>
      fail
        ? new Response("{}", { status: 500 })
        : Response.json({ items: [entry()], nextCursor: null, retentionDays: 90 });
    renderAt("/audit");
    expect(await screen.findByText("Couldn't load the audit log.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Signed in")).toBeTruthy();
    expect(screen.queryByText("Couldn't load the audit log.")).toBeNull();
  });
});

describe("permission", () => {
  it("renders no permission without audit.read, makes no audit call and shows no nav item", async () => {
    me = VIEWER;
    renderAt("/audit");
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(calls.some((c) => c.path.startsWith("/api/audit"))).toBe(false);
    const sidebar = document.querySelector("[data-slot=sidebar]") as HTMLElement;
    expect(within(sidebar).queryByRole("link", { name: "Audit" })).toBeNull();
  });

  it("puts Audit in the sidebar for an admin, next to Settings", async () => {
    renderAt("/audit");
    await screen.findByText("Signed in");
    const sidebar = document.querySelector("[data-slot=sidebar]") as HTMLElement;
    const link = await within(sidebar).findByRole("link", { name: "Audit" });
    expect(link.getAttribute("href")).toBe("/audit");
    expect(within(sidebar).getByRole("link", { name: "Settings" })).toBeTruthy();
  });
});
