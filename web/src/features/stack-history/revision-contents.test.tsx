// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackRevision, StackRevisionDetail } from "@/api/stack-history";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RevisionContents } from "@/features/stack-history/revision-contents";
import "@/i18n";
import { TestRouter } from "@/test-router";

// The Contents tab of a revision shows the subnets the deploy gave its networks (D-20): a rollback marks
// the ones it re-used.

afterEach(() => cleanup());

function detail(
  subnets: Record<string, string>,
  origin: StackRevision["origin"] = "deploy",
): StackRevisionDetail {
  return {
    deployedSeq: 2,
    files: [{ path: "compose.yaml", secret: false }],
    vars: [],
    pins: [],
    rendered: "",
    subnets,
    yaml: { hunks: [], truncated: false, tooLarge: false },
    item: {
      seq: 2,
      origin,
      restoredFrom: origin === "rollback" ? 1 : 0,
      outcome: "good",
      createdBy: "ada",
      createdAt: "2026-10-20T10:00:00Z",
      failedService: "",
      code: "",
      reason: "",
      deployed: true,
      images: [],
      pinned: 0,
      adoptedFrom: "",
      commit: "",
      tree: "",
      syncTrigger: "",
    },
  } as unknown as StackRevisionDetail;
}

function mount(d: StackRevisionDetail) {
  render(
    <TestRouter>
      <TooltipProvider>
        <RevisionContents detail={d} stackName="shop" />
      </TooltipProvider>
    </TestRouter>,
  );
}

describe("RevisionContents networks", () => {
  it("lists recorded subnets", () => {
    mount(detail({ shop_backend: "10.0.4.0/26", shop_front: "10.0.4.64/26" }));
    expect(screen.getByRole("heading", { name: "Networks" })).toBeTruthy();
    const back = screen.getByText(/network backend:/).closest("li") as HTMLElement;
    expect(within(back).getByText("10.0.4.0/26").className).toContain("font-mono");
    expect(screen.getByText(/network front:/)).toBeTruthy();
    expect(screen.getByText("2 subnets")).toBeTruthy();
    expect(screen.queryByText("reused on rollback")).toBeNull();
  });

  it("marks rollback reuse", () => {
    mount(detail({ shop_backend: "10.0.4.0/26" }, "rollback"));
    expect(screen.getByText("reused on rollback")).toBeTruthy();
  });

  it("keeps a name that does not start with the stack name", () => {
    mount(detail({ other: "10.1.0.0/26" }));
    expect(screen.getByText(/network other:/)).toBeTruthy();
  });

  it("omits the section when nothing is recorded", () => {
    mount(detail({}));
    expect(screen.queryByRole("heading", { name: "Networks" })).toBeNull();
    expect(screen.queryByText(/network /)).toBeNull();
  });
});
