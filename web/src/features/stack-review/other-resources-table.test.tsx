// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackReviewObject } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { OtherResourcesTable } from "@/features/stack-review/other-resources-table";
import "@/i18n";

// The Other resources card of the Changes tab (plan 07-15, STK-06, D-07, D-11).

afterEach(() => cleanup());

const object = (over: Partial<StackReviewObject>): StackReviewObject => ({
  change: "added",
  detail: "",
  kind: "network",
  name: "front",
  ...over,
});

describe("OtherResourcesTable", () => {
  it("is a table of the change, the kind, the name and the detail", () => {
    render(
      <OtherResourcesTable
        objects={[
          object({
            change: "changed",
            kind: "secret",
            name: "db_pass",
            detail: "New version. The old one is removed after the deploy.",
          }),
        ]}
      />,
    );
    expect(screen.getByRole("heading", { name: "Other resources" })).toBeTruthy();
    const table = screen.getByRole("table", { name: "Other resources" });
    for (const col of ["Change", "Kind", "Name", "Detail"]) {
      expect(within(table).getByRole("columnheader", { name: col })).toBeTruthy();
    }
    const row = within(table).getByText("db_pass").closest("tr") as HTMLElement;
    expect(within(row).getByText("Changed")).toBeTruthy();
    expect(within(row).getByText("Secret")).toBeTruthy();
    expect(
      within(row).getByText("New version. The old one is removed after the deploy."),
    ).toBeTruthy();
  });

  it("words an external object and a removed one", () => {
    render(
      <OtherResourcesTable
        objects={[
          object({
            change: "external",
            kind: "network",
            name: "proxy",
            detail: "External. TierPlan doesn't touch it.",
          }),
          object({
            change: "removed",
            kind: "config",
            name: "old_conf",
            detail: "Removed from the stack. Deleted with the deploy.",
          }),
        ]}
      />,
    );
    const ext = screen.getByText("proxy").closest("tr") as HTMLElement;
    expect(within(ext).getByText("External")).toBeTruthy();
    expect(within(ext).getByText("Network")).toBeTruthy();
    expect(within(ext).getByText("External. TierPlan doesn't touch it.")).toBeTruthy();
    const gone = screen.getByText("old_conf").closest("tr") as HTMLElement;
    expect(within(gone).getByText("Removed")).toBeTruthy();
    expect(within(gone).getByText("Config")).toBeTruthy();
    expect(within(gone).getByText("Removed from the stack. Deleted with the deploy.")).toBeTruthy();
  });

  it("words the detail itself when the server sent none for an external or a removed object", () => {
    render(
      <OtherResourcesTable
        objects={[
          object({ change: "external", name: "proxy" }),
          object({ change: "removed", kind: "secret", name: "gone" }),
          object({ change: "added", name: "front" }),
        ]}
      />,
    );
    expect(screen.getByText("External. TierPlan doesn't touch it.")).toBeTruthy();
    expect(screen.getByText("Removed from the stack. Deleted with the deploy.")).toBeTruthy();
    // A new object with nothing to add reads as a dash, never blank.
    const added = screen.getByText("front").closest("tr") as HTMLElement;
    expect(within(added).getByText("—")).toBeTruthy();
  });

  it("words a network the deploy recreates and a volume TierPlan keeps with the server's sentences (D-46, D-56)", () => {
    render(
      <OtherResourcesTable
        objects={[
          object({
            change: "changed",
            kind: "network",
            name: "demo_back",
            detail:
              "Its settings differ from the compose file. The deploy recreates it, which restarts the containers attached to it.",
          }),
          object({
            change: "changed",
            kind: "volume",
            name: "demo_data",
            detail: "Its settings differ from the compose file. TierPlan keeps it as it is.",
          }),
        ]}
      />,
    );
    const table = screen.getByRole("table", { name: "Other resources" });
    const net = within(table).getByText("demo_back").closest("tr") as HTMLElement;
    expect(within(net).getByText("Changed")).toBeTruthy();
    expect(within(net).getByText("Network")).toBeTruthy();
    expect(
      within(net).getByText(
        "Its settings differ from the compose file. The deploy recreates it, which restarts the containers attached to it.",
      ),
    ).toBeTruthy();
    const vol = within(table).getByText("demo_data").closest("tr") as HTMLElement;
    expect(within(vol).getByText("Changed")).toBeTruthy();
    expect(within(vol).getByText("Volume")).toBeTruthy();
    expect(
      within(vol).getByText(
        "Its settings differ from the compose file. TierPlan keeps it as it is.",
      ),
    ).toBeTruthy();
    // The glyph is only for scanning: the word carries the meaning, and the glyph is hidden from a reader.
    expect(net.querySelector('[aria-hidden="true"]')?.textContent).toBe("~");
  });

  it("keeps a network and a volume of one name as two rows", () => {
    render(
      <OtherResourcesTable
        objects={[
          object({ change: "changed", kind: "network", name: "data", detail: "net" }),
          object({ change: "changed", kind: "volume", name: "data", detail: "vol" }),
        ]}
      />,
    );
    expect(screen.getAllByText("data")).toHaveLength(2);
    expect(screen.getByText("net")).toBeTruthy();
    expect(screen.getByText("vol")).toBeTruthy();
  });

  it("draws a name with markup in it as text", () => {
    render(<OtherResourcesTable objects={[object({ name: "<i>net</i>" })]} />);
    expect(screen.getByText("<i>net</i>").querySelector("i")).toBeNull();
  });

  it("is not drawn when the stack has no other resources", () => {
    render(<OtherResourcesTable objects={[]} />);
    expect(screen.queryByRole("heading", { name: "Other resources" })).toBeNull();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows the small-subnet badge", async () => {
    render(
      <TooltipProvider>
        <OtherResourcesTable
          objects={[object({ name: "backend", subnet: "10.0.4.0/26", smallSubnetDefault: true })]}
        />
      </TooltipProvider>,
    );
    const row = screen.getByText("backend").closest("tr") as HTMLElement;
    expect(within(row).getByText(/network backend:/)).toBeTruthy();
    expect(within(row).getByText("10.0.4.0/26").className).toContain("font-mono");
    const badge = within(row).getByText("Small-subnet default");
    badge.focus();
    await waitFor(() =>
      expect(
        screen.getAllByText(
          "TierPlan picked this subnet because small stack subnets are on for this environment. Your compose file isn't changed.",
        ).length,
      ).toBeGreaterThan(0),
    );
  });

  it("shows a plain subnet without the badge", () => {
    render(
      <TooltipProvider>
        <OtherResourcesTable objects={[object({ name: "backend", subnet: "10.9.0.0/24" })]} />
      </TooltipProvider>,
    );
    const row = screen.getByText("backend").closest("tr") as HTMLElement;
    expect(within(row).getByText("10.9.0.0/24")).toBeTruthy();
    expect(within(row).queryByText("Small-subnet default")).toBeNull();
  });
});
