// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import type { JanitorRunItem } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  RunItemsTable,
  type RunItemsTableProps,
  runSearchSchema,
} from "@/features/janitor/run-items-table";
import "@/i18n";
import { evidence, runItem } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let client: QueryClient;
let meEnvs: Record<string, "viewer" | "operator" | "admin">;

// jsdom lays nothing out, so the scrolling area is given a height and the virtualiser can tell how
// many rows fit (as the preview sheet's tests do).
const saved: Record<string, PropertyDescriptor | undefined> = {};
function lay(prop: string, value: number) {
  saved[prop] = Object.getOwnPropertyDescriptor(HTMLElement.prototype, prop);
  Object.defineProperty(HTMLElement.prototype, prop, { configurable: true, value });
}

beforeEach(() => {
  setCsrfToken(null);
  meEnvs = { [ENV]: "viewer" };
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(makeMe({ envs: meEnvs }));
      if (url.pathname.endsWith("/evidence")) return Response.json(evidence());
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
  for (const [prop, descriptor] of Object.entries(saved)) {
    if (descriptor) Object.defineProperty(HTMLElement.prototype, prop, descriptor);
    else delete (HTMLElement.prototype as unknown as Record<string, unknown>)[prop];
  }
});

function mount(items: JanitorRunItem[], over: Partial<RunItemsTableProps> = {}) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RunItemsTable envId={ENV} runId="r1" items={items} show="all" {...over} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const table = () => screen.getByRole("table", { name: "Items in this run" });
const bodyRows = () => within(table()).getAllByRole("row").slice(1);
const names = () => bodyRows().map((r) => within(r).getAllByRole("cell")[1]?.textContent);

const mixed = () => [
  runItem({ seq: 1, name: "ok-1", outcome: "removed" }),
  runItem({ seq: 2, name: "busy-2", outcome: "refused_in_use", code: "in_use" }),
  runItem({ seq: 3, name: "held-3", outcome: "skipped_protected", code: "no_longer_stale" }),
  runItem({
    seq: 4,
    name: "away-4",
    outcome: "skipped_node_offline",
    code: "node_offline",
    node: "node-9",
  }),
  runItem({ seq: 5, name: "broke-5", outcome: "error", code: "action_failed" }),
];

describe("the outcome of each item", () => {
  it("is said in words with the reason, and kept apart from a failure", () => {
    mount(mixed());
    const row = (name: string) => within(table()).getByText(name).closest("tr") as HTMLElement;
    expect(within(row("ok-1")).getByText("Removed")).toBeTruthy();
    expect(within(row("busy-2")).getByText("In use, kept")).toBeTruthy();
    expect(within(row("held-3")).getByText("Protected")).toBeTruthy();
    expect(within(row("away-4")).getByText("Node not reporting")).toBeTruthy();
    expect(
      within(row("away-4")).getByText(
        "node-9 isn't reporting, so TierPlan skipped its containers and images.",
      ),
    ).toBeTruthy();
    expect(within(row("broke-5")).getByText("Failed")).toBeTruthy();
    expect(
      within(row("broke-5")).getByText("TierPlan didn't remove broke-5: Docker refused it."),
    ).toBeTruthy();
  });

  it("says a crashed container was kept when its evidence could not be saved", () => {
    mount([runItem({ name: "crash-1", outcome: "error", code: "evidence_unsaved" })]);
    expect(
      screen.getByText("TierPlan kept crash-1: it couldn't save why the container stopped first."),
    ).toBeTruthy();
  });
});

describe("the Show filter", () => {
  it("lists only what was removed", () => {
    mount(mixed(), { show: "removed" });
    expect(names()).toEqual(["ok-1"]);
  });

  it("lists what was kept: in use and protected", () => {
    mount(mixed(), { show: "kept" });
    expect(names()).toEqual(["busy-2", "held-3"]);
  });

  it("lists only the problems: Failed and Node not reporting", () => {
    mount(mixed(), { show: "problems" });
    expect(names()).toEqual(["away-4", "broke-5"]);
    expect(screen.getByText("2 of 5 items")).toBeTruthy();
  });

  it("lists everything for all", () => {
    mount(mixed(), { show: "all" });
    expect(names()).toHaveLength(5);
    expect(screen.getByText("5 items")).toBeTruthy();
  });
});

describe("the search", () => {
  it("filters by name, ignoring case, together with the Show filter", () => {
    mount(
      [
        runItem({ seq: 1, name: "web.1.abc" }),
        runItem({ seq: 2, name: "WEB.2.def", outcome: "error", code: "action_failed" }),
        runItem({ seq: 3, name: "db.1.ghi" }),
      ],
      { q: "web" },
    );
    expect(names()).toEqual(["web.1.abc", "WEB.2.def"]);
    cleanup();
    mount(
      [
        runItem({ seq: 1, name: "web.1.abc" }),
        runItem({ seq: 2, name: "WEB.2.def", outcome: "error", code: "action_failed" }),
      ],
      { q: "web", show: "problems" },
    );
    expect(names()).toEqual(["WEB.2.def"]);
  });

  it("says nothing matches and offers to clear the filters", () => {
    const onClearFilters = vi.fn();
    mount(mixed(), { q: "zzz", onClearFilters });
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.getByText("No items match.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(onClearFilters).toHaveBeenCalledTimes(1);
  });
});

describe("the headers", () => {
  it("sort the rows, are buttons with aria-sort, and go back to the order of the run", () => {
    mount([
      runItem({ seq: 1, name: "charlie" }),
      runItem({ seq: 2, name: "alpha" }),
      runItem({ seq: 3, name: "bravo" }),
    ]);
    const header = () => screen.getByRole("columnheader", { name: "Name" });
    expect(header().getAttribute("aria-sort")).toBe("none");
    const button = () => within(header()).getByRole("button", { name: "Name" });
    fireEvent.click(button());
    expect(header().getAttribute("aria-sort")).toBe("ascending");
    expect(names()).toEqual(["alpha", "bravo", "charlie"]);
    fireEvent.click(button());
    expect(header().getAttribute("aria-sort")).toBe("descending");
    expect(names()).toEqual(["charlie", "bravo", "alpha"]);
    fireEvent.click(button());
    expect(header().getAttribute("aria-sort")).toBe("none");
    expect(names()).toEqual(["charlie", "alpha", "bravo"]);
  });

  it("sort by outcome using the words shown", () => {
    mount(mixed());
    fireEvent.click(screen.getByRole("button", { name: "Outcome" }));
    expect(names()).toEqual(["broke-5", "busy-2", "away-4", "held-3", "ok-1"]);
  });
});

describe("the columns", () => {
  it("leave out Node where no item has one (a standalone host)", () => {
    mount([runItem({ node: "" })]);
    expect(screen.queryByRole("columnheader", { name: "Node" })).toBeNull();
    cleanup();
    mount([runItem({ node: "node-1" })]);
    expect(screen.getByRole("columnheader", { name: "Node" })).toBeTruthy();
  });

  it("show an unknown reason as a dash", () => {
    mount([runItem({ reason: { code: "" } as unknown as JanitorRunItem["reason"] })]);
    const cells = within(bodyRows()[0] as HTMLElement).getAllByRole("cell");
    expect(cells.map((c) => c.textContent)).toContain("—");
  });
});

describe("text from Docker", () => {
  it("is drawn as text, never as markup", () => {
    mount([runItem({ name: "<script>alert(1)</script>", node: "<img src=x onerror=alert(2)>" })]);
    expect(within(table()).getByText("<script>alert(1)</script>")).toBeTruthy();
    expect(within(table()).getByText("<img src=x onerror=alert(2)>")).toBeTruthy();
    expect(table().querySelector("script, img")).toBeNull();
  });
});

describe("a long list", () => {
  const many = (n: number) =>
    Array.from({ length: n }, (_, i) => runItem({ seq: i + 1, name: `task-${i + 1}` }));

  it("draws every row up to 100 and the rows in view over 100, and says how many there are", () => {
    lay("offsetHeight", 400);
    lay("offsetWidth", 800);
    mount(many(100));
    expect(bodyRows()).toHaveLength(100);
    expect(screen.getByText("100 items")).toBeTruthy();
    cleanup();

    mount(many(150));
    const drawn = bodyRows().filter((r) => within(r).queryAllByRole("cell").length > 1);
    expect(drawn.length).toBeGreaterThan(0);
    expect(drawn.length).toBeLessThan(100);
    expect(screen.getByText("150 items")).toBeTruthy();
    // The scrolling area is bounded, so the page does not grow with the list.
    expect(table().parentElement?.className).toContain("max-h-[70svh]");
  });

  it("keeps a filter over a virtualised list counting every match, drawn or not", () => {
    lay("offsetHeight", 400);
    lay("offsetWidth", 800);
    const items = many(2000);
    mount(items, { q: "task-1" });
    // task-1, task-10 to task-19, task-100 to task-199 and task-1000 to task-1999.
    expect(screen.getByText("1,111 of 2,000 items")).toBeTruthy();
    expect(bodyRows().length).toBeLessThan(200);
  });
});

describe("the search of the address", () => {
  it("keeps a known show, a search and an item, and drops the rest", () => {
    expect(runSearchSchema.parse({ show: "problems", q: "web", item: 3 })).toEqual({
      show: "problems",
      q: "web",
      item: 3,
    });
    expect(runSearchSchema.parse({ show: "all" }).show).toBe("all");
  });

  it("drops an invalid show instead of failing the page", () => {
    expect(runSearchSchema.parse({ show: "everything" })).toEqual({});
    expect(runSearchSchema.parse({ show: 3 })).toEqual({});
  });

  it("drops a search that is empty or too long, and an item that is not a positive whole number", () => {
    expect(runSearchSchema.parse({ q: "" })).toEqual({});
    expect(runSearchSchema.parse({ q: "x".repeat(129) })).toEqual({});
    expect(runSearchSchema.parse({ item: "abc" })).toEqual({});
    expect(runSearchSchema.parse({ item: 0 })).toEqual({});
    expect(runSearchSchema.parse({ item: 1.5 })).toEqual({});
    expect(runSearchSchema.parse({ item: "7" })).toEqual({ item: 7 });
  });
});

describe("the Evidence column", () => {
  const crashed = () => [
    runItem({ seq: 1, name: "crashed-1", outcome: "removed", hasEvidence: true }),
    runItem({ seq: 2, name: "clean-2", outcome: "removed", hasEvidence: false }),
    runItem({
      seq: 3,
      name: "kept-3",
      outcome: "refused_in_use",
      code: "in_use",
      hasEvidence: true,
    }),
    runItem({ seq: 4, name: "img-4", type: "image", outcome: "removed", hasEvidence: false }),
  ];
  const row = (name: string) => within(table()).getByText(name).closest("tr") as HTMLElement;

  // The page keeps the item in its address; this keeps it in state, as the route does.
  function Harness({ start, onChange }: { start?: number; onChange?: (seq?: number) => void }) {
    const [item, setItem] = useState<number | undefined>(start);
    return (
      <RunItemsTable
        envId={ENV}
        runId="r1"
        items={crashed()}
        show="all"
        item={item}
        onItemChange={(seq) => {
          setItem(seq);
          onChange?.(seq);
        }}
      />
    );
  }
  function mountHarness(start?: number, onChange?: (seq?: number) => void) {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <Harness start={start} onChange={onChange} />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  it("offers View evidence for a removed container that has some, and a dash for the rest", async () => {
    mountHarness();
    expect(screen.getByRole("columnheader", { name: "Evidence" })).toBeTruthy();
    expect(await screen.findByRole("button", { name: "View evidence for crashed-1" })).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /^View evidence/ })).toHaveLength(1);
    for (const name of ["clean-2", "kept-3", "img-4"]) {
      const cells = within(row(name)).getAllByRole("cell");
      expect(cells.at(-1)?.textContent).toBe("—");
    }
  });

  it("offers none to a person who may not read the containers", async () => {
    meEnvs = {};
    mountHarness();
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: /^View evidence/ })).toBeNull(),
    );
    expect(within(row("crashed-1")).getAllByRole("cell").at(-1)?.textContent).toBe("—");
  });

  it("opens the sheet for the item and puts it in the address, and closing clears it and gives the button the focus back", async () => {
    const changes: (number | undefined)[] = [];
    mountHarness(undefined, (seq) => changes.push(seq));
    const button = await screen.findByRole("button", { name: "View evidence for crashed-1" });
    button.focus();
    fireEvent.click(button);
    expect(changes).toEqual([1]);
    const box = await screen.findByRole("dialog", { name: "Crash evidence for crashed-1" });
    expect(await screen.findByText("Exit code")).toBeTruthy();

    fireEvent.click(within(box).getByRole("button", { name: "Close" }));
    expect(changes).toEqual([1, undefined]);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(button));
  });

  it("opens the sheet from an address that names the item", async () => {
    mountHarness(1);
    expect(
      await screen.findByRole("dialog", { name: "Crash evidence for crashed-1" }),
    ).toBeTruthy();
  });

  it("opens nothing for an item the run does not have", async () => {
    mountHarness(99);
    await screen.findByRole("button", { name: "View evidence for crashed-1" });
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
