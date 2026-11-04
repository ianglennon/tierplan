// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useRouterState } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { UpdateInfo, UpdateState } from "@/api/updates";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UpdateBadge } from "@/features/updates/update-badge";
import "@/i18n";
import { TestRouter } from "@/test-router";

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
  task: false,
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

describe("UpdateBadge: a skipped version", () => {
  const skip = (extra: Partial<NonNullable<UpdateInfo["ignore"]>> = {}) => ({
    mode: "skip" as const,
    scope: "workload" as const,
    setBy: "Ada",
    setAt: new Date(Date.now() - 2 * 3_600_000).toISOString(),
    digest: `sha256:${"c".repeat(64)}`,
    ...extra,
  });

  it("says who skipped which version and when, and that a newer one will show", () => {
    show({ state: "skipped", ignore: skip() });
    expect(descriptionOf("Version skipped")).toBe(
      "Ada skipped sha256:cccccccccccc 2 h ago. A newer version will show again.",
    );
  });

  it("says Someone when the person is gone", () => {
    show({ state: "skipped", ignore: skip({ setBy: "" }) });
    expect(descriptionOf("Version skipped")).toContain("Someone skipped sha256:cccccccccccc");
  });

  it("draws markup in a name as literal text", () => {
    const { container } = show({ state: "skipped", ignore: skip({ setBy: "<i>Ada</i>" }) });
    expect(container.querySelector("i")).toBeNull();
    expect(descriptionOf("Version skipped")).toContain("<i>Ada</i> skipped");
  });
});

describe("UpdateBadge: ignored updates", () => {
  const held = (extra: Partial<NonNullable<UpdateInfo["ignore"]>> = {}) => ({
    mode: "ignore" as const,
    scope: "workload" as const,
    setBy: "Ada",
    setAt: new Date(Date.now() - 3 * 86_400_000).toISOString(),
    digest: "",
    ...extra,
  });

  it("says who ignored updates for the workload, and when", () => {
    show({ state: "ignored", ignore: held() });
    expect(descriptionOf("Updates ignored")).toBe("Ada ignored updates for this workload 3 d ago.");
  });

  it("names the image when the ignore covers the image", () => {
    show({ state: "ignored", ignore: held({ scope: "image" }), repository: "library/nginx" });
    expect(descriptionOf("Updates ignored")).toBe("Ada ignored updates for nginx:1.4 3 d ago.");
  });

  it("says the label did it, with no person and no time", () => {
    show({ state: "ignored", ignore: held({ scope: "label", setBy: "", setAt: "" }) });
    expect(descriptionOf("Updates ignored")).toBe("Ignored by the tierplan.updates=ignore label.");
  });

  it("falls back to a plain sentence when the server names no ignore", () => {
    show({ state: "ignored" });
    expect(descriptionOf("Updates ignored")).toBe("Updates are ignored for this service.");
  });

  it("draws markup in a name or an image as literal text", () => {
    const { container } = show({
      state: "ignored",
      ignore: held({ scope: "image", setBy: "<b>Ada</b>" }),
      repository: "acme/<i>x</i>",
    });
    expect(container.querySelector("b")).toBeNull();
    expect(container.querySelector("i")).toBeNull();
    expect(descriptionOf("Updates ignored")).toContain(
      "<b>Ada</b> ignored updates for acme/<i>x</i>:1.4",
    );
  });
});

// The failure a rolled-back state names (UpdateInfoBody.failure, 12-22).
const failed = (extra: Partial<NonNullable<UpdateInfo["failure"]>> = {}) => ({
  digest: `sha256:${"b".repeat(64)}`,
  reason: "didnt_start",
  at: new Date(Date.now() - 2 * 3_600_000).toISOString(),
  rollbackFailed: false,
  jobId: "",
  ...extra,
});

describe("UpdateBadge: a rolled-back update (IMG-07, D-12)", () => {
  it("says who rolled it back, when and why, and that the version won't be offered again", () => {
    render(
      <TooltipProvider>
        <UpdateBadge info={info({ state: "rolled_back", failure: failed() })} name="web" />
      </TooltipProvider>,
    );
    expect(descriptionOf("Update failed, rolled back")).toBe(
      "TierPlan rolled web back 2 h ago because it didn't start. It won't offer sha256:bbbbbbbbbbbb again until a newer version appears. You can still update by hand.",
    );
  });

  it("words a restart as the reason without a subject", () => {
    render(
      <TooltipProvider>
        <UpdateBadge
          info={info({ state: "rolled_back", failure: failed({ reason: "server_restarted" }) })}
          name="web"
        />
      </TooltipProvider>,
    );
    expect(descriptionOf("Update failed, rolled back")).toContain(
      "because TierPlan restarted during this update.",
    );
  });

  it("is a plain badge when the update's console is not named", () => {
    render(
      <TooltipProvider>
        <UpdateBadge info={info({ state: "rolled_back", failure: failed() })} name="web" />
      </TooltipProvider>,
    );
    expect(screen.queryByRole("button")).toBeNull();
  });

  function Where() {
    return (
      <span data-testid="where">{useRouterState({ select: (s) => s.location.pathname })}</span>
    );
  }

  function showWithRouter(extra: Partial<UpdateInfo>, props: { stackId?: string } = {}) {
    return render(
      <TestRouter path="/env/e1">
        <TooltipProvider>
          <UpdateBadge info={info(extra)} name="web" envId="e1" {...props} />
          <Where />
        </TooltipProvider>
      </TestRouter>,
    );
  }

  it("is a button that opens the update's console, where there is one to open", async () => {
    showWithRouter({ state: "rolled_back", failure: failed({ jobId: "j1" }) });
    const button = screen.getByRole("button", { name: /Update failed, rolled back/ });
    expect(button.getAttribute("aria-describedby")).toBeTruthy();
    fireEvent.click(button);
    expect(await screen.findByText("/env/e1/update-jobs/j1")).toBeTruthy();
  });

  it("opens the stack's own console for a workload a managed stack owns", async () => {
    showWithRouter(
      { state: "rolled_back", failure: failed({ jobId: "j9" }) },
      { stackId: "stk-1" },
    );
    fireEvent.click(screen.getByRole("button", { name: /Update failed, rolled back/ }));
    expect(await screen.findByText("/env/e1/stacks/stk-1/deploys/j9")).toBeTruthy();
  });

  it("is a button for a failed rollback too", async () => {
    showWithRouter({
      state: "rollback_failed",
      failure: failed({ jobId: "j2", rollbackFailed: true }),
    });
    fireEvent.click(screen.getByRole("button", { name: /Rollback failed/ }));
    expect(await screen.findByText("/env/e1/update-jobs/j2")).toBeTruthy();
  });

  it("stays a plain element in every state that opens nothing", () => {
    for (const state of ["update_available", "up_to_date", "check_failed", "skipped"] as const) {
      showWithRouter({ state, failure: failed({ jobId: "j1" }) });
      expect(screen.queryByRole("button"), state).toBeNull();
      cleanup();
    }
  });

  it("draws a workload name with markup as text", () => {
    const { container } = render(
      <TooltipProvider>
        <UpdateBadge info={info({ state: "rolled_back", failure: failed() })} name="<b>web</b>" />
      </TooltipProvider>,
    );
    expect(container.querySelector("b")).toBeNull();
    expect(descriptionOf("Update failed, rolled back")).toContain("rolled <b>web</b> back");
  });
});

describe("UpdateBadge: an image that could not be pulled (user decision, D-14)", () => {
  const pulled = () =>
    info({ state: "rolled_back", failure: failed({ reason: "pull_failed", jobId: "j3" }) });

  it("says the image could not be pulled and nothing was changed, never that it was rolled back", () => {
    render(
      <TestRouter path="/env/e1">
        <TooltipProvider>
          <UpdateBadge info={pulled()} name="web" envId="e1" />
        </TooltipProvider>
      </TestRouter>,
    );
    expect(screen.getByText("Couldn't pull the image")).toBeTruthy();
    const text = descriptionOf("Couldn't pull the image");
    expect(text).toContain("TierPlan couldn't pull the image for sha256:bbbbbbbbbbbb 2 h ago");
    expect(text).toContain("nothing was changed on web");
    expect(text).toContain("You can still try it with Update anyway.");
    expect(document.body.textContent).not.toMatch(/rolled back/i);
  });
});
