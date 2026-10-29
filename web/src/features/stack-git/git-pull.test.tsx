// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useLocation } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { StackGit } from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { GitStrip } from "@/features/stack-git/git-strip";
import { PullAndRedeployAction } from "@/features/stacks/stack-actions";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Pull & redeploy… and Review update (plan 09-19, GIT-04, D-17): both fetch the newest commit into the draft, then open
// the Review. There is no other path to a deploy.

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const ENV = "e1";
const DEPLOYED = "0123456789abcdef0123456789abcdef01234567";
const HEAD = "fedcba9876543210fedcba9876543210fedcba98";

let role: "viewer" | "operator";
let pulls: Array<{ draftVersion: number }>;
let pullAnswer: { status: number; body: unknown };
let client: QueryClient;

function git(over: Partial<StackGit> = {}): StackGit {
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
    headSha: HEAD,
    behind: 2,
    behindCapped: false,
    status: "behind",
    checking: false,
    autoDeploy: false,
    autoDeployPaused: false,
    pollEnabled: true,
    pollMinutes: 5,
    ...over,
  };
}

function stack(g: StackGit): StackDetail {
  return {
    id: "s1",
    envId: ENV,
    name: "web",
    target: "standalone",
    source: "git",
    state: "running",
    git: g,
  } as StackDetail;
}

beforeEach(() => {
  role = "operator";
  pulls = [];
  pullAnswer = {
    status: 200,
    body: { draftVersion: 8, commit: HEAD, ref: "main", deployedCommit: DEPLOYED, touched: 2 },
  };
  toasts.error.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (req.method === "GET" && path === `/api/env/${ENV}/stacks/s1/draft`) {
        return Response.json({ version: 7, files: [], vars: [], composeFiles: ["compose.yaml"] });
      }
      if (req.method === "POST" && path === `/api/env/${ENV}/stacks/s1/git/pull`) {
        pulls.push((await req.json()) as { draftVersion: number });
        return Response.json(pullAnswer.body, { status: pullAnswer.status });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function Where() {
  return <p data-testid="where">{useLocation().pathname}</p>;
}

function mount(ui: React.ReactNode) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          {ui}
          <Where />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("Pull & redeploy…", () => {
  it("pulls at the draft version it read, then opens the Review", async () => {
    mount(<PullAndRedeployAction stack={stack(git())} />);
    fireEvent.click(await screen.findByRole("button", { name: "Pull & redeploy…" }));
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/review"),
    );
    expect(pulls).toEqual([{ draftVersion: 7 }]);
  });

  it("stays where it is and says why when the draft moved", async () => {
    pullAnswer = {
      status: 409,
      body: { type: "draft_changed", title: "Conflict", status: 409, detail: "moved" },
    };
    mount(<PullAndRedeployAction stack={stack(git())} />);
    fireEvent.click(await screen.findByRole("button", { name: "Pull & redeploy…" }));
    await waitFor(() => expect(toasts.error).toHaveBeenCalledTimes(1));
    expect(toasts.error.mock.calls[0][0]).toBe(
      "Couldn't pull main. The draft changed while you were pulling. Reload it and try again.",
    );
    expect(screen.getByTestId("where").textContent).toBe("/");
  });

  it("says the repository can't be reached for a failed fetch", async () => {
    pullAnswer = {
      status: 422,
      body: { type: "unreachable", title: "Unprocessable", status: 422, detail: "down" },
    };
    mount(<PullAndRedeployAction stack={stack(git())} />);
    fireEvent.click(await screen.findByRole("button", { name: "Pull & redeploy…" }));
    await waitFor(() => expect(toasts.error).toHaveBeenCalledTimes(1));
    expect(toasts.error.mock.calls[0][0]).toBe(
      "Couldn't pull main. The repository can't be reached.",
    );
  });

  it("is off with the role it takes for a Viewer, and pulls nothing", async () => {
    role = "viewer";
    mount(<PullAndRedeployAction stack={stack(git())} />);
    const button = await screen.findByRole("button", { name: "Pull & redeploy…" });
    fireEvent.click(button);
    expect(pulls).toEqual([]);
    expect(screen.getByTestId("where").textContent).toBe("/");
  });
});

describe("Review update", () => {
  it("is on the strip while the stack is behind, and pulls then opens the Review", async () => {
    mount(<GitStrip stack={stack(git())} />);
    fireEvent.click(await screen.findByRole("button", { name: "Review update" }));
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/review"),
    );
    expect(pulls).toEqual([{ draftVersion: 7 }]);
  });

  it("is not on the strip of a stack in sync", async () => {
    mount(<GitStrip stack={stack(git({ status: "in_sync", behind: 0, headSha: DEPLOYED }))} />);
    await screen.findByRole("region", { name: "Git sync" });
    expect(screen.queryByRole("button", { name: "Review update" })).toBeNull();
  });
});
