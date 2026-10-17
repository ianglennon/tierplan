// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  ImageList,
  type ImageSearch,
  type ImageSearchPatch,
  imageSearchSchema,
} from "@/features/images/image-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The images list: merged and per-node views, the Dangling chip, the search, the Used by and
// Nodes cells, the captions and the four empty states (plan 05-15, D-16, D-17, RES-09).

const ENV = "e1";
const ID = (c: string) => `sha256:${c.repeat(64)}`;

interface Holder {
  nodeId: string;
  name: string;
  containers: number;
}

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let swarmEnv: boolean;
let urls: string[];
let search: ImageSearch;
let changes: ImageSearchPatch[];
let body: Record<string, unknown>;
let listAnswer: () => Response | Promise<Response>;
let readOnlyNodes: boolean;
let posts: { bulkId?: string; targets: { nodeId: string; imageId: string }[] }[];
let removeAnswer: (call: number) => Response;

const holder = (nodeId: string, containers = 0): Holder => ({
  nodeId,
  name: nodeId.replace("-id", ""),
  containers,
});

const image = (id: string, extra: Record<string, unknown> = {}) => ({
  node: "node-1",
  nodeId: "node-1-id",
  id: ID(id),
  shortId: id.repeat(12),
  repoTags: [`app-${id}:1`],
  dangling: false,
  size: 5 * 1024 * 1024,
  created: "2026-10-01T10:00:00Z",
  usedByContainers: 0,
  usedByServices: 0,
  readOnly: false,
  nodes: [holder("node-1-id")],
  ...extra,
});

const list = (items: unknown[], extra: Record<string, unknown> = {}) => ({
  items,
  total: items.length,
  truncated: false,
  nodes: ["node-1"],
  nodeRefs: [{ nodeId: "node-1-id", name: "node-1", containers: 0 }],
  nodeCount: 1,
  notReporting: [],
  danglingCount: 0,
  unfilteredTotal: items.length,
  ...extra,
});

const NODES = ["node-1", "node-2", "node-3"];
const refs = NODES.map((n) => ({ nodeId: `${n}-id`, name: n, containers: 0 }));

beforeEach(() => {
  me = makeMe({ envs: { [ENV]: "admin" } });
  swarmEnv = false;
  readOnlyNodes = false;
  posts = [];
  removeAnswer = () => Response.json({ results: [] });
  urls = [];
  search = {};
  changes = [];
  body = list([
    image("a", { repoTags: ["nginx:1.27", "nginx:latest", "nginx:stable"], usedByContainers: 2 }),
    image("b", { repoTags: ["<none>"], dangling: true, size: 0, created: undefined }),
    image("c"),
  ]);
  listAnswer = () => Response.json(body);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      urls.push(url.pathname + url.search);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/images/remove`) {
        const sentBody = await req.clone().json();
        posts.push(sentBody);
        const answer = removeAnswer(posts.length);
        if (answer.status !== 200) return answer;
        return Response.json({
          results: sentBody.targets.map((t: object) => ({ ...t, result: "done" })),
        });
      }
      if (url.pathname === `/api/env/${ENV}/images`) return listAnswer();
      if (url.pathname === `/api/env/${ENV}/images/pull-targets`) {
        return Response.json({
          nodes: [{ nodeId: "node-1-id", name: "node-1", readOnly: readOnlyNodes }],
        });
      }
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: swarmEnv ? [{ id: ENV, name: "Swarm", envType: "swarm" }] : [],
        });
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

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ImageList envId={ENV} search={search} onSearchChange={(patch) => changes.push(patch)} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const row = async (text: string) => (await screen.findByText(text)).closest("tr") as HTMLElement;

function swarmBody(view: "merged" | "node" = "merged") {
  const a = image("a", {
    node: "",
    nodeId: "",
    repoTags: ["nginx:1.27"],
    usedByContainers: 3,
    usedByServices: 1,
    nodes: [holder("node-1-id", 2), holder("node-2-id", 1)],
  });
  const all = image("b", {
    node: "",
    nodeId: "",
    repoTags: ["redis:8"],
    nodes: NODES.map((n) => holder(`${n}-id`)),
  });
  const copies = NODES.slice(0, 2).map((n) =>
    image("a", {
      node: n,
      nodeId: `${n}-id`,
      repoTags: ["nginx:1.27"],
      nodes: [holder(`${n}-id`, 1)],
    }),
  );
  const items = view === "merged" ? [a, all] : copies;
  return list(items, {
    nodes: NODES,
    nodeRefs: refs,
    nodeCount: 3,
    unfilteredTotal: items.length,
  });
}

describe("ImageList columns", () => {
  it("shows Image, ID, Used by, Size, Created and the actions column on a standalone host, with no Nodes column", async () => {
    mount();
    await screen.findByText("nginx:1.27");
    const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads).toEqual(["", "Image", "Image ID", "Used by", "Size", "Created", "Actions"]);
  });

  it("shows the first tag with +n and the rest in a tooltip, and Untagged with a Dangling badge", async () => {
    mount();
    const a = await row("nginx:1.27");
    expect(within(a).getByText("+2")).toBeTruthy();
    const more = within(a).getByRole("button", { name: /nginx:latest, nginx:stable/ });
    fireEvent.focus(more);
    expect((await screen.findAllByText("nginx:latest")).length).toBeGreaterThan(0);
    const b = await row("Untagged");
    expect(within(b).getByText("Dangling")).toBeTruthy();
  });

  it("links the image to its page", async () => {
    mount();
    const link = within(await row("nginx:1.27")).getByRole("link", { name: "nginx:1.27" });
    expect(decodeURIComponent(link.getAttribute("href") ?? "")).toBe(
      `/env/${ENV}/images/${ID("a")}`,
    );
  });

  it("reads Used by as the containers using the image, or Not in use in muted", async () => {
    mount();
    expect(within(await row("nginx:1.27")).getByText("2 containers")).toBeTruthy();
    const none = within(await row("app-c:1")).getByText("Not in use");
    expect(none.className).toContain("text-muted-foreground");
  });

  it("adds the services on a swarm", async () => {
    swarmEnv = true;
    body = swarmBody();
    mount();
    expect(await screen.findByText("3 containers, 1 service")).toBeTruthy();
  });

  it("shows a dash for a missing size or created time", async () => {
    mount();
    const cells = within(await row("Untagged")).getAllByRole("cell");
    expect(cells.filter((c) => c.textContent === "—").length).toBeGreaterThanOrEqual(2);
  });

  it("lets a long reference be cut with a tooltip and keeps the Image column at least 240px", async () => {
    body = list([
      image("a", { repoTags: [`registry.example.com/${"very-long/".repeat(8)}app:1`] }),
    ]);
    mount();
    const long = await screen.findByText(/very-long/);
    expect(long.className).toContain("truncate");
    const head = screen.getAllByRole("columnheader")[1] as HTMLElement;
    expect(head.className).toContain("min-w-[240px]");
  });
});

describe("ImageList on a swarm", () => {
  beforeEach(() => {
    swarmEnv = true;
    body = swarmBody();
  });

  it("shows the Nodes column in the merged view as on n/m nodes or on all m nodes", async () => {
    mount();
    expect(await screen.findByRole("button", { name: "on 2/3 nodes" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "on all 3 nodes" })).toBeTruthy();
    const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads).toEqual([
      "",
      "Image",
      "Image ID",
      "Nodes",
      "Used by",
      "Size",
      "Created",
      "Actions",
    ]);
  });

  it("opens a popover with every node, its container count, and Missing where it has none", async () => {
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "on 2/3 nodes" }));
    expect(await screen.findByText("Nodes with this image")).toBeTruthy();
    const pop = document.querySelector('[data-slot="popover-content"]') as HTMLElement;
    // Every node of the environment is listed once: the two that have it, then the one that does not.
    expect(within(pop).getAllByRole("listitem")).toHaveLength(3);
    expect(within(pop).getByText("node-1").closest("li")?.textContent).toContain("2 containers");
    expect(within(pop).getByText("node-2").closest("li")?.textContent).toContain("1 container");
    expect(within(pop).getByText("node-3").closest("li")?.textContent).toContain("Missing");
  });

  it("opens the popover on hover too", async () => {
    mount();
    fireEvent.mouseEnter(await screen.findByRole("button", { name: "on 2/3 nodes" }));
    expect(await screen.findByText("Nodes with this image")).toBeTruthy();
  });

  it("names a node that is not reporting in the banner and as Missing", async () => {
    body = { ...swarmBody(), notReporting: ["node-4"], nodeCount: 4 };
    mount();
    expect(await screen.findByText(/node-4/)).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "on 2/4 nodes" }));
    const pop = document.querySelector('[data-slot="popover-content"]') as HTMLElement;
    expect(within(pop).getByText("node-4").closest("li")?.textContent).toContain("Missing");
  });

  it("writes view=node from Group by node, and asks the server for it", async () => {
    mount();
    const group = await screen.findByRole("group", { name: "Group by node" });
    expect(within(group).getByRole("radio", { name: "Merged" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    fireEvent.click(within(group).getByRole("radio", { name: "Per node" }));
    expect(changes).toEqual([{ view: "node" }]);
    cleanup();
    search = { view: "node" };
    body = swarmBody("node");
    urls = [];
    mount();
    await screen.findAllByText("nginx:1.27");
    expect(urls.some((u) => u.includes("view=node"))).toBe(true);
  });

  it("shows a Node column in the per-node view, one row per node copy", async () => {
    search = { view: "node" };
    body = swarmBody("node");
    mount();
    await screen.findAllByText("nginx:1.27");
    const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads).toEqual([
      "",
      "Image",
      "Image ID",
      "Node",
      "Used by",
      "Size",
      "Created",
      "Actions",
    ]);
    expect(screen.getAllByRole("row")).toHaveLength(3);
    expect(screen.getByText("node-2")).toBeTruthy();
  });

  it("filters by node and sends it to the server", async () => {
    search = { node: "node-2" };
    mount();
    await screen.findAllByText("nginx:1.27");
    expect(urls.some((u) => u.includes("node=node-2"))).toBe(true);
  });

  it("has no Group by node on a standalone host", async () => {
    cleanup();
    swarmEnv = false;
    body = list([image("a")]);
    mount();
    await screen.findByText("app-a:1");
    expect(screen.queryByRole("group", { name: "Group by node" })).toBeNull();
  });
});

describe("ImageList Dangling chip and search", () => {
  it("shows the count in the chip and writes dangling=1 when it is pressed", async () => {
    body = list([image("a")], { danglingCount: 6 });
    mount();
    const group = await screen.findByRole("group", { name: "Filter images" });
    const chip = await within(group).findByRole("button", { name: "Dangling 6" });
    expect(chip.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(chip);
    expect(changes).toEqual([{ dangling: 1 }]);
  });

  it("wraps the filter chips onto further rows instead of scrolling sideways (G-06-2)", async () => {
    body = list([image("a")], { danglingCount: 6 });
    mount();
    const group = await screen.findByRole("group", { name: "Filter images" });
    await within(group).findByRole("button", { name: "Dangling 6" });
    // The chips wrap onto further rows at every width, so none is cut off at 375px (G-06-2).
    expect(group.classList.contains("flex-wrap")).toBe(true);
    expect(group.className).not.toMatch(/overflow-x-auto|flex-nowrap/);
  });

  it("shows the chip pressed when dangling is in the URL, sends it, and clears it when pressed again", async () => {
    search = { dangling: 1 };
    body = list([image("b", { repoTags: ["<none>"], dangling: true })], {
      danglingCount: 1,
      unfilteredTotal: 4,
    });
    mount();
    const chip = await screen.findByRole("button", { name: "Dangling 1" });
    expect(chip.getAttribute("aria-pressed")).toBe("true");
    expect(urls.some((u) => u.includes("dangling=true"))).toBe(true);
    fireEvent.click(chip);
    expect(changes.at(-1)).toEqual({ dangling: undefined });
  });

  it("sends the search to the server and commits typing after 250 ms", async () => {
    search = { q: "ngi" };
    mount();
    await screen.findByText("nginx:1.27");
    expect(urls.some((u) => u.includes("q=ngi"))).toBe(true);
    cleanup();
    changes = [];
    search = {};
    mount();
    const box = await screen.findByRole("searchbox", { name: "Search images" });
    expect(box.getAttribute("placeholder")).toBe("Search by name, tag or ID");
    // The debounce is a timer of the component's own, set by the keystroke below. The clock is
    // fully fake from here, so real time cannot fire it before the test moves the clock.
    vi.useFakeTimers();
    try {
      fireEvent.change(box, { target: { value: "redis" } });
      expect(changes).toEqual([]);
      await vi.advanceTimersByTimeAsync(300);
      expect(changes).toEqual([{ q: "redis" }]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("offers Clear filters only when something is set, and clears the search, chip and node", async () => {
    mount();
    await screen.findByText("nginx:1.27");
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    cleanup();
    search = { q: "x", dangling: 1 };
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Clear filters" }));
    expect(changes.at(-1)).toEqual({ q: undefined, dangling: undefined, node: undefined });
  });
});

describe("ImageList captions", () => {
  it("counts images, in the singular too", async () => {
    mount();
    expect(await screen.findByText("3 images")).toBeTruthy();
    cleanup();
    body = list([image("a")]);
    mount();
    expect(await screen.findByText("1 image")).toBeTruthy();
  });

  it("counts images across nodes in the merged view and image copies across nodes per node", async () => {
    swarmEnv = true;
    body = swarmBody();
    mount();
    expect(await screen.findByText("2 images across 3 nodes")).toBeTruthy();
    cleanup();
    search = { view: "node" };
    body = swarmBody("node");
    mount();
    expect(await screen.findByText("2 image copies across 3 nodes")).toBeTruthy();
  });

  it("says n of total when a search or filter is set", async () => {
    search = { q: "ngi" };
    body = list([image("a")], { unfilteredTotal: 5 });
    mount();
    expect(await screen.findByText("1 of 5 images")).toBeTruthy();
  });

  it("says the first 500 are shown when the server capped the list", async () => {
    body = list([image("a")], { total: 600, truncated: true });
    mount();
    expect(await screen.findByText(/first 500 of 600/i)).toBeTruthy();
  });
});

describe("ImageList states", () => {
  it("shows 8 skeleton rows and the toolbar at once while loading", async () => {
    listAnswer = () => new Promise<Response>(() => undefined);
    mount();
    expect(await screen.findByRole("searchbox", { name: "Search images" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Filter images" })).toBeTruthy();
    expect(screen.getAllByRole("row")).toHaveLength(9);
  });

  it("says Couldn't load images. with Retry when the read fails", async () => {
    let fail = true;
    listAnswer = () =>
      fail ? Response.json({ status: 500 }, { status: 500 }) : Response.json(body);
    mount();
    expect(await screen.findByText("Couldn't load images.")).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("nginx:1.27")).toBeTruthy();
  });

  it("says No images with the way to pull one when there are none", async () => {
    body = list([]);
    mount();
    expect(await screen.findByText("No images")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Pull image" })).toHaveLength(1);
  });

  it("says No dangling images when the chip is on and none are", async () => {
    search = { dangling: 1 };
    body = list([], { unfilteredTotal: 3 });
    mount();
    expect(await screen.findByText("No dangling images")).toBeTruthy();
    expect(screen.getByText("Untagged images with nothing using them show up here.")).toBeTruthy();
  });

  it("says No images match “query”. with Clear search when a search matches nothing", async () => {
    search = { q: "zzz" };
    body = list([], { unfilteredTotal: 3 });
    mount();
    expect(await screen.findByText("No images match “zzz”.")).toBeTruthy();
    // The search box has a Clear search too; the empty state's is the last.
    const clears = screen.getAllByRole("button", { name: "Clear search" });
    fireEvent.click(clears[clears.length - 1] as HTMLElement);
    expect(changes.at(-1)).toEqual({ q: undefined });
  });

  it("warns that the host is read-only above the table", async () => {
    readOnlyNodes = true;
    mount();
    expect(await screen.findByText(/This host is read-only/)).toBeTruthy();
  });

  it("needs images.read", async () => {
    me = makeMe({ envs: {} });
    mount();
    await waitFor(() => expect(screen.queryByText("nginx:1.27")).toBeNull());
  });
});

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
  return screen.findByRole("menu");
}

describe("ImageList row actions", () => {
  beforeEach(() => {
    body = list([
      image("a", { repoTags: ["nginx:1.27", "nginx:latest"] }),
      image("u", {
        repoTags: ["redis:8"],
        usedByContainers: 1,
        nodes: [holder("node-1-id", 1)],
      }),
      image("c", { repoTags: ["busybox:1"] }),
    ]);
  });

  it("offers Open, Pull again…, Remove… and Untag… for an image with several names", async () => {
    mount();
    const menu = await openRowMenu("nginx:1.27");
    for (const name of ["Open", "Pull again…", "Remove…", "Untag…"]) {
      expect(within(menu).getByRole("menuitem", { name })).toBeTruthy();
    }
  });

  it("has no Untag… for an image with one name", async () => {
    mount();
    const menu = await openRowMenu("busybox:1");
    expect(within(menu).queryByRole("menuitem", { name: "Untag…" })).toBeNull();
    expect(within(menu).getByRole("menuitem", { name: "Remove…" })).toBeTruthy();
  });

  it("opens Remove {ref}? from Remove… on an unused image", async () => {
    mount();
    const menu = await openRowMenu("busybox:1");
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Remove…" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Remove busybox:1?")).toBeTruthy();
  });

  it("opens the untag dialog from Untag…", async () => {
    mount();
    const menu = await openRowMenu("nginx:1.27");
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Untag…" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Untag nginx:1.27?")).toBeTruthy();
  });

  it("disables Remove… with the reason while a container uses the image everywhere it is", async () => {
    mount();
    const menu = await openRowMenu("redis:8");
    const item = within(menu).getByRole("menuitem", { name: "Remove…" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(item);
    expect(
      (await screen.findAllByText("In use by 1 container. Open it to see what uses it.")).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("disables Remove… and Untag… with Needs Operator for a viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    const menu = await openRowMenu("nginx:1.27");
    expect(
      within(menu).getByRole("menuitem", { name: "Remove…" }).getAttribute("aria-disabled"),
    ).toBe("true");
    expect(
      within(menu).getByRole("menuitem", { name: "Untag…" }).getAttribute("aria-disabled"),
    ).toBe("true");
  });
});

describe("imageSearchSchema", () => {
  it("reads dangling=1 as the number or the text, and view=merged as the default", () => {
    expect(imageSearchSchema.parse({ dangling: 1 }).dangling).toBe(1);
    expect(imageSearchSchema.parse({ dangling: "1" }).dangling).toBe(1);
    expect(imageSearchSchema.parse({ dangling: 0 }).dangling).toBeUndefined();
    expect(imageSearchSchema.parse({ view: "node" }).view).toBe("node");
    expect(imageSearchSchema.parse({ view: "merged" }).view).toBeUndefined();
    expect(imageSearchSchema.parse({ view: "sideways" }).view).toBeUndefined();
    expect(imageSearchSchema.parse({ q: "" }).q).toBeUndefined();
  });
});

// Deleting dangling images in bulk (plan 05-17, RES-09, D-17).
describe("ImageList bulk delete of dangling images", () => {
  const dangling = (c: string, extra: Record<string, unknown> = {}) =>
    image(c, { repoTags: ["<none>"], dangling: true, ...extra });
  const bulkBody = () =>
    list([
      image("a", { repoTags: ["nginx:1.27"] }),
      dangling("b"),
      dangling("c"),
      dangling("d", { usedByContainers: 1, nodes: [holder("node-1-id", 1)] }),
      dangling("e"),
    ]);
  const boxOf = async (id: string) => {
    const r = (await screen.findByText(`${id.repeat(12)}`)).closest("tr") as HTMLElement;
    return within(r).getByRole("checkbox");
  };

  it("adds a checkbox column whose header is named Select all dangling images", async () => {
    body = bulkBody();
    mount();
    await boxOf("b");
    expect(screen.getByRole("checkbox", { name: "Select all dangling images" })).toBeTruthy();
    expect(screen.getAllByRole("checkbox")).toHaveLength(6);
  });

  it("lets only dangling, unused rows be ticked and says why on the others", async () => {
    body = bulkBody();
    mount();
    const named = await boxOf("a");
    fireEvent.click(named);
    expect(named.getAttribute("aria-checked")).toBe("false");
    const used = await boxOf("d");
    fireEvent.click(used);
    expect(used.getAttribute("aria-checked")).toBe("false");
    fireEvent.focus(named);
    expect(
      (
        await screen.findAllByText(
          "Only dangling images can be deleted in bulk. Open an image to remove it.",
        )
      ).length,
    ).toBeGreaterThan(0);
    fireEvent.focus(used);
    expect((await screen.findAllByText("In use, so it can't be removed.")).length).toBeGreaterThan(
      0,
    );
    expect(screen.queryByText(/selected/)).toBeNull();
  });

  it("shows the bulk bar with the count, Remove… and Clear selection once a row is ticked", async () => {
    body = bulkBody();
    mount();
    fireEvent.click(await boxOf("b"));
    const bar = await screen.findByRole("toolbar", { name: "Bulk actions" });
    expect(within(bar).getByText("1 selected")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Remove…" })).toBeTruthy();
    fireEvent.click(within(bar).getByRole("button", { name: "Clear selection" }));
    await waitFor(() => expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull());
  });

  it("makes the header checkbox tri-state over the selectable rows", async () => {
    body = bulkBody();
    mount();
    const all = await screen.findByRole("checkbox", { name: "Select all dangling images" });
    fireEvent.click(await boxOf("b"));
    await waitFor(() => expect(all.getAttribute("aria-checked")).toBe("mixed"));
    fireEvent.click(all);
    await waitFor(() => expect(all.getAttribute("aria-checked")).toBe("true"));
    expect(screen.getByText("3 selected")).toBeTruthy();
    fireEvent.click(all);
    await waitFor(() => expect(all.getAttribute("aria-checked")).toBe("false"));
  });

  it("selects a range with Shift+click and clears with Escape", async () => {
    body = bulkBody();
    mount();
    fireEvent.click(await boxOf("b"));
    fireEvent.click(await boxOf("e"), { shiftKey: true });
    expect(await screen.findByText("3 selected")).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("table"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByText("3 selected")).toBeNull());
  });

  it("deletes the selection in one confirm and sends each image on its node", async () => {
    body = bulkBody();
    mount();
    fireEvent.click(await boxOf("b"));
    fireEvent.click(await boxOf("c"));
    fireEvent.click(screen.getByRole("button", { name: "Remove…" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete 2 images?")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 2 images" }));
    expect(await screen.findByText("Deleted 2 images.")).toBeTruthy();
    expect(posts).toHaveLength(1);
    expect(posts[0]?.targets).toEqual([
      { nodeId: "node-1-id", imageId: ID("b") },
      { nodeId: "node-1-id", imageId: ID("c") },
    ]);
    await waitFor(() => expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull());
  });

  it("marks the targeted rows Removing… with their checkbox and menu disabled until the list changes", async () => {
    body = bulkBody();
    mount();
    fireEvent.click(await boxOf("b"));
    fireEvent.click(screen.getByRole("button", { name: "Remove…" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 1 image" }));
    const r = (await screen.findByText("b".repeat(12))).closest("tr") as HTMLElement;
    await waitFor(() => expect(within(r).getByText("Removing…")).toBeTruthy());
    expect(within(r).getByRole("checkbox").hasAttribute("disabled")).toBe(true);
    expect(
      within(r)
        .getByRole("button", { name: /Actions for/ })
        .hasAttribute("disabled"),
    ).toBe(true);
  });

  it("holds new rows behind a pill while rows are selected", async () => {
    body = bulkBody();
    mount();
    fireEvent.click(await boxOf("b"));
    body = list([...(bulkBody().items as unknown[]), dangling("f")]);
    await act(async () => {
      await client.invalidateQueries();
    });
    expect(await screen.findByText("1 change waiting")).toBeTruthy();
    expect(screen.queryByText("f".repeat(12))).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show changes" }));
    expect(await screen.findByText("f".repeat(12))).toBeTruthy();
  });

  it("offers Select all {n} once every visible row is selected and more dangling images exist", async () => {
    const shown = [dangling("b"), dangling("c")];
    body = list(shown, { danglingCount: 4, total: 2, unfilteredTotal: 2, truncated: true });
    listAnswer = () => Response.json(body);
    mount();
    await boxOf("b");
    fireEvent.click(screen.getByRole("checkbox", { name: "Select all dangling images" }));
    const bar = await screen.findByRole("toolbar", { name: "Bulk actions" });
    listAnswer = () =>
      Response.json(list([...shown, dangling("f"), dangling("g")], { danglingCount: 4 }));
    fireEvent.click(within(bar).getByRole("button", { name: "Select all 4" }));
    expect(await screen.findByText("4 selected")).toBeTruthy();
    expect(urls.some((u) => u.includes("dangling=true"))).toBe(true);
    expect(screen.queryByRole("button", { name: "Select all 4" })).toBeNull();
  });

  it("disables Remove… with the reason for a viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    body = bulkBody();
    mount();
    fireEvent.click(await boxOf("b"));
    const bar = await screen.findByRole("toolbar", { name: "Bulk actions" });
    const remove = within(bar).getByRole("button", { name: "Remove…" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(remove);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});
