// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type ContainerRow, listKey } from "@/api/resources";
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
    await screen.findByText("Couldn't load containers.", {}, { timeout: 4000 });
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
      return within(tr).getAllByRole("cell")[1]?.textContent;
    };
    expect(cellOf("ok")).toBe("Running");
    expect(cellOf("sick")).toBe("RunningUnhealthy");
    expect(cellOf("stopped")).toBe("Exited");
    expect(cellOf("booting")).toBe("Running");
  });

  it("badges Bayplan's own server and agent containers and no other", async () => {
    answer = () =>
      Response.json(
        body([
          row("bayplan", { isSelf: true, selfKind: "server" }),
          row("bayplan-agent", { selfKind: "agent" }),
          row("web"),
        ]),
      );
    mount({ group: "none" });
    await screen.findByText("web");
    const nameCell = (name: string) =>
      (screen.getByText(name).closest("td") as HTMLElement).textContent;
    expect(nameCell("bayplan")).toBe("bayplanBayplan (this instance)");
    expect(nameCell("bayplan-agent")).toBe("bayplan-agentBayplan agent");
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
      within(screen.getByText(name).closest("tr") as HTMLElement).getAllByRole("cell")[3];
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
    expect(cells[2]?.textContent).toBe("\u2014");
    expect(cells[4]?.textContent).toBe("\u2014");
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

const rowOf = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;

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
    await waitFor(() => expect(rowOf("shop-web").hasAttribute("data-changed")).toBe(false), {
      timeout: 3000,
    });
  });

  it("slides a new row in without a reload", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    answer = () => Response.json(body([...ROWS, row("zeta")]));
    await refetch();
    await screen.findByText("zeta");
    expect(rowOf("zeta").hasAttribute("data-added")).toBe(true);
    expect(rowOf("zeta").className).toContain("motion-safe:animate-in");
  });

  it("fades a removed row out and then drops it", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    answer = () => Response.json(body(ROWS.filter((r) => r.name !== "loose")));
    await refetch();
    await waitFor(() => expect(screen.queryByText("loose")).toBeNull(), { timeout: 2000 });
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
    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector("td")?.textContent);
    expect(names).toEqual(["blog", "loose", "shop-db", "shop-web"]);
    // The gone row stays, dimmed, as Removed, with its menu disabled.
    expect(within(rowOf("loose")).getByText("Removed")).toBeTruthy();
    expect(rowOf("loose").hasAttribute("data-removed")).toBe(true);
    expect(
      (
        within(rowOf("loose")).getByRole("button", {
          name: "Actions for loose",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    expect(screen.getByText("2 changes waiting")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Show changes" })).toBeTruthy();
  });

  it("applies the held changes at once with Show changes", async () => {
    mount({ group: "none" });
    await screen.findByText("shop-web");
    await openRowMenu("blog");
    answer = () => Response.json(body([...ROWS, row("zeta")]));
    await refetch();
    expect(await screen.findByText("1 change waiting")).toBeTruthy();
    expect(screen.queryByText("zeta")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show changes" }));
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
    await waitFor(() => expect(screen.queryByText("loose")).toBeNull(), { timeout: 2000 });
    for (const name of ["shop-web", "shop-db"]) {
      expect(rowOf(name).hasAttribute("data-added")).toBe(false);
      expect(rowOf(name).hasAttribute("data-removed")).toBe(false);
    }
  });
});
