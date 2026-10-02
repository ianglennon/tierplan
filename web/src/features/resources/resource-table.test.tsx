// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { type Column, type ListQuery, ResourceTable } from "@/features/resources/resource-table";
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
