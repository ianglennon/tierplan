// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { EnvironmentItem } from "@/api/environments";
import { TooltipProvider } from "@/components/ui/tooltip";
import { PoolCell, poolPercent, poolRank } from "@/features/environments/pool-cell";
import "@/i18n";

type Space = NonNullable<EnvironmentItem["addressSpace"]>;

const base: Space = {
  worstName: "10.0.0.0/16",
  worstKind: "pool",
  used: 42,
  capacity: 100,
  percent: 42,
  state: "normal",
  unit: "subnets",
  estimated: false,
  orphaned: 0,
};

afterEach(cleanup);

function mount(addressSpace: EnvironmentItem["addressSpace"], offline = false) {
  return render(
    <TooltipProvider>
      <PoolCell addressSpace={addressSpace} offline={offline} />
    </TooltipProvider>,
  );
}

describe("PoolCell", () => {
  it("shows a normal pool as a plain percent", () => {
    const { container } = mount(base);
    expect(screen.getByLabelText("42 percent, normal").textContent).toBe("42%");
    expect(container.querySelector("svg")).toBeNull();
  });

  it("shows a warning icon from 70 percent", () => {
    const { container } = mount({ ...base, percent: 72, used: 72, state: "getting_full" });
    expect(screen.getByLabelText("72 percent, getting full").textContent).toBe("72%");
    expect(container.querySelector("svg.lucide-triangle-alert")).toBeTruthy();
  });

  it("shows a cross from 90 percent", () => {
    const { container } = mount({ ...base, percent: 91, used: 91, state: "almost_full" });
    expect(screen.getByLabelText("91 percent, almost full").textContent).toBe("91%");
    expect(container.querySelector("svg.lucide-circle-x")).toBeTruthy();
  });

  it("follows an estimate with est.", () => {
    mount({ ...base, estimated: true });
    expect(screen.getByLabelText("42 percent, normal").textContent).toBe("42%est.");
  });

  it("shows a dash that reads Not measured when nothing is measured or the host is offline", () => {
    mount(null);
    expect(screen.getByLabelText("Not measured").textContent).toBe("—");
    cleanup();
    mount({ ...base, state: "unknown" });
    expect(screen.getByLabelText("Not measured").textContent).toBe("—");
    cleanup();
    mount(base, true);
    expect(screen.getByLabelText("Not measured").textContent).toBe("—");
  });

  it("names the fullest pool in the tooltip, and says an estimate comes from the defaults", async () => {
    mount({ ...base, estimated: true });
    fireEvent.focus(screen.getByLabelText("42 percent, normal"));
    expect(
      (await screen.findAllByText("10.0.0.0/16 is the fullest: 42 of 100 used."))[0],
    ).toBeTruthy();
    expect((await screen.findAllByText("Estimated from Docker's default pools."))[0]).toBeTruthy();
  });
});

describe("poolRank and poolPercent", () => {
  it("ranks 70 and 90 percent and nothing for a pool that is fine or unknown", () => {
    expect(poolRank({ addressSpace: base })).toBe(0);
    expect(poolRank({ addressSpace: { ...base, state: "getting_full" } })).toBe(1);
    expect(poolRank({ addressSpace: { ...base, state: "almost_full" } })).toBe(2);
    expect(poolRank({ addressSpace: { ...base, state: "unknown" } })).toBe(0);
    expect(poolRank({ addressSpace: null })).toBe(0);
    expect(poolPercent({ addressSpace: null })).toBeUndefined();
    expect(poolPercent({ addressSpace: base })).toBe(42);
  });
});
