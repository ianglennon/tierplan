// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackReviewObject } from "@/api/stacks";
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
            detail: "External. Bayplan doesn't touch it.",
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
    expect(within(ext).getByText("External. Bayplan doesn't touch it.")).toBeTruthy();
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
    expect(screen.getByText("External. Bayplan doesn't touch it.")).toBeTruthy();
    expect(screen.getByText("Removed from the stack. Deleted with the deploy.")).toBeTruthy();
    // A new object with nothing to add reads as a dash, never blank.
    const added = screen.getByText("front").closest("tr") as HTMLElement;
    expect(within(added).getByText("—")).toBeTruthy();
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
});
