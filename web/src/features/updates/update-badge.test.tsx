// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { UpdateInfo, UpdateState } from "@/api/updates";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UpdateBadge } from "@/features/updates/update-badge";
import "@/i18n";

afterEach(cleanup);

const info = (extra: Partial<UpdateInfo> = {}): UpdateInfo => ({
  state: "not_checked",
  reason: "",
  registry: "registry.example.com",
  repository: "acme/web",
  tag: "1.4",
  runningDigest: `sha256:${"a".repeat(64)}`,
  remoteDigest: `sha256:${"b".repeat(64)}`,
  changedPlatforms: [],
  checkedAt: new Date().toISOString(),
  successAt: "",
  note: "",
  ...extra,
});

function show(extra: Partial<UpdateInfo>, count?: number) {
  return render(
    <TooltipProvider>
      <UpdateBadge info={info(extra)} count={count} />
    </TooltipProvider>,
  );
}

// The words of UI-SPEC "Update state vocabulary", one for each state the server names.
const WORDS: Record<UpdateState, string> = {
  update_available: "Update available",
  updating: "Updating…",
  busy: "Busy",
  queued: "Queued",
  up_to_date: "Up to date",
  check_stale: "Check stale",
  check_failed: "Check failed",
  rolled_back: "Update failed, rolled back",
  rollback_failed: "Rollback failed",
  not_from_registry: "Not from a registry",
  pinned_in_file: "Pinned in file",
  not_checked: "Not checked yet",
  skipped: "Version skipped",
  ignored: "Updates ignored",
};

describe("UpdateBadge", () => {
  it.each(Object.entries(WORDS))("renders %s as %s", (state, word) => {
    show({ state: state as UpdateState });
    expect(screen.getByText(word)).toBeTruthy();
  });
});

// The text a reader gets without hovering: the visually hidden description the badge points at.
function descriptionOf(word: string): string {
  const badge = screen.getByText(word).closest("[aria-describedby]");
  const id = badge?.getAttribute("aria-describedby") ?? "";
  return document.getElementById(id)?.textContent ?? "";
}

describe("UpdateBadge: the vocabulary, accessibly", () => {
  it("draws Up to date as plain text with an icon, not as a badge", () => {
    const { container } = show({ state: "up_to_date" });
    expect(container.querySelector("[data-variant]")).toBeNull();
    expect(screen.getByText("Up to date")).toBeTruthy();
    expect(descriptionOf("Up to date")).toMatch(/^Checked /);
  });

  it("draws every other state as an outline badge", () => {
    for (const state of Object.keys(WORDS) as UpdateState[]) {
      if (state === "up_to_date") continue;
      const { container } = show({ state });
      const badge = container.querySelector("[data-variant]");
      expect(badge?.getAttribute("data-variant"), state).toBe("outline");
      cleanup();
    }
  });

  it("hides every icon from assistive technology", () => {
    for (const state of Object.keys(WORDS) as UpdateState[]) {
      const { container } = show({ state });
      const icons = container.querySelectorAll("svg");
      expect(icons.length, state).toBeGreaterThan(0);
      for (const icon of icons) expect(icon.getAttribute("aria-hidden"), state).toBe("true");
      cleanup();
    }
  });

  it("points the badge at its detail, which is in the page for a reader who never hovers", () => {
    show({ state: "update_available" });
    const text = descriptionOf("Update available");
    expect(text).toContain("Running sha256:aaaaaaaaaaaa, sha256:bbbbbbbbbbbb is available.");
    expect(text).toContain("Checked just now.");
  });

  it("names the platforms that changed", () => {
    show({ state: "update_available", changedPlatforms: ["linux/arm64"] });
    expect(descriptionOf("Update available")).toContain("Changed for linux/arm64.");
  });

  it("says the check may be out of date when the server says so, and keeps the state", () => {
    show({ state: "update_available", note: "may_be_stale" });
    expect(descriptionOf("Update available")).toContain("may be out of date.");
  });

  it("says the running version is gone from the registry", () => {
    show({ state: "update_available", note: "running_index_gone" });
    expect(descriptionOf("Update available")).toContain(
      "The registry no longer has the version this service runs",
    );
  });

  it("words a rate-limited failure and the last success", () => {
    show({
      state: "check_failed",
      reason: "rate_limited",
      successAt: new Date(Date.now() - 2 * 3_600_000).toISOString(),
    });
    expect(descriptionOf("Check failed")).toBe(
      "registry.example.com is limiting requests. TierPlan will try again later. Last successful check 2 h ago.",
    );
  });

  it("ends a failure with It has never succeeded when no check ever did", () => {
    show({ state: "check_failed", reason: "unreachable", successAt: "" });
    const text = descriptionOf("Check failed");
    expect(text).toBe("TierPlan couldn't reach registry.example.com. It has never succeeded.");
  });

  it("words each failure reason from its code, and an unknown code as other", () => {
    const expected: Record<string, string> = {
      auth: "registry.example.com refused TierPlan's credentials. Check them under Registries.",
      not_found: "registry.example.com doesn't have 1.4 of acme/web any more.",
      unsupported: "registry.example.com gave an answer TierPlan can't read.",
      other: "The check didn't finish. TierPlan will try again at the next check.",
    };
    for (const [reason, sentence] of Object.entries(expected)) {
      show({ state: "check_failed", reason: reason as UpdateInfo["reason"] });
      expect(descriptionOf("Check failed")).toContain(sentence);
      cleanup();
    }
    show({ state: "check_failed", reason: "from_the_future" as UpdateInfo["reason"] });
    expect(descriptionOf("Check failed")).toContain(expected.other);
  });

  it("never shows a registry's own text: the badge reads only codes and digests", () => {
    show({ state: "check_failed", reason: "auth" });
    expect(descriptionOf("Check failed")).not.toContain("denied");
  });

  it("shows a stack's count with plurals and grouping", () => {
    show({ state: "update_available" }, 1);
    expect(screen.getByText("1 update")).toBeTruthy();
    cleanup();
    show({ state: "update_available" }, 1200);
    expect(screen.getByText("1,200 updates")).toBeTruthy();
  });

  it("ignores a count on a state that is not Update available", () => {
    show({ state: "check_failed", reason: "auth" }, 3);
    expect(screen.getByText("Check failed")).toBeTruthy();
  });

  it("draws markup in a registry value as literal text", () => {
    const { container } = show({
      state: "check_failed",
      reason: "auth",
      registry: "<b>x</b>.example.com",
    });
    expect(container.querySelector("b")).toBeNull();
    expect(descriptionOf("Check failed")).toContain("<b>x</b>.example.com refused");
    expect(document.body.querySelector("b")).toBeNull();
  });
});
