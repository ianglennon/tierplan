// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ContainerRow } from "@/api/resources";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ContainerList } from "@/features/containers/container-list";
import {
  type ContainerSearch,
  type ContainerSearchPatch,
  containerSearchSchema,
} from "@/features/containers/list-search";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";

function row(name: string, extra: Partial<ContainerRow> = {}): ContainerRow {
  return {
    node: "node-1",
    nodeId: "n1",
    id: `id-${name}`,
    name,
    state: "running",
    status: "Up 2 hours",
    image: "nginx:1.27",
    created: "2026-10-01T10:00:00Z",
    ports: [],
    isSelf: false,
    project: "",
    stack: "",
    service: "",
    health: "none",
    selfKind: "",
    readOnly: false,
    ...extra,
  };
}

const ROWS = [
  row("shop-web", { project: "shop" }),
  row("shop-db", { project: "shop", state: "exited" }),
  row("blog", { stack: "Blog" }),
  row("loose"),
];

interface Counts {
  running: number;
  exited: number;
  paused: number;
  restarting: number;
  unhealthy: number;
}

const COUNTS: Counts = { running: 0, exited: 0, paused: 0, restarting: 0, unhealthy: 0 };

// What the stubbed list answers for a request.
let answer: (url: URL) => Response | Promise<Response>;
let urls: string[];
let changes: ContainerSearchPatch[];
let client: QueryClient;
let envType: "standalone" | "swarm";

function body(items: ContainerRow[], extra: Record<string, unknown> = {}) {
  return {
    items,
    total: items.length,
    truncated: false,
    unfilteredTotal: items.length,
    statusCounts: COUNTS,
    nodes: [],
    notReporting: [],
    ...extra,
  };
}

beforeEach(() => {
  urls = [];
  changes = [];
  envType = "standalone";
  answer = () => Response.json(body(ROWS));
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (url.pathname === "/api/environments")
        return Response.json({
          items: [
            { id: ENV, name: "Lab host", type: envType, envType, status: { state: "online" } },
          ],
        });
      if (url.pathname === `/api/env/${ENV}/summary`)
        return Response.json({ environment: { id: ENV, type: envType } });
      if (url.pathname === `/api/env/${ENV}/containers`) {
        urls.push(`${url.pathname}${url.search}`);
        return answer(url);
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

function Harness({ initial = {} }: { initial?: ContainerSearch }) {
  const [search, setSearch] = useState<ContainerSearch>(initial);
  return (
    <ContainerList
      envId={ENV}
      search={search}
      onSearchChange={(patch) => {
        changes.push(patch);
        setSearch((prev) => ({ ...prev, ...patch }));
      }}
    />
  );
}

function mount(initial: ContainerSearch = {}) {
  return render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <Harness initial={initial} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const bodyRows = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.textContent ?? "");

describe("container list: groups", () => {
  it("opens grouped by stack or project, expanded, Ungrouped last, with counts and kind badges", async () => {
    mount();
    await screen.findByText("shop-web");
    const headers = screen.getAllByRole("button", { name: /^Collapse / });
    expect(headers.map((b) => b.getAttribute("aria-label"))).toEqual([
      "Collapse Blog",
      "Collapse shop",
      "Collapse Ungrouped",
    ]);
    for (const b of headers) expect(b.getAttribute("aria-expanded")).toBe("true");
    const rows = bodyRows();
    expect(rows[0]).toContain("Blog");
    expect(rows[0]).toContain("1 of 1 running");
    expect(rows[0]).toContain("Stack");
    expect(rows[2]).toContain("shop");
    expect(rows[2]).toContain("1 of 2 running");
    expect(rows[2]).toContain("Project");
    // Rows of a group are sorted by name.
    expect(rows[3]).toContain("shop-db");
    expect(rows[4]).toContain("shop-web");
    expect(rows[5]).toContain("Ungrouped");
    expect(rows[5]).not.toContain("Stack");
  });

  it("folds a group's rows away with its chevron and shows them again", async () => {
    mount();
    await screen.findByText("shop-web");
    const toggle = screen.getByRole("button", { name: "Collapse shop" });
    const controls = toggle.getAttribute("aria-controls") as string;
    expect(document.getElementById(controls)).not.toBeNull();
    fireEvent.click(toggle);
    expect(screen.queryByText("shop-web")).toBeNull();
    expect(screen.getByText("blog")).toBeTruthy();
    const folded = screen.getByRole("button", { name: "Expand shop" });
    expect(folded.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(folded);
    expect(screen.getByText("shop-web")).toBeTruthy();
  });

  it("is one flat table without headers when grouping is none", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    expect(screen.queryByRole("button", { name: /^(Collapse|Expand) / })).toBeNull();
    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector("td")?.textContent);
    expect(names).toEqual(["blog", "loose", "shop-db", "shop-web"]);
  });

  it("drops an invalid group from the URL search and uses the default", () => {
    const parsed = containerSearchSchema.parse({ group: "foo", sort: "size", dir: "up", q: "x" });
    expect(parsed.group).toBeUndefined();
    expect(parsed.sort).toBeUndefined();
    expect(parsed.dir).toBeUndefined();
    expect(parsed.q).toBe("x");
    expect(containerSearchSchema.parse({ group: "node" }).group).toBe("node");
    expect(containerSearchSchema.parse({ status: "exited,bogus,running,exited" }).status).toBe(
      "running,exited",
    );
    expect(containerSearchSchema.parse({ q: "a".repeat(129) }).q).toBeUndefined();
  });
});

describe("container list: search", () => {
  it("waits 250 ms, then puts the search in the URL and asks the server for it", async () => {
    mount();
    await screen.findByText("shop-web");
    const box = screen.getByRole("searchbox", { name: "Search containers" });
    expect(box.getAttribute("placeholder")).toBe("Search by name, image, label or project");
    answer = (url) =>
      Response.json(body(ROWS.filter((r) => r.project === url.searchParams.get("q"))));
    fireEvent.change(box, { target: { value: "shop" } });
    expect((box as HTMLInputElement).value).toBe("shop");
    // Not yet: the box waits for the typing to stop.
    expect(changes).toEqual([]);
    await waitFor(() => expect(changes).toEqual([{ q: "shop" }]), { timeout: 1500 });
    await waitFor(() => expect(urls).toContain(`/api/env/${ENV}/containers?q=shop`));
    await waitFor(() => expect(screen.queryByText("loose")).toBeNull());
    expect(screen.getByText("shop-web")).toBeTruthy();
  });

  it("sends one search for a run of keys", async () => {
    mount();
    await screen.findByText("shop-web");
    const box = screen.getByRole("searchbox", { name: "Search containers" });
    fireEvent.change(box, { target: { value: "s" } });
    fireEvent.change(box, { target: { value: "sh" } });
    fireEvent.change(box, { target: { value: "sho" } });
    await waitFor(() => expect(changes).toEqual([{ q: "sho" }]), { timeout: 1500 });
  });

  it("clears the search at once with Clear search", async () => {
    mount({ q: "shop" });
    await screen.findByText("shop-web");
    const box = screen.getByRole("searchbox", { name: "Search containers" }) as HTMLInputElement;
    expect(box.value).toBe("shop");
    fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
    expect(box.value).toBe("");
    expect(changes).toEqual([{ q: undefined }]);
    expect(screen.queryByRole("button", { name: "Clear search" })).toBeNull();
  });

  it("follows the URL when something else changes the search", async () => {
    const view = mount({ q: "shop" });
    await screen.findByText("shop-web");
    view.unmount();
    mount({ q: "blog" });
    await screen.findByText("shop-web");
    expect(
      (screen.getByRole("searchbox", { name: "Search containers" }) as HTMLInputElement).value,
    ).toBe("blog");
  });

  it("sorts from a column header and flips the direction on a second press", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    const head = screen.getByRole("columnheader", { name: "Name" });
    expect(head.getAttribute("aria-sort")).toBe("ascending");
    fireEvent.click(within(head).getByRole("button"));
    expect(changes).toEqual([{ sort: "name", dir: "desc" }]);
    expect(
      within(screen.getByRole("columnheader", { name: "Name" })).getByRole("button"),
    ).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Name" }).getAttribute("aria-sort")).toBe(
      "descending",
    );
    expect(screen.getByRole("columnheader", { name: "State" }).getAttribute("aria-sort")).toBe(
      "none",
    );
    expect(
      screen.getByRole("columnheader", { name: "Image" }).getAttribute("aria-sort"),
    ).toBeNull();
  });
});
