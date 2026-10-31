// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import {
  type AddressMapRow,
  filterMapRows,
  MAP_RENDER_CAP,
  type MapSearch,
  SubnetMap,
  sortMapRows,
} from "@/features/address-space/subnet-map";
import "@/i18n";
import { TestRouter } from "@/test-router";

const ENV = "e1";

const row = (name: string, order: number, extra: Partial<AddressMapRow> = {}): AddressMapRow => ({
  id: `id-${name}`,
  name,
  driver: "bridge",
  scope: "local",
  node: "node-1",
  nodeId: "node-1-id",
  subnets: [`10.0.${order}.0/24`],
  created: "2026-10-01T10:00:00Z",
  builtIn: false,
  ingress: false,
  attachedContainers: 0,
  attachedServices: 0,
  order,
  overlaps: [],
  owner: "",
  ownerKind: "none",
  ownerStackId: "",
  ...extra,
});

interface MountProps {
  rows?: AddressMapRow[] | undefined;
  swarm?: boolean;
  search?: MapSearch;
  onSearchChange?: (patch: Partial<MapSearch>) => void;
}

function mount(props: MountProps = {}) {
  const ui = (p: MountProps) => (
    <TestRouter>
      <TooltipProvider>
        <SubnetMap
          envId={ENV}
          rows={"rows" in p ? p.rows : []}
          total={p.rows?.length ?? 0}
          truncated={false}
          swarm={p.swarm ?? false}
          search={p.search ?? {}}
          onSearchChange={p.onSearchChange ?? (() => {})}
        />
      </TooltipProvider>
    </TestRouter>
  );
  const view = render(ui(props));
  return { rerender: (next: MountProps) => view.rerender(ui(next)) };
}

afterEach(() => {
  cleanup();
});

// The names in the Network column, in the order they show.
function names(): string[] {
  const table = screen.getByRole("table", { name: "Subnet map" });
  return within(table)
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).getAllByRole("cell")[0]?.textContent ?? "");
}

describe("the Subnet map", () => {
  it("lists every network in address order", () => {
    mount({ rows: [row("alpha", 0), row("beta", 1), row("gamma", 2)] });
    expect(
      screen.getByText("Every network, sorted by address, so overlaps sit next to each other."),
    ).toBeTruthy();
    expect(names()).toEqual(["alpha", "beta", "gamma"]);
    expect(screen.getByText("3 networks")).toBeTruthy();
    const heads = within(screen.getByRole("table", { name: "Subnet map" }))
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(heads).toEqual([
      "Network",
      "Subnet",
      "Scope",
      "Driver",
      "Owner",
      "Attached",
      "Age",
      "Addresses used",
    ]);
  });

  it("words the owner by kind", () => {
    mount({
      rows: [
        row("managed", 0, { ownerKind: "stack", owner: "shop", ownerStackId: "stack-1" }),
        row("foreign", 1, { ownerKind: "project", owner: "blog" }),
        row("made", 2, { ownerKind: "service", owner: "api" }),
        row("bare", 3),
      ],
    });
    const table = screen.getByRole("table", { name: "Subnet map" });
    const cells = (name: string) => {
      const r = within(table)
        .getAllByRole("row")
        .find((x) => within(x).queryByRole("link", { name }) !== null);
      if (!r) throw new Error(`no row for ${name}`);
      return within(r).getAllByRole("cell");
    };
    const owner = (name: string) => cells(name)[4];
    const managed = within(owner("managed") as HTMLElement).getByRole("link", { name: "shop" });
    expect(managed.getAttribute("href")).toBe("/env/e1/stacks/stack-1");
    const foreign = owner("foreign") as HTMLElement;
    expect(within(foreign).queryByRole("link")).toBeNull();
    expect(foreign.textContent).toBe("blog");
    expect(owner("made")?.textContent).toBe("Service api");
    expect(owner("bare")?.textContent).toBe("—");
  });
});

const usage = (
  used: number,
  capacity: number,
  state: "normal" | "getting_full" | "almost_full",
) => ({
  used,
  capacity,
  percent: Math.floor((used * 100) / capacity),
  state,
});

const overlay = (name: string, order: number, extra: Partial<AddressMapRow> = {}) =>
  row(name, order, { scope: "swarm", driver: "overlay", node: "Cluster", ...extra });

describe("filterMapRows", () => {
  const rows = [
    row("shop_default", 0, { ownerKind: "stack", owner: "shop" }),
    overlay("mesh", 1, { subnets: ["10.1.0.0/24"], usage: usage(20, 256, "normal") }),
    overlay("busy", 2, { subnets: ["10.2.0.0/24"], usage: usage(200, 256, "getting_full") }),
    overlay("full", 3, { subnets: ["10.3.0.0/24"], usage: usage(250, 256, "almost_full") }),
    row("clash", 4, { overlaps: [{ name: "mesh", subnet: "10.1.0.0/24", node: "" }] }),
  ];
  const namesOf = (r: readonly AddressMapRow[]) => r.map((x) => x.name);

  it("filters by search and chips", () => {
    // Name, subnet text and owner, whatever the case.
    expect(namesOf(filterMapRows(rows, { q: "SHOP" }))).toEqual(["shop_default"]);
    expect(namesOf(filterMapRows(rows, { q: "10.2.0" }))).toEqual(["busy"]);
    expect(namesOf(filterMapRows(rows, { q: "mes" }))).toEqual(["mesh"]);
    // The chips.
    expect(namesOf(filterMapRows(rows, { show: "overlay" }))).toEqual(["mesh", "busy", "full"]);
    expect(namesOf(filterMapRows(rows, { show: "local" }))).toEqual(["shop_default", "clash"]);
    expect(namesOf(filterMapRows(rows, { show: "filling" }))).toEqual(["busy", "full"]);
    expect(namesOf(filterMapRows(rows, { show: "overlap" }))).toEqual(["clash"]);
    expect(namesOf(filterMapRows(rows, { show: "all" }))).toHaveLength(5);
    // The node.
    expect(namesOf(filterMapRows(rows, { node: "Cluster" }))).toEqual(["mesh", "busy", "full"]);
    // Together.
    expect(namesOf(filterMapRows(rows, { q: "b", show: "filling" }))).toEqual(["busy"]);
  });

  it("narrows the rendered map by the URL search", () => {
    mount({ rows, swarm: true, search: { show: "overlap" } });
    expect(names()).toEqual(["clash"]);
    cleanup();
    mount({ rows, swarm: true, search: { q: "shop" } });
    expect(names()).toEqual(["shop_default"]);
  });
});

describe("sortMapRows", () => {
  it("sorts subnets numerically", () => {
    // The server's order is numeric; the text order of these would put 10 before 9.
    const rows = [
      row("ten", 1, { subnets: ["10.0.10.0/24"] }),
      row("nine", 0, { subnets: ["10.0.9.0/24"] }),
    ];
    expect(sortMapRows(rows, "subnet", "asc").map((r) => r.name)).toEqual(["nine", "ten"]);
    expect(sortMapRows(rows, "subnet", "desc").map((r) => r.name)).toEqual(["ten", "nine"]);
  });

  it("sorts by use, age and name, with the unmeasured last", () => {
    const rows = [
      overlay("low", 0, { usage: usage(10, 256, "normal"), created: "2026-10-03T00:00:00Z" }),
      row("bare", 1, { created: undefined }),
      overlay("high", 2, {
        usage: usage(200, 256, "getting_full"),
        created: "2026-10-01T00:00:00Z",
      }),
      overlay("blind", 3, { created: "2026-10-02T00:00:00Z" }),
    ];
    const by = (key: "used" | "age" | "name", dir: "asc" | "desc") =>
      sortMapRows(rows, key, dir).map((r) => r.name);
    expect(by("used", "asc")).toEqual(["low", "high", "bare", "blind"]);
    expect(by("used", "desc")).toEqual(["high", "low", "bare", "blind"]);
    expect(by("age", "asc")).toEqual(["high", "blind", "low", "bare"]);
    expect(by("age", "desc")).toEqual(["low", "blind", "high", "bare"]);
    expect(by("name", "asc")).toEqual(["bare", "blind", "high", "low"]);
    expect(by("name", "desc")).toEqual(["low", "high", "blind", "bare"]);
  });

  it("sorts equal names by node", () => {
    const rows = [row("web", 0, { node: "b" }), row("web", 1, { node: "a" })];
    expect(sortMapRows(rows, "name", "asc").map((r) => r.node)).toEqual(["a", "b"]);
  });

  it("marks the sorted column and asks for a sort on a click", () => {
    const onSearchChange = vi.fn();
    const rows = [row("b", 0), row("a", 1)];
    mount({ rows, search: { sort: "name", dir: "asc" }, onSearchChange });
    const table = screen.getByRole("table", { name: "Subnet map" });
    const head = (name: string) => within(table).getByRole("columnheader", { name });
    expect(names()).toEqual(["a", "b"]);
    expect(head("Network").getAttribute("aria-sort")).toBe("ascending");
    expect(head("Subnet").getAttribute("aria-sort")).toBe("none");
    expect(head("Scope").getAttribute("aria-sort")).toBeNull();
    fireEvent.click(within(head("Network")).getByRole("button", { name: "Network" }));
    expect(onSearchChange).toHaveBeenLastCalledWith({ sort: "name", dir: "desc" });
    fireEvent.click(within(head("Age")).getByRole("button", { name: "Age" }));
    expect(onSearchChange).toHaveBeenLastCalledWith({ sort: "age", dir: "asc" });
    cleanup();
    mount({ rows, search: { sort: "name", dir: "desc" } });
    expect(names()).toEqual(["b", "a"]);
    expect(
      within(screen.getByRole("table", { name: "Subnet map" }))
        .getByRole("columnheader", { name: "Network" })
        .getAttribute("aria-sort"),
    ).toBe("descending");
  });

  it("starts on the address order", () => {
    mount({ rows: [row("b", 0), row("a", 1)] });
    const table = screen.getByRole("table", { name: "Subnet map" });
    expect(
      within(table).getByRole("columnheader", { name: "Subnet" }).getAttribute("aria-sort"),
    ).toBe("ascending");
    expect(names()).toEqual(["b", "a"]);
  });
});

describe("the map toolbar", () => {
  it("offers the chips as one choice and the node on a swarm", () => {
    const onSearchChange = vi.fn();
    mount({
      rows: [row("a", 0), overlay("b", 1)],
      swarm: true,
      search: { show: "local" },
      onSearchChange,
    });
    const group = screen.getByRole("group", { name: "Show" });
    const chip = (name: string) => within(group).getByRole("radio", { name });
    expect(
      ["All", "Overlays", "Local", "Filling up", "Overlapping"].map((n) => chip(n) && n),
    ).toHaveLength(5);
    expect(chip("Local").getAttribute("aria-checked")).toBe("true");
    fireEvent.click(chip("Overlays"));
    expect(onSearchChange).toHaveBeenLastCalledWith({ show: "overlay" });
    // Pressing the chosen one is All.
    fireEvent.click(chip("Local"));
    expect(onSearchChange).toHaveBeenLastCalledWith({ show: undefined });
    expect(screen.getByRole("searchbox", { name: "Search networks" })).toBeTruthy();
    expect(screen.getByPlaceholderText("Search by name, subnet or owner")).toBeTruthy();
    expect(screen.getByRole("combobox")).toBeTruthy();
  });

  it("leaves the node select out of a standalone host", () => {
    mount({ rows: [row("a", 0)], swarm: false });
    expect(screen.queryByRole("combobox")).toBeNull();
  });
});

describe("the 500-row cap", () => {
  const many = Array.from({ length: 650 }, (_, i) =>
    row(`net-${String(i).padStart(4, "0")}`, i, { subnets: [`10.${i >> 8}.${i & 255}.0/24`] }),
  );

  it("caps rendering at 500 and searches the rest", () => {
    expect(MAP_RENDER_CAP).toBe(500);
    mount({ rows: many });
    expect(names()).toHaveLength(500);
    expect(screen.getByText("Showing the first 500 of 650. Search to find the rest.")).toBeTruthy();
    expect(screen.queryByText("net-0600")).toBeNull();
    cleanup();
    mount({ rows: many, search: { q: "net-0600" } });
    expect(names()).toEqual(["net-0600"]);
    expect(screen.queryByText(/Showing the first/)).toBeNull();
  });
});

describe("overlap flags", () => {
  it("flags overlaps with real text", async () => {
    mount({
      rows: [
        row("mine", 0, {
          overlaps: [{ name: "other_net", subnet: "10.0.0.0/16", node: "" }],
        }),
        row("local", 1, {
          overlaps: [{ name: "side", subnet: "10.0.1.0/24", node: "node-2" }],
        }),
        row("clean", 2),
      ],
    });
    // Real text for assistive technology, not only a tooltip.
    const hidden = screen.getByText("Overlaps other_net");
    expect(hidden.className).toContain("sr-only");
    const trigger = hidden.closest("[tabindex]") as HTMLElement;
    fireEvent.focus(trigger);
    expect(
      (await screen.findAllByText("Overlaps other_net (10.0.0.0/16).")).length,
    ).toBeGreaterThan(0);
    const other = screen.getByText("Overlaps side").closest("[tabindex]") as HTMLElement;
    fireEvent.focus(other);
    expect(
      (await screen.findAllByText("Overlaps side (10.0.1.0/24) on node-2.")).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByText(/Overlaps clean/)).toBeNull();
  });
});

describe("an empty map", () => {
  it("shows the empty state with clear filters", () => {
    const onSearchChange = vi.fn();
    mount({ rows: [row("a", 0)], search: { q: "zzz" }, onSearchChange });
    expect(screen.getByText("No networks match “zzz”.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(onSearchChange).toHaveBeenCalledWith({ q: undefined, show: undefined, node: undefined });
    cleanup();
    mount({ rows: [row("a", 0)], search: { show: "overlay" } });
    expect(screen.getByText("No networks match this filter.")).toBeTruthy();
  });

  it("says so when the environment has no networks", () => {
    mount({ rows: [] });
    expect(screen.getByText("No networks to show.")).toBeTruthy();
  });
});

describe("Addresses used", () => {
  it("shows used addresses for overlays only", async () => {
    mount({
      swarm: true,
      rows: [
        overlay("calm", 0, { usage: usage(20, 256, "normal") }),
        overlay("busy", 1, { usage: usage(200, 256, "getting_full") }),
        overlay("blind", 2),
        row("bridgey", 3),
      ],
    });
    const table = screen.getByRole("table", { name: "Subnet map" });
    const last = (name: string) => {
      const r = within(table)
        .getAllByRole("row")
        .find((x) => within(x).queryByRole("link", { name }) !== null) as HTMLElement;
      return within(r).getAllByRole("cell").at(-1) as HTMLElement;
    };
    expect(within(table).getByRole("columnheader", { name: "Addresses used" })).toBeTruthy();
    // A meter, the numbers and, from 70%, the state as words.
    const calm = last("calm");
    expect(within(calm).getByRole("meter", { name: "calm" })).toBeTruthy();
    expect(calm.textContent).toContain("20 / 256");
    expect(within(calm).queryByText("Getting full")).toBeNull();
    const busy = last("busy");
    expect(busy.textContent).toContain("200 / 256");
    expect(within(busy).getByText("Getting full")).toBeTruthy();
    // An overlay whose count could not be read says why; a local network has nothing to count.
    const blind = last("blind");
    expect(within(blind).queryByRole("meter")).toBeNull();
    const dash = within(blind).getByText("—");
    fireEvent.focus(dash);
    expect(
      (
        await screen.findAllByText(
          "Counting used addresses needs Docker Engine 29 or newer on the manager.",
        )
      ).length,
    ).toBeGreaterThan(0);
    const bridgey = last("bridgey");
    expect(bridgey.textContent).toBe("—");
    expect(within(bridgey).queryByRole("meter")).toBeNull();
  });
});

describe("loading", () => {
  it("draws eight skeleton rows and the toolbar at once", () => {
    mount({ rows: undefined, swarm: true });
    const table = screen.getByRole("table", { name: "Subnet map" });
    expect(within(table).getAllByRole("row")).toHaveLength(9);
    expect(screen.getByRole("searchbox", { name: "Search networks" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Show" })).toBeTruthy();
  });
});
