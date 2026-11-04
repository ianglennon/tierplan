// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FleetNotIncluded, FleetUpdateCoverage } from "@/api/fleet";
import { FleetEmptyUpdates } from "@/features/fleet/fleet-empty-updates";
import "@/i18n";

// The honest empty states of the Updates tab (plan 12-29, D-03, UI-SPEC "Honest empty states"): the
// heading is chosen from what the data supports, and "up to date" is claimed only when every check
// that counts succeeded.

const FIVE_MIN_AGO = new Date(Date.now() - 5 * 60_000).toISOString();

function coverage(extra: Partial<FleetUpdateCoverage> = {}): FleetUpdateCoverage {
  return {
    checkable: 12,
    checked: 12,
    environments: 3,
    failed: 0,
    neverChecked: 0,
    skipped: 0,
    firstCheckAt: "",
    lastCheckedAt: FIVE_MIN_AGO,
    ...extra,
  };
}

const MISSING: FleetNotIncluded = { envId: "e9", envName: "lab", reason: "offline", nodes: 0 };

afterEach(cleanup);

function mount(
  props: Partial<React.ComponentProps<typeof FleetEmptyUpdates>> & {
    coverage?: FleetUpdateCoverage;
  } = {},
) {
  const onShow = vi.fn();
  render(<FleetEmptyUpdates coverage={coverage()} notIncluded={[]} onShow={onShow} {...props} />);
  return onShow;
}

const heading = () => screen.getByRole("heading", { level: 2 }).textContent;

describe("FleetEmptyUpdates", () => {
  it("says Everything is up to date when every check that counts succeeded", () => {
    mount();
    expect(heading()).toBe("Everything is up to date");
    expect(
      screen.getByText(
        "TierPlan checked 12 workloads in 3 environments, the last check 5 min ago.",
      ),
    ).toBeTruthy();
  });

  it("names the interval when it is known", () => {
    mount({ interval: "6 hours" });
    expect(
      screen.getByText(
        "TierPlan checked 12 workloads in 3 environments, the last check 5 min ago. Checks run every 6 hours.",
      ),
    ).toBeTruthy();
  });

  it("says 1 workload in 1 environment, and 1,200 workloads with a separator", () => {
    mount({ coverage: coverage({ checked: 1, checkable: 1, environments: 1 }) });
    expect(
      screen.getByText("TierPlan checked 1 workload in 1 environment, the last check 5 min ago."),
    ).toBeTruthy();
    cleanup();
    mount({ coverage: coverage({ checked: 1200, checkable: 1200, environments: 40 }) });
    expect(
      screen.getByText(
        "TierPlan checked 1,200 workloads in 40 environments, the last check 5 min ago.",
      ),
    ).toBeTruthy();
  });

  it("does not say up to date when checks failed, and offers Show problems", () => {
    const onShow = mount({ coverage: coverage({ failed: 2 }) });
    expect(heading()).toBe("No updates found, but 2 checks failed");
    expect(screen.getByText(/TierPlan can't tell for those workloads/)).toBeTruthy();
    expect(screen.queryByText("Everything is up to date")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show problems" }));
    expect(onShow).toHaveBeenCalledWith("problems");
  });

  it("says one check failed in the singular", () => {
    mount({ coverage: coverage({ failed: 1 }) });
    expect(heading()).toBe("No updates found, but 1 check failed");
  });

  it("says No updates found yet for workloads never checked, and when the first check runs", () => {
    const soon = new Date(Date.now() + 3 * 3_600_000).toISOString();
    mount({ coverage: coverage({ neverChecked: 5, firstCheckAt: soon }) });
    expect(heading()).toBe("No updates found yet");
    const body = screen.getByText(/5 workloads haven't been checked/);
    expect(body.textContent).toMatch(/The first check runs at .+, or choose Check now\.$/);
    expect(screen.queryByText("Everything is up to date")).toBeNull();
  });

  it("says the first check is a moment away when it is due, and is plain when none is scheduled", () => {
    const due = new Date(Date.now() - 60_000).toISOString();
    mount({ coverage: coverage({ neverChecked: 1, firstCheckAt: due }) });
    expect(
      screen.getByText(
        "1 workload hasn't been checked. The first check runs in a moment, or choose Check now.",
      ),
    ).toBeTruthy();
    cleanup();
    mount({ coverage: coverage({ neverChecked: 2, firstCheckAt: "" }) });
    expect(screen.getByText("2 workloads haven't been checked. Choose Check now.")).toBeTruthy();
  });

  it("says Nothing to check yet when no environment is included", () => {
    mount({
      coverage: coverage({ environments: 0, checkable: 0, checked: 0, lastCheckedAt: "" }),
      notIncluded: [MISSING],
    });
    expect(heading()).toBe("Nothing to check yet");
    expect(screen.getByText("No environment is reporting.")).toBeTruthy();
  });

  it("does not claim up to date when no workload follows a tag", () => {
    mount({ coverage: coverage({ checkable: 0, checked: 0, lastCheckedAt: "" }) });
    expect(heading()).toBe("Nothing to check yet");
    expect(screen.queryByText("Everything is up to date")).toBeNull();
  });

  it("says No updates to show when only skipped or ignored ones remain", () => {
    const onShow = mount({ coverage: coverage({ skipped: 4 }) });
    expect(heading()).toBe("No updates to show");
    expect(
      screen.getByText(
        "4 updates are skipped or ignored. Open Skipped and ignored to review them.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Show skipped and ignored" }));
    expect(onShow).toHaveBeenCalledWith("ignored");
  });

  it("says one update is skipped in the singular", () => {
    mount({ coverage: coverage({ skipped: 1 }) });
    expect(
      screen.getByText("1 update is skipped or ignored. Open Skipped and ignored to review it."),
    ).toBeTruthy();
  });

  it("never says Everything is up to date while an environment is not included", () => {
    mount({ notIncluded: [MISSING] });
    expect(screen.queryByText("Everything is up to date")).toBeNull();
    expect(heading()).toBe("No updates found in the environments that reported");
  });

  it("puts a failed check before a missing environment", () => {
    mount({ coverage: coverage({ failed: 1 }), notIncluded: [MISSING] });
    expect(heading()).toBe("No updates found, but 1 check failed");
  });

  it("words an empty Problems view and an empty Skipped and ignored view on their own", () => {
    mount({ show: "problems" });
    expect(heading()).toBe("No problems to show");
    cleanup();
    mount({ show: "problems", notIncluded: [MISSING] });
    expect(heading()).toBe("No problems in the environments that reported");
    cleanup();
    mount({ show: "ignored" });
    expect(heading()).toBe("Nothing is skipped or ignored");
    cleanup();
    mount({ show: "ignored", notIncluded: [MISSING] });
    expect(heading()).toBe("Nothing is skipped or ignored in the environments that reported");
  });
});
