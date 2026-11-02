// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { LIST_REFETCH_MS } from "@/api/resources";
import { STACK_GIT_POLL_MS } from "@/api/stack-git";
import { TooltipProvider } from "@/components/ui/tooltip";
import type { StackListSearch, StackListSearchPatch } from "@/features/stacks/list-search";
import { StackList } from "@/features/stacks/stack-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

const ENV = "e1";

let me: ReturnType<typeof makeMe>;
let stacks: unknown;
let client: QueryClient;
let listStatus: number;
// The list body the server answers; a test sets the fields it cares about.
let listExtra: Record<string, unknown>;
let requests: URL[];
// The POSTs the list sent for a stop or a start, as "METHOD path".
let posts: string[];

const stack = (over: Record<string, unknown> = {}) => ({
  id: "s1",
  name: "web",
  target: "swarm",
  source: "inline",
  managed: true,
  state: "not_deployed",
  lastDeployedAt: "",
  lastDeployedBy: "",
  hasDraft: false,
  ...over,
});

// A stack found on the engine that TierPlan did not make: no ID, a state and the service counts, and the flags.
const unmanaged = (
  name: string,
  over: Record<string, unknown> = {},
  flags: Record<string, unknown> = {},
) => ({
  id: "",
  name,
  target: "swarm",
  source: "unmanaged",
  managed: false,
  state: "running",
  lastDeployedAt: "",
  lastDeployedBy: "",
  hasDraft: false,
  servicesRunning: 2,
  servicesTotal: 2,
  unmanaged: { orphan: false, portainer: false, owner: "", adoptable: true, reason: "", ...flags },
  ...over,
});

beforeEach(() => {
  me = makeMe({ envs: { [ENV]: "admin" } });
  stacks = { items: [stack()] };
  listStatus = 200;
  listExtra = {};
  requests = [];
  posts = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks`) {
        if (listStatus !== 200)
          return Response.json({ title: "x", status: listStatus }, { status: listStatus });
        requests.push(new URL(req.url));
        const items = (stacks as { items: unknown[] }).items;
        return Response.json({
          total: items.length,
          unfilteredTotal: items.length,
          capped: false,
          statusCounts: {
            degraded: 0,
            stopped: 0,
            draft: 0,
            deployFailed: 0,
            drifted: 0,
            behind: 0,
            syncError: 0,
          },
          ...(stacks as object),
          ...listExtra,
        });
      }
      if (path === `/api/env/${ENV}/stacks/s1/services`) {
        return Response.json({
          items: [{ name: "web_web", mode: "replicated", running: 1, desired: 1 }],
        });
      }
      if (req.method === "POST" && /\/stacks\/s1\/(stop|start)$/.test(path)) {
        posts.push(`POST ${path}`);
        return Response.json({ jobId: "j1", recreates: false }, { status: 202 });
      }
      if (path === `/api/env/${ENV}/stacks/s1/deploys/j1`) {
        return Response.json({ id: "j1", state: "done", items: [] });
      }
      if (path === `/api/env/${ENV}/stacks/s1/removal-preview`) {
        return Response.json({
          services: [],
          containers: [],
          networks: [],
          secrets: [],
          configs: [],
          volumes: [],
          externals: [],
          notReporting: [],
          hasDraft: false,
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

// The route owns the search; this holds it the way the route's URL does, and reports each change.
function Harness({
  initial,
  onChange,
  creating,
  onCreatingChange,
}: {
  initial: StackListSearch;
  onChange: (patch: StackListSearchPatch) => void;
  creating: boolean;
  onCreatingChange: (open: boolean) => void;
}) {
  const [search, setSearch] = useState(initial);
  return (
    <StackList
      envId={ENV}
      creating={creating}
      onCreatingChange={onCreatingChange}
      search={search}
      onSearchChange={(patch) => {
        onChange(patch);
        setSearch((prev) => {
          const next: Record<string, unknown> = { ...prev, ...patch };
          for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
          return next as StackListSearch;
        });
      }}
    />
  );
}

function mount(props: { creating?: boolean; search?: StackListSearch } = {}) {
  const onCreatingChange = vi.fn();
  const onChange = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness
            initial={props.search ?? {}}
            onChange={onChange}
            creating={props.creating ?? false}
            onCreatingChange={onCreatingChange}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return { onCreatingChange, onChange };
}

describe("StackList", () => {
  it("shows the title, the caption naming the environment and a row linking to the stack", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Stacks", level: 1 })).toBeTruthy();
    const link = await screen.findByRole("link", { name: "web" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s1");
    expect(screen.getByText(/Stacks on Prod\./)).toBeTruthy();
    expect(screen.getByText(/1 stack$/)).toBeTruthy();
  });

  it("words each of the four states as text, with the source as an Inline badge", async () => {
    stacks = {
      items: [
        stack({ id: "a", name: "a-stack", state: "not_deployed" }),
        stack({ id: "b", name: "b-stack", state: "deploying" }),
        stack({ id: "c", name: "c-stack", state: "deploy_failed" }),
        stack({
          id: "d",
          name: "d-stack",
          state: "running",
          lastDeployedAt: "2026-10-18T10:00:00Z",
          lastDeployedBy: "ada",
        }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "d-stack" });
    const rowOf = (name: string) => screen.getByRole("link", { name }).closest("tr") as HTMLElement;
    expect(within(rowOf("a-stack")).getByText("Not deployed")).toBeTruthy();
    expect(within(rowOf("b-stack")).getByText("Deploying…")).toBeTruthy();
    expect(within(rowOf("c-stack")).getByText("Deploy failed")).toBeTruthy();
    expect(within(rowOf("d-stack")).getByText("Running")).toBeTruthy();
    for (const name of ["a-stack", "b-stack", "c-stack", "d-stack"]) {
      expect(within(rowOf(name)).getByText("Inline")).toBeTruthy();
    }
    // A stack that was never deployed says Never; a deployed one says who deployed it.
    expect(within(rowOf("a-stack")).getByText("Never")).toBeTruthy();
    expect(within(rowOf("d-stack")).getByText("by ada")).toBeTruthy();
  });

  it("badges a stack whose draft holds changes and no other", async () => {
    stacks = {
      items: [
        stack({ id: "a", name: "plain" }),
        stack({ id: "b", name: "drafty", hasDraft: true }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "drafty" });
    const row = (n: string) => screen.getByRole("link", { name: n }).closest("tr") as HTMLElement;
    expect(within(row("drafty")).getByText("Draft")).toBeTruthy();
    expect(within(row("plain")).queryByText("Draft")).toBeNull();
  });

  it("badges a stack that drifted after its name, after Draft when it has both, with the revision in its tooltip text", async () => {
    stacks = {
      items: [
        stack({
          id: "a",
          name: "both",
          hasDraft: true,
          drift: { state: "drifted", revision: 3, count: 2 },
        }),
        stack({ id: "b", name: "alone", drift: { state: "drifted", revision: 1, count: 1 } }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "both" });
    const row = (n: string) => screen.getByRole("link", { name: n }).closest("tr") as HTMLElement;
    const badges = (n: string) =>
      within(row(n))
        .getAllByText(/^(Draft|Drifted)$/)
        .map((b) => b.textContent);
    expect(badges("both")).toEqual(["Draft", "Drifted"]);
    expect(badges("alone")).toEqual(["Drifted"]);
    // The words of the Tooltip are also the badge's description, for a reader who never opens it.
    const badge = within(row("both")).getByText("Drifted");
    const description = document.getElementById(badge.getAttribute("aria-describedby") ?? "");
    expect(description?.textContent).toBe("Running state no longer matches revision #3.");
  });

  it("shows no Drifted badge for a stack that was not found drifted, stopped, not deployed or not checked", async () => {
    stacks = {
      items: [
        stack({
          id: "a",
          name: "fine",
          state: "running",
          drift: { state: "none", revision: 1, count: 0 },
        }),
        stack({
          id: "b",
          name: "halted",
          state: "stopped",
          drift: { state: "stopped", revision: 1, count: 0 },
        }),
        stack({ id: "c", name: "fresh", drift: { state: "not_deployed", revision: 0, count: 0 } }),
        stack({
          id: "d",
          name: "opaque",
          state: "running",
          drift: { state: "unknown", revision: 2, count: 0 },
        }),
        stack({ id: "e", name: "unchecked", state: "running" }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "fine" });
    expect(screen.queryByText("Drifted")).toBeNull();
  });

  it("renders a name with markup in it as text", async () => {
    stacks = { items: [stack({ name: "<img src=x onerror=alert(1)>" })] };
    mount();
    const link = await screen.findByRole("link", { name: "<img src=x onerror=alert(1)>" });
    expect(link.querySelector("img")).toBeNull();
  });

  it("with no stacks tells an Admin what a stack is and offers Create stack", async () => {
    stacks = { items: [] };
    const { onCreatingChange } = mount();
    expect(await screen.findByRole("heading", { name: "No stacks", level: 2 })).toBeTruthy();
    expect(
      screen.getByText(/A stack is a compose file that TierPlan deploys and keeps tidy\./),
    ).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Create stack" });
    expect(buttons.length).toBeGreaterThan(0);
    fireEvent.click(buttons[0] as HTMLElement);
    expect(onCreatingChange).toHaveBeenCalledWith(true);
  });

  it("with no stacks says there is nothing to show to a user who cannot create them", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    stacks = { items: [] };
    mount();
    expect(await screen.findByRole("heading", { name: "No stacks", level: 2 })).toBeTruthy();
    expect(screen.getByText("Nothing to show on Prod right now.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Create stack" })).toBeNull();
  });

  it("hides Create stack from a user without stacks.create, though stacks are listed", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(screen.queryByRole("button", { name: "Create stack" })).toBeNull();
  });

  it("shows Create stack to an Admin and opens the dialog by the address flag", async () => {
    const { onCreatingChange } = mount();
    await screen.findByRole("link", { name: "web" });
    fireEvent.click(screen.getByRole("button", { name: "Create stack" }));
    expect(onCreatingChange).toHaveBeenCalledWith(true);
  });

  it("shows the Create stack dialog while the address says new=1", async () => {
    mount({ creating: true });
    expect(await screen.findByRole("dialog", { name: "Create a stack" })).toBeTruthy();
  });

  it("shows the no-permission page to a user who cannot read stacks", async () => {
    me = makeMe({
      envs: { [ENV]: "viewer" },
      permissions: splitPermissions(["containers.read", "environments.read"]),
    });
    mount();
    expect(await screen.findByText(/permission/i)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "web" })).toBeNull();
  });

  it("says it could not load the stacks, with a retry", async () => {
    listStatus = 500;
    mount();
    expect(await screen.findByText("Couldn't load stacks.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry loading stacks" })).toBeTruthy();
  });

  it("has a row menu with Open stack and Deploy… for a user who may deploy", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    const trigger = await screen.findByRole("button", { name: "Actions for web" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    const open = await screen.findByRole("menuitem", { name: "Open stack" });
    expect(open.getAttribute("href")).toBe("/env/e1/stacks/s1");
    const deploy = screen.getByRole("menuitem", { name: "Deploy…" });
    expect(deploy.getAttribute("href")).toBe("/env/e1/stacks/s1/review");
  });

  it("shows Deploy… off with Needs Operator to a Viewer, and does not follow it", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    const trigger = await screen.findByRole("button", { name: "Actions for web" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    const deploy = await screen.findByRole("menuitem", { name: "Deploy…" });
    expect(deploy.getAttribute("aria-disabled")).toBe("true");
    expect(deploy.getAttribute("href")).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Open stack" })).toBeTruthy();
  });
});

describe("StackList row menu: Stop and Start", () => {
  async function openMenu() {
    const trigger = await screen.findByRole("button", { name: "Actions for web" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
  }

  it("offers Stop stack… for a stack that runs and opens the dialog without leaving the list", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    stacks = { items: [stack({ state: "running" })] };
    mount();
    await openMenu();
    expect(screen.queryByRole("menuitem", { name: "Start stack" })).toBeNull();

    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop stack…" }));

    expect(await screen.findByRole("heading", { name: "Stop web?" })).toBeTruthy();
    expect(posts).toEqual([]);
  });

  it("offers Start stack for a stopped stack and starts it at once", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    stacks = { items: [stack({ state: "stopped" })] };
    mount();
    await openMenu();
    expect(screen.queryByRole("menuitem", { name: "Stop stack…" })).toBeNull();

    fireEvent.click(await screen.findByRole("menuitem", { name: "Start stack" }));

    await waitFor(() => expect(posts).toEqual(["POST /api/env/e1/stacks/s1/start"]));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("offers neither for a stack that was never deployed", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    await openMenu();
    await screen.findByRole("menuitem", { name: "Open stack" });
    expect(screen.queryByRole("menuitem", { name: "Stop stack…" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Start stack" })).toBeNull();
  });

  it("shows a Viewer Stop stack… off with Needs Operator and sends nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    stacks = { items: [stack({ state: "running" })] };
    mount();
    await openMenu();

    const stop = await screen.findByRole("menuitem", { name: "Stop stack…" });
    expect(stop.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(stop);
    expect(screen.queryByRole("heading", { name: "Stop web?" })).toBeNull();
    expect(posts).toEqual([]);
  });
});

describe("StackList row menu: Update images", () => {
  async function openMenu() {
    const trigger = await screen.findByRole("button", { name: "Actions for web" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
  }

  it("offers Update images… to an Operator of a running stack, which opens its dialog", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    stacks = { items: [stack({ state: "running" })] };
    mount();
    await openMenu();

    fireEvent.click(await screen.findByRole("menuitem", { name: "Update images…" }));

    expect(await screen.findByRole("heading", { name: "Update images in web" })).toBeTruthy();
  });

  it("shows it off with the reason for a stack with nothing running, and for a Viewer", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    stacks = { items: [stack({ state: "not_deployed" })] };
    mount();
    await openMenu();
    const item = await screen.findByRole("menuitem", { name: "Update images…" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(item);
    expect(screen.queryByRole("heading", { name: "Update images in web" })).toBeNull();
    cleanup();

    me = makeMe({ envs: { [ENV]: "viewer" } });
    stacks = { items: [stack({ state: "running" })] };
    mount();
    await openMenu();
    const viewer = await screen.findByRole("menuitem", { name: "Update images…" });
    expect(viewer.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(viewer);
    expect(screen.queryByRole("heading", { name: "Update images in web" })).toBeNull();
    expect(posts).toEqual([]);
  });
});

describe("StackList row menu: Remove", () => {
  async function openMenu() {
    const trigger = await screen.findByRole("button", { name: "Actions for web" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
  }

  it("offers Remove… to an Env Admin, which opens its dialog without leaving the list", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    stacks = { items: [stack({ state: "running" })] };
    mount();
    await openMenu();

    fireEvent.click(await screen.findByRole("menuitem", { name: "Remove…" }));

    expect(await screen.findByRole("heading", { name: "Remove web?" })).toBeTruthy();
    // The list is still there behind the modal dialog.
    expect(screen.getByRole("link", { name: "web", hidden: true })).toBeTruthy();
  });

  it("shows an Operator Remove… off with Needs Admin", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    stacks = { items: [stack({ state: "running" })] };
    mount();
    await openMenu();

    const remove = await screen.findByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(remove);
    expect(screen.queryByRole("heading", { name: "Remove web?" })).toBeNull();
  });

  it("shows a Viewer Remove… off with Needs Admin and sends nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    stacks = { items: [stack({ state: "running" })] };
    mount();
    await openMenu();

    const remove = await screen.findByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(remove);
    expect(screen.queryByRole("heading", { name: "Remove web?" })).toBeNull();
    expect(posts).toEqual([]);
  });
});

// The names in the table, top to bottom: the first link of each body row.
const names = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).getAllByRole("link")[0]?.textContent);

describe("StackList: search and status chips", () => {
  it("has a search labelled Search stacks, with the words of the spec", async () => {
    mount();
    await screen.findByRole("link", { name: "web" });
    const box = screen.getByRole("searchbox", { name: "Search stacks" });
    expect(box.getAttribute("placeholder")).toBe("Search by name or service");
  });

  it("waits 250 ms, then puts the search in the URL and asks the server for it", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "web" });
    fireEvent.change(screen.getByRole("searchbox", { name: "Search stacks" }), {
      target: { value: "web" },
    });
    expect(onChange).not.toHaveBeenCalled();
    await waitFor(() => expect(onChange).toHaveBeenCalledWith({ q: "web" }));
    await waitFor(() => expect(requests.some((u) => u.searchParams.get("q") === "web")).toBe(true));
  });

  it("sends the search, statuses and sort of the URL to the server", async () => {
    mount({ search: { q: "api", status: "degraded,draft", sort: "state", dir: "desc" } });
    await screen.findByRole("link", { name: "web" });
    await waitFor(() => expect(requests.length).toBeGreaterThan(0));
    const last = requests.at(-1) as URL;
    expect(last.searchParams.get("q")).toBe("api");
    expect(last.searchParams.get("status")).toBe("degraded,draft");
    expect(last.searchParams.get("sort")).toBe("state");
    expect(last.searchParams.get("dir")).toBe("desc");
  });

  it("shows the seven status chips with the counts the server gave, in a labelled group that wraps, Behind and Sync error after Drifted", async () => {
    listExtra = {
      statusCounts: {
        degraded: 2,
        stopped: 1,
        draft: 3,
        deployFailed: 0,
        drifted: 4,
        behind: 5,
        syncError: 1,
      },
      unfilteredTotal: 9,
    };
    mount({ search: { q: "zzz" } });
    await screen.findByRole("link", { name: "web" });
    const group = screen.getByRole("group", { name: "Filter by status" });
    expect(group.className).toContain("flex-wrap");
    const labels = [
      "Degraded 2",
      "Stopped 1",
      "Draft 3",
      "Deploy failed 0",
      "Drifted 4",
      "Behind 5",
      "Sync error 1",
    ];
    for (const label of labels) {
      expect(within(group).getByRole("button", { name: label })).toBeTruthy();
    }
    expect(
      within(group)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(labels);
  });

  it("puts the Drifted chip in the URL after the other chips, and takes it out again", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "web" });
    fireEvent.click(screen.getByRole("button", { name: /^Drifted/ }));
    expect(onChange).toHaveBeenLastCalledWith({ status: "drifted" });
    fireEvent.click(screen.getByRole("button", { name: /^Draft/ }));
    expect(onChange).toHaveBeenLastCalledWith({ status: "draft,drifted" });
    fireEvent.click(screen.getByRole("button", { name: /^Drifted/ }));
    expect(onChange).toHaveBeenLastCalledWith({ status: "draft" });
  });

  it("restores the Drifted chip from the URL and sends status=drifted to the server", async () => {
    mount({ search: { status: "drifted" } });
    await screen.findByRole("link", { name: "web" });
    const chip = screen.getByRole("button", { name: /^Drifted/ });
    expect(chip.getAttribute("aria-pressed")).toBe("true");
    const last = requests.at(-1) as URL;
    expect(last.searchParams.get("status")).toBe("drifted");
  });

  it("puts a chosen chip in the URL and takes it out again", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "web" });
    fireEvent.click(screen.getByRole("button", { name: /^Degraded/ }));
    expect(onChange).toHaveBeenLastCalledWith({ status: "degraded" });
    fireEvent.click(screen.getByRole("button", { name: /^Draft/ }));
    expect(onChange).toHaveBeenLastCalledWith({ status: "degraded,draft" });
    fireEvent.click(screen.getByRole("button", { name: /^Degraded/ }));
    fireEvent.click(screen.getByRole("button", { name: /^Draft/ }));
    expect(onChange).toHaveBeenLastCalledWith({ status: undefined });
  });

  it("offers Clear filters only while a search or a chip is on, and clears both", async () => {
    const { onChange } = mount({ search: { q: "web", status: "stopped" } });
    await screen.findByRole("link", { name: "web" });
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(onChange).toHaveBeenCalledWith({ q: undefined, status: undefined });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull());
  });

  it("says no stack matches the search, and clears the search", async () => {
    stacks = { items: [] };
    listExtra = { unfilteredTotal: 4 };
    const { onChange } = mount({ search: { q: "zzz" } });
    expect(
      await screen.findByRole("heading", { name: "No stacks match “zzz”.", level: 2 }),
    ).toBeTruthy();
    // The search box has a Clear search of its own, above the empty state's.
    const clears = screen.getAllByRole("button", { name: "Clear search" });
    fireEvent.click(clears[clears.length - 1] as HTMLElement);
    expect(onChange).toHaveBeenCalledWith({ q: undefined });
  });

  it("says no stack matches the filters when a chip is on, and clears them", async () => {
    stacks = { items: [] };
    listExtra = { unfilteredTotal: 4 };
    const { onChange } = mount({ search: { q: "zzz", status: "draft" } });
    expect(
      await screen.findByRole("heading", { name: "No stacks match these filters.", level: 2 }),
    ).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Clear filters" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    expect(onChange).toHaveBeenCalledWith({ q: undefined, status: undefined });
  });
});

describe("StackList: captions, sort and the cap", () => {
  it("counts the stacks, and says n of total while a filter is on", async () => {
    stacks = { items: [stack({ id: "a", name: "alpha" }), stack({ id: "b", name: "beta" })] };
    listExtra = { total: 2, unfilteredTotal: 9 };
    mount({ search: { status: "draft" } });
    await screen.findByRole("link", { name: "alpha" });
    expect(screen.getByText(/2 of 9 stacks$/)).toBeTruthy();
  });

  it("counts one stack in the singular", async () => {
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(screen.getByText(/1 stack$/)).toBeTruthy();
  });

  it("says Showing the first 500 of n when the server capped the list", async () => {
    listExtra = { capped: true, total: 640, unfilteredTotal: 640 };
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(
      screen.getByText("Showing the first 500 of 640. Narrow the search to see the rest."),
    ).toBeTruthy();
    expect(screen.getByText(/640 stacks$/)).toBeTruthy();
  });

  it("sorts by Name, State and Last deployed from the headers, with aria-sort", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "web" });
    const header = (n: string) => screen.getByRole("columnheader", { name: n });
    expect(header("Name").getAttribute("aria-sort")).toBe("ascending");
    expect(header("State").getAttribute("aria-sort")).toBe("none");
    expect(header("Last deployed").getAttribute("aria-sort")).toBe("none");
    expect(header("Services").getAttribute("aria-sort")).toBeNull();
    expect(header("Source").getAttribute("aria-sort")).toBeNull();

    fireEvent.click(within(header("Name")).getByRole("button"));
    expect(onChange).toHaveBeenLastCalledWith({ sort: "name", dir: "desc" });
    await waitFor(() => expect(header("Name").getAttribute("aria-sort")).toBe("descending"));
    fireEvent.click(within(header("State")).getByRole("button"));
    expect(onChange).toHaveBeenLastCalledWith({ sort: "state", dir: "asc" });
    fireEvent.click(within(header("Last deployed")).getByRole("button"));
    expect(onChange).toHaveBeenLastCalledWith({ sort: "lastDeployed", dir: "asc" });
    await waitFor(() =>
      expect(header("Last deployed").getAttribute("aria-sort")).toBe("ascending"),
    );
  });

  it("shows the rows in the order the server answered", async () => {
    stacks = {
      items: [stack({ id: "z", name: "zeta-stack" }), stack({ id: "a", name: "alpha-stack" })],
    };
    mount({ search: { sort: "state", dir: "desc" } });
    await screen.findByRole("link", { name: "zeta-stack" });
    expect(names()).toEqual(["zeta-stack", "alpha-stack"]);
  });
});

describe("StackList: columns and pending rows", () => {
  it("shows Services as running over total, and a dash and Never for a stack not deployed", async () => {
    stacks = {
      items: [
        stack({ id: "a", name: "alpha", state: "degraded", servicesRunning: 1, servicesTotal: 3 }),
        stack({ id: "b", name: "beta" }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "alpha" });
    const row = (n: string) => screen.getByRole("link", { name: n }).closest("tr") as HTMLElement;
    expect(screen.getByRole("columnheader", { name: "Services" })).toBeTruthy();
    expect(within(row("alpha")).getByText("1/3")).toBeTruthy();
    expect(within(row("beta")).getByText("—")).toBeTruthy();
    expect(within(row("beta")).getByText("Not deployed")).toBeTruthy();
    expect(within(row("beta")).getByText("Never")).toBeTruthy();
  });

  it("labels a stack with an operation under way and turns its menu off", async () => {
    stacks = {
      items: [
        stack({ id: "a", name: "a-stack", state: "deploying" }),
        stack({ id: "b", name: "b-stack", state: "rolling_back" }),
        stack({ id: "c", name: "c-stack", state: "starting" }),
        stack({ id: "d", name: "d-stack", state: "removing" }),
        stack({ id: "f", name: "f-stack", state: "stopping" }),
        stack({ id: "e", name: "e-stack", state: "running", servicesRunning: 1, servicesTotal: 1 }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "e-stack" });
    const row = (n: string) => screen.getByRole("link", { name: n }).closest("tr") as HTMLElement;
    const pending: Record<string, string> = {
      "a-stack": "Deploying…",
      "b-stack": "Rolling back…",
      "c-stack": "Starting…",
      "d-stack": "Removing…",
      "f-stack": "Stopping…",
    };
    for (const [name, label] of Object.entries(pending)) {
      expect(within(row(name)).getByText(label)).toBeTruthy();
      const menu = within(row(name)).getByRole("button", { name: `Actions for ${name}` });
      expect((menu as HTMLButtonElement).disabled).toBe(true);
    }
    const idle = within(row("e-stack")).getByRole("button", { name: "Actions for e-stack" });
    expect((idle as HTMLButtonElement).disabled).toBe(false);
    expect(within(row("e-stack")).getByText("Running")).toBeTruthy();
  });
});

describe("StackList unmanaged stacks", () => {
  const rowOfName = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;

  it("lists a stack TierPlan did not make with the Unmanaged badge, its state and services, a link to its page and Open stack and Adopt stack… only", async () => {
    stacks = {
      items: [
        stack(),
        unmanaged("legacy", { state: "degraded", servicesRunning: 1, servicesTotal: 3 }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "web" });

    const row = rowOfName("legacy");
    expect(within(row).getByText("Unmanaged")).toBeTruthy();
    expect(within(row).getByText("Degraded")).toBeTruthy();
    expect(within(row).getByText("1/3")).toBeTruthy();
    // Source is a dash without a Portainer marker and Last deployed is a dash: two cells say so.
    expect(within(row).getAllByText("\u2014")).toHaveLength(2);
    // The name links to the stack's read-only page, built from the name.
    expect(within(row).getByRole("link", { name: "legacy" }).getAttribute("href")).toBe(
      "/env/e1/unmanaged-stacks/legacy",
    );
    // A managed row beside it keeps its link and menu.
    expect(within(rowOfName("web")).getByRole("button", { name: /web/ })).toBeTruthy();
    // The row's menu offers Open stack and Adopt stack… (to an Env Admin) and nothing that changes the stack: Deploy,
    // Update images, Stop, Start and Remove are not offered.
    fireEvent.pointerDown(within(row).getByRole("button", { name: "Actions for legacy" }), {
      button: 0,
      ctrlKey: false,
    });
    const items = (await screen.findAllByRole("menuitem")).map((i) => i.textContent);
    expect(items).toEqual(["Open stack", "Adopt stack…"]);
    expect(screen.getByRole("menuitem", { name: "Adopt stack…" }).getAttribute("href")).toBe(
      "/env/e1/unmanaged-stacks/legacy/adopt",
    );
  });

  it("shows an Operator Adopt stack… off with Needs Admin", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    stacks = { items: [unmanaged("legacy")] };
    mount();
    await screen.findByText("legacy");
    fireEvent.pointerDown(
      within(rowOfName("legacy")).getByRole("button", { name: "Actions for legacy" }),
      { button: 0, ctrlKey: false },
    );

    const adopt = await screen.findByRole("menuitem", { name: "Adopt stack…" });
    await waitFor(() => expect(adopt.getAttribute("aria-disabled")).toBe("true"));
    expect(adopt.getAttribute("href")).toBeNull();
  });

  it("shows Adopt stack… off, with the reason, for a stack that cannot be adopted", async () => {
    stacks = {
      items: [
        unmanaged(
          "selfstack",
          {},
          { owner: "tierplan", adoptable: false, reason: "This is TierPlan's own stack." },
        ),
      ],
    };
    mount();
    await screen.findByText("selfstack");
    fireEvent.pointerDown(
      within(rowOfName("selfstack")).getByRole("button", { name: "Actions for selfstack" }),
      { button: 0, ctrlKey: false },
    );

    const adopt = await screen.findByRole("menuitem", { name: "Adopt stack…" });
    expect(adopt.getAttribute("aria-disabled")).toBe("true");
    expect(adopt.getAttribute("href")).toBeNull();
  });

  it("reads a source this build has no word for as a dash, never as a raw key", async () => {
    stacks = { items: [stack({ id: "z1", name: "odd", source: "svn" })] };
    mount();
    await screen.findByRole("link", { name: "odd" });

    const row = rowOfName("odd");
    expect(within(row).queryByText(/stacks\.source/)).toBeNull();
    expect(within(row).getAllByText("\u2014")).not.toHaveLength(0);
  });

  it("shows a stack that was adopted with the outline Source badge Adopted", async () => {
    stacks = {
      items: [
        stack({
          id: "a1",
          name: "legacy",
          source: "adopted",
          state: "running",
          servicesRunning: 2,
          servicesTotal: 2,
        }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "legacy" });

    const row = rowOfName("legacy");
    expect(within(row).getByText("Adopted")).toBeTruthy();
    expect(within(row).queryByText("Unmanaged")).toBeNull();
    expect(within(row).getByText("Running")).toBeTruthy();
    expect(within(row).queryByText("Draft")).toBeNull();
  });

  it("renders the name of an unmanaged stack with markup in it as text", async () => {
    stacks = { items: [unmanaged("<img src=x onerror=alert(1)>")] };
    mount();
    const name = await screen.findByText("<img src=x onerror=alert(1)>");
    expect(name.querySelector("img")).toBeNull();
  });

  it("explains in a Tooltip that TierPlan did not create the stack", async () => {
    stacks = { items: [unmanaged("legacy")] };
    mount();
    await screen.findByText("legacy");
    fireEvent.focus(within(rowOfName("legacy")).getByText("Unmanaged"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe(
      "TierPlan didn't create this stack and doesn't manage it. Adopt it to manage it here.",
    );
  });
});

describe("StackList: the Show group, captions and discovery", () => {
  const counts = (all: number, managed: number, unmanagedCount: number) => ({
    all,
    managed,
    unmanaged: unmanagedCount,
  });

  it("shows a group named Show beside the search with All, Managed and Unmanaged and their counts", async () => {
    listExtra = { kindCounts: counts(3, 1, 2) };
    mount();
    await screen.findByRole("link", { name: "web" });
    const show = screen.getByRole("group", { name: "Show" });
    expect(within(show).getByRole("radio", { name: "All, 3" })).toBeTruthy();
    expect(within(show).getByRole("radio", { name: "Managed, 1" })).toBeTruthy();
    expect(within(show).getByRole("radio", { name: "Unmanaged, 2" })).toBeTruthy();
    // It sits in toolbar row 1, with the search.
    expect(show.parentElement?.contains(screen.getByRole("searchbox"))).toBe(true);
  });

  it("puts the kind in the URL, and takes it out when the chosen one is pressed again", async () => {
    listExtra = { kindCounts: counts(3, 1, 2) };
    const { onChange } = mount();
    await screen.findByRole("link", { name: "web" });
    fireEvent.click(screen.getByRole("radio", { name: "Unmanaged, 2" }));
    expect(onChange).toHaveBeenLastCalledWith({ kind: "unmanaged" });
    fireEvent.click(screen.getByRole("radio", { name: "Managed, 1" }));
    expect(onChange).toHaveBeenLastCalledWith({ kind: "managed" });
    fireEvent.click(screen.getByRole("radio", { name: "Managed, 1" }));
    expect(onChange).toHaveBeenLastCalledWith({ kind: undefined });
  });

  it("restores the kind from the URL and sends it to the server", async () => {
    listExtra = { kindCounts: counts(3, 1, 2) };
    mount({ search: { kind: "unmanaged" } });
    await screen.findByRole("link", { name: "web" });
    expect(screen.getByRole("radio", { name: "Unmanaged, 2" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    await waitFor(() => expect(requests.at(-1)?.searchParams.get("kind")).toBe("unmanaged"));
  });

  it("offers Clear filters while a kind is on, and clears it", async () => {
    listExtra = { kindCounts: counts(3, 1, 2) };
    const { onChange } = mount({ search: { kind: "managed" } });
    await screen.findByRole("link", { name: "web" });
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(onChange).toHaveBeenCalledWith({ q: undefined, status: undefined, kind: undefined });
  });

  it("captions the stacks and the unmanaged ones among them, plural-aware", async () => {
    stacks = { items: [stack(), unmanaged("legacy"), unmanaged("blog")] };
    listExtra = { kindCounts: counts(3, 1, 2) };
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(screen.getByText(/Stacks on Prod\./)).toBeTruthy();
    expect(screen.getByText(/3 stacks, 2 unmanaged$/)).toBeTruthy();
    cleanup();
    client.clear();

    stacks = { items: [unmanaged("legacy")] };
    listExtra = { kindCounts: counts(1, 0, 1) };
    mount();
    await screen.findByText("legacy");
    expect(screen.getByText(/1 stack, 1 unmanaged$/)).toBeTruthy();
  });

  it("leaves the unmanaged part out of the caption when there are none", async () => {
    listExtra = { kindCounts: counts(1, 1, 0) };
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(screen.getByText(/1 stack$/)).toBeTruthy();
    expect(screen.queryByText(/unmanaged$/)).toBeNull();
  });

  it("lists an environment that has only unmanaged stacks and does not say there are no stacks", async () => {
    stacks = { items: [unmanaged("legacy")] };
    listExtra = { kindCounts: counts(1, 0, 1) };
    mount();
    expect(await screen.findByText("legacy")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "No stacks", level: 2 })).toBeNull();
  });

  it("says there are no unmanaged stacks, and clears the filter", async () => {
    stacks = { items: [] };
    listExtra = { unfilteredTotal: 1, kindCounts: counts(1, 1, 0) };
    const { onChange } = mount({ search: { kind: "unmanaged" } });
    expect(
      await screen.findByRole("heading", { name: "No unmanaged stacks", level: 2 }),
    ).toBeTruthy();
    expect(screen.getByText("Every stack on Prod is managed by TierPlan.")).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Clear filters" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    expect(onChange).toHaveBeenCalledWith({ q: undefined, status: undefined, kind: undefined });
  });

  it("says there are no managed stacks when every stack was created elsewhere", async () => {
    stacks = { items: [] };
    listExtra = { unfilteredTotal: 2, kindCounts: counts(2, 0, 2) };
    mount({ search: { kind: "managed" } });
    expect(
      await screen.findByRole("heading", { name: "No managed stacks", level: 2 }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "The stacks on Prod were all created elsewhere. Adopt one to manage it here.",
      ),
    ).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Clear filters" }).length).toBeGreaterThan(0);
  });

  it("says no stack matches the filters for a status chip with the Unmanaged kind", async () => {
    stacks = { items: [] };
    listExtra = { unfilteredTotal: 3, kindCounts: counts(3, 1, 2) };
    mount({ search: { kind: "unmanaged", status: "degraded" } });
    expect(
      await screen.findByRole("heading", { name: "No stacks match these filters.", level: 2 }),
    ).toBeTruthy();
  });

  it("shows a retry row when the look for unmanaged stacks failed, with the managed stacks still listed", async () => {
    listExtra = { discoveryFailed: true, kindCounts: counts(1, 1, 0) };
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(screen.getByText("Couldn't look for unmanaged stacks on Prod.")).toBeTruthy();
    const before = requests.length;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(requests.length).toBeGreaterThan(before));
  });

  it("shows no retry row when the look worked", async () => {
    listExtra = { discoveryFailed: false, kindCounts: counts(1, 1, 0) };
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(screen.queryByText(/Couldn't look for unmanaged stacks/)).toBeNull();
  });
});

describe("StackList: flags on unmanaged stacks", () => {
  const rowOfName = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;

  it("adds the lost-record sentence to the Tooltip of an orphan row's Unmanaged badge", async () => {
    stacks = { items: [unmanaged("orphan", {}, { orphan: true })] };
    mount();
    await screen.findByText("orphan");
    fireEvent.focus(within(rowOfName("orphan")).getByText("Unmanaged"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe(
      "TierPlan didn't create this stack and doesn't manage it. Adopt it to manage it here. Created by TierPlan (another instance or a lost record).",
    );
  });

  it("shows the Portainer badge in Source for a Portainer stack and a dash for any other", async () => {
    stacks = {
      items: [unmanaged("viaportainer", {}, { portainer: true }), unmanaged("plain")],
    };
    mount();
    await screen.findByText("viaportainer");
    const source = (name: string) => within(rowOfName(name)).getAllByRole("cell")[3] as HTMLElement;
    expect(within(source("viaportainer")).getByText("Portainer")).toBeTruthy();
    expect(within(source("plain")).queryByText("Portainer")).toBeNull();
    expect(source("plain").textContent).toBe("—");
  });

  it("badges TierPlan's own stack and the agent's stack beside Unmanaged, with the reason in a Tooltip", async () => {
    stacks = {
      items: [
        unmanaged(
          "self",
          {},
          { owner: "tierplan", adoptable: false, reason: "This is TierPlan's own stack." },
        ),
        unmanaged(
          "agents",
          {},
          { owner: "agent", adoptable: false, reason: "This stack runs TierPlan's agent." },
        ),
        unmanaged("blog"),
      ],
    };
    mount();
    await screen.findByText("self");
    const own = rowOfName("self");
    expect(within(own).getByText("Unmanaged")).toBeTruthy();
    expect(within(own).getByText("TierPlan")).toBeTruthy();
    expect(within(rowOfName("agents")).getByText("TierPlan agent")).toBeTruthy();
    expect(within(rowOfName("blog")).queryByText(/^TierPlan/)).toBeNull();

    fireEvent.focus(within(own).getByText("TierPlan"));
    expect((await screen.findByRole("tooltip")).textContent).toBe("This is TierPlan's own stack.");
  });

  it("draws every flag as a neutral outline badge, never a status fill, and keeps names as text", async () => {
    stacks = {
      items: [
        unmanaged(
          "<b>flagged</b>",
          { state: "degraded", servicesRunning: 1, servicesTotal: 2 },
          { orphan: true, portainer: true, owner: "agent", adoptable: false, reason: "r" },
        ),
      ],
    };
    mount();
    const name = await screen.findByText("<b>flagged</b>");
    expect(name.querySelector("b")).toBeNull();
    const row = name.closest("tr") as HTMLElement;
    for (const word of ["Unmanaged", "TierPlan agent", "Portainer"]) {
      const badge = within(row).getByText(word);
      expect(badge.getAttribute("data-variant")).toBe("outline");
    }
  });
});

describe("StackList git stacks", () => {
  const SHA = "0123456789abcdef0123456789abcdef01234567";
  const git = (over: Record<string, unknown> = {}) => ({
    repoId: "r1",
    repoName: "stacks repo",
    repoHost: "github.com",
    repoPath: "acme/stacks",
    refKind: "branch",
    ref: "main",
    folder: "",
    deployedSha: SHA,
    headSha: SHA,
    behind: 0,
    behindCapped: false,
    status: "in_sync",
    checking: false,
    autoDeploy: false,
    autoDeployPaused: false,
    ...over,
  });
  const gitStack = (name: string, g: Record<string, unknown> = {}, over = {}) =>
    stack({
      id: name,
      name,
      source: "git",
      state: "running",
      lastDeployedAt: "2026-10-19T11:00:00Z",
      lastDeployedBy: "ada",
      git: git(g),
      ...over,
    });
  const row = (n: string) => screen.getByRole("link", { name: n }).closest("tr") as HTMLElement;
  const badgeTexts = (n: string) =>
    within(row(n))
      .getAllByText(/^(Draft|Drifted|Behind.*|Sync error)$/)
      .map((b) => b.textContent);

  it("shows the outline Git source badge, with the repository and the ref in its description", async () => {
    stacks = { items: [gitStack("web")] };
    mount();
    await screen.findByRole("link", { name: "web" });
    const badge = within(row("web")).getByText("Git");
    expect(badge.getAttribute("data-variant")).toBe("outline");
    const description = document.getElementById(badge.getAttribute("aria-describedby") ?? "");
    expect(description?.textContent).toBe("stacks repo · main");
  });

  it("reads Last deployed as the time and then the short commit and who", async () => {
    stacks = {
      items: [
        gitStack("web"),
        stack({
          id: "plain",
          name: "plain",
          state: "running",
          lastDeployedAt: "2026-10-19T11:00:00Z",
          lastDeployedBy: "bob",
        }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(within(row("web")).getByText("0123456 · by ada")).toBeTruthy();
    // A stack that is not from git keeps the P7 wording.
    expect(within(row("plain")).getByText("by bob")).toBeTruthy();
  });

  it("badges a stack that is behind with the count, and one whose sync failed", async () => {
    stacks = {
      items: [
        gitStack("behind", { status: "behind", behind: 3, headSha: "f".repeat(40) }),
        gitStack("failing", {
          status: "error",
          reasonCode: "ref_gone",
          reasonDetail: "The branch or tag no longer exists.",
        }),
        gitStack("fine"),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "behind" });
    expect(badgeTexts("behind")).toEqual(["Behind 3"]);
    expect(badgeTexts("failing")).toEqual(["Sync error"]);
    expect(within(row("fine")).queryByText(/^(Behind|Sync error)/)).toBeNull();
    const badge = within(row("failing")).getByText("Sync error");
    expect(document.getElementById(badge.getAttribute("aria-describedby") ?? "")?.textContent).toBe(
      "main no longer exists.",
    );
  });

  it("shows Draft, Drifted, Behind and Sync error together after the name, in that order", async () => {
    stacks = {
      items: [
        gitStack(
          "everything",
          { status: "error", behind: 2, reasonCode: "fetch_failed" },
          { hasDraft: true, drift: { state: "drifted", revision: 3, count: 2 } },
        ),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "everything" });
    expect(badgeTexts("everything")).toEqual(["Draft", "Drifted", "Behind 2", "Sync error"]);
    // They sit after the name in the same cell, which wraps them under it on a narrow screen.
    const cell = screen.getByRole("link", { name: "everything" }).closest("td") as HTMLElement;
    expect(cell.querySelector("span")?.className).toContain("flex-wrap");
  });

  it("keeps the badges after a truncated name and gives each of 30 stacks its own", async () => {
    const long = `${"a".repeat(100)}-monorepo`;
    stacks = {
      items: [
        gitStack(long, { status: "behind", behind: 1 }),
        ...Array.from({ length: 30 }, (_, i) =>
          gitStack(`svc-${i}`, { status: "behind", behind: i + 1 }),
        ),
      ],
    };
    mount();
    await screen.findByRole("link", { name: long });
    const link = screen.getByRole("link", { name: long });
    const cell = link.closest("td") as HTMLElement;
    const children = Array.from(cell.querySelectorAll("span, a"));
    const nameAt = children.indexOf(link);
    const badgeAt = children.findIndex((c) => c.textContent === "Behind 1");
    expect(nameAt).toBeGreaterThanOrEqual(0);
    expect(badgeAt).toBeGreaterThan(nameAt);
    for (let i = 0; i < 30; i++) {
      expect(badgeTexts(`svc-${i}`)).toEqual([`Behind ${i + 1}`]);
    }
  });

  it("offers Check now in the menu of a git stack, which asks the server to check it", async () => {
    stacks = { items: [gitStack("web")] };
    const checks: string[] = [];
    const base = vi.mocked(fetch).getMockImplementation();
    vi.mocked(fetch).mockImplementation(async (req) => {
      const r = req as Request;
      const path = new URL(r.url).pathname;
      if (r.method === "POST" && path === `/api/env/${ENV}/stacks/web/git/check`) {
        checks.push(path);
        return Response.json({ checking: true }, { status: 202 });
      }
      return (base as (r: Request) => Promise<Response>)(r);
    });
    mount();
    const trigger = await screen.findByRole("button", { name: "Actions for web" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Check now" }));
    await waitFor(() => expect(checks).toEqual([`/api/env/${ENV}/stacks/web/git/check`]));
  });

  it("goes on waiting for a check after the menu closed, and reads the list again so the badge follows the answer", async () => {
    stacks = { items: [gitStack("web")] };
    // What the server answers for the stack while the check runs, and once it has ended.
    let checking = true;
    const base = vi.mocked(fetch).getMockImplementation();
    vi.mocked(fetch).mockImplementation(async (req) => {
      const r = req as Request;
      const path = new URL(r.url).pathname;
      if (r.method === "POST" && path === `/api/env/${ENV}/stacks/web/git/check`) {
        return Response.json({ checking: true }, { status: 202 });
      }
      if (r.method === "GET" && path === `/api/env/${ENV}/stacks/web`) {
        return Response.json({
          ...gitStack("web"),
          envId: ENV,
          git: git({ checking }),
        });
      }
      return (base as (r: Request) => Promise<Response>)(r);
    });
    mount();
    const trigger = await screen.findByRole("button", { name: "Actions for web" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    const item = await screen.findByRole("menuitem", { name: "Check now" });
    vi.useFakeTimers();
    try {
      fireEvent.click(item);
      // The menu is gone with the click, and the check is still waited for.
      await waitUnderFakeClock(() => expect(screen.queryByRole("menuitem")).toBeNull());
      // The answer to the click has come and the lists were read once for it, before the check ended.
      await waitUnderFakeClock(() => {
        expect(client.isMutating()).toBe(0);
        expect(client.isFetching()).toBe(0);
      });
      const listReads = requests.length;
      // The server has ended the check and found the stack behind: the next poll of the stack says so.
      checking = false;
      stacks = {
        items: [gitStack("web", { status: "behind", behind: 2, headSha: "f".repeat(40) })],
      };
      const before = Date.now();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(STACK_GIT_POLL_MS);
      });
      await waitUnderFakeClock(() => expect(screen.getByText("Behind 2")).toBeTruthy());
      // It came from the end of the check and not from the list's own 30 s refresh.
      expect(Date.now() - before).toBeLessThan(LIST_REFETCH_MS);
      expect(requests.length).toBeGreaterThan(listReads);
    } finally {
      vi.useRealTimers();
    }
  });

  it("reads Pull & redeploy… for a git stack in the place of Deploy…, and Review update only while it is behind", async () => {
    stacks = {
      items: [
        stack({ id: "inline", name: "inline", state: "running" }),
        gitStack("current", { status: "in_sync", behind: 0 }),
        gitStack("late", { status: "behind", behind: 2 }),
      ],
    };
    mount();
    const expected: Record<string, { pull: boolean; deploy: boolean; review: boolean }> = {
      inline: { pull: false, deploy: true, review: false },
      current: { pull: true, deploy: false, review: false },
      late: { pull: true, deploy: false, review: true },
    };
    for (const [name, want] of Object.entries(expected)) {
      const trigger = await screen.findByRole("button", { name: `Actions for ${name}` });
      fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
      await screen.findByRole("menuitem", { name: "Open stack" });
      expect(!!screen.queryByRole("menuitem", { name: "Pull & redeploy…" })).toBe(want.pull);
      expect(!!screen.queryByRole("menuitem", { name: "Deploy…" })).toBe(want.deploy);
      expect(!!screen.queryByRole("menuitem", { name: "Review update" })).toBe(want.review);
      fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    }
  });

  it("offers no Check now for an inline or an adopted stack, or for a fixed commit", async () => {
    stacks = {
      items: [
        stack({ id: "inline", name: "inline", state: "running" }),
        stack({ id: "adopted", name: "adopted", source: "adopted", state: "running" }),
        gitStack("pinned", { refKind: "commit", ref: SHA }),
      ],
    };
    mount();
    for (const name of ["inline", "adopted", "pinned"]) {
      const trigger = await screen.findByRole("button", { name: `Actions for ${name}` });
      fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
      await screen.findByRole("menuitem", { name: "Open stack" });
      expect(screen.queryByRole("menuitem", { name: "Check now" })).toBeNull();
      fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    }
  });

  it("puts Behind and Sync error in the URL after the other chips, and restores them for the server", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "web" });
    fireEvent.click(screen.getByRole("button", { name: /^Sync error/ }));
    expect(onChange).toHaveBeenLastCalledWith({ status: "sync-error" });
    fireEvent.click(screen.getByRole("button", { name: /^Behind/ }));
    expect(onChange).toHaveBeenLastCalledWith({ status: "behind,sync-error" });
    await waitFor(() =>
      expect((requests.at(-1) as URL).searchParams.get("status")).toBe("behind,sync-error"),
    );
  });
});
