// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { EditorView } from "@codemirror/view";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { StackOwnedState } from "@/features/service-editor/stack-owned-state";
import { renderSpec, SPEC_FORMAT_KEY } from "@/features/spec-editor/spec-format";
import "@/i18n";
import { TestRouter } from "@/test-router";
import { editSpec, SERVICE_ENV, SERVICE_ID } from "@/test-service";

// A service a stack owns is changed through the stack, so its edit page is this read-only state
// (plan 06-19, D-05, UI-SPEC "Service editor").

afterEach(() => {
  cleanup();
  localStorage.clear();
});

function renderState(tierplanStackId?: string) {
  return render(
    <TestRouter>
      <StackOwnedState
        envId={SERVICE_ENV}
        serviceId={SERVICE_ID}
        name="api"
        stack="shop"
        tierplanStackId={tierplanStackId}
        spec={editSpec()}
      />
    </TestRouter>,
  );
}

describe("StackOwnedState", () => {
  it("says the service is managed by its stack, and where to change it", () => {
    renderState();
    expect(screen.getByRole("heading", { name: "api is managed by stack shop" })).toBeTruthy();
    expect(
      screen.getByText(
        "Change its settings through the stack so the next deploy doesn't overwrite them.",
      ),
    ).toBeTruthy();
  });

  it("goes back to the service", () => {
    renderState();
    const back = screen.getByRole("link", { name: "Back to api" });
    expect(back.getAttribute("href")).toBe(`/env/${SERVICE_ENV}/services/${SERVICE_ID}`);
  });

  it("offers Open stack beside Back to {name} when TierPlan manages the stack", () => {
    renderState("stk-1");
    const open = screen.getByRole("link", { name: "Open stack" });
    expect(open.getAttribute("href")).toBe(`/env/${SERVICE_ENV}/stacks/stk-1`);
    // The accent button is the stack; going back stays the outline one.
    expect(open.getAttribute("data-variant")).toBe("default");
    expect(screen.getByRole("link", { name: "Back to api" }).getAttribute("data-variant")).toBe(
      "outline",
    );
    expect(screen.getByRole("heading", { name: "api is managed by stack shop" })).toBeTruthy();
  });

  it("offers no Open stack for a stack TierPlan does not manage", () => {
    renderState();
    expect(screen.queryByRole("link", { name: "Open stack" })).toBeNull();
    expect(screen.getByRole("link", { name: "Back to api" })).toBeTruthy();
  });

  it("shows the spec read-only, hidden values as the server sent them", async () => {
    renderState();
    const box = await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(box.getAttribute("aria-readonly")).toBe("true");
    const dom = document.querySelector<HTMLElement>(".cm-editor");
    const view = dom ? EditorView.findFromDOM(dom) : null;
    expect(view?.state.doc.toString()).toBe(renderSpec(editSpec(), "yaml"));
    expect(view?.state.doc.toString()).toContain("DB_PASSWORD=<hidden>");
  });

  it("shows the spec in the format the person chose", async () => {
    localStorage.setItem(SPEC_FORMAT_KEY, "json");
    renderState();
    await screen.findByRole("textbox", { name: "Service spec, JSON" });
  });

  it("offers nothing to edit or review", async () => {
    renderState();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(screen.queryByRole("button", { name: "Review changes" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Discard edits" })).toBeNull();
  });
});
