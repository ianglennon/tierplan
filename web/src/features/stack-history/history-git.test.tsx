// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { HistoryPage } from "@/features/stack-history/history-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The History of a git stack (plan 09-20, GIT-03, D-08): the commit of every revision and the source a roll back
// restores, in the words of the UI-SPEC "History additions (git stacks)".

const ENV = "e1";
const COMMIT_NEW = "fedcba9876543210fedcba9876543210fedcba98";
const COMMIT_OLD = "0123456789abcdef0123456789abcdef01234567";
const TREE_NEW = "aabbccddeeff00112233445566778899aabbccdd";

type Json = Record<string, unknown>;

let stackGit: boolean;
let autoDeploy: boolean;
let revisions: Json[];
let details: Record<number, Json>;
let client: QueryClient;

const SOURCE = {
  repoId: "r1",
  repoName: "acme",
  refKind: "branch",
  ref: "main",
  folder: "stacks/web",
  composeFiles: ["compose.yaml", "compose.prod.yaml"],
};

function revision(seq: number, over: Json = {}): Json {
  return {
    seq,
    origin: "deploy",
    restoredFrom: 0,
    outcome: "good",
    createdBy: "ada",
    createdAt: "2026-10-20T10:00:00Z",
    failedService: "",
    code: "",
    reason: "",
    deployed: false,
    images: [],
    pinned: 0,
    adoptedFrom: "",
    commit: "",
    tree: "",
    syncTrigger: "",
    ...over,
  };
}

const gitRevision = (seq: number, commit: string, over: Json = {}) =>
  revision(seq, { commit, tree: TREE_NEW, source: SOURCE, ...over });

function detailOf(item: Json): Json {
  return {
    item,
    deployedSeq: 3,
    rendered: "",
    files: [{ path: "compose.yaml", secret: false }],
    vars: [],
    pins: [],
    yaml: { hunks: [], lines: 0 },
  };
}

beforeEach(() => {
  stackGit = true;
  autoDeploy = false;
  revisions = [
    gitRevision(3, COMMIT_NEW, { deployed: true }),
    gitRevision(2, COMMIT_OLD),
    revision(1, { pinned: 1 }),
  ];
  details = Object.fromEntries(revisions.map((r) => [r.seq as number, detailOf(r)]));
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) {
        return Response.json({
          id: "s1",
          name: "web",
          target: "swarm",
          source: stackGit ? "git" : "inline",
          state: "running",
          lastDeployedAt: "2026-10-20T10:00:00Z",
          lastDeployedBy: "ada",
          hasDraft: false,
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
                  repoHost: "github.com",
                  repoPath: "acme/stacks",
                  refKind: "branch",
                  ref: "main",
                  folder: "stacks/web",
                  composeFiles: ["compose.yaml"],
                  deployedSha: COMMIT_NEW,
                  headSha: COMMIT_NEW,
                  behind: 0,
                  behindCapped: false,
                  status: "in_sync",
                  checking: false,
                  autoDeploy,
                  autoDeployPaused: false,
                  pollEnabled: true,
                  pollMinutes: 5,
                },
              }
            : {}),
        });
      }
      if (path === `/api/env/${ENV}/stacks/s1/revisions`) {
        return Response.json({ items: revisions, capped: false, kept: 20 });
      }
      const one = /\/revisions\/(\d+)$/.exec(path);
      if (one && details[Number(one[1])]) return Response.json(details[Number(one[1])]);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(rev?: number) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <HistoryPage envId={ENV} stackId="s1" rev={rev} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function openContents() {
  const tab = await screen.findByRole("tab", { name: "Contents" });
  fireEvent.mouseDown(tab);
  fireEvent.click(tab);
}

const headers = (table: HTMLElement) =>
  within(table)
    .getAllByRole("columnheader")
    .map((h) => h.textContent);

describe("the Commit column", () => {
  it("comes after Kind for a git stack, with the short commit as a link on a known host", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Revisions of web" });
    expect(headers(table)).toEqual([
      "Revision",
      "Kind",
      "Commit",
      "Result",
      "Who",
      "When",
      "Images",
      "Actions",
    ]);
    const row = within(table).getAllByRole("row")[1] as HTMLElement;
    const link = within(row).getByRole("link", { name: /^Open commit fedcba9 on github\.com/ });
    expect(link.getAttribute("href")).toBe(`https://github.com/acme/stacks/commit/${COMMIT_NEW}`);
    expect(within(link).getByText("fedcba9").tagName).toBe("CODE");
  });

  it("shows a dash for a revision from before the stack used git", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Revisions of web" });
    const row = within(table)
      .getAllByRole("row")
      .find((r) => r.querySelector("td:first-child a")?.textContent === "#1") as HTMLElement;
    const cells = within(row).getAllByRole("cell");
    // Revision, Kind, Commit
    expect(cells[2]?.textContent).toBe("—");
  });

  it("is not there for a stack that is not from git", async () => {
    stackGit = false;
    revisions = [revision(1, { deployed: true })];
    details = { 1: detailOf(revisions[0] as Json) };
    mount();
    const table = await screen.findByRole("table", { name: "Revisions of web" });
    expect(headers(table)).not.toContain("Commit");
  });
});

describe("the Roll back note of a git stack", () => {
  it("says rolling back pauses auto-deploy, under the Roll back action, when auto-deploy is on", async () => {
    autoDeploy = true;
    mount(2);
    const action = await screen.findByRole("button", { name: /Roll back/ });
    const note = await screen.findByText("Rolling back pauses auto-deploy.");
    expect(action.parentElement?.contains(note)).toBe(true);
  });

  it("is not there while auto-deploy is off, for a stack not from git, or for the deployed revision", async () => {
    mount(2);
    await screen.findByRole("button", { name: /Roll back/ });
    expect(screen.queryByText("Rolling back pauses auto-deploy.")).toBeNull();
    cleanup();

    autoDeploy = true;
    stackGit = false;
    mount(2);
    await screen.findByRole("button", { name: /Roll back/ });
    expect(screen.queryByText("Rolling back pauses auto-deploy.")).toBeNull();
    cleanup();

    stackGit = true;
    mount(3);
    await screen.findByText("This is the deployed revision.");
    expect(screen.queryByText("Rolling back pauses auto-deploy.")).toBeNull();
  });
});

describe("the revision card of a git stack", () => {
  it("adds the commit to the line that says who made the revision", async () => {
    mount(3);
    const heading = await screen.findByRole("heading", { name: /Revision/ });
    const line = heading.nextElementSibling as HTMLElement;
    expect(line.textContent).toContain("Deploy by ada");
    expect(line.textContent).toContain(" · commit fedcba9");
    expect(within(line).getByText("fedcba9").tagName).toBe("CODE");
  });

  it("leaves it out for a revision from before the stack used git", async () => {
    mount(1);
    const heading = await screen.findByRole("heading", { name: /Revision/ });
    expect((heading.nextElementSibling as HTMLElement).textContent).not.toContain("commit");
  });

  it("shows the Source in Contents: repository, ref, full commit with Copy, folder tree, compose files", async () => {
    mount(3);
    await openContents();
    const table = await screen.findByRole("table", { name: "Source of revision #3" });
    const rows = within(table).getAllByRole("row");
    const cell = (label: string) =>
      (
        rows.find((r) => r.querySelector("th")?.textContent === label) as HTMLElement
      )?.querySelector("td") as HTMLElement;
    expect(cell("Repository").textContent).toBe("acme");
    expect(cell("Ref").textContent).toBe("main");
    expect(cell("Commit").textContent).toContain(COMMIT_NEW);
    expect(within(cell("Commit")).getByRole("button", { name: "Copy commit ID" })).toBeTruthy();
    expect(cell("Folder tree").textContent).toBe("aabbccd");
    expect(cell("Folder").textContent).toBe("stacks/web");
    expect(cell("Compose files").textContent).toBe("compose.yamlcompose.prod.yaml");
    expect(
      screen.getByText(
        "A roll back restores this revision from what Coaming stored. It never needs the repository.",
      ),
    ).toBeTruthy();
  });

  it("has no Source table for a revision from before the stack used git", async () => {
    mount(1);
    await openContents();
    await screen.findByRole("table", { name: "Files of revision #1" });
    expect(screen.queryByRole("table", { name: /Source of revision/ })).toBeNull();
    expect(screen.queryByText(/It never needs the repository/)).toBeNull();
  });
});
