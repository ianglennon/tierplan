// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { IngressOwnedState } from "@/features/service-editor/ingress-owned-state";
import "@/i18n";
import { TestRouter } from "@/test-router";
import { SERVICE_ENV, SERVICE_ID } from "@/test-service";

// TierPlan's ingress proxy is made and updated by TierPlan, so its edit page is this read-only
// state (UI-SPEC S3, D-29).

afterEach(cleanup);

function renderState() {
  return render(
    <TestRouter>
      <IngressOwnedState envId={SERVICE_ENV} serviceId={SERVICE_ID} />
    </TestRouter>,
  );
}

describe("IngressOwnedState", () => {
  it("says TierPlan ingress can't be edited here, and why", () => {
    renderState();
    expect(
      screen.getByRole("heading", { name: "TierPlan ingress can't be edited here", level: 2 }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "TierPlan creates and updates this service itself. Switch ingress off to remove it.",
      ),
    ).toBeTruthy();
  });

  it("goes back to the service's Overview with an outline button 44px high on touch", () => {
    renderState();
    const back = screen.getByRole("link", { name: "Back to service" });
    expect(back.getAttribute("href")).toBe(`/env/${SERVICE_ENV}/services/${SERVICE_ID}`);
    expect(back.getAttribute("data-variant")).toBe("outline");
    expect(back.className).toContain("max-md:h-11");
  });

  it("renders no field, review or apply control", () => {
    const { container } = renderState();
    expect(container.querySelector("input, textarea, select")).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByText(/Review changes|Apply changes/)).toBeNull();
  });
});
