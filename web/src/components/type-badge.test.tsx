// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TypeBadge } from "@/components/type-badge";
import "@/i18n";

afterEach(cleanup);

describe("TypeBadge", () => {
  it("shows Swarm and an outline Local socket badge for a local swarm manager", () => {
    render(<TypeBadge envType="swarm" type="swarm-manager" isLocalSocket />);
    expect(screen.getByText("Swarm")).toBeTruthy();
    const local = screen.getByText("Local socket");
    expect(local.getAttribute("data-variant")).toBe("outline");
    // The role stays on the node list, not on the badge.
    expect(screen.queryByText("Swarm manager")).toBeNull();
  });

  it("shows Standalone for a standalone agent environment, with no Local socket badge", () => {
    render(<TypeBadge envType="standalone" isLocalSocket={false} />);
    expect(screen.getByText("Standalone")).toBeTruthy();
    expect(screen.queryByText("Local socket")).toBeNull();
  });

  it("reads the type from the engine role when only that is known", () => {
    render(<TypeBadge type="swarm-worker" />);
    expect(screen.getByText("Swarm")).toBeTruthy();
  });

  it("renders nothing for a pending environment whose type is not known yet", () => {
    const { container } = render(<TypeBadge isLocalSocket={false} />);
    expect(container.textContent).toBe("");
  });

  it("never truncates either badge", () => {
    render(<TypeBadge envType="swarm" isLocalSocket />);
    expect(screen.getByText("Swarm").className).toContain("shrink-0");
    expect(screen.getByText("Local socket").className).toContain("shrink-0");
  });
});
