// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { JanitorPage } from "@/features/janitor/janitor-page";
import { RunPage } from "@/features/janitor/run-page";
import "@/i18n";
import { janitorStatus, preview, previewItem, run, runItem, runSummary } from "@/test-janitor";
import { makeMe } from "@/test-me";

const ENV = "e1";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

let me: ReturnType<typeof makeMe>;
let envBody: Record<string, unknown>;
let previewBody: ReturnType<typeof preview>;
let started: { body: unknown; calls: number };
let runBody: ReturnType<typeof run>;
let client: QueryClient;
let lockAnswer: (() => Response) | undefined;
let statusBody: ReturnType<typeof janitorStatus>;
let history: ReturnType<typeof runSummary>[];
let statusFails: boolean;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  envBody = { id: ENV, name: "Prod", envType: "swarm", nodes: { ready: 2, total: 2 } };
  previewBody = preview({
    items: [
      previewItem({ name: "web.1.aaa", ref: "c1" }),
      previewItem({ name: "web.2.bbb", ref: "c2" }),
      previewItem({ name: "web.3.ccc", ref: "c3" }),
    ],
  });
  started = { body: undefined, calls: 0 };
  lockAnswer = undefined;
  statusBody = janitorStatus();
  history = [];
  statusFails = false;
  toasts.error.mockReset();
  toasts.info.mockReset();
  runBody = run({
    state: "completed",
    counts: {
      failed: 0,
      kept: 0,
      removedContainers: 2,
      removedImages: 0,
      removedNetworks: 0,
      skippedOffline: 0,
    },
    items: [
      runItem({ seq: 1, name: "web.1.aaa", ref: "c1" }),
      runItem({ seq: 2, name: "web.3.ccc", ref: "c3" }),
    ],
  });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") return Response.json({ items: [envBody] });
      if (url.pathname === `/api/env/${ENV}/janitor/preview`) return Response.json(previewBody);
      if (url.pathname === `/api/env/${ENV}/janitor/runs` && req.method === "POST") {
        started.calls++;
        started.body = await req.json();
        if (lockAnswer) return lockAnswer();
        return Response.json({ runId: "r1", jobId: "j1" }, { status: 202 });
      }
      if (url.pathname === `/api/env/${ENV}/janitor`) {
        return statusFails
          ? Response.json({ title: "x", status: 500 }, { status: 500 })
          : Response.json(statusBody);
      }
      if (url.pathname === `/api/env/${ENV}/janitor/runs` && req.method === "GET") {
        const limit = Number(url.searchParams.get("limit") ?? 25);
        return Response.json({ runs: history.slice(0, limit), total: history.length });
      }
      if (url.pathname === `/api/env/${ENV}/janitor/runs/r1`) return Response.json(runBody);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  const root = createRootRoute();
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: "/env/$envId/janitor",
        component: () => <JanitorPage envId={ENV} />,
      }),
      createRoute({
        getParentRoute: () => root,
        path: "/env/$envId/janitor/runs/$runId",
        component: () => <RunPage envId={ENV} runId="r1" />,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: [`/env/${ENV}/janitor`] }),
  });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return router;
}

describe("JanitorPageCleanNow", () => {
  it("previews a cleanup, sends exactly the ticked items and lands on the run", async () => {
    const router = mount();
    expect(await screen.findByRole("heading", { level: 1, name: "Janitor" })).toBeTruthy();

    fireEvent.click(await screen.findByRole("button", { name: "Clean now" }));
    const sheet = await screen.findByRole("dialog", { name: "Clean up Prod" });
    // Everything starts ticked, with the reason in words.
    const boxes = await within(sheet).findAllByRole("checkbox", { name: /^Select web\./ });
    expect(boxes).toHaveLength(3);
    for (const b of boxes) expect(b.getAttribute("aria-checked")).toBe("true");
    expect(
      within(sheet).getAllByText("Exited task, 3rd oldest of service web slot 1."),
    ).toHaveLength(3);

    // Untick one.
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "Select web.2.bbb" }));
    const remove = await within(sheet).findByRole("button", { name: "Remove 2 items" });
    fireEvent.click(remove);

    // The request holds exactly the two ticked refs.
    await waitFor(() => expect(started.calls).toBe(1));
    expect(started.body).toEqual({
      items: [
        { type: "container", nodeId: "n1", ref: "c1" },
        { type: "container", nodeId: "n1", ref: "c3" },
      ],
    });

    // The browser lands on the run, which shows two Removed rows.
    await waitFor(() => expect(router.state.location.pathname).toBe(`/env/${ENV}/janitor/runs/r1`));
    expect(await screen.findByRole("heading", { level: 1, name: /Cleanup of Prod/ })).toBeTruthy();
    const rows = await screen.findAllByRole("row");
    const removedRows = rows.filter((r) => within(r).queryByText("Removed") !== null);
    expect(removedRows).toHaveLength(2);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("JanitorPageStatusAndHistory", () => {
  const removedSome = {
    failed: 0,
    kept: 0,
    removedContainers: 14,
    removedImages: 2,
    removedNetworks: 3,
    skippedOffline: 0,
  };

  it("tells whether the janitor is on, what it last did and when it runs next, and lists the runs", async () => {
    const newest = runSummary({
      id: "r2",
      trigger: "schedule",
      actor: "system:janitor",
      actorName: "TierPlan",
      counts: removedSome,
      estBytes: 1_200_000_000,
    });
    const older = runSummary({ id: "r1", startedAt: "2026-11-01T09:00:00Z" });
    statusBody = janitorStatus({ lastRun: newest });
    history = [newest, older];
    const router = mount();

    const status = await screen.findByRole("region", { name: "Janitor status" });
    expect(within(status).getByText("On")).toBeTruthy();
    expect(within(status).getByText("Daily at 03:00, server time (Europe/London).")).toBeTruthy();
    expect(
      within(status).getByText("Removed 14 containers, 3 networks, 2 images, ~1.2 GB."),
    ).toBeTruthy();
    expect(within(status).getByText("in 5 hours")).toBeTruthy();

    // The history has both runs, and a Started link opens the run.
    const table = await screen.findByRole("table", { name: "Run history" });
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    const link = within(table).getAllByRole("link", { name: /^Open run from / })[1] as HTMLElement;
    fireEvent.click(link);
    await waitFor(() => expect(router.state.location.pathname).toBe(`/env/${ENV}/janitor/runs/r1`));
  });

  it("says it couldn't load the janitor, keeps Clean now and reads again on request", async () => {
    statusFails = true;
    mount();
    expect(await screen.findByText("Couldn't load the janitor.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Clean now" })).toBeTruthy();
    statusFails = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading janitor" }));
    expect(await screen.findByRole("region", { name: "Janitor status" })).toBeTruthy();
    expect(screen.queryByText("Couldn't load the janitor.")).toBeNull();
  });

  it("asks for 25 more runs through the page and lands on the longer list", async () => {
    history = Array.from({ length: 30 }, (_, i) => runSummary({ id: `h${i}` }));
    mount();
    expect(await screen.findAllByRole("link", { name: /^Open run from / })).toHaveLength(25);
    fireEvent.click(screen.getByRole("button", { name: "Show 25 more" }));
    await waitFor(() =>
      expect(screen.getAllByRole("link", { name: /^Open run from / })).toHaveLength(30),
    );
    expect(screen.queryByRole("button", { name: "Show 25 more" })).toBeNull();
  });

  it("keeps the rules open until the janitor has run once, then closed", async () => {
    statusBody = janitorStatus();
    mount();
    const open = await screen.findByRole("button", {
      name: "What the janitor removes and never touches",
    });
    await waitFor(() => expect(open.getAttribute("aria-expanded")).toBe("true"));
    expect(screen.getByText("Never touches")).toBeTruthy();
    cleanup();

    statusBody = janitorStatus({ lastRun: runSummary() });
    mount();
    const closed = await screen.findByRole("button", {
      name: "What the janitor removes and never touches",
    });
    await waitFor(() => expect(closed.getAttribute("aria-expanded")).toBe("false"));
    expect(screen.queryByText("Never touches")).toBeNull();
  });
});

describe("who may clean, and when", () => {
  const reasonOf = (button: HTMLElement) =>
    document.getElementById(button.getAttribute("aria-describedby") ?? "")?.textContent;

  it("disables Clean now with the role it needs for a user below Env Admin", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    const button = await screen.findByRole("button", { name: "Clean now" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(reasonOf(button)).toBe("Needs Admin on Prod");
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("disables Clean now, with the reason, when no node is reporting", async () => {
    envBody = { id: ENV, name: "Prod", envType: "swarm", nodes: { ready: 0, total: 2 } };
    mount();
    const button = await screen.findByRole("button", { name: "Clean now" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(reasonOf(button)).toBe("No node is reporting, so there is nothing TierPlan can check.");
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("describes Clean now as showing what would be removed first", async () => {
    mount();
    const button = await screen.findByRole("button", { name: "Clean now" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBeNull());
    expect(reasonOf(button)).toBe("Shows what would be removed first.");
  });

  it("keeps the lock on the page once the sheet is closed on a refused Remove", async () => {
    lockAnswer = () =>
      new Response(
        JSON.stringify({
          type: "operation_locked",
          title: "Conflict",
          status: 409,
          errors: [
            { location: "kind", message: "janitor" },
            { location: "runId", message: "r9" },
            { location: "job", message: "j9" },
            { location: "started", message: new Date().toISOString() },
          ],
        }),
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Clean now" }));
    const sheet = await screen.findByRole("dialog", { name: "Clean up Prod" });
    fireEvent.click(await within(sheet).findByRole("button", { name: "Remove 3 items" }));
    await waitFor(() => expect(toasts.error).toHaveBeenCalled());
    fireEvent.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText(/The janitor is cleaning Prod, started/)).toBeTruthy();
  });

  it("shows nothing but the no-access page to a user who can't read the environment", async () => {
    me = makeMe({ envs: {} });
    mount();
    await waitFor(() => expect(screen.queryByRole("button", { name: "Clean now" })).toBeNull());
    expect(screen.queryByRole("heading", { level: 1, name: "Janitor" })).toBeNull();
  });
});
