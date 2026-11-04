// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type {
  FleetCheckResult,
  FleetUpdateCoverage,
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

// The Updates tab of the Overview (plan 12-29, ENV-10, IMG-05, D-17): every pending update across
// the environments, grouped, with the old and new digests, the Show chips and the fleet Check now.

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
    stack: "shop",
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

const COVERAGE: FleetUpdateCoverage = {
  checkable: 3,
  checked: 3,
  environments: 2,
  failed: 0,
  neverChecked: 0,
  skipped: 0,
  firstCheckAt: "",
  lastCheckedAt: NOW,
};

function body(groups: FleetUpdateGroup[], extra: Partial<FleetUpdates> = {}): FleetUpdates {
  return {
    counts: { updates: 3, problems: 2, ignored: 1, all: 6 },
    coverage: COVERAGE,
    groups,
    notIncluded: [],
    readAt: NOW,
    ...extra,
  };
}

interface Sent {
  method: string;
  path: string;
  query: string;
  body?: unknown;
}

let client: QueryClient;
let sent: Sent[];
let answer: (query: URLSearchParams) => Response | Promise<Response>;
let answerCheck: () => Response | Promise<Response>;
let envChanges: string[][];
let showChanges: string[];

beforeEach(() => {
  setCsrfToken(null);
  sent = [];
  envChanges = [];
  showChanges = [];
  answer = () =>
    Response.json(
      body([
        group({
          envId: "e2",
          envName: "edge",
          type: "standalone",
          checkFailed: "unreachable",
          items: [
            item({
              id: "c9",
              kind: "container",
              name: "cache",
              stack: "",
              image: "redis:7",
              toDigest: "",
              fromDigest: OLD,
              update: info({ state: "check_failed", reason: "unreachable", successAt: "" }),
            }),
          ],
        }),
        group(),
      ]),
    );
  answerCheck = () =>
    Response.json({ checked: 4, failed: 0, skipped: [], updates: 2 } satisfies FleetCheckResult);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const text = await req.clone().text();
      sent.push({
        method: req.method,
        path: url.pathname,
        query: url.search,
        body: text ? JSON.parse(text) : undefined,
      });
      if (url.pathname === "/api/me") {
        return Response.json(makeMe({ envs: { e1: "operator", e2: "operator" } }));
      }
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: [
            { id: "e1", name: "prod" },
            { id: "e2", name: "edge" },
          ],
        });
      }
      if (url.pathname === "/api/fleet/updates" && req.method === "GET") {
        return answer(url.searchParams);
      }
      if (url.pathname === "/api/fleet/updates/check") return answerCheck();
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

function mount(props: Partial<React.ComponentProps<typeof FleetUpdatesTab>> = {}) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <TestRouter>
          <FleetUpdatesTab
            env={[]}
            show="updates"
            onEnvChange={(ids) => envChanges.push(ids)}
            onShowChange={(s) => showChanges.push(s)}
            {...props}
          />
          <Toaster />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const table = async () => screen.findByRole("table", { name: "Image updates" });
const checkPosts = () => sent.filter((s) => s.path === "/api/fleet/updates/check");

describe("FleetUpdatesTab", () => {
  it("lists two environments' updates, the failed check first, with old and new digests", async () => {
    mount();
    const t = await table();
    const groups = within(t)
      .getAllByRole("rowheader")
      .filter((h) => h.getAttribute("scope") === "rowgroup");
    expect(groups.map((g) => within(g).getAllByRole("link")[0]?.textContent)).toEqual([
      "edge",
      "prod",
    ]);
    // The failed group says why, with a warning, and offers its own Check now.
    const edge = groups[0] as HTMLElement;
    expect(edge.textContent).toContain("The last check failed: a registry could not be reached.");
    expect(edge.textContent).toContain("Standalone");
    expect(edge.textContent).toContain("1 update, checked just now");
    expect(within(edge).getByRole("button", { name: "Check edge now" })).toBeTruthy();
    expect(
      within(groups[1] as HTMLElement).getByRole("button", { name: "Check prod now" }),
    ).toBeTruthy();

    const row = within(t).getByRole("link", { name: "web" }).closest("tr") as HTMLElement;
    expect(within(row).getByRole("link", { name: "web" }).getAttribute("href")).toBe(
      "/env/e1/services/svc1",
    );
    expect(within(row).getByText("in shop")).toBeTruthy();
    expect(within(row).getByText("nginx:1.27")).toBeTruthy();
    expect(within(row).getByRole("button", { name: /sha256:aaaaaaaaaaaa/ })).toBeTruthy();
    expect(within(row).getByRole("button", { name: /sha256:bbbbbbbbbbbb/ })).toBeTruthy();
    expect(within(row).getByText("Update available")).toBeTruthy();
    expect(within(row).getByRole("button", { name: "Actions for web" })).toBeTruthy();
    for (const column of ["Workload", "Image", "Change", "Status", "Checked"]) {
      expect(within(t).getByRole("columnheader", { name: column })).toBeTruthy();
    }
  });

  it("shows a dash for a change with no new version and Never for a workload never checked", async () => {
    mount();
    const t = await table();
    const row = within(t).getByRole("link", { name: "cache" }).closest("tr") as HTMLElement;
    expect(within(row).getAllByText("—").length).toBeGreaterThan(0);
    expect(within(row).getByText("Never")).toBeTruthy();
    expect(within(row).getByRole("link", { name: "cache" }).getAttribute("href")).toBe(
      "/env/e2/containers/c9",
    );
  });

  it("names the platforms that changed for a mixed-architecture workload", async () => {
    answer = () =>
      Response.json(
        body([
          group({
            items: [
              item({
                changedPlatforms: ["arm64"],
                update: info({ changedPlatforms: ["arm64"] }),
              }),
            ],
          }),
        ]),
      );
    mount();
    const t = await table();
    const row = within(t).getByRole("link", { name: "web" }).closest("tr") as HTMLElement;
    expect(within(row).getByText("arm64")).toBeTruthy();
    // The badge's description names them too, for a reader with no hover.
    expect(row.textContent).toContain("Changed for arm64.");
  });

  it("puts the counts in the Show chips and sets show when one is pressed", async () => {
    mount();
    await table();
    expect(screen.getByRole("radio", { name: "Updates (3)" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(screen.getByRole("radio", { name: "Problems (2)" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Skipped and ignored (1)" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "All" })).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: "Problems (2)" }));
    expect(showChanges).toEqual(["problems"]);
    // The chip that is on cannot be turned off.
    fireEvent.click(screen.getByRole("radio", { name: "Updates (3)" }));
    expect(showChanges).toEqual(["problems"]);
  });

  it("asks the server for the show value and the environments chosen", async () => {
    mount({ env: ["e1"], show: "problems" });
    await table();
    const get = sent.find((s) => s.path === "/api/fleet/updates" && s.method === "GET");
    expect(get?.query).toContain("show=problems");
    expect(get?.query).toContain("env=e1");
  });

  it("says when the last check was and across how many environments", async () => {
    mount();
    await table();
    const line = screen.getByText(
      (_, el) => el?.tagName === "P" && (el.textContent ?? "").startsWith("Last checked"),
    );
    expect(line.textContent).toBe("Last checked just now across 2 environments.");
  });

  it("checks the filtered environments and names the ones that were skipped", async () => {
    answerCheck = () =>
      Response.json({
        checked: 4,
        failed: 0,
        updates: 2,
        skipped: [{ envId: "e2", envName: "edge", reason: "no_permission" }],
      } satisfies FleetCheckResult);
    mount({ env: ["e1", "e2"] });
    await table();
    fireEvent.click(screen.getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(checkPosts()).toHaveLength(1));
    expect(checkPosts()[0]?.body).toEqual({ envIds: ["e1", "e2"] });
    const note = await screen.findByText(/Not checked: edge \(you can't check it\)\./);
    expect(note.textContent).toContain("Checked 4 images.");
    expect(note.textContent).toContain("2 updates available.");
  });

  it("sends no body when no environment is chosen and reads the tab again", async () => {
    mount();
    await table();
    const reads = sent.filter((s) => s.path === "/api/fleet/updates" && s.method === "GET").length;
    fireEvent.click(screen.getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(checkPosts()).toHaveLength(1));
    expect(checkPosts()[0]?.body).toBeUndefined();
    await waitFor(() =>
      expect(
        sent.filter((s) => s.path === "/api/fleet/updates" && s.method === "GET").length,
      ).toBeGreaterThan(reads),
    );
  });

  it("words a second press within the limit as a wait, not a failure", async () => {
    answerCheck = () =>
      Response.json(
        { type: "too_many_requests", title: "Too Many Requests", status: 429 },
        { status: 429, headers: { "Content-Type": "application/problem+json" } },
      );
    mount();
    await table();
    fireEvent.click(screen.getByRole("button", { name: "Check now" }));
    expect(await screen.findByText(/Wait a moment before checking again/)).toBeTruthy();
    expect(screen.queryByText(/Couldn't check for updates/)).toBeNull();
  });

  it("shows Checking… and presses once while a check is out", async () => {
    let release: (r: Response) => void = () => {};
    answerCheck = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    await table();
    fireEvent.click(screen.getByRole("button", { name: "Check now" }));
    const busy = await screen.findByRole("button", { name: "Checking…" });
    fireEvent.click(busy);
    expect(checkPosts()).toHaveLength(1);
    await act(async () => {
      release(Response.json({ checked: 1, failed: 0, skipped: [], updates: 0 }));
    });
    await screen.findByRole("button", { name: "Check now" });
  });

  it("folds a group away and opens it again", async () => {
    mount();
    const t = await table();
    expect(within(t).getByRole("link", { name: "web" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Collapse prod" }));
    expect(within(t).queryByRole("link", { name: "web" })).toBeNull();
    expect(within(t).getByRole("link", { name: "cache" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Expand prod" }));
    expect(within(t).getByRole("link", { name: "web" })).toBeTruthy();
  });

  it("draws a name, an image and a stack as text, never as markup", async () => {
    answer = () =>
      Response.json(
        body([
          group({
            envName: "<i>env</i>",
            items: [
              item({
                name: "<img src=x onerror=alert(1)>",
                image: "<script>alert(2)</script>",
                stack: "<b>stack</b>",
              }),
            ],
          }),
        ]),
      );
    mount();
    const t = await table();
    expect(within(t).getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(within(t).getByText("<script>alert(2)</script>")).toBeTruthy();
    expect(t.querySelector("img, script, b, i")).toBeNull();
  });

  it("draws only the rows in view past 100 rows", async () => {
    const items = Array.from({ length: 150 }, (_, i) =>
      item({ id: `s${i}`, name: `svc-${String(i).padStart(3, "0")}` }),
    );
    answer = () => Response.json(body([group({ items })]));
    mount();
    const t = await table();
    expect(t.parentElement?.className).toContain("max-h-[70svh]");
    expect(t.querySelectorAll("tbody tr").length).toBeLessThan(150);
  });

  it("offers a retry when the tab cannot be read", async () => {
    answer = () => Response.json({}, { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't load this view.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry loading" })).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows the coverage alert for an environment it could not read", async () => {
    answer = () =>
      Response.json(
        body([group()], {
          notIncluded: [{ envId: "e3", envName: "lab", reason: "offline", nodes: 0 }],
        }),
      );
    mount();
    await table();
    expect(screen.getByRole("status", { name: "" }).textContent).toContain("lab (offline)");
  });
});
