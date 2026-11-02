// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { NodesBanner } from "@/features/environments/nodes-banner";
import "@/i18n";

afterEach(cleanup);

describe("NodesBanner", () => {
  it("names the one node that is not reporting, in the singular", () => {
    render(<NodesBanner notReporting={["n2"]} />);
    expect(screen.getByRole("alert").textContent).toBe(
      "1 node isn't reporting: n2. The totals below leave it out.",
    );
  });

  it("names two and three nodes in full, in the plural", () => {
    render(<NodesBanner notReporting={["n1", "n2", "n3"]} />);
    expect(screen.getByRole("alert").textContent).toBe(
      "3 nodes aren't reporting: n1, n2, n3. The totals below leave them out.",
    );
    cleanup();
    render(<NodesBanner notReporting={["n1", "n2"]} />);
    expect(screen.getByRole("alert").textContent).toBe(
      "2 nodes aren't reporting: n1, n2. The totals below leave them out.",
    );
  });

  it("lists three names and counts the rest when there are more", () => {
    render(<NodesBanner notReporting={["n1", "n2", "n3", "n4", "n5"]} />);
    expect(screen.getByRole("alert").textContent).toBe(
      "5 nodes aren't reporting: n1, n2, n3 and 2 more. The totals below leave them out.",
    );
    cleanup();
    render(<NodesBanner notReporting={["n1", "n2", "n3", "n4"]} />);
    expect(screen.getByRole("alert").textContent).toContain("n1, n2, n3 and 1 more.");
  });

  it("renders nothing for none, for null and for undefined", () => {
    const { container, rerender } = render(<NodesBanner notReporting={[]} />);
    expect(container.firstChild).toBeNull();
    rerender(<NodesBanner notReporting={null} />);
    expect(container.firstChild).toBeNull();
    rerender(<NodesBanner />);
    expect(container.firstChild).toBeNull();
  });

  it("shows a hostile node name as text, never as markup (T-02-75)", () => {
    const evil = '<img src=x onerror="alert(1)">';
    render(<NodesBanner notReporting={[evil]} />);
    const banner = screen.getByRole("alert");
    expect(banner.querySelector("img")).toBeNull();
    expect(banner.textContent).toContain(evil);
  });
});
