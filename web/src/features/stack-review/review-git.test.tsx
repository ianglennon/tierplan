// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
const NEWER = "1111111111111111111111111111111111111111";

type Git = Record<string, unknown> | undefined;

let git: Git;
let stackGit: boolean;
let client: QueryClient;
// openCard waits for the commits to arrive, which close the card past 5, and opens it.
async function openCard() {
  const trigger = await screen.findByRole("button", { name: "What changed in the repository" });
  await waitFor(() => expect(trigger.getAttribute("aria-expanded")).toBe("false"));
  fireEvent.click(trigger);
}

// The answer of the commits route; undefined makes it fail.
let commits: Record<string, unknown> | undefined;
let commitRequests: string[];
// The head the stack's own page last saw, and every request made, in order.
let stackHead: string;
let sent: { method: string; path: string; body?: unknown }[];
let reviewReads: number;

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
    headCommit: HEAD,
    ...over,
  };
}

function commit(i: number, over: Record<string, unknown> = {}) {
  return {
    sha: i.toString(16).padStart(40, "0"),
    subject: `change ${i}`,
    author: "ada",
    when: `2026-10-19T1${i % 10}:00:00Z`,
    ...over,
  };
}

function commitList(n: number, over: Record<string, unknown> = {}) {
  return {
    commits: Array.from({ length: n }, (_, i) => commit(i + 1)),
    total: n,
    capped: false,
    available: true,
    ...over,
  };
}

beforeEach(() => {
  git = gitBody();
  commits = commitList(2);
  commitRequests = [];
  stackHead = HEAD;
  sent = [];
  reviewReads = 0;
  stackGit = true;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({
        method: req.method,
        path,
        body:
          req.method === "POST"
            ? await req
                .clone()
                .json()
                .catch(() => undefined)
            : undefined,
      });
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
                  headSha: stackHead,
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
      if (path === `/api/env/${ENV}/stacks/${STACK}/review`) {
        reviewReads++;
        return Response.json(reviewBody());
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/draft`) return Response.json({ version: 7 });
      if (path === `/api/env/${ENV}/stacks/${STACK}/git/pull`) {
        // The pull refreshes the draft to the newest commit, which is then what the review shows.
        git = gitBody({ commit: NEWER, headCommit: NEWER });
        return Response.json({
          draftVersion: 8,
          commit: NEWER,
          ref: "main",
          deployedCommit: DEPLOYED,
          touched: 4,
        });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/deploys`) {
        return Response.json({ jobId: "j1", revision: 3 }, { status: 202 });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/git/commits`) {
        commitRequests.push(new URL(req.url).search);
        return commits ? Response.json(commits) : Response.json({}, { status: 500 });
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

describe("the Commits card of a git review", () => {
  it("lists the commits that changed the folder, newest first, open when there are few", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Commits that changed stacks/web" });
    expect(screen.getByRole("heading", { name: "What changed in the repository" })).toBeTruthy();
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    expect(within(rows[0] as HTMLElement).getByText("0000000").tagName).toBe("CODE");
    expect(within(rows[0] as HTMLElement).getByText("change 1")).toBeTruthy();
    expect(rows[0]?.textContent).toContain("ada, ");
    expect(rows[0]?.querySelector("time")?.getAttribute("datetime")).toBe("2026-10-19T11:00:00Z");
    // The review asks for the commits up to the one it deploys.
    expect(commitRequests).toEqual([`?head=${HEAD}`]);
  });

  it("names the repository root when the folder is the root", async () => {
    git = gitBody({ folder: "" });
    mount();
    expect(
      await screen.findByRole("table", { name: "Commits that changed the repository root" }),
    ).toBeTruthy();
  });

  it("is closed by default past 5 commits and opens on its title", async () => {
    commits = commitList(7);
    mount();
    const trigger = await screen.findByRole("button", { name: "What changed in the repository" });
    await waitFor(() => expect(trigger.getAttribute("aria-expanded")).toBe("false"));
    expect(screen.queryByRole("table", { name: /Commits that changed/ })).toBeNull();
    fireEvent.click(trigger);
    const table = await screen.findByRole("table", { name: /Commits that changed/ });
    expect(within(table).getAllByRole("row")).toHaveLength(8);
  });

  it("shows 10 rows then Show all, which shows every commit", async () => {
    commits = commitList(12);
    mount();
    await openCard();
    const table = await screen.findByRole("table", { name: /Commits that changed/ });
    expect(within(table).getAllByRole("row")).toHaveLength(11);
    fireEvent.click(screen.getByRole("button", { name: "Show all 12" }));
    expect(within(table).getAllByRole("row")).toHaveLength(13);
    expect(screen.getByRole("button", { name: "Show fewer" })).toBeTruthy();
  });

  it("says how many more there are when the list names only the newest", async () => {
    commits = commitList(3, { total: 80 });
    mount();
    expect(await screen.findByText("Showing the newest 3 of 80.")).toBeTruthy();
  });

  it("says Bayplan can't list the commits when the route can't, and the review goes on", async () => {
    commits = { commits: [], total: 0, capped: false, available: false };
    mount();
    expect(
      await screen.findByText("Bayplan can't list the commits, so only the changes are shown."),
    ).toBeTruthy();
    expect(screen.getByRole("table", { name: "Summary of changes" })).toBeTruthy();
  });

  it("says the same when the request fails", async () => {
    commits = undefined;
    mount();
    expect(
      await screen.findByText("Bayplan can't list the commits, so only the changes are shown."),
    ).toBeTruthy();
  });

  it("is left out for a first deploy and for a change of source, and asks for nothing", async () => {
    git = gitBody({ deployedCommit: "", touched: 0 });
    mount();
    await screen.findByTestId("review-commit-line");
    expect(screen.queryByRole("heading", { name: "What changed in the repository" })).toBeNull();
    cleanup();
    git = gitBody({ sourceChanged: true, touched: 0 });
    mount();
    await screen.findAllByTestId("review-commit-line");
    expect(screen.queryByRole("heading", { name: "What changed in the repository" })).toBeNull();
    expect(commitRequests).toEqual([]);
  });

  it("is left out of the review of a stack that is not from git", async () => {
    git = undefined;
    stackGit = false;
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(screen.queryByRole("heading", { name: "What changed in the repository" })).toBeNull();
  });

  it("draws a long subject on one line with the whole subject in the page, and wraps it once all are shown", async () => {
    const long = `${"a very long subject ".repeat(20)}<b>not bold</b>`;
    commits = commitList(12);
    (commits.commits as Record<string, unknown>[])[0] = commit(1, { subject: long });
    mount();
    await openCard();
    const clamped = await screen.findByText(long);
    expect(clamped.className).toContain("truncate");
    // Text, never markup.
    expect(clamped.querySelector("b")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show all 12" }));
    const wrapped = await screen.findByText(long);
    expect(wrapped.className).not.toContain("truncate");
    expect(wrapped.className).toContain("break-words");
  });
});

const IMAGE_NOTE =
  "Only images whose reference changed in the repository are looked up again. The others keep the digest they run.";

describe("the Images card of a git review", () => {
  it("says which images are looked up again", async () => {
    mount();
    expect(await screen.findByText(IMAGE_NOTE)).toBeTruthy();
  });

  it("has no such note for a stack that is not from git", async () => {
    git = undefined;
    stackGit = false;
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(screen.queryByText(IMAGE_NOTE)).toBeNull();
  });
});

describe("the moved-head note of a git review", () => {
  it("says the branch moved on after the review, and offers to review the new commit", async () => {
    git = gitBody({ headCommit: NEWER });
    mount();
    expect(await screen.findByText("main has moved on")).toBeTruthy();
    expect(
      screen.getByText(
        "This deploy uses fedcba9, the commit you reviewed. main is now at 1111111.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Review 1111111" })).toBeTruthy();
  });

  it("learns of a head the stack's page saw after the review was prepared", async () => {
    stackHead = NEWER;
    mount();
    expect(await screen.findByText("main has moved on")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Review 1111111" })).toBeTruthy();
  });

  it("is not shown while the head is the reviewed commit, or when no head is known", async () => {
    mount();
    await screen.findByTestId("review-commit-line");
    expect(screen.queryByText("main has moved on")).toBeNull();
    cleanup();
    git = gitBody({ headCommit: "" });
    stackHead = "";
    mount();
    await screen.findAllByTestId("review-commit-line");
    expect(screen.queryByText("main has moved on")).toBeNull();
  });

  it("is not shown for a fixed commit, which does not move", async () => {
    git = gitBody({ refKind: "commit", ref: HEAD, headCommit: NEWER });
    mount();
    await screen.findByTestId("review-commit-line");
    expect(screen.queryByText(/has moved on/)).toBeNull();
  });

  it("pulls again and reviews the new commit when Review is pressed", async () => {
    git = gitBody({ headCommit: NEWER });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Review 1111111" }));
    expect(
      await screen.findByText(
        "Bayplan fetched main just now and rendered stacks/web at commit 1111111. This shows what would change on Prod.",
      ),
    ).toBeTruthy();
    expect(sent.filter((r) => r.path.endsWith("/git/pull"))).toHaveLength(1);
    expect(reviewReads).toBe(2);
    expect(screen.queryByText("main has moved on")).toBeNull();
  });

  it("still deploys the commit that was reviewed", async () => {
    git = gitBody({ headCommit: NEWER });
    mount();
    await screen.findByText("main has moved on");
    fireEvent.click(await screen.findByRole("button", { name: "Deploy stack" }));
    await waitFor(() => expect(sent.some((r) => r.path.endsWith("/deploys"))).toBe(true));
    expect(sent.find((r) => r.path.endsWith("/deploys"))?.body).toMatchObject({ draftVersion: 7 });
    expect(sent.some((r) => r.path.endsWith("/git/pull"))).toBe(false);
  });
});
