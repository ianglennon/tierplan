// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import {
  type Column,
  type ListQuery,
  ResourceTable,
  TruncatedText,
} from "@/features/resources/resource-table";
import "@/i18n";

interface Row {
  id: string;
  name: string;
}

const COLUMNS: Column<Row>[] = [
  { id: "name", header: "Name", cell: (r) => r.name },
  { id: "id", header: "ID", cell: (r) => r.id },
];

function query(over: Partial<ListQuery<Row>> = {}): ListQuery<Row> {
  return {
    isPending: false,
    isError: false,
    isFetching: false,
    refetch: () => undefined,
    ...over,
  };
}

function show(q: ListQuery<Row>) {
  render(
    <TooltipProvider>
      <ResourceTable
        resource="containers"
        columns={COLUMNS}
        query={q}
        envName="Lab host"
        getRowKey={(r) => r.id}
      />
    </TooltipProvider>,
  );
}

afterEach(cleanup);

describe("ResourceTable", () => {
  it("keeps the server's order and offers no selection, actions or links (D-56)", () => {
    show(
      query({
        data: {
          items: [
            { id: "b", name: "zeta" },
            { id: "a", name: "alpha" },
          ],
          total: 2,
          truncated: false,
        },
      }),
    );
    const body = screen.getAllByRole("rowgroup")[1] as HTMLElement;
    const names = within(body)
      .getAllByRole("row")
      .map((r) => r.querySelector("td")?.textContent);
    expect(names).toEqual(["zeta", "alpha"]);
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("has a sticky header, dense 36px rows and a horizontally scrolling wrapper", () => {
    show(query({ data: { items: [{ id: "a", name: "alpha" }], total: 1, truncated: false } }));
    const head = screen.getByRole("columnheader", { name: "Name" });
    expect(head.className).toContain("sticky");
    expect(head.className).toContain("bg-muted");
    const row = screen.getAllByRole("row")[1] as HTMLElement;
    expect(row.className).toContain("h-9");
    expect(row.className).toContain("max-md:h-11");
    expect(screen.getByRole("table").parentElement?.className).toContain("overflow-x-auto");
  });

  it("counts with the plural: '1 container', '0 containers', '2 containers'", () => {
    show(query({ data: { items: [{ id: "a", name: "alpha" }], total: 1, truncated: false } }));
    expect(screen.getByText("1 container")).toBeTruthy();
    cleanup();
    show(query({ data: { items: [], total: 0, truncated: false } }));
    expect(screen.getByText("0 containers")).toBeTruthy();
    cleanup();
    show(
      query({
        data: {
          items: [
            { id: "a", name: "a" },
            { id: "b", name: "b" },
          ],
          total: 2,
          truncated: false,
        },
      }),
    );
    expect(screen.getByText("2 containers")).toBeTruthy();
  });

  it("shows 8 skeleton rows while loading, with the header in place", () => {
    show(query({ isPending: true }));
    expect(screen.getAllByRole("row")).toHaveLength(9);
    expect(screen.getByRole("columnheader", { name: "Name" })).toBeTruthy();
    expect(screen.getByRole("table").getAttribute("aria-busy")).toBe("true");
  });

  it("shows the empty state naming the environment", () => {
    show(query({ data: { items: [], total: 0, truncated: false } }));
    expect(screen.getByText("No containers")).toBeTruthy();
    expect(screen.getByText("Nothing to show on Lab host right now.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows an inline error with Retry that refetches", () => {
    const refetch = vi.fn();
    show(query({ isError: true, refetch }));
    expect(screen.getByText("Couldn't load containers.")).toBeTruthy();
    expect(screen.getByText("Docker may be unreachable. Try again.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(refetch).toHaveBeenCalledTimes(1);
  });

  it("keeps the rows it has when a later refresh fails", () => {
    show(
      query({
        isError: true,
        data: { items: [{ id: "a", name: "alpha" }], total: 1, truncated: false },
      }),
    );
    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.queryByText("Couldn't load containers.")).toBeNull();
  });

  it("says how many it left out when the server capped the list", () => {
    show(
      query({
        data: { items: [{ id: "a", name: "alpha" }], total: 1234, truncated: true },
      }),
    );
    expect(screen.getByText(/Showing the first 500 of 1,?234\./)).toBeTruthy();
    expect(screen.getByText(/1,?234 containers/)).toBeTruthy();
  });

  it("does not mention a cap when the list is complete", () => {
    show(query({ data: { items: [{ id: "a", name: "alpha" }], total: 1, truncated: false } }));
    expect(screen.queryByText(/Showing the first/)).toBeNull();
  });
});

// The swarm lists (plan 22, D-46): a Node column after Name, the Node select,
// the caption across nodes and the banner slot.
describe("ResourceTable on a swarm", () => {
  const NODE: Column<Row> = { id: "node", header: "Node", cell: (r) => `host-of-${r.id}` };

  function swarm(
    q: ListQuery<Row>,
    over: Partial<{
      value: string;
      onChange: (n: string | undefined) => void;
      banner: React.ReactNode;
      envName: string;
    }> = {},
  ) {
    render(
      <TooltipProvider>
        <ResourceTable
          resource="containers"
          columns={COLUMNS}
          query={q}
          envName={over.envName ?? "Lab swarm"}
          getRowKey={(r) => r.id}
          nodeColumn={NODE}
          nodeFilter={{ value: over.value, onChange: over.onChange ?? (() => undefined) }}
          banner={over.banner}
        />
      </TooltipProvider>,
    );
  }

  const rows = (n: number): Row[] =>
    Array.from({ length: n }, (_, i) => ({ id: `r${i}`, name: `c${i}` }));

  function openNodeSelect() {
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Node" }), { key: "Enter" });
  }

  it("puts the Node column right after the first column", () => {
    swarm(query({ data: { items: rows(2), total: 2, truncated: false, nodes: ["a", "b"] } }));
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual(["Name", "Node", "ID"]);
    const first = within(screen.getAllByRole("row")[1] as HTMLElement)
      .getAllByRole("cell")
      .map((c) => c.textContent);
    expect(first).toEqual(["c0", "host-of-r0", "r0"]);
  });

  it("counts across the nodes that reported, in the singular and the plural", () => {
    swarm(query({ data: { items: rows(1), total: 12, truncated: false, nodes: ["a", "b", "c"] } }));
    expect(screen.getByText("12 containers across 3 nodes")).toBeTruthy();
    cleanup();
    swarm(query({ data: { items: rows(1), total: 1, truncated: false, nodes: ["a"] } }));
    expect(screen.getByText("1 container across 1 node")).toBeTruthy();
  });

  it("does not count the swarm-wide Cluster entry as a node", () => {
    swarm(
      query({ data: { items: rows(1), total: 5, truncated: false, nodes: ["Cluster", "a", "b"] } }),
    );
    expect(screen.getByText("5 containers across 2 nodes")).toBeTruthy();
  });

  it("says the count is on one node while a node is chosen", () => {
    swarm(query({ data: { items: rows(2), total: 4, truncated: false, nodes: ["n1", "n2"] } }), {
      value: "n2",
    });
    expect(screen.getByText("4 containers on n2")).toBeTruthy();
    expect(screen.queryByText(/across/)).toBeNull();
  });

  it("falls back to the plain count when no node reported", () => {
    swarm(query({ data: { items: [], total: 0, truncated: false, nodes: [] } }));
    expect(screen.getByText("0 containers")).toBeTruthy();
  });

  it("offers All nodes and each node, and reports the choice", () => {
    const onChange = vi.fn();
    swarm(
      query({ data: { items: rows(1), total: 1, truncated: false, nodes: ["n1", "n2", "n3"] } }),
      { onChange },
    );
    expect(screen.getByRole("combobox", { name: "Node" }).textContent).toContain("All nodes");
    openNodeSelect();
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["All nodes", "n1", "n2", "n3"]);
    fireEvent.keyDown(screen.getByRole("option", { name: "n2" }), { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("n2");
  });

  it("goes back to all nodes with undefined", () => {
    const onChange = vi.fn();
    swarm(query({ data: { items: rows(1), total: 1, truncated: false, nodes: ["n1", "n2"] } }), {
      value: "n2",
      onChange,
    });
    expect(screen.getByRole("combobox", { name: "Node" }).textContent).toContain("n2");
    openNodeSelect();
    fireEvent.keyDown(screen.getByRole("option", { name: "All nodes" }), { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith(undefined);
  });

  it("keeps a node that is in the URL but not reported as the chosen one", () => {
    swarm(query({ data: { items: [], total: 0, truncated: false, nodes: ["n1"] } }), {
      value: "gone",
    });
    expect(screen.getByRole("combobox", { name: "Node" }).textContent).toContain("gone");
  });

  it("names the node in the empty state and keeps the filter", () => {
    swarm(query({ data: { items: [], total: 0, truncated: false, nodes: ["n1", "n2"] } }), {
      value: "n2",
    });
    expect(screen.getByText("No containers")).toBeTruthy();
    expect(screen.getByText("Nothing to show on n2 right now.")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Node" })).toBeTruthy();
  });

  it("keeps the Node column and the filter while loading, with 8 skeleton rows", () => {
    swarm(query({ isPending: true }));
    expect(screen.getAllByRole("row")).toHaveLength(9);
    expect(screen.getByRole("columnheader", { name: "Node" })).toBeTruthy();
    const filter = screen.getByRole("combobox", { name: "Node" }) as HTMLButtonElement;
    expect(filter.disabled).toBe(false);
  });

  it("shows the banner above the filter and the table", () => {
    swarm(query({ data: { items: rows(1), total: 1, truncated: false, nodes: ["n1"] } }), {
      banner: <p>banner here</p>,
    });
    const banner = screen.getByText("banner here");
    const filter = screen.getByRole("combobox", { name: "Node" });
    const table = screen.getByRole("table");
    expect(banner.compareDocumentPosition(filter) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(filter.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("truncates a long node name in the column through TruncatedText", () => {
    const long = "a-very-long-node-host-name.example.internal".repeat(3);
    render(
      <TooltipProvider>
        <ResourceTable
          resource="containers"
          columns={COLUMNS}
          query={query({ data: { items: rows(1), total: 1, truncated: false, nodes: [long] } })}
          envName="Lab"
          getRowKey={(r) => r.id}
          nodeColumn={{
            id: "node",
            header: "Node",
            cell: () => <TruncatedText>{long}</TruncatedText>,
          }}
          nodeFilter={{ onChange: () => undefined }}
        />
      </TooltipProvider>,
    );
    expect(screen.getByText(long).className).toContain("truncate");
  });
});

// Groups, the toolbar slot, the empty and cap overrides and the sort headers
// (plan 04-07).
describe("ResourceTable groups and slots", () => {
  const rowsA: Row[] = [
    { id: "a1", name: "alpha-1" },
    { id: "a2", name: "alpha-2" },
  ];
  const rowsB: Row[] = [{ id: "b1", name: "beta-1" }];

  function grouped(
    over: Partial<{
      collapsed: string[];
      toggle: (key: string) => void;
      toolbar: React.ReactNode;
      empty: React.ReactNode;
      capNote: string;
      caption: string;
      sort: { key: string; dir: "asc" | "desc"; onSort: (k: string) => void };
      columns: Column<Row>[];
      items: { key: string; rows: Row[]; header?: { name: string } }[];
      total: number;
      truncated: boolean;
    }> = {},
  ) {
    const items = over.items ?? [
      { key: "a", rows: rowsA, header: { name: "Group A" } },
      { key: "b", rows: rowsB, header: { name: "Group B" } },
    ];
    render(
      <TooltipProvider>
        <ResourceTable
          resource="containers"
          columns={over.columns ?? COLUMNS}
          query={query({
            data: {
              items: items.flatMap((g) => g.rows),
              total: over.total ?? 3,
              truncated: over.truncated ?? false,
            },
          })}
          envName="Lab host"
          getRowKey={(r) => r.id}
          groups={{
            items,
            collapse: {
              collapsed: new Set(over.collapsed ?? []),
              toggle: over.toggle ?? (() => undefined),
              collapseAll: () => undefined,
              expandAll: () => undefined,
            },
          }}
          toolbar={over.toolbar}
          empty={over.empty}
          capNote={over.capNote}
          caption={over.caption}
          sort={over.sort}
        />
      </TooltipProvider>,
    );
  }

  it("draws a header band before each group's rows, spanning every column", () => {
    grouped();
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      "Group A",
      "alpha-1a1",
      "alpha-2a2",
      "Group B",
      "beta-1b1",
    ]);
    const band = rows[0]?.querySelector("td");
    expect(band?.getAttribute("colspan")).toBe("2");
    expect(rows[0]?.className).toContain("bg-muted");
    expect(rows[0]?.className).toContain("h-9");
  });

  it("gives the chevron aria-expanded and aria-controls of the group's rows, and reports the toggle", () => {
    const toggle = vi.fn();
    grouped({ toggle });
    const button = screen.getByRole("button", { name: "Collapse Group A" });
    expect(button.getAttribute("aria-expanded")).toBe("true");
    const rows = document.getElementById(button.getAttribute("aria-controls") as string);
    expect(rows?.textContent).toBe("alpha-1a1alpha-2a2");
    fireEvent.click(button);
    expect(toggle).toHaveBeenCalledWith("a");
  });

  it("hides the rows of a collapsed group and keeps its header", () => {
    grouped({ collapsed: ["a"] });
    expect(screen.queryByText("alpha-1")).toBeNull();
    expect(screen.getByText("beta-1")).toBeTruthy();
    const button = screen.getByRole("button", { name: "Expand Group A" });
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(document.getElementById(button.getAttribute("aria-controls") as string)).not.toBeNull();
  });

  it("renders a group without a header as plain rows", () => {
    grouped({ items: [{ key: "all", rows: rowsA }] });
    expect(screen.getAllByRole("row").slice(1)).toHaveLength(2);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("shows the empty state when the groups hold no rows", () => {
    grouped({ items: [] });
    expect(screen.getByText("No containers")).toBeTruthy();
    cleanup();
    grouped({ items: [], empty: <p>custom empty</p> });
    expect(screen.getByText("custom empty")).toBeTruthy();
    expect(screen.queryByText("No containers")).toBeNull();
  });

  it("puts the toolbar between the header and the table, and swaps the caption and the cap sentence", () => {
    grouped({
      toolbar: <p>the toolbar</p>,
      caption: "3 of 9 containers",
      capNote: "Narrow it down.",
      truncated: true,
      total: 900,
    });
    expect(screen.getByText("3 of 9 containers")).toBeTruthy();
    expect(screen.getByText("Narrow it down.")).toBeTruthy();
    expect(
      screen.getByText("the toolbar").compareDocumentPosition(screen.getByRole("table")) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("marks the sorted column with aria-sort and reports a press on a sortable header", () => {
    const onSort = vi.fn();
    grouped({
      columns: [
        { id: "name", header: "Name", cell: (r) => r.name, sortKey: "name" },
        { id: "id", header: "ID", cell: (r) => r.id },
      ],
      sort: { key: "name", dir: "desc", onSort },
    });
    const name = screen.getByRole("columnheader", { name: "Name" });
    expect(name.getAttribute("aria-sort")).toBe("descending");
    expect(screen.getByRole("columnheader", { name: "ID" }).getAttribute("aria-sort")).toBeNull();
    fireEvent.click(within(name).getByRole("button"));
    expect(onSort).toHaveBeenCalledWith("name");
  });
});
