// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type ContainerRow, listKey } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ContainerList } from "@/features/containers/container-list";
import {
  type ContainerSearch,
  type ContainerSearchPatch,
  containerSearchSchema,
} from "@/features/containers/list-search";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

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
    coamingStackId: "",
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
// The writes the list sent, and what the server answers to one.
let actionPosts: { path: string; body: { targets: { id: string }[]; [k: string]: unknown } }[];
let actionAnswer: (req: (typeof actionPosts)[number]) => Response | Promise<Response>;

function allDone(req: (typeof actionPosts)[number]) {
  return Response.json({
    bulkId: "018f0000-0000-7000-8000-000000000001",
    results: req.body.targets.map((t) => ({
      nodeId: "n1",
      id: t.id,
      name: t.id.replace("id-", ""),
      result: "done",
    })),
  });
}

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
  actionPosts = [];
  actionAnswer = allDone;
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
      if (url.pathname.startsWith(`/api/env/${ENV}/containers/`) && req.method === "POST") {
        const entry = { path: url.pathname, body: await req.clone().json() };
        actionPosts.push(entry);
        return actionAnswer(entry);
      }
      if (url.pathname === `/api/env/${ENV}/containers`) {
        urls.push(`${url.pathname}${url.search}`);
        return answer(url);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
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
      <TestRouter>
        <TooltipProvider>
          <Harness initial={initial} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const bodyRows = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.textContent ?? "");

describe("container list: the name opens the container", () => {
  it("links each name to the container's page, carrying the node it is on", async () => {
    mount();
    await screen.findByText("shop-web");
    const link = screen.getByRole("link", { name: "shop-web" });
    expect(link.getAttribute("href")).toBe("/env/e1/containers/id-shop-web?node=n1");
    expect(screen.getByRole("link", { name: "loose" }).getAttribute("href")).toBe(
      "/env/e1/containers/id-loose?node=n1",
    );
  });
});

describe("container list: stack links", () => {
  it("links a stack or project Coaming manages in its group header, and a foreign one to its read-only page", async () => {
    answer = () =>
      Response.json(
        body([
          row("shop-web", { project: "shop", coamingStackId: "stk-shop" }),
          row("blog", { stack: "Blog" }),
        ]),
      );
    mount();
    await screen.findByText("shop-web");
    const headers = Array.from(
      document.querySelectorAll<HTMLElement>('[data-slot="group-header-row"]'),
    );
    const shop = headers.find((h) => h.textContent?.startsWith("shop")) as HTMLElement;
    const blog = headers.find((h) => h.textContent?.startsWith("Blog")) as HTMLElement;
    expect(within(shop).getByRole("link", { name: "shop" }).getAttribute("href")).toBe(
      "/env/e1/stacks/stk-shop",
    );
    expect(within(blog).getByRole("link", { name: "Blog" }).getAttribute("href")).toBe(
      "/env/e1/unmanaged-stacks/Blog",
    );
  });
});

describe("container list: groups", () => {
  it("opens grouped by stack or project, expanded, Ungrouped last, with counts and kind badges", async () => {
    mount();
    await screen.findByText("shop-web");
    const headers = screen.getAllByRole("button", { name: /^Collapse (?!all)/ });
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
    expect(screen.queryByRole("button", { name: /^(Collapse|Expand) (?!all)/ })).toBeNull();
    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelectorAll("td")[1]?.textContent);
    expect(names).toEqual(["blog", "loose", "shop-db", "shop-web"]);
  });

  it("drops an invalid group from the URL search and uses the default", () => {
    const parsed = containerSearchSchema.parse({ group: "foo", sort: "size", dir: "up", q: "x" });
    expect(parsed.group).toBeUndefined();
    expect(parsed.sort).toBeUndefined();
    expect(parsed.dir).toBeUndefined();
    expect(parsed.q).toBe("x");
    expect(containerSearchSchema.parse({ group: "node" }).group).toBe("node");
    // The live stats switch is stats=1 and nothing else.
    expect(containerSearchSchema.parse({ stats: 1 }).stats).toBe(1);
    expect(containerSearchSchema.parse({ stats: "1" }).stats).toBeUndefined();
    expect(containerSearchSchema.parse({ stats: 2 }).stats).toBeUndefined();
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
    await waitFor(() => expect(changes).toEqual([{ q: "shop" }]));
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
    await waitFor(() => expect(changes).toEqual([{ q: "sho" }]));
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

const CHIP_COUNTS: Counts = { running: 24, exited: 3, paused: 0, restarting: 1, unhealthy: 2 };

async function pick(name: string, option: string) {
  fireEvent.keyDown(screen.getByRole("combobox", { name }), { key: "Enter" });
  fireEvent.keyDown(await screen.findByRole("option", { name: option }), { key: "Enter" });
}

describe("container list: status chips", () => {
  beforeEach(() => {
    answer = () => Response.json(body(ROWS, { statusCounts: CHIP_COUNTS, unfilteredTotal: 30 }));
  });

  it("shows the five chips with their counts in a group named Filter by status", async () => {
    mount();
    await screen.findByText("shop-web");
    const group = screen.getByRole("group", { name: "Filter by status" });
    expect(
      within(group)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Running 24", "Exited 3", "Paused 0", "Restarting 1", "Unhealthy 2"]);
    // The chips wrap onto further rows at every width, so none is cut off at 375px (G-06-2).
    expect(group.classList.contains("flex-wrap")).toBe(true);
    expect(group.className).not.toMatch(/overflow-x-auto|flex-nowrap/);
  });

  it("filters on the server with the chosen statuses and keeps the counts as they were", async () => {
    mount();
    await screen.findByText("shop-web");
    answer = (url) =>
      Response.json(
        body(ROWS.slice(0, 1), {
          statusCounts: CHIP_COUNTS,
          unfilteredTotal: 30,
          total: url.searchParams.get("status") ? 1 : 4,
        }),
      );
    fireEvent.click(screen.getByRole("button", { name: "Running 24" }));
    fireEvent.click(screen.getByRole("button", { name: "Unhealthy 2" }));
    expect(changes).toEqual([{ status: "running" }, { status: "running,unhealthy" }]);
    await waitFor(() =>
      expect(urls).toContain(`/api/env/${ENV}/containers?status=running%2Cunhealthy`),
    );
    expect(screen.getByRole("button", { name: "Running 24" }).getAttribute("aria-pressed")).toBe(
      "true",
    );
    expect(screen.getByRole("button", { name: "Exited 3" }).getAttribute("aria-pressed")).toBe(
      "false",
    );
    expect(screen.getByRole("button", { name: "Unhealthy 2" })).toBeTruthy();
  });

  it("takes a chip off the list when it is pressed again", async () => {
    mount({ status: "running,exited" });
    await screen.findByText("shop-web");
    fireEvent.click(screen.getByRole("button", { name: "Exited 3" }));
    expect(changes).toEqual([{ status: "running" }]);
    fireEvent.click(screen.getByRole("button", { name: "Running 24" }));
    expect(changes[1]).toEqual({ status: undefined });
  });

  it("renders the toolbar and the chips at once while the first answer is on its way", () => {
    answer = () => new Promise<Response>(() => {});
    mount();
    expect(screen.getByRole("searchbox", { name: "Search containers" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Filter by status" })).toBeTruthy();
    expect(screen.getAllByRole("row")).toHaveLength(9);
  });
});

describe("container list: group by, collapse all and clear", () => {
  it("offers Stack or project and None on a standalone host, and Node on a swarm", async () => {
    mount();
    await screen.findByText("shop-web");
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Group by" }), { key: "Enter" });
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
      "Stack or project",
      "None",
    ]);
    cleanup();
    envType = "swarm";
    mount();
    await screen.findByText("shop-web");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Node" })).toBeTruthy());
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Group by" }), { key: "Enter" });
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
      "Stack or project",
      "Node",
      "None",
    ]);
  });

  it("puts the choice in the search, and the default stays out of the URL", async () => {
    mount();
    await screen.findByText("shop-web");
    expect(screen.getByRole("combobox", { name: "Group by" }).textContent).toContain(
      "Stack or project",
    );
    await pick("Group by", "None");
    expect(changes).toEqual([{ group: "none" }]);
    expect(screen.queryByRole("button", { name: /^Collapse / })).toBeNull();
    await pick("Group by", "Stack or project");
    expect(changes[1]).toEqual({ group: undefined });
  });

  it("groups by node on a swarm", async () => {
    envType = "swarm";
    answer = () =>
      Response.json(
        body([row("a", { node: "w2" }), row("b", { node: "m1" })], { nodes: ["m1", "w2"] }),
      );
    mount({ group: "node" });
    await screen.findByText("a");
    expect(screen.getByRole("button", { name: "Collapse m1" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Collapse w2" })).toBeTruthy();
    // A node group has no Stack or Project badge.
    expect(screen.queryByText("Stack")).toBeNull();
    expect(screen.queryByText("Project")).toBeNull();
  });

  it("falls back to stack grouping for ?group=node on a standalone host", async () => {
    mount({ group: "node" });
    await screen.findByText("shop-web");
    expect(screen.getByRole("button", { name: "Collapse shop" })).toBeTruthy();
  });

  it("folds every group with Collapse all groups and opens them with Expand all groups", async () => {
    mount();
    await screen.findByText("shop-web");
    fireEvent.click(screen.getByRole("button", { name: "Collapse all groups" }));
    expect(screen.queryByText("shop-web")).toBeNull();
    expect(screen.queryByText("loose")).toBeNull();
    expect(screen.getByRole("button", { name: "Expand Blog" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Expand all groups" }));
    expect(screen.getByText("shop-web")).toBeTruthy();
    expect(screen.getByText("loose")).toBeTruthy();
  });

  it("has no collapse-all button when the list is not grouped", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    expect(screen.queryByRole("button", { name: /all groups$/ })).toBeNull();
  });

  it("shows Clear filters only while a search, a status or a node is set, and resets all three", async () => {
    mount();
    await screen.findByText("shop-web");
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    cleanup();
    mount({ q: "shop", status: "running" });
    await screen.findByText("shop-web");
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(changes).toEqual([{ q: undefined, status: undefined, node: undefined }]);
    await waitFor(() => expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull());
    expect(
      (screen.getByRole("searchbox", { name: "Search containers" }) as HTMLInputElement).value,
    ).toBe("");
  });

  it("shows Clear filters for a node on a swarm", async () => {
    envType = "swarm";
    mount({ node: "w2" });
    await screen.findByText("shop-web");
    expect(screen.getByRole("button", { name: "Clear filters" })).toBeTruthy();
  });
});

describe("container list: empty states", () => {
  it("says there are no containers, naming the environment, when there are none at all", async () => {
    answer = () => Response.json(body([]));
    mount();
    await screen.findByText("No containers");
    expect(screen.getByText("Nothing to show on Lab host right now.")).toBeTruthy();
    // The toolbar still renders.
    expect(screen.getByRole("searchbox", { name: "Search containers" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Clear search" })).toBeNull();
  });

  it("says no container matches the filters, and Clear filters brings them back", async () => {
    answer = () => Response.json(body([], { statusCounts: CHIP_COUNTS, unfilteredTotal: 30 }));
    mount({ status: "paused" });
    await screen.findByText("No containers match these filters.");
    // The chips keep the totals of the unfiltered list.
    expect(screen.getByRole("button", { name: "Running 24" })).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Clear filters" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    expect(changes).toEqual([{ q: undefined, status: undefined, node: undefined }]);
  });

  it("says no container matches the search, and Clear search clears it", async () => {
    answer = () => Response.json(body([], { unfilteredTotal: 30 }));
    mount({ q: "nope" });
    await screen.findByText("No containers match \u201Cnope\u201D.");
    const buttons = screen.getAllByRole("button", { name: "Clear search" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    expect(changes).toEqual([{ q: undefined }]);
  });

  it("calls a search with a status chip a filter match, since either may be the cause", async () => {
    answer = () => Response.json(body([], { unfilteredTotal: 30 }));
    mount({ q: "nope", status: "running" });
    await screen.findByText("No containers match these filters.");
  });

  it("shows an inline error with Retry", async () => {
    answer = () => Response.json({ title: "boom" }, { status: 500 });
    mount();
    await screen.findByText("Couldn't load containers.");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});

describe("container list: caption", () => {
  it("counts the containers, plural-aware", async () => {
    answer = () => Response.json(body([row("a")]));
    mount();
    expect(await screen.findByText("1 container")).toBeTruthy();
  });

  it("says n of total while a search or a status is set", async () => {
    answer = () => Response.json(body(ROWS.slice(0, 2), { unfilteredTotal: 4 }));
    mount({ q: "shop" });
    expect(await screen.findByText("2 of 4 containers")).toBeTruthy();
  });

  it("says how many it left out when the server capped the list", async () => {
    answer = () => Response.json(body(ROWS, { total: 612, unfilteredTotal: 612, truncated: true }));
    mount();
    await screen.findByText("shop-web");
    expect(
      screen.getByText("Showing the first 500 of 612. Narrow the search to see the rest."),
    ).toBeTruthy();
    expect(screen.getByText("612 containers")).toBeTruthy();
  });

  it("says across the nodes on a swarm, and the banner for a node that did not report", async () => {
    envType = "swarm";
    answer = () => Response.json(body(ROWS, { nodes: ["n1", "n2", "n3"], notReporting: ["n4"] }));
    mount();
    expect(await screen.findByText("4 containers across 3 nodes")).toBeTruthy();
    expect(
      screen.getByText("1 node isn't reporting: n4. The totals below leave it out."),
    ).toBeTruthy();
  });

  it("says n of total across the nodes when a swarm list is filtered", async () => {
    envType = "swarm";
    answer = () =>
      Response.json(body(ROWS.slice(0, 2), { unfilteredTotal: 4, nodes: ["n1", "n2", "n3"] }));
    mount({ q: "shop" });
    expect(await screen.findByText("2 of 4 containers across 3 nodes")).toBeTruthy();
  });
});

describe("container list: columns and cells", () => {
  it("shows Name, State, Image, Ports, Created and the actions column", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "",
      "Name",
      "State",
      "Image",
      "Ports",
      "Created",
      "Actions",
    ]);
  });

  it("keeps the Node column on a swarm", async () => {
    envType = "swarm";
    mount({ group: "none" });
    await screen.findByText("shop-web");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "",
      "Name",
      "Node",
      "State",
      "Image",
      "Ports",
      "Created",
      "Actions",
    ]);
  });

  it("pairs the state dot with its text, and adds an Unhealthy dot and label for a failing check", async () => {
    answer = () =>
      Response.json(
        body([
          row("ok"),
          row("sick", { health: "unhealthy" }),
          row("stopped", { state: "exited" }),
          row("booting", { health: "starting" }),
        ]),
      );
    mount({ group: "none" });
    await screen.findByText("sick");
    const cellOf = (name: string) => {
      const tr = screen.getByText(name).closest("tr") as HTMLElement;
      return within(tr).getAllByRole("cell")[2]?.textContent;
    };
    expect(cellOf("ok")).toBe("Running");
    expect(cellOf("sick")).toBe("RunningUnhealthy");
    expect(cellOf("stopped")).toBe("Exited");
    expect(cellOf("booting")).toBe("Running");
  });

  it("badges Coaming's own server and agent containers and no other", async () => {
    answer = () =>
      Response.json(
        body([
          row("coaming", { isSelf: true, selfKind: "server" }),
          row("coaming-agent", { selfKind: "agent" }),
          row("web"),
        ]),
      );
    mount({ group: "none" });
    await screen.findByText("web");
    const nameCell = (name: string) =>
      (screen.getByText(name).closest("td") as HTMLElement).textContent;
    expect(nameCell("coaming")).toBe("coamingCoaming (this instance)");
    expect(nameCell("coaming-agent")).toBe("coaming-agentCoaming agent");
    expect(nameCell("web")).toBe("web");
  });

  it("shows the first two ports and +n for the rest, and a dash for none", async () => {
    answer = () =>
      Response.json(
        body([
          row("many", { ports: ["80/tcp", "443/tcp", "8080/tcp", "9090/tcp"] }),
          row("two", { ports: ["80/tcp", "443/tcp"] }),
          row("none", { ports: null }),
        ]),
      );
    mount({ group: "none" });
    await screen.findByText("many");
    const portsOf = (name: string) =>
      within(screen.getByText(name).closest("tr") as HTMLElement).getAllByRole("cell")[4];
    expect(portsOf("many")?.textContent).toBe("80/tcp, 443/tcp+2");
    expect(within(portsOf("many") as HTMLElement).getByText("+2")).toBeTruthy();
    expect(portsOf("two")?.textContent).toBe("80/tcp, 443/tcp");
    expect(portsOf("none")?.textContent).toBe("\u2014");
  });

  it("renders a dash for a missing image and created time", async () => {
    answer = () =>
      Response.json(body([row("bare", { image: "", created: undefined as unknown as string })]));
    mount({ group: "none" });
    await screen.findByText("bare");
    const cells = within(screen.getByText("bare").closest("tr") as HTMLElement).getAllByRole(
      "cell",
    );
    expect(cells[3]?.textContent).toBe("\u2014");
    expect(cells[5]?.textContent).toBe("\u2014");
  });

  it("gives the Name column a 200px minimum and keeps long names to one line", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    expect(screen.getByRole("columnheader", { name: "Name" }).className).toContain("min-w-[200px]");
    expect(screen.getByText("shop-web").className).toContain("truncate");
  });
});

// A new answer arriving through a refetch, as the events feed or the 30 s poll
// would bring it.
async function refetch() {
  await act(async () => {
    await client.invalidateQueries({ queryKey: listKey(ENV, "containers") });
  });
}

// A row of the list itself, even while a dialog is open with the same name in
// its target list.
const rowOf = (name: string) =>
  screen
    .getAllByText(name)
    .map((el) => el.closest("tr"))
    .find((tr) => tr?.closest("table")?.getAttribute("aria-label") === "Containers") as HTMLElement;

async function openRowMenu(name: string) {
  await waitFor(() =>
    expect(screen.getByRole("button", { name: `Actions for ${name}` })).toBeTruthy(),
  );
  await act(async () => {});
  await act(async () => {
    fireEvent.keyDown(screen.getByRole("button", { name: `Actions for ${name}` }), {
      key: "Enter",
    });
  });
  await screen.findByRole("menu");
}

describe("container list: live updates in place", () => {
  it("updates a row whose state changed in place and marks it for a moment", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    expect(rowOf("shop-web").hasAttribute("data-changed")).toBe(false);
    answer = () =>
      Response.json(body(ROWS.map((r) => (r.name === "shop-web" ? { ...r, state: "paused" } : r))));
    await refetch();
    await waitFor(() => expect(within(rowOf("shop-web")).getByText("Paused")).toBeTruthy());
    expect(rowOf("shop-web").hasAttribute("data-changed")).toBe(true);
    expect(rowOf("blog").hasAttribute("data-changed")).toBe(false);
    await waitFor(() => expect(rowOf("shop-web").hasAttribute("data-changed")).toBe(false));
  });

  it("slides a new row in without a reload", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    answer = () => Response.json(body([...ROWS, row("zeta")]));
    // The mark lasts 300 ms of Date.now(); a loaded host can spend that between the render and
    // the assertion, so the clock stands still until the mark has been seen.
    const clock = vi.spyOn(Date, "now").mockReturnValue(Date.now());
    try {
      await refetch();
      await screen.findByText("zeta");
      expect(rowOf("zeta").hasAttribute("data-added")).toBe(true);
      expect(rowOf("zeta").className).toContain("motion-safe:animate-in");
    } finally {
      clock.mockRestore();
    }
  });

  it("fades a removed row out and then drops it", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    answer = () => Response.json(body(ROWS.filter((r) => r.name !== "loose")));
    await refetch();
    await waitFor(() => expect(screen.queryByText("loose")).toBeNull());
  });

  it("holds everything still while a row menu is open, and says so", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    await openRowMenu("blog");
    answer = () =>
      Response.json(
        body([
          row("zeta"),
          ...ROWS.filter((r) => r.name !== "loose").map((r) =>
            r.name === "shop-web" ? { ...r, state: "paused" as const } : r,
          ),
        ]),
      );
    await refetch();
    // The changed row updates in place; nothing else moves.
    await waitFor(() => expect(within(rowOf("shop-web")).getByText("Paused")).toBeTruthy());
    expect(screen.queryByText("zeta")).toBeNull();
    expect(screen.getByText("loose")).toBeTruthy();
    // An open menu hides the page from assistive technology, so the roles are
    // looked up with hidden on.
    const names = screen
      .getAllByRole("row", { hidden: true })
      .slice(1)
      .map((r) => r.querySelectorAll("td")[1]?.textContent);
    expect(names).toEqual(["blog", "loose", "shop-db", "shop-web"]);
    // The gone row stays, dimmed, as Removed, with its menu disabled.
    expect(within(rowOf("loose")).getByText("Removed")).toBeTruthy();
    expect(rowOf("loose").hasAttribute("data-removed")).toBe(true);
    expect(
      (
        within(rowOf("loose")).getByRole("button", {
          name: "Actions for loose",
          hidden: true,
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    expect(screen.getByText("2 changes waiting")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Show changes", hidden: true })).toBeTruthy();
  });

  it("applies the held changes at once with Show changes", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    await openRowMenu("blog");
    answer = () => Response.json(body([...ROWS, row("zeta")]));
    await refetch();
    expect(await screen.findByText("1 change waiting")).toBeTruthy();
    expect(screen.queryByText("zeta")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show changes", hidden: true }));
    await screen.findByText("zeta");
    expect(screen.queryByText(/change waiting|changes waiting/)).toBeNull();
  });

  it("applies the held changes by itself when the menu closes", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    await openRowMenu("blog");
    answer = () => Response.json(body([...ROWS, row("zeta")]));
    await refetch();
    await screen.findByText("1 change waiting");
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    await screen.findByText("zeta");
    expect(screen.queryByText("1 change waiting")).toBeNull();
  });

  it("does not flag the rows of a new search as added or removed", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    answer = (url) =>
      Response.json(body(ROWS.filter((r) => r.name.includes(url.searchParams.get("q") ?? ""))));
    fireEvent.change(screen.getByRole("searchbox", { name: "Search containers" }), {
      target: { value: "shop" },
    });
    await waitFor(() => expect(screen.queryByText("loose")).toBeNull());
    for (const name of ["shop-web", "shop-db"]) {
      expect(rowOf(name).hasAttribute("data-added")).toBe(false);
      expect(rowOf(name).hasAttribute("data-removed")).toBe(false);
    }
  });
});

const box = (name: string) => screen.getByRole("checkbox", { name: `Select ${name}` });
const isOn = (el: HTMLElement) => el.getAttribute("aria-checked");
const selectedCount = () => screen.queryByRole("toolbar", { name: "Bulk actions" });

// The rows of ROWS in the order group none shows them.
async function mountFlat(initial: ContainerSearch = { group: "none" }) {
  mount(initial);
  await screen.findByText("shop-web");
  await waitFor(() => expect(screen.getAllByRole("checkbox").length).toBeGreaterThan(0));
}

describe("container list: selection", () => {
  it("gives every row a named checkbox and the header one to select every container", async () => {
    await mountFlat();
    for (const name of ["shop-web", "shop-db", "blog", "loose"]) expect(box(name)).toBeTruthy();
    expect(screen.getByRole("checkbox", { name: "Select all containers" })).toBeTruthy();
    expect(selectedCount()).toBeNull();
  });

  it("shows the bulk bar in place of the caption with '{n} selected', and Clear selection empties it", async () => {
    await mountFlat();
    fireEvent.click(box("blog"));
    fireEvent.click(box("loose"));
    const bar = screen.getByRole("toolbar", { name: "Bulk actions" });
    expect(within(bar).getByText("2 selected")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Start" })).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Stop…" })).toBeTruthy();
    expect(screen.queryByText("4 containers")).toBeNull();
    fireEvent.click(within(bar).getByRole("button", { name: "Clear selection" }));
    expect(selectedCount()).toBeNull();
    expect(isOn(box("blog"))).toBe("false");
  });

  it("clears the selection on Escape from inside the table", async () => {
    await mountFlat();
    fireEvent.click(box("blog"));
    expect(selectedCount()).toBeTruthy();
    fireEvent.keyDown(box("blog"), { key: "Escape" });
    expect(selectedCount()).toBeNull();
  });

  it("selects the range between two rows on Shift+click, in the order shown", async () => {
    await mountFlat();
    fireEvent.click(box("blog"));
    fireEvent.click(box("shop-db"), { shiftKey: true });
    // blog, loose, shop-db, shop-web
    expect(["blog", "loose", "shop-db", "shop-web"].map((n) => isOn(box(n)))).toEqual([
      "true",
      "true",
      "true",
      "false",
    ]);
  });

  it("selects every eligible visible row from the header and clears them from it", async () => {
    await mountFlat();
    const all = screen.getByRole("checkbox", { name: "Select all containers" });
    fireEvent.click(all);
    expect(within(screen.getByRole("toolbar")).getByText("4 selected")).toBeTruthy();
    expect(isOn(all)).toBe("true");
    fireEvent.click(all);
    expect(selectedCount()).toBeNull();
  });

  it("shows the header checkbox mixed when only some rows are selected", async () => {
    await mountFlat();
    fireEvent.click(box("blog"));
    expect(isOn(screen.getByRole("checkbox", { name: "Select all containers" }))).toBe("mixed");
  });

  it("has a tri-state checkbox on each group that selects the group's rows", async () => {
    mount();
    await screen.findByText("shop-web");
    await waitFor(() => expect(screen.getAllByRole("checkbox").length).toBeGreaterThan(0));
    const group = screen.getByRole("checkbox", { name: "Select shop" });
    expect(isOn(group)).toBe("false");
    fireEvent.click(box("shop-web"));
    expect(isOn(group)).toBe("mixed");
    fireEvent.click(group);
    expect(isOn(group)).toBe("true");
    expect(isOn(box("shop-db"))).toBe("true");
    expect(isOn(box("blog"))).toBe("false");
    fireEvent.click(group);
    expect(selectedCount()).toBeNull();
  });

  it("gives Coaming's own containers a checkbox that cannot be ticked, and keeps them out of select all", async () => {
    answer = () =>
      Response.json(body([row("coaming", { selfKind: "server", isSelf: true }), ...ROWS]));
    await mountFlat();
    const own = box("coaming");
    expect(own.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(own);
    expect(isOn(own)).toBe("false");
    expect(selectedCount()).toBeNull();
    fireEvent.click(screen.getByRole("checkbox", { name: "Select all containers" }));
    expect(within(screen.getByRole("toolbar")).getByText("4 selected")).toBeTruthy();
    expect(isOn(own)).toBe("false");
  });

  it("holds the list still while rows are selected, then applies the held changes", async () => {
    await mountFlat();
    fireEvent.click(box("blog"));
    answer = () => Response.json(body([...ROWS, row("zeta")]));
    await refetch();
    expect(await screen.findByText("1 change waiting")).toBeTruthy();
    expect(screen.queryByText("zeta")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
    await screen.findByText("zeta");
  });
});

describe("container list: stopping from the bulk bar", () => {
  async function chooseTwoAndStop() {
    await mountFlat();
    fireEvent.click(box("loose"));
    fireEvent.click(box("shop-web"));
    fireEvent.click(screen.getByRole("button", { name: "Stop…" }));
    return screen.findByRole("alertdialog", { name: "Stop 2 containers?" });
  }

  it("opens one dialog naming both containers and sends nothing until confirmed", async () => {
    const dialog = await chooseTwoAndStop();
    expect(
      within(dialog)
        .getAllByRole("row")
        .slice(1)
        .map((r) => r.textContent),
    ).toEqual(["loose", "shop-webPart of project shop"]);
    expect(actionPosts).toEqual([]);
  });

  it("posts one request with both targets and toasts 'Stopped 2 containers.'", async () => {
    const dialog = await chooseTwoAndStop();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    await waitFor(() => expect(actionPosts).toHaveLength(1));
    expect(actionPosts[0]?.path).toBe(`/api/env/${ENV}/containers/actions`);
    expect(actionPosts[0]?.body).toEqual({
      action: "stop",
      targets: [
        { nodeId: "n1", id: "id-loose" },
        { nodeId: "n1", id: "id-shop-web" },
      ],
    });
    expect(await screen.findByText("Stopped 2 containers.")).toBeTruthy();
    // The selection is spent.
    await waitFor(() => expect(selectedCount()).toBeNull());
  });

  it("shows both rows as Stopping… with a pulsing dot, disabled checkbox and menu, until the state changes", async () => {
    let release: (r: Response) => void = () => {};
    actionAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    const dialog = await chooseTwoAndStop();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    await waitFor(() => expect(within(rowOf("loose")).getByText("Stopping…")).toBeTruthy());
    expect(within(rowOf("shop-web")).getByText("Stopping…")).toBeTruthy();
    expect(within(rowOf("blog")).queryByText("Stopping…")).toBeNull();
    expect(rowOf("loose").querySelector("[data-pending]")).toBeTruthy();
    expect(
      within(rowOf("loose"))
        .getByRole("checkbox", { name: "Select loose", hidden: true })
        .hasAttribute("disabled"),
    ).toBe(true);
    expect(
      (
        within(rowOf("loose")).getByRole("button", {
          name: "Actions for loose",
          hidden: true,
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    // Politely announced once.
    expect(
      screen
        .getAllByRole("status", { hidden: true })
        .some((el) => el.textContent === "Stopping loose, shop-web"),
    ).toBe(true);

    await act(async () =>
      release(
        allDone({ path: "", body: { targets: [{ id: "id-loose" }, { id: "id-shop-web" }] } }),
      ),
    );
    // Still waiting: nothing has confirmed the new state yet.
    expect(within(rowOf("loose")).getByText("Stopping…")).toBeTruthy();
    answer = () =>
      Response.json(
        body(
          ROWS.map((r) =>
            ["loose", "shop-web"].includes(r.name)
              ? { ...r, state: "exited" as const, status: "Exited (0) now" }
              : r,
          ),
        ),
      );
    await refetch();
    await waitFor(() => expect(within(rowOf("loose")).queryByText("Stopping…")).toBeNull());
    expect(within(rowOf("loose")).getByText("Exited")).toBeTruthy();
    expect(within(rowOf("shop-web")).getByText("Exited")).toBeTruthy();
  });

  it("reads the list again at once and once more 2000 ms after the answer", async () => {
    const dialog = await chooseTwoAndStop();
    const timers = vi.spyOn(globalThis, "setTimeout");
    const invalidate = vi.spyOn(client, "invalidateQueries");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    await waitFor(() => expect(invalidate).toHaveBeenCalled());
    expect(timers.mock.calls.some((c) => c[1] === 2000)).toBe(true);
  });

  it("takes the label off a container that failed at once and leaves the other stopping", async () => {
    actionAnswer = (req) =>
      Response.json({
        results: req.body.targets.map((t, i) => ({
          nodeId: "n1",
          id: t.id,
          name: t.id,
          result: i === 0 ? "failed" : "done",
          code: i === 0 ? "node_unreachable" : undefined,
        })),
      });
    const dialog = await chooseTwoAndStop();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    expect(await screen.findByText("Stopped 1, 1 failed")).toBeTruthy();
    expect(within(rowOf("loose")).queryByText("Stopping…")).toBeNull();
    expect(within(rowOf("shop-web")).getByText("Stopping…")).toBeTruthy();
  });

  it("runs Start at once on the stopped rows only and counts the rest as skipped", async () => {
    await mountFlat();
    fireEvent.click(box("shop-web"));
    fireEvent.click(box("shop-db"));
    fireEvent.click(screen.getByRole("button", { name: "Start" }));
    await waitFor(() => expect(actionPosts).toHaveLength(1));
    expect(actionPosts[0]?.body).toEqual({
      action: "start",
      targets: [{ nodeId: "n1", id: "id-shop-db" }],
    });
    expect(await screen.findByText("Started 1 container. 1 skipped")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("disables Start with the reason when no chosen container is stopped", async () => {
    await mountFlat();
    fireEvent.click(box("shop-web"));
    const start = screen.getByRole("button", { name: "Start" });
    expect(start.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(start);
    expect(actionPosts).toEqual([]);
    expect(
      screen.getAllByText("None of the selected containers can be started.").length,
    ).toBeGreaterThan(0);
  });
});

describe("container list: every bulk action", () => {
  it("offers Start, Stop…, Restart, Pause, Unpause, Kill… and Remove… and Clear selection", async () => {
    await mountFlat();
    fireEvent.click(box("loose"));
    const bar = screen.getByRole("toolbar", { name: "Bulk actions" });
    expect(
      within(bar)
        .getAllByRole("button")
        .map((b) => b.textContent)
        .filter((n) => n !== "More"),
    ).toEqual([
      "Start",
      "Stop…",
      "Restart",
      "Pause",
      "Unpause",
      "Kill…",
      "Remove…",
      "Clear selection",
    ]);
  });

  it("says why Pause cannot be used when nothing selected is running", async () => {
    await mountFlat();
    fireEvent.click(box("shop-db"));
    const pause = screen.getByRole("button", { name: "Pause" });
    expect(pause.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(pause);
    expect(actionPosts).toEqual([]);
    expect(
      screen.getAllByText("None of the selected containers can be paused.").length,
    ).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: "Unpause" }).getAttribute("aria-disabled")).toBe(
      "true",
    );
  });

  it("restarts the chosen containers at once with no dialog", async () => {
    await mountFlat();
    fireEvent.click(box("loose"));
    fireEvent.click(box("shop-web"));
    fireEvent.click(screen.getByRole("button", { name: "Restart" }));
    await waitFor(() => expect(actionPosts).toHaveLength(1));
    expect(actionPosts[0]?.body.action).toBe("restart");
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(await screen.findByText("Restarted 2 containers.")).toBeTruthy();
  });

  it("lists a stopped container as skipped in the Kill dialog, kills the others and counts it in the toast", async () => {
    await mountFlat();
    fireEvent.click(box("loose"));
    fireEvent.click(box("shop-db"));
    fireEvent.click(box("shop-web"));
    fireEvent.click(screen.getByRole("button", { name: "Kill…" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Kill 2 containers?" });
    const rows = within(dialog).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      "loose",
      "shop-dbNot running, skipped",
      "shop-webPart of project shop",
    ]);
    const confirm = within(dialog).getByRole("button", { name: "Kill 2 containers" });
    expect(confirm.className).toContain("bg-destructive-fill");
    fireEvent.click(confirm);
    await waitFor(() => expect(actionPosts).toHaveLength(1));
    expect(actionPosts[0]?.body).toEqual({
      action: "kill",
      targets: [
        { nodeId: "n1", id: "id-loose" },
        { nodeId: "n1", id: "id-shop-web" },
      ],
    });
    expect(await screen.findByText("Killed 2 containers. 1 skipped")).toBeTruthy();
  });

  it("removes the chosen containers once Force is ticked, from the remove route", async () => {
    await mountFlat();
    fireEvent.click(box("loose"));
    fireEvent.click(box("shop-db"));
    fireEvent.click(screen.getByRole("button", { name: "Remove…" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Remove 2 containers?" });
    const confirm = within(dialog).getByRole("button", { name: "Remove 2 containers" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Force (stop if running)" }));
    fireEvent.click(confirm);
    await waitFor(() => expect(actionPosts).toHaveLength(1));
    expect(actionPosts[0]?.path).toBe(`/api/env/${ENV}/containers/remove`);
    expect(actionPosts[0]?.body).toMatchObject({ force: true, removeVolumes: false });
    expect(await screen.findByText("Removed 2 containers.")).toBeTruthy();
  });

  it("opens the results from a partial toast, with the agent update notice for an agent that refused", async () => {
    actionAnswer = (req) =>
      Response.json({
        results: req.body.targets.map((t, i) => ({
          nodeId: "n1",
          id: t.id,
          name: t.id.replace("id-", ""),
          result: i === 0 ? "done" : "failed",
          code: i === 0 ? undefined : "agent_policy_denied",
        })),
      });
    await mountFlat();
    fireEvent.click(box("loose"));
    fireEvent.click(box("shop-web"));
    fireEvent.click(screen.getByRole("button", { name: "Stop…" }));
    fireEvent.click(await screen.findByRole("button", { name: "Stop 2 containers" }));
    expect(await screen.findByText("Stopped 1, 1 failed")).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Details" }));
    const results = await screen.findByRole("dialog", { name: "Stop results" });
    expect(within(results).getByRole("alert").textContent).toContain(
      "This host's agent needs an update",
    );
    expect(
      within(results)
        .getAllByRole("row")
        .slice(1)
        .map((r) => (r as HTMLTableRowElement).cells[0]?.textContent),
    ).toEqual(["loose", "shop-web"]);
  });
});

describe("container list: Coaming's own container", () => {
  it("opens the own-container dialog from the row menu, where the name must be typed", async () => {
    answer = () =>
      Response.json(body([row("coaming", { selfKind: "server", isSelf: true }), ...ROWS]));
    mount({ group: "none" });
    await screen.findByText("coaming");
    await openRowMenu("coaming");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Stop coaming?" });
    expect(dialog.textContent).toContain("This is Coaming's own container.");
    const confirm = within(dialog).getByRole("button", { name: "Stop Coaming" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(actionPosts).toEqual([]);
  });
});

describe("container list: Live stats (06-33)", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  const liveSource = (): FakeEventSource => {
    const s = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    if (!s) throw new Error("no stats stream was opened");
    return s;
  };
  const idsOf = (s: FakeEventSource) => new URL(s.url, "http://x").searchParams.get("ids");

  it("has the switch, off by default, with no stats columns and no stream", async () => {
    mount();
    await screen.findByText("shop-web");
    expect(screen.getByRole("switch", { name: "Live stats" }).getAttribute("aria-checked")).toBe(
      "false",
    );
    for (const name of ["CPU", "Memory", "Network"]) {
      expect(screen.queryByRole("columnheader", { name })).toBeNull();
    }
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("asks for stats through the search when the switch is turned on, and takes it out when off", async () => {
    mount();
    await screen.findByText("shop-web");
    fireEvent.click(screen.getByRole("switch", { name: "Live stats" }));
    expect(changes).toEqual([{ stats: 1 }]);
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Live stats" }).getAttribute("aria-checked")).toBe(
        "true",
      ),
    );
    fireEvent.click(screen.getByRole("switch", { name: "Live stats" }));
    expect(changes.at(-1)).toEqual({ stats: undefined });
  });

  it("adds CPU, Memory and Network after Created, and one stream for the running rows on screen", async () => {
    mount({ stats: 1 });
    await screen.findByText("shop-web");
    const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads.slice(heads.indexOf("Created"))).toEqual(
      expect.arrayContaining(["Created", "CPU", "Memory", "Network"]),
    );
    expect(FakeEventSource.instances).toHaveLength(1);
    const url = new URL(liveSource().url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/containers/stats/stream");
    // The node and the container, as the row is named; the exited container is not sampled.
    expect((idsOf(liveSource()) ?? "").split(",").sort()).toEqual([
      "n1|id-blog",
      "n1|id-loose",
      "n1|id-shop-web",
    ]);
  });

  it("fills a row's cells from the stream and leaves a stopped container's as dashes", async () => {
    mount({ stats: 1 });
    await screen.findByText("shop-web");
    act(() =>
      liveSource().sendFrame("sample", {
        t: 1,
        containers: [
          {
            id: "n1|id-shop-web",
            cpu: 142.4,
            mem: 1.2 * 1024 ** 3,
            netRx: 1.2 * 1024 ** 2,
            netTx: 340 * 1024,
            noData: false,
            stopped: false,
          },
        ],
      }),
    );
    const web = screen.getByRole("row", { name: /shop-web/ });
    expect(within(web).getByText("142%")).toBeTruthy();
    expect(within(web).getByText("1.2 GiB")).toBeTruthy();
    expect(within(web).getByText("↓ 1.2 MiB/s ↑ 340 KiB/s")).toBeTruthy();
    const db = screen.getByRole("row", { name: /shop-db/ });
    expect(within(db).getAllByText("—").length).toBeGreaterThanOrEqual(3);
  });

  it("closes the stream and takes the columns away when the switch is turned off", async () => {
    mount({ stats: 1 });
    await screen.findByText("shop-web");
    const s = liveSource();
    fireEvent.click(screen.getByRole("switch", { name: "Live stats" }));
    await waitFor(() => expect(s.closed).toBe(true));
    expect(screen.queryByRole("columnheader", { name: "CPU" })).toBeNull();
  });
});

// Restart loops of standalone containers (plan 06-36, RES-13, D-15): the badge sits in the Name
// cell, beside the self badge, and only on a container the server flagged.
describe("container list: restart loops", () => {
  const LOOPING = [
    row("crashy", {
      restartLoop: { state: "flagged", count: 3, windowMinutes: 10, lastExitCode: "3" },
    }),
    row("hushed", {
      restartLoop: { state: "muted", count: 4, windowMinutes: 10, lastExitCode: "1" },
    }),
    row("calm"),
  ];

  it("flags the container the server flagged, and no other", async () => {
    answer = () => Response.json(body(LOOPING));
    mount();
    const crashy = (await screen.findByRole("row", { name: /crashy/ })) as HTMLElement;
    expect(within(crashy).getByText("Restart loop")).toBeTruthy();
    expect(
      within(screen.getByRole("row", { name: /calm/ })).queryByText("Restart loop"),
    ).toBeNull();
  });

  it("says the restarts and the last exit code in the badge's tooltip", async () => {
    answer = () => Response.json(body(LOOPING));
    mount();
    const crashy = (await screen.findByRole("row", { name: /crashy/ })) as HTMLElement;
    fireEvent.focus(within(crashy).getByText("Restart loop"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe("3 restarts in the last 10 minutes. Last exit code 3.");
  });

  it("shows a muted loop as the grey Muted badge, not as Restart loop", async () => {
    answer = () => Response.json(body(LOOPING));
    mount();
    const hushed = (await screen.findByRole("row", { name: /hushed/ })) as HTMLElement;
    expect(within(hushed).getByText("Muted")).toBeTruthy();
    expect(within(hushed).queryByText("Restart loop")).toBeNull();
  });
});
