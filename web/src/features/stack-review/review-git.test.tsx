// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ReviewPage } from "@/features/stack-review/review-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The review of a git stack (plan 09-19, GIT-04, D-17): it says which commit it deploys, in the words of the UI-SPEC
// "Review additions (git stacks)". The review itself is the one every stack has.

const ENV = "e1";
const STACK = "s1";
const D1 = `sha256:${"a1".repeat(32)}`;
const DEPLOYED = "0123456789abcdef0123456789abcdef01234567";
const HEAD = "fedcba9876543210fedcba9876543210fedcba98";

type Git = Record<string, unknown> | undefined;

let git: Git;
let stackGit: boolean;
let client: QueryClient;

const counts = (added = 0, changed = 0, removed = 0) => ({ added, changed, removed });

function reviewBody() {
  return {
    draftVersion: 7,
    images: [
      { service: "web", image: "nginx:1.27", digest: D1, current: D1, state: "pinned", note: "" },
    ],
    needsAdmin: [],
    objects: [],
    pins: { s1_web: `nginx:1.27@${D1}` },
    problems: [],
    services: [],
    summary: {
      services: counts(),
      networks: counts(),
      volumes: counts(),
      secrets: counts(),
      configs: counts(),
      imagesNew: 0,
      imagesPinned: 1,
    },
    ...(git ? { git } : {}),
  };
}

function gitBody(over: Record<string, unknown> = {}) {
  return {
    commit: HEAD,
    ref: "main",
    refKind: "branch",
    folder: "stacks/web",
    repoName: "acme",
    deployedCommit: DEPLOYED,
    touched: 3,
    sourceChanged: false,
    ...over,
  };
}

beforeEach(() => {
  git = gitBody();
  stackGit = true;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}`) {
        return Response.json({
          id: STACK,
          name: "web",
          target: "swarm",
          source: stackGit ? "git" : "inline",
          state: "running",
          lastDeployedAt: "2026-10-19T11:00:00Z",
          lastDeployedBy: "bob",
          hasDraft: true,
          envId: ENV,
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: false,
          ...(stackGit
            ? {
                git: {
                  repoId: "r1",
                  repoName: "acme",
                  refKind: "branch",
                  ref: "main",
                  folder: "stacks/web",
                  composeFiles: ["compose.yaml"],
                  deployedSha: DEPLOYED,
                  headSha: HEAD,
                  behind: 3,
                  behindCapped: false,
                  status: "behind",
                  checking: false,
                  autoDeploy: false,
                  autoDeployPaused: false,
                  pollEnabled: true,
                  pollMinutes: 5,
                },
              }
            : {}),
        });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/review`) return Response.json(reviewBody());
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ReviewPage envId={ENV} stackId={STACK} onDeployed={vi.fn()} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("the review of a git stack", () => {
  it("is titled Pull and redeploy and says which commit of which ref and folder it shows", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Pull and redeploy web" })).toBeTruthy();
    expect(
      await screen.findByText(
        "Bayplan fetched main just now and rendered stacks/web at commit fedcba9. This shows what would change on Prod.",
      ),
    ).toBeTruthy();
  });

  it("names the repository root for the root folder", async () => {
    git = gitBody({ folder: "" });
    mount();
    expect(
      await screen.findByText(
        "Bayplan fetched main just now and rendered the repository root at commit fedcba9. This shows what would change on Prod.",
      ),
    ).toBeTruthy();
  });

  it("puts the commit line in the Summary with how many commits changed the folder", async () => {
    mount();
    const card = (await screen.findByRole("table", { name: "Summary of changes" })).closest(
      "section, div",
    ) as HTMLElement;
    const line = await screen.findByTestId("review-commit-line");
    expect(line.textContent).toBe(
      "Commit fedcba9 on main 3 commits changed this folder since 0123456.",
    );
    expect(within(line).getByText("fedcba9").tagName).toBe("CODE");
    expect(card).toBeTruthy();
  });

  it("reads one commit in the singular", async () => {
    git = gitBody({ touched: 1 });
    mount();
    expect((await screen.findByTestId("review-commit-line")).textContent).toContain(
      "1 commit changed this folder since 0123456.",
    );
  });

  it("leaves out the count when nothing is deployed yet or the source changed", async () => {
    git = gitBody({ deployedCommit: "", touched: 0 });
    mount();
    const line = await screen.findByTestId("review-commit-line");
    expect(line.textContent).toBe("Commit fedcba9 on main");
    cleanup();
    git = gitBody({ sourceChanged: true, touched: 0 });
    mount();
    const again = await screen.findAllByTestId("review-commit-line");
    expect(again.at(-1)?.textContent).toBe("Commit fedcba9 on main");
  });

  it("keeps the ordinary title, body and no commit line for a stack that is not from git", async () => {
    git = undefined;
    stackGit = false;
    mount();
    expect(await screen.findByRole("heading", { name: "Review web" })).toBeTruthy();
    expect(await screen.findByText("Check what will change on Prod, then deploy.")).toBeTruthy();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(screen.queryByTestId("review-commit-line")).toBeNull();
  });
});
