// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { type AddressMapRow, type MapSearch, SubnetMap } from "@/features/address-space/subnet-map";
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
    expect(heads).toEqual(["Network", "Subnet", "Scope", "Driver", "Owner", "Attached", "Age"]);
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
