// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackGit } from "@/api/stack-git";
import { TooltipProvider } from "@/components/ui/tooltip";
import { BehindBadge, SyncErrorBadge } from "@/features/stack-git/sync-badges";
import "@/i18n";

const DEPLOYED = "0123456789abcdef0123456789abcdef01234567";

function makeGit(over: Partial<StackGit> = {}): StackGit {
  return {
    repoId: "r1",
    repoName: "stacks repo",
    repoHost: "github.com",
    repoPath: "acme/stacks",
    refKind: "branch",
    ref: "main",
    folder: "",
    composeFiles: ["compose.yaml"],
    deployedSha: DEPLOYED,
    headSha: DEPLOYED,
    behind: 0,
    behindCapped: false,
    status: "in_sync",
    checking: false,
    autoDeploy: false,
    autoDeployPaused: false,
    ...over,
  };
}

afterEach(cleanup);

function mount(git: StackGit | undefined, which: "behind" | "error") {
  return render(
    <TooltipProvider>
      {which === "behind" ? <BehindBadge git={git} /> : <SyncErrorBadge git={git} />}
    </TooltipProvider>,
  );
}

// The Tooltip's words are also the badge's description, for a reader who never opens it.
function descriptionOf(badge: HTMLElement): string | undefined {
  return (
    document.getElementById(badge.getAttribute("aria-describedby") ?? "")?.textContent ?? undefined
  );
}

describe("BehindBadge", () => {
  it("says Behind with the count and an arrow, and describes it in words", () => {
    mount(makeGit({ status: "behind", behind: 3 }), "behind");
    const badge = screen.getByText("Behind 3");
    expect(badge.querySelector("svg")).toBeTruthy();
    expect(descriptionOf(badge)).toBe(
      "3 commits changed this stack's folder since it was deployed. Review update.",
    );
  });

  it("reads the singular for one commit", () => {
    mount(makeGit({ status: "behind", behind: 1 }), "behind");
    expect(descriptionOf(screen.getByText("Behind 1"))).toBe(
      "1 commit changed this stack's folder since it was deployed. Review update.",
    );
  });

  it("marks a count that is a floor", () => {
    mount(makeGit({ status: "behind", behind: 2000, behindCapped: true }), "behind");
    expect(descriptionOf(screen.getByText("Behind 2000+"))).toBe(
      "At least 2000 commits changed this stack's folder since it was deployed. Review update.",
    );
  });

  it("says Behind without a count when the source changed, since no commits were counted", () => {
    mount(makeGit({ status: "behind", behind: 1, reasonCode: "source_changed" }), "behind");
    const badge = screen.getByText("Behind");
    expect(descriptionOf(badge)).toBe(
      "The repository, ref or folder is not the one that is deployed. Review update.",
    );
  });

  it("still says Behind for a stack whose check failed after it was last seen behind", () => {
    mount(makeGit({ status: "error", reasonCode: "fetch_failed", behind: 2 }), "behind");
    expect(screen.getByText("Behind 2")).toBeTruthy();
  });

  it("shows its Tooltip on hover", async () => {
    mount(makeGit({ status: "behind", behind: 2 }), "behind");
    const badge = screen.getByText("Behind 2");
    fireEvent.pointerMove(badge);
    fireEvent.mouseEnter(badge);
    expect((await screen.findByRole("tooltip")).textContent).toBe(
      "2 commits changed this stack's folder since it was deployed. Review update.",
    );
  });

  it.each([
    ["in sync", makeGit()],
    ["in error and never seen behind", makeGit({ status: "error", reasonCode: "ref_gone" })],
    ["unknown", makeGit({ status: "unknown" })],
    ["not from git", undefined],
  ])("draws nothing for a stack that is %s", (_name, git) => {
    const { container } = mount(git, "behind");
    expect(container.textContent).toBe("");
  });
});

describe("SyncErrorBadge", () => {
  it("says Sync error with an icon and the reason in words", () => {
    mount(
      makeGit({
        status: "error",
        reasonCode: "ref_gone",
        reasonDetail: "The branch or tag no longer exists.",
      }),
      "error",
    );
    const badge = screen.getByText("Sync error");
    expect(badge.querySelector("svg")).toBeTruthy();
    expect(descriptionOf(badge)).toBe("main no longer exists.");
  });

  it("names the host of a changed host key", () => {
    mount(makeGit({ status: "error", reasonCode: "host_key_changed" }), "error");
    expect(descriptionOf(screen.getByText("Sync error"))).toBe(
      "The host key for github.com has changed.",
    );
  });

  it("falls back to the server's sentence for a code this build has no words for", () => {
    mount(
      makeGit({
        status: "error",
        reasonCode: "from_a_newer_server" as StackGit["reasonCode"],
        reasonDetail: "Something else went wrong.",
      }),
      "error",
    );
    expect(descriptionOf(screen.getByText("Sync error"))).toBe("Something else went wrong.");
  });

  it.each([
    ["in sync", makeGit()],
    ["behind", makeGit({ status: "behind", behind: 2 })],
    ["not from git", undefined],
  ])("draws nothing for a stack that is %s", (_name, git) => {
    const { container } = mount(git, "error");
    expect(container.textContent).toBe("");
  });

  it("draws the reason as text, never as markup", () => {
    mount(
      makeGit({
        status: "error",
        reasonCode: "checks_failed",
        headSha: DEPLOYED,
        reasonDetail: "compose.yaml: <img src=x onerror=alert(1)>",
      }),
      "error",
    );
    const badge = screen.getByText("Sync error");
    expect(descriptionOf(badge)).toBe(
      "Commit 0123456 didn't pass the checks: compose.yaml: <img src=x onerror=alert(1)>.",
    );
    expect(document.querySelector("img")).toBeNull();
  });
});
