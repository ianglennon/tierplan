// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { GlobalStackList } from "@/features/stacks/global-stack-list";
import type { GlobalStackSearch, GlobalStackSearchPatch } from "@/features/stacks/list-search";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";
import { TestRouter } from "@/test-router";

let me: ReturnType<typeof makeMe>;
let client: QueryClient;
let answer: unknown;
let listStatus: number;
let requests: string[];
// Fields of the answer a test sets on top of the groups.
let extra: Record<string, unknown>;

const stack = (over: Record<string, unknown> = {}) => ({
  id: "s1",
  name: "web",
  target: "swarm",
  source: "inline",
  state: "not_deployed",
  lastDeployedAt: "",
  lastDeployedBy: "",
  hasDraft: false,
  ...over,
});

const group = (envId: string, envName: string, target: string, stacks: unknown[]) => ({
  envId,
  envName,
  target,
  stacks,
});

const ENVIRONMENTS = [
  { id: "e1", name: "Prod", envType: "swarm" },
  { id: "e2", name: "Staging", envType: "standalone" },
];

beforeEach(() => {
  me = makeMe({ envs: { e1: "admin", e2: "admin" } });
  answer = {
    groups: [
      group("e1", "Prod", "swarm", [
        stack({ id: "a", name: "alpha" }),
        stack({ id: "b", name: "beta" }),
      ]),
      group("e2", "Staging", "standalone", [
        stack({ id: "c", name: "gamma", target: "standalone" }),
      ]),
    ],
    failedEnvironments: [],
  };
  listStatus = 200;
  requests = [];
  extra = {};
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") return Response.json({ items: ENVIRONMENTS });
      if (url.pathname === "/api/stacks") {
        requests.push(url.search);
        if (listStatus !== 200)
          return Response.json({ title: "x", status: listStatus }, { status: listStatus });
        const groups = (answer as { groups: { stacks: unknown[] }[] }).groups;
        const rows = groups.reduce((n, g) => n + g.stacks.length, 0);
        return Response.json({
          total: rows,
          unfilteredTotal: rows,
          capped: false,
          statusCounts: { degraded: 0, stopped: 0, draft: 0, deployFailed: 0 },
          ...(answer as object),
          ...extra,
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
}: {
  initial: GlobalStackSearch;
  onChange: (patch: GlobalStackSearchPatch) => void;
}) {
  const [search, setSearch] = useState(initial);
  return (
    <GlobalStackList
      search={search}
      onSearchChange={(patch) => {
        onChange(patch);
        setSearch((prev) => {
          const next: Record<string, unknown> = { ...prev, ...patch };
          for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
          return next as GlobalStackSearch;
        });
      }}
    />
  );
}

function mount(initial: GlobalStackSearch = {}) {
  const onChange = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness initial={initial} onChange={onChange} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return { onChange };
}

describe("GlobalStackList", () => {
  it("lists the stacks of every environment under a header band with its name, count and target", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Stacks", level: 1 })).toBeTruthy();
    await screen.findByRole("link", { name: "alpha" });
    expect(screen.getByText(/Stacks Bayplan manages across your environments\./)).toBeTruthy();
    expect(screen.getByText(/3 stacks in 2 environments$/)).toBeTruthy();
    const bands = document.querySelectorAll('[data-slot="group-header-row"]');
    expect(bands).toHaveLength(2);
    const [prod, staging] = Array.from(bands) as HTMLElement[];
    expect(within(prod as HTMLElement).getByText("Prod")).toBeTruthy();
    expect(within(prod as HTMLElement).getByText("2 stacks")).toBeTruthy();
    expect(within(prod as HTMLElement).getByText("Swarm")).toBeTruthy();
    expect(within(staging as HTMLElement).getByText("Staging")).toBeTruthy();
    expect(within(staging as HTMLElement).getByText("1 stack")).toBeTruthy();
    expect(within(staging as HTMLElement).getByText("Standalone")).toBeTruthy();
  });

  it("names each stack's environment in a column, as a link to that environment's list with its target", async () => {
    mount();
    const row = (await screen.findByRole("link", { name: "gamma" })).closest("tr") as HTMLElement;
    const env = within(row).getByRole("link", { name: "Staging" });
    expect(env.getAttribute("href")).toBe("/env/e2/stacks");
    expect(within(row).getByText("Standalone")).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Environment" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "gamma" }).getAttribute("href")).toBe(
      "/env/e2/stacks/c",
    );
  });

  it("shows Services as running over total and a dash for a stack not deployed", async () => {
    answer = {
      groups: [
        group("e1", "Prod", "swarm", [
          stack({
            id: "a",
            name: "alpha",
            state: "degraded",
            servicesRunning: 1,
            servicesTotal: 3,
          }),
          stack({ id: "b", name: "beta" }),
        ]),
      ],
      failedEnvironments: [],
    };
    mount();
    const row = (n: string) => screen.getByRole("link", { name: n }).closest("tr") as HTMLElement;
    await screen.findByRole("link", { name: "alpha" });
    expect(within(row("alpha")).getByText("1/3")).toBeTruthy();
    expect(within(row("beta")).getByText("—")).toBeTruthy();
    expect(within(row("beta")).getByText("Never")).toBeTruthy();
  });

  it("puts the environments chosen in the search and asks the server for them", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "alpha" });
    fireEvent.click(screen.getByRole("button", { name: /Environment/ }));
    fireEvent.click(await screen.findByRole("checkbox", { name: "Staging" }));
    expect(onChange).toHaveBeenCalledWith({ env: "e2" });
    await waitFor(() => expect(requests).toContain("?env=e2"));
    expect(screen.getByRole("button", { name: /1 environment/ })).toBeTruthy();
  });

  it("starts with the environments in the address ticked", async () => {
    mount({ env: "e1,e2" });
    await screen.findByRole("link", { name: "alpha" });
    fireEvent.click(screen.getByRole("button", { name: /Environment/ }));
    expect(
      (await screen.findByRole("checkbox", { name: "Prod" })).getAttribute("aria-checked"),
    ).toBe("true");
    expect(screen.getByRole("checkbox", { name: "Staging" }).getAttribute("aria-checked")).toBe(
      "true",
    );
  });

  it("offers only the environments the user may read stacks on", async () => {
    me = makeMe({ envs: { e1: "viewer", e2: "viewer" } });
    me = {
      ...me,
      permissions: {
        ...me.permissions,
        environments: { e1: ["stacks.read", "environments.read"], e2: ["environments.read"] },
      },
    };
    mount();
    await screen.findByRole("link", { name: "alpha" });
    fireEvent.click(screen.getByRole("button", { name: /Environment/ }));
    expect(await screen.findByRole("checkbox", { name: "Prod" })).toBeTruthy();
    expect(screen.queryByRole("checkbox", { name: "Staging" })).toBeNull();
  });

  it("groups by environment by default and in one list when Group by is None", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "alpha" });
    const byEnv = screen.getByRole("radio", { name: "Environment" });
    expect(byEnv.getAttribute("aria-checked")).toBe("true");
    fireEvent.click(screen.getByRole("radio", { name: "None" }));
    expect(onChange).toHaveBeenCalledWith({ group: "none" });
    await waitFor(() =>
      expect(document.querySelectorAll('[data-slot="group-header-row"]')).toHaveLength(0),
    );
    expect(screen.getAllByRole("link", { name: /alpha|beta|gamma/ })).toHaveLength(3);
  });

  it("puts a failed environment's notice in place of its group and still renders the others", async () => {
    answer = {
      groups: [group("e1", "Prod", "swarm", [stack({ id: "a", name: "alpha" })])],
      failedEnvironments: [{ envId: "e2", envName: "Staging" }],
    };
    mount();
    await screen.findByRole("link", { name: "alpha" });
    const notice = await screen.findByText("Couldn't load stacks on Staging.");
    expect(notice).toBeTruthy();
    const alert = notice.closest('[role="alert"]') as HTMLElement;
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
    const bands = Array.from(document.querySelectorAll('[data-slot="group-header-row"]'));
    expect(bands.map((b) => b.textContent)).toEqual([
      expect.stringContaining("Prod"),
      expect.stringContaining("Staging"),
    ]);
    const before = requests.length;
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(requests.length).toBeGreaterThan(before));
  });

  it("says no stacks yet when nothing is listed anywhere", async () => {
    answer = { groups: [], failedEnvironments: [] };
    mount();
    expect(await screen.findByRole("heading", { name: "No stacks yet", level: 2 })).toBeTruthy();
    expect(screen.getByText("Stacks you create on any environment show up here.")).toBeTruthy();
  });

  it("shows the failed environments when none could be read at all", async () => {
    answer = { groups: [], failedEnvironments: [{ envId: "e1", envName: "Prod" }] };
    mount();
    expect(await screen.findByText("Couldn't load stacks on Prod.")).toBeTruthy();
  });

  it("says it could not load the stacks, with a retry", async () => {
    listStatus = 500;
    mount();
    expect(await screen.findByText("Couldn't load stacks.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry loading stacks" })).toBeTruthy();
  });

  it("shows eight skeleton rows and the toolbar while the list loads", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/me") return Response.json(me);
        if (path === "/api/environments") return Response.json({ items: ENVIRONMENTS });
        return new Promise<Response>(() => {});
      }),
    );
    mount();
    const table = await screen.findByRole("table", { name: "Stacks" });
    await waitFor(() => expect(table.getAttribute("aria-busy")).toBe("true"));
    expect(table.querySelectorAll("tbody tr")).toHaveLength(8);
    expect(screen.getByRole("button", { name: /Environment/ })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "None" })).toBeTruthy();
  });

  it("shows the no-permission page to a user who may read stacks nowhere", async () => {
    me = makeMe({
      envs: { e1: "viewer" },
      permissions: splitPermissions(["environments.read"]),
    });
    mount();
    expect(await screen.findByText(/permission/i)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "alpha" })).toBeNull();
  });

  it("renders a stack's name and an environment's name as text", async () => {
    answer = {
      groups: [
        group("e1", "<b>Prod</b>", "swarm", [
          stack({ id: "a", name: "<img src=x onerror=alert(1)>" }),
        ]),
      ],
      failedEnvironments: [],
    };
    mount();
    const link = await screen.findByRole("link", { name: "<img src=x onerror=alert(1)>" });
    expect(link.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
  });

  it("folds a group away from its header", async () => {
    mount();
    await screen.findByRole("link", { name: "alpha" });
    fireEvent.click(screen.getByRole("button", { name: /Collapse Prod/ }));
    await waitFor(() => expect(screen.queryByRole("link", { name: "alpha" })).toBeNull());
    expect(screen.getByRole("link", { name: "gamma" })).toBeTruthy();
  });
});

const searchOf = (query: string) => new URLSearchParams(query);

describe("GlobalStackList: search, status chips, sort and the cap", () => {
  it("has a search labelled Search stacks and sends it, after 250 ms, to the server", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "alpha" });
    const box = screen.getByRole("searchbox", { name: "Search stacks" });
    expect(box.getAttribute("placeholder")).toBe("Search by name or service");
    fireEvent.change(box, { target: { value: "gam" } });
    expect(onChange).not.toHaveBeenCalled();
    await waitFor(() => expect(onChange).toHaveBeenCalledWith({ q: "gam" }));
    await waitFor(() => expect(requests.some((r) => searchOf(r).get("q") === "gam")).toBe(true));
  });

  it("sends the search, the statuses, the sort and the environments of the URL", async () => {
    mount({
      q: "a",
      status: "stopped,deploy_failed",
      sort: "lastDeployed",
      dir: "desc",
      env: "e1",
    });
    await screen.findByRole("link", { name: "alpha" });
    await waitFor(() => expect(requests.length).toBeGreaterThan(0));
    const sent = searchOf(requests.at(-1) as string);
    expect(Object.fromEntries(sent)).toEqual({
      q: "a",
      status: "stopped,deploy_failed",
      sort: "lastDeployed",
      dir: "desc",
      env: "e1",
    });
  });

  it("shows the status chips with the counts of the whole set, and puts a chosen one in the URL", async () => {
    extra = { statusCounts: { degraded: 4, stopped: 0, draft: 1, deployFailed: 2 } };
    const { onChange } = mount({ q: "zzz" });
    await screen.findByRole("link", { name: "alpha" });
    const group = screen.getByRole("group", { name: "Filter by status" });
    expect(group.className).toContain("flex-wrap");
    for (const label of ["Degraded 4", "Stopped 0", "Draft 1", "Deploy failed 2"]) {
      expect(within(group).getByRole("button", { name: label })).toBeTruthy();
    }
    fireEvent.click(within(group).getByRole("button", { name: "Deploy failed 2" }));
    expect(onChange).toHaveBeenLastCalledWith({ status: "deploy_failed" });
  });

  it("offers Clear filters while a search, a chip or an environment is on, and clears them all", async () => {
    const { onChange } = mount({ q: "a", status: "draft", env: "e1" });
    await screen.findByRole("link", { name: "alpha" });
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(onChange).toHaveBeenCalledWith({ q: undefined, status: undefined, env: undefined });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull());
  });

  it("does not offer Clear filters when only the grouping or the sort is set", async () => {
    mount({ group: "none", sort: "state" });
    await screen.findByRole("link", { name: "alpha" });
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  });

  it("says nothing matches the search, or the filters, when stacks exist but none are listed", async () => {
    answer = { groups: [], failedEnvironments: [] };
    extra = { unfilteredTotal: 7 };
    const { onChange } = mount({ q: "zzz" });
    expect(
      await screen.findByRole("heading", { name: "No stacks match “zzz”.", level: 2 }),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
    expect(onChange).toHaveBeenCalledWith({ q: undefined });
    cleanup();
    client.clear();
    mount({ status: "draft" });
    expect(
      await screen.findByRole("heading", { name: "No stacks match these filters.", level: 2 }),
    ).toBeTruthy();
  });

  it("counts n of total while a filter is on, and the plain count otherwise", async () => {
    extra = { total: 3, unfilteredTotal: 9 };
    mount({ status: "draft" });
    await screen.findByRole("link", { name: "alpha" });
    expect(screen.getByText(/3 of 9 stacks in 2 environments$/)).toBeTruthy();
  });

  it("says Showing the first 500 of n when the server capped the list", async () => {
    extra = { capped: true, total: 640, unfilteredTotal: 640 };
    mount();
    await screen.findByRole("link", { name: "alpha" });
    expect(
      screen.getByText("Showing the first 500 of 640. Narrow the search to see the rest."),
    ).toBeTruthy();
  });

  it("sorts by Name, State and Last deployed from the headers, with aria-sort", async () => {
    const { onChange } = mount();
    await screen.findByRole("link", { name: "alpha" });
    const header = (n: string) => screen.getByRole("columnheader", { name: n });
    expect(header("Name").getAttribute("aria-sort")).toBe("ascending");
    expect(header("State").getAttribute("aria-sort")).toBe("none");
    expect(header("Environment").getAttribute("aria-sort")).toBeNull();
    fireEvent.click(within(header("State")).getByRole("button"));
    expect(onChange).toHaveBeenLastCalledWith({ sort: "state", dir: "asc" });
    fireEvent.click(within(header("State")).getByRole("button"));
    expect(onChange).toHaveBeenLastCalledWith({ sort: "state", dir: "desc" });
    await waitFor(() => expect(header("State").getAttribute("aria-sort")).toBe("descending"));
    fireEvent.click(within(header("Last deployed")).getByRole("button"));
    expect(onChange).toHaveBeenLastCalledWith({ sort: "lastDeployed", dir: "asc" });
  });

  it("keeps the server's order inside each environment", async () => {
    answer = {
      groups: [
        group("e1", "Prod", "swarm", [
          stack({ id: "z", name: "zulu" }),
          stack({ id: "a", name: "alpha" }),
        ]),
      ],
      failedEnvironments: [],
    };
    mount({ sort: "state", dir: "desc" });
    await screen.findByRole("link", { name: "zulu" });
    const rows = screen.getAllByRole("row").slice(2);
    expect(rows.map((r) => within(r).getAllByRole("link")[0]?.textContent)).toEqual([
      "zulu",
      "alpha",
    ]);
  });

  it("sorts one list across the environments when Group by is None", async () => {
    answer = {
      groups: [
        group("e1", "Prod", "swarm", [
          stack({ id: "b", name: "beta" }),
          stack({ id: "d", name: "delta" }),
        ]),
        group("e2", "Staging", "standalone", [
          stack({ id: "a", name: "alpha", target: "standalone" }),
          stack({ id: "g", name: "gamma", target: "standalone" }),
        ]),
      ],
      failedEnvironments: [],
    };
    mount({ group: "none" });
    await screen.findByRole("link", { name: "alpha" });
    const order = () =>
      screen
        .getAllByRole("row")
        .slice(1)
        .map((r) => within(r).getAllByRole("link")[0]?.textContent);
    expect(order()).toEqual(["alpha", "beta", "delta", "gamma"]);
    fireEvent.click(within(screen.getByRole("columnheader", { name: "Name" })).getByRole("button"));
    await waitFor(() => expect(order()).toEqual(["gamma", "delta", "beta", "alpha"]));
  });

  it("labels a stack with an operation under way and turns its menu off", async () => {
    answer = {
      groups: [
        group("e1", "Prod", "swarm", [
          stack({ id: "a", name: "alpha", state: "deploying" }),
          stack({ id: "b", name: "beta", state: "running", servicesRunning: 2, servicesTotal: 2 }),
        ]),
      ],
      failedEnvironments: [],
    };
    mount();
    await screen.findByRole("link", { name: "beta" });
    const row = (n: string) => screen.getByRole("link", { name: n }).closest("tr") as HTMLElement;
    expect(within(row("alpha")).getByText("Deploying…")).toBeTruthy();
    expect(
      (within(row("alpha")).getByRole("button", { name: "Actions for alpha" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expect(
      (within(row("beta")).getByRole("button", { name: "Actions for beta" }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);
  });
});
