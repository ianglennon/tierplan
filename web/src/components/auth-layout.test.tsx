// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { AuthLayout } from "@/components/auth-layout";
import "@/i18n";

// The footer has its own test and its own endpoint.
vi.mock("@/components/version-footer", () => ({ VersionFooter: () => null }));

afterEach(cleanup);

// The two images inside a logo: the artwork for light mode and the artwork for dark mode.
function artwork(logo: HTMLElement) {
  const images = Array.from(logo.querySelectorAll("img"));
  const find = (re: RegExp) => images.find((i) => re.test(i.getAttribute("src") ?? ""));
  return { images, light: find(/tierplan-wordmark-light/), dark: find(/tierplan-wordmark-dark/) };
}

describe("the TierPlan logo on the auth screens", () => {
  it("shows the wordmark above the title, named TierPlan", () => {
    render(<AuthLayout title="Homelab" />);
    const logo = screen.getByRole("img", { name: "TierPlan" });
    const heading = screen.getByRole("heading", { level: 1, name: "Homelab" });
    // It comes before the title in the page.
    expect(logo.compareDocumentPosition(heading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(logo).queryAllByRole("img")).toHaveLength(0);
  });

  it("swaps the dark-ink and light-ink artwork by the dark class, both hidden from assistive technology", () => {
    render(<AuthLayout title="Homelab" />);
    const { images, light, dark } = artwork(screen.getByRole("img", { name: "TierPlan" }));
    expect(images).toHaveLength(2);
    expect(light).toBeTruthy();
    expect(dark).toBeTruthy();
    expect(light?.classList.contains("dark:hidden")).toBe(true);
    expect(light?.classList.contains("hidden")).toBe(false);
    expect(dark?.classList.contains("hidden")).toBe(true);
    expect(dark?.classList.contains("dark:block")).toBe(true);
    for (const img of images) {
      expect(img.getAttribute("alt")).toBe("");
      expect(img.getAttribute("aria-hidden")).toBe("true");
    }
  });

  it("is the heading when the title is just TierPlan, with the name not written out again", () => {
    render(<AuthLayout title="TierPlan" subtitle="Create the first account" />);
    const heading = screen.getByRole("heading", { level: 1, name: "TierPlan" });
    const logo = within(heading).getByRole("img", { name: "TierPlan" });
    expect(artwork(logo).images).toHaveLength(2);
    // No text spells the name out: the heading's only content is the logo.
    expect(heading.textContent).toBe("");
    expect(screen.queryByText("TierPlan")).toBeNull();
    expect(screen.getByText("Create the first account")).toBeTruthy();
  });
});
