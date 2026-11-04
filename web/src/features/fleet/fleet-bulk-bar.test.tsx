// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type {
  FleetBulkRunResult,
  FleetUpdateGroup,
  FleetUpdateItem,
  FleetUpdates,
} from "@/api/fleet";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FleetUpdatesTab } from "@/features/fleet/fleet-updates-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// "Update selected" on the Updates tab (plan 12-31, D-11, IMG-06): ticks across environments, the bulk
// bar, the confirm grouped by environment, and the rows going Queued once it is started.

const OLD = `sha256:${"a".repeat(64)}`;
const NEW = `sha256:${"b".repeat(64)}`;
const NOW = new Date().toISOString();

function info(extra: Partial<FleetUpdateItem["update"]> = {}): FleetUpdateItem["update"] {
  return {
    state: "update_available",
    reason: "",
    registry: "docker.io",
    repository: "library/nginx",
    tag: "1.27",
    runningDigest: OLD,
    remoteDigest: NEW,
    changedPlatforms: [],
    checkedAt: NOW,
    successAt: NOW,
    note: "",
    task: false,
    ...extra,
  };
}

function item(extra: Partial<FleetUpdateItem> = {}): FleetUpdateItem {
  return {
    kind: "service",
    id: "svc1",
    name: "web",
    stack: "",
    image: "nginx:1.27",
    fromDigest: OLD,
    toDigest: NEW,
    changedPlatforms: [],
    selectable: true,
    update: info(),
    ...extra,
  };
}

function group(extra: Partial<FleetUpdateGroup> = {}): FleetUpdateGroup {
  return {
    envId: "e1",
    envName: "prod",
    type: "swarm",
    checkFailed: null,
    lastCheckedAt: NOW,
    items: [item()],
    ...extra,
  };
}

function body(groups: FleetUpdateGroup[], extra: Partial<FleetUpdates> = {}): FleetUpdates {
  return {
    counts: { updates: 3, problems: 0, ignored: 0, all: 3 },
    coverage: {
      checkable: 3,
      checked: 3,
      environments: 2,
      failed: 0,
      neverChecked: 0,
      skipped: 0,
      firstCheckAt: "",
      lastCheckedAt: NOW,
    },
    groups,
    notIncluded: [],
    readAt: NOW,
    ...extra,
  };
}

// prod (swarm): web and api. edge (standalone): cache.
function threeUpdates(state: FleetUpdateItem["update"]["state"] = "update_available") {
  const queued = state !== "update_available";
  const rest = { selectable: !queued, update: info({ state }) };
  return body([
    group({
      items: [
        item({ id: "svc1", name: "web", ...rest }),
        item({ id: "svc2", name: "api", image: "api:2", ...rest }),
      ],
    }),
    group({
      envId: "e2",
      envName: "edge",
      type: "standalone",
      items: [item({ id: "c9", kind: "container", name: "cache", image: "redis:7", ...rest })],
    }),
  ]);
}

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let client: QueryClient;
let sent: Sent[];
let answer: () => Response | Promise<Response>;
let answerRun: (body: unknown) => Response | Promise<Response>;
let answerSkip: (body: unknown) => Response | Promise<Response>;
let answerJobs: () => Response | Promise<Response>;
let me = makeMe({ envs: { e1: "operator", e2: "operator" } });

beforeEach(() => {
  setCsrfToken(null);
  sent = [];
  me = makeMe({ envs: { e1: "operator", e2: "operator" } });
  answer = () => Response.json(threeUpdates());
  answerRun = () =>
    Response.json({
      bulkId: "bulk1",
      jobs: [
        { envId: "e1", jobId: "j1" },
        { envId: "e2", jobId: "j2" },
      ],
      skipped: [],
    } satisfies FleetBulkRunResult);
  answerSkip = () => Response.json({ bulkId: "bulk2", refused: [], skipped: 3 });
  answerJobs = () => Response.json({ jobs: [] });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const text = await req.clone().text();
      const parsed = text ? JSON.parse(text) : undefined;
      sent.push({ method: req.method, path: url.pathname, body: parsed });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: [
            { id: "e1", name: "prod" },
            { id: "e2", name: "edge" },
          ],
        });
      }
      if (url.pathname === "/api/fleet/updates" && req.method === "GET") return answer();
      if (url.pathname === "/api/fleet/updates/run") return answerRun(parsed);
      if (url.pathname === "/api/fleet/updates/skip") return answerSkip(parsed);
      if (url.pathname === "/api/update-jobs") return answerJobs();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  setCsrfToken(null);
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <TestRouter>
          <FleetUpdatesTab env={[]} show="updates" onEnvChange={() => {}} onShowChange={() => {}} />
          <Toaster />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const table = async () => screen.findByRole("table", { name: "Image updates" });
const runPosts = () => sent.filter((s) => s.path === "/api/fleet/updates/run");
const skipPosts = () => sent.filter((s) => s.path === "/api/fleet/updates/skip");
const itemsOf = (s: Sent | undefined) => (s?.body as { items: unknown[] } | undefined)?.items ?? [];
const tick = (name: string) => fireEvent.click(screen.getByRole("checkbox", { name }));

async function tickAll3() {
  await table();
  tick("Select web");
  tick("Select api");
  tick("Select cache");
}

describe("FleetBulkBar", () => {
  it("shows the bulk bar with Update 3 selected once three rows in two environments are ticked", async () => {
    mount();
    expect(await screen.findByRole("button", { name: "Check now" })).toBeTruthy();
    await tickAll3();

    const bar = screen.getByRole("toolbar", { name: "Bulk actions" });
    expect(within(bar).getByText("3 selected")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Update 3 selected" })).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Skip selected versions" })).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Clear selection" })).toBeTruthy();
    // The bar takes Check now's place until the ticks are cleared.
    expect(screen.queryByRole("button", { name: "Check now" })).toBeNull();

    fireEvent.click(within(bar).getByRole("button", { name: "Clear selection" }));
    expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull();
    expect(screen.getByRole("button", { name: "Check now" })).toBeTruthy();
    expect((screen.getByRole("checkbox", { name: "Select web" }) as HTMLElement).ariaChecked).toBe(
      "false",
    );
  });

  it("confirms grouped by environment, posts the three items with their digests, and the rows read Queued", async () => {
    mount();
    await tickAll3();
    fireEvent.click(screen.getByRole("button", { name: "Update 3 selected" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Update 3 items", { selector: "h2" })).toBeTruthy();
    expect(
      within(dialog).getByText(
        "One update runs at a time in each environment. If one fails, only that one is rolled back.",
      ),
    ).toBeTruthy();
    const groups = within(dialog).getAllByRole("region");
    expect(groups.map((g) => g.getAttribute("aria-label"))).toEqual(["edge", "prod"]);
    const edge = groups[0] as HTMLElement;
    const prod = groups[1] as HTMLElement;
    expect(within(edge).getByText("Standalone")).toBeTruthy();
    expect(within(edge).getByText("1 item")).toBeTruthy();
    expect(within(edge).getByText("cache")).toBeTruthy();
    expect(within(prod).getByText("Swarm")).toBeTruthy();
    expect(within(prod).getByText("2 items")).toBeTruthy();
    expect(within(prod).getByText("web")).toBeTruthy();
    expect(within(prod).getByText("api")).toBeTruthy();
    // It does not promise every item will update.
    expect(within(dialog).getByText(/checked again when its turn comes/)).toBeTruthy();

    answer = () => Response.json(threeUpdates("queued"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Update 3 items" }));

    await waitFor(() => expect(runPosts()).toHaveLength(1));
    const posted = runPosts()[0]?.body as {
      items: { envId: string; kind: string; name: string; expect: Record<string, string> }[];
    };
    expect(posted.items).toHaveLength(3);
    expect(posted.items).toEqual(
      expect.arrayContaining([
        { envId: "e1", kind: "service", name: "web", expect: { web: NEW } },
        { envId: "e1", kind: "service", name: "api", expect: { api: NEW } },
        { envId: "e2", kind: "container", name: "cache", expect: { cache: NEW } },
      ]),
    );

    // The dialog closes, the ticks go, and the tab stays and reads the rows again.
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull();
    await waitFor(() => expect(screen.getAllByText("Queued")).toHaveLength(3));
    expect(screen.queryByRole("checkbox", { name: "Select web" })).toBeNull();
    // The answer says what was started, and that each item is checked again at its turn.
    await waitFor(() => expect(screen.getByText(/Started 3 updates/)).toBeTruthy());
    expect(screen.getByText(/some may be skipped/)).toBeTruthy();
  });

  it("names what the server did not take, and leaves an environment that is not reporting out of the count", async () => {
    answer = () =>
      Response.json({
        ...threeUpdates(),
        notIncluded: [{ envId: "e2", envName: "edge", nodes: 0, reason: "offline" }],
      });
    answerRun = () =>
      Response.json({
        bulkId: "bulk1",
        jobs: [{ envId: "e1", jobId: "j1" }],
        skipped: [{ envId: "e1", kind: "service", name: "api", reason: "locked" }],
      } satisfies FleetBulkRunResult);
    mount();
    await tickAll3();
    fireEvent.click(screen.getByRole("button", { name: "Update 3 selected" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Update 2 items", { selector: "h2" })).toBeTruthy();
    expect(within(dialog).getByText("Skipped: edge isn't reporting.")).toBeTruthy();
    expect(within(dialog).queryByText("cache")).toBeNull();

    fireEvent.click(within(dialog).getByRole("button", { name: "Update 2 items" }));
    await waitFor(() => expect(runPosts()).toHaveLength(1));
    expect(itemsOf(runPosts()[0])).toHaveLength(2);
    await waitFor(() =>
      expect(
        screen.getByText(/1 wasn't started: api \(another operation is running there\)/),
      ).toBeTruthy(),
    );
  });

  it("keeps the dialog open and the ticks when the server refuses to start", async () => {
    answerRun = () => Response.json({ title: "no" }, { status: 500 });
    mount();
    await tickAll3();
    fireEvent.click(screen.getByRole("button", { name: "Update 3 selected" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Update 3 items" }));
    await waitFor(() =>
      expect(screen.getByText(/Couldn't start the updates\. Nothing was changed/)).toBeTruthy(),
    );
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(runPosts()).toHaveLength(1);
  });

  it("sends one request for two presses in the same breath", async () => {
    let release: (r: Response) => void = () => {};
    answerRun = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    await tickAll3();
    fireEvent.click(screen.getByRole("button", { name: "Update 3 selected" }));
    const dialog = await screen.findByRole("dialog");
    const commit = within(dialog).getByRole("button", { name: "Update 3 items" });
    fireEvent.click(commit);
    fireEvent.click(commit);
    await waitFor(() => expect(runPosts()).toHaveLength(1));
    release(Response.json({ bulkId: "b", jobs: [], skipped: [] }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(runPosts()).toHaveLength(1);
  });

  it("shows the lock Alert for an environment where an update is running, and leaves it out", async () => {
    answerJobs = () =>
      Response.json({
        jobs: [
          {
            id: "j9",
            envId: "e2",
            envName: "edge",
            workload: "db",
            kind: "container",
            state: "running",
            startedAt: NOW,
            endedAt: "",
          },
        ],
      });
    mount();
    await table();
    // The jobs list arrives with the tab; wait for it before the confirm is built.
    await waitFor(() => expect(sent.some((s) => s.path === "/api/update-jobs")).toBe(true));
    tick("Select web");
    tick("Select cache");
    fireEvent.click(screen.getByRole("button", { name: "Update 2 selected" }));
    const dialog = await screen.findByRole("dialog");
    await waitFor(() =>
      expect(within(dialog).getByText("Update 1 item", { selector: "h2" })).toBeTruthy(),
    );
    expect(within(dialog).getByText(/db is being updated on edge/)).toBeTruthy();
  });

  it("is unavailable, with why, to someone who can't update", async () => {
    me = makeMe({ envs: { e1: "viewer", e2: "viewer" } });
    mount();
    await table();
    tick("Select web");
    const update = screen.getByRole("button", { name: "Update 1 selected" });
    await waitFor(() => expect(update.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(update);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("Skip selected versions", () => {
  // Skipping a version needs updates.manage, which an Env Admin holds.
  beforeEach(() => {
    me = makeMe({ envs: { e1: "admin", e2: "admin" } });
  });

  it("posts the ticked items, toasts the count and clears the selection", async () => {
    mount();
    await tickAll3();
    fireEvent.click(screen.getByRole("button", { name: "Skip selected versions" }));
    await waitFor(() => expect(skipPosts()).toHaveLength(1));
    expect(skipPosts()[0]?.body).toEqual({
      items: expect.arrayContaining([
        { envId: "e1", kind: "service", name: "web" },
        { envId: "e1", kind: "service", name: "api" },
        { envId: "e2", kind: "container", name: "cache" },
      ]),
    });
    await waitFor(() => expect(screen.getByText(/Skipped 3 versions/)).toBeTruthy());
    await waitFor(() => expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull());
  });

  it("names the items the server refused", async () => {
    answerSkip = () =>
      Response.json({
        bulkId: "b",
        skipped: 2,
        refused: [{ envId: "e2", kind: "container", name: "cache", reason: "no_permission" }],
      });
    mount();
    await tickAll3();
    fireEvent.click(screen.getByRole("button", { name: "Skip selected versions" }));
    await waitFor(() =>
      expect(screen.getByText(/1 wasn't skipped: cache \(you can't do this there\)/)).toBeTruthy(),
    );
  });

  it("is unavailable, with why, to an Operator", async () => {
    me = makeMe({ envs: { e1: "operator", e2: "operator" } });
    mount();
    await table();
    tick("Select web");
    const skip = screen.getByRole("button", { name: "Skip selected versions" });
    await waitFor(() => expect(skip.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(skip);
    expect(skipPosts()).toHaveLength(0);
  });

  it("says so when the request fails and keeps the ticks", async () => {
    answerSkip = () => Response.json({ title: "no" }, { status: 500 });
    mount();
    await tickAll3();
    fireEvent.click(screen.getByRole("button", { name: "Skip selected versions" }));
    await waitFor(() => expect(screen.getByText(/Couldn't skip these versions/)).toBeTruthy());
    expect(screen.getByRole("toolbar", { name: "Bulk actions" })).toBeTruthy();
  });
});
