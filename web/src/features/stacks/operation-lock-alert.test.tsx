// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackLock } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { OperationLockAlert } from "@/features/stacks/operation-lock-alert";
import "@/i18n";
import { TestRouter } from "@/test-router";

// The operation lock (plan 07-18, UI-SPEC "Shared refusals", "Operation lock"): a warning on every stack
// page of an environment while one stack operation runs on it.

afterEach(cleanup);

const lock = (over: Partial<StackLock> = {}): StackLock => ({
  stackId: "s9",
  stackName: "tracer",
  jobId: "j9",
  kind: "deploy",
  startedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
  startedBy: "ada",
  ...over,
});

function mount(l: StackLock) {
  render(
    <TestRouter>
      <TooltipProvider>
        <OperationLockAlert lock={l} envId="e1" envName="Prod" />
      </TooltipProvider>
    </TestRouter>,
  );
}

describe("the operation lock alert", () => {
  it("says which stack holds the environment and since when", async () => {
    mount(lock());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(
      /^Another stack operation is running on Prod: tracer, started 5 min ago\./,
    );
  });

  it("links Open it to the console of the operation that holds it", async () => {
    mount(lock());
    const link = await screen.findByRole("link", { name: "Open it" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s9/deploys/j9");
  });

  it("links to the holder's stack when its job is not known yet", async () => {
    mount(lock({ jobId: "" }));
    const link = await screen.findByRole("link", { name: "Open it" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s9");
  });

  it("draws the stack's name as text", async () => {
    mount(lock({ stackName: "<b>x</b>" }));
    const alert = await screen.findByRole("alert");
    expect(alert.querySelector("b")).toBeNull();
    expect(alert.textContent).toContain("<b>x</b>");
  });
});
