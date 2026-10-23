// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { StackLink } from "@/features/stacks/stack-link";
import "@/i18n";
import { TestRouter } from "@/test-router";

// StackLink (plan 07-30, STK-10, UI-SPEC "Links on other pages"): a stack name on another page is a link
// to the Bayplan stack when the server says Bayplan manages it, and plain text when it does not.

afterEach(cleanup);

function mount(props: { name: string; bayplanStackId?: string }) {
  render(
    <TestRouter>
      <p data-testid="host">
        <StackLink envId="env-1" {...props} />
      </p>
    </TestRouter>,
  );
}

describe("StackLink", () => {
  it("links the name to the Bayplan stack when the server names it", () => {
    mount({ name: "shop", bayplanStackId: "stk-1" });
    const link = screen.getByRole("link", { name: "shop" });
    expect(link.getAttribute("href")).toBe("/env/env-1/stacks/stk-1");
  });

  it.each([
    ["has no Bayplan stack ID", undefined],
    ["has an empty Bayplan stack ID", ""],
  ])("draws plain text when the stack %s", (_name, bayplanStackId) => {
    mount({ name: "legacy", bayplanStackId });
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByTestId("host").textContent).toBe("legacy");
  });

  it("draws a name from Docker as text, never as markup", () => {
    mount({ name: "<b>shop</b>", bayplanStackId: "stk-1" });
    expect(document.querySelector("b")).toBeNull();
    expect(screen.getByRole("link", { name: "<b>shop</b>" })).toBeTruthy();
  });

  it("builds the address from the ID by the router, never from the label's text", () => {
    mount({ name: "shop", bayplanStackId: "stk-1" });
    // The link goes to the stack page of this environment; the name never takes part in it.
    expect(screen.getByRole("link").getAttribute("href")).not.toContain("shop");
  });
});
