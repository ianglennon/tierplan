// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackUpdate } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackUpdateCell } from "@/features/stacks/stack-update-cell";
import "@/i18n";

afterEach(cleanup);

const stackUpdate = (extra: Partial<StackUpdate> = {}): StackUpdate => ({
  state: "up_to_date",
  count: 0,
  notCheckable: null,
  ...extra,
});

function show(update: StackUpdate | undefined) {
  return render(
    <TooltipProvider>
      <StackUpdateCell update={update} />
    </TooltipProvider>,
  );
}

describe("StackUpdateCell: the stack aggregate (12-10, UI-SPEC 'Stack aggregate')", () => {
  it("reads '1 update' for one service with an update and '2 updates' for two", () => {
    show(stackUpdate({ state: "update_available", count: 1 }));
    expect(screen.getByText("1 update")).toBeTruthy();
    cleanup();
    show(stackUpdate({ state: "update_available", count: 2 }));
    expect(screen.getByText("2 updates")).toBeTruthy();
  });

  it("groups a large count with the locale's separators", () => {
    show(stackUpdate({ state: "update_available", count: 1200 }));
    expect(screen.getByText("1,200 updates")).toBeTruthy();
  });

  it("says in words, for a keyboard or touch reader, which services the count is of", () => {
    show(stackUpdate({ state: "update_available", count: 2 }));
    expect(
      screen.getByText("2 services of this stack have an update available.", { selector: "span" }),
    ).toBeTruthy();
  });

  it("reads a dash with the reason, and never 'Up to date', when nothing can be checked", () => {
    show(stackUpdate({ state: "none", notCheckable: ["pinned_in_file"] }));
    expect(screen.getByText("—")).toBeTruthy();
    expect(
      screen.getByText("No service of this stack can be checked: Pinned in file."),
    ).toBeTruthy();
    expect(screen.queryByText("Up to date")).toBeNull();
  });

  it("names both reasons when a stack has pinned services and services from no registry", () => {
    show(stackUpdate({ state: "none", notCheckable: ["pinned_in_file", "not_from_registry"] }));
    expect(
      screen.getByText(
        "No service of this stack can be checked: Pinned in file, Not from a registry.",
      ),
    ).toBeTruthy();
  });

  it("leaves a reason code it has no word for out, and says no reason when none is left", () => {
    show(stackUpdate({ state: "none", notCheckable: ["from_a_newer_server" as never] }));
    expect(screen.getByText("No service of this stack can be checked.")).toBeTruthy();
    expect(screen.queryByText(/from_a_newer_server/)).toBeNull();
  });

  it("reads a dash alone when the server sent no roll-up", () => {
    show(undefined);
    expect(screen.getByText("—")).toBeTruthy();
    expect(screen.queryByText("Up to date")).toBeNull();
  });

  it.each([
    ["check_failed", "Check failed"],
    ["not_checked", "Not checked yet"],
    ["rollback_failed", "Rollback failed"],
    ["up_to_date", "Up to date"],
    ["ignored", "Updates ignored"],
  ] as const)("reads the aggregate state %s as '%s'", (state, word) => {
    show(stackUpdate({ state }));
    expect(screen.getByText(word)).toBeTruthy();
  });
});
