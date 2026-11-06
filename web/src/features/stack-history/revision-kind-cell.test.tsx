// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackRevision } from "@/api/stack-history";
import { RevisionKindCell } from "@/features/stack-history/revision-kind-cell";
import "@/i18n";
import { TestRouter } from "@/test-router";

// How a revision came to be, as History words it (plan 12-19, UI-SPEC "Existing custom components"): a one-click
// image update is "Image update", the Update images deploy keeps "Update images".

function item(over: Partial<StackRevision> = {}): StackRevision {
  return {
    seq: 4,
    origin: "deploy",
    restoredFrom: 0,
    outcome: "good",
    createdBy: "ada",
    createdAt: "2026-10-20T10:00:00Z",
    failedService: "",
    code: "",
    reason: "",
    deployed: false,
    images: [],
    pinned: 0,
    adoptedFrom: "",
    commit: "",
    tree: "",
    syncTrigger: "",
    imageUpdate: false,
    ...over,
  };
}

function mount(over: Partial<StackRevision>) {
  render(
    <TestRouter>
      <RevisionKindCell item={item(over)} envId="e1" stackId="s1" />
    </TestRouter>,
  );
}

afterEach(() => cleanup());

describe("RevisionKindCell", () => {
  it("reads Image update for a one-click update", () => {
    mount({ origin: "update", imageUpdate: true });
    expect(screen.getByText("Image update")).toBeTruthy();
    expect(screen.queryByText("Update images")).toBeNull();
  });

  it("keeps Update images for a revision of the Update images deploy", () => {
    mount({ origin: "update", imageUpdate: false });
    expect(screen.getByText("Update images")).toBeTruthy();
    expect(screen.queryByText("Image update")).toBeNull();
  });

  it("keeps Roll back with the revision it copied, and Deploy, whatever imageUpdate says", () => {
    mount({ origin: "rollback", restoredFrom: 2 });
    expect(screen.getByText("Roll back")).toBeTruthy();
    expect(screen.getByRole("link", { name: "#2" })).toBeTruthy();
    cleanup();
    mount({ origin: "deploy", imageUpdate: true });
    expect(screen.getByText("Deploy")).toBeTruthy();
  });
});
