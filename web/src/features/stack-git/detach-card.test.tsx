// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { StackGit } from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { GitTab } from "@/features/stack-git/git-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Detach from git, in the Advanced section of the Git tab (plan 09-31, GIT-02, D-25): collapsed and for Admins of the
// environment only; the dialog asks first, with focus on keeping the tracking, and a detach leads to the Overview.

const ENV = "e1";
const DEPLOYED = "0123456789abcdef0123456789abcdef01234567";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

function makeGit(): StackGit {
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
    checkedAt: "2026-10-28T10:00:00Z",
    checking: false,
    autoDeploy: false,
    autoDeployPaused: false,
    pollEnabled: true,
    pollMinutes: 5,
  };
}

function makeStack(git: StackGit | undefined, over: Partial<StackDetail> = {}): StackDetail {
  return {
    id: "s1",
    name: "web",
    target: "standalone",
    source: git ? "git" : "inline",
    state: "running",
    lastDeployedAt: "",
    lastDeployedBy: "",
    hasDraft: false,
    envId: ENV,
    sourceRef: "",
    createdBy: "ada",
    createdAt: "2026-10-19T10:00:00Z",
    stopped: false,
    deployedRevision: 3,
    git,
    ...over,
  } as StackDetail;
}

let git: StackGit | undefined;
let revision: number | undefined;
let hasDraft: boolean;
let role: "viewer" | "operator" | "admin";
// How the detach route answers, and how often it was called.
let detachAnswer: () => Response;
let detaches: number;
let client: QueryClient;

beforeEach(() => {
  git = makeGit();
  revision = 3;
  hasDraft = false;
  role = "admin";
  detaches = 0;
  detachAnswer = () => new Response(null, { status: 204 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  toasts.success.mockReset();
  toasts.error.mockReset();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === "/api/environments")
        return Response.json({ items: [{ id: ENV, name: "Homelab" }] });
      if (path === `/api/env/${ENV}/stacks/s1/git/detach` && req.method === "POST") {
        detaches++;
        const res = detachAnswer();
        if (res.ok) git = undefined;
        return res;
      }
      if (path === `/api/env/${ENV}/stacks/s1/git`) {
        if (!git) {
          return Response.json(
            { type: "stack_not_git", title: "Conflict", status: 409, detail: "no" },
            { status: 409 },
          );
        }
        return Response.json(git);
      }
      if (path === `/api/env/${ENV}/stacks/s1`) {
        return Response.json(makeStack(git, { deployedRevision: revision, hasDraft }));
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
  const path = useRouterState({ select: (s) => s.location.pathname });
  return <span data-testid="where">{path}</span>;
}

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter path={`/env/${ENV}/stacks/s1/git`}>
        <TooltipProvider>
          <GitTab envId={ENV} stackId="s1" />
          <Where />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The Updates card is the first to load: once it is there the whole column is.
async function loaded() {
  await screen.findByRole("region", { name: "Updates" });
}

async function openAdvanced() {
  await loaded();
  fireEvent.click(await screen.findByRole("button", { name: "Advanced" }));
  return screen.findByRole("button", { name: "Detach from git…" });
}

describe("Detach, in the Advanced section", () => {
  it("is closed by default and holds the card once opened", async () => {
    mount();
    await loaded();
    const advanced = await screen.findByRole("button", { name: "Advanced" });
    expect(advanced.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("button", { name: "Detach from git…" })).toBeNull();
    fireEvent.click(advanced);
    expect(advanced.getAttribute("aria-expanded")).toBe("true");
    expect(await screen.findByRole("button", { name: "Detach from git…" })).toBeTruthy();
    expect(
      screen.getByText(
        "Turn web back into an inline stack you edit in Bayplan. The files now deployed become an editable draft. Bayplan stops checking the repository, removes the webhook and turns off auto-deploy. Nothing running changes.",
      ),
    ).toBeTruthy();
  });

  it("comes last, after the Deploying card", async () => {
    mount();
    await loaded();
    const advanced = await screen.findByRole("button", { name: "Advanced" });
    const sections = Array.from(document.querySelectorAll("section")).map(
      (s) => s.textContent ?? "",
    );
    const deploying = sections.findIndex((s) => s.includes("Deploying"));
    const last = sections.findIndex((s) => s.includes("Advanced"));
    expect(deploying).toBeGreaterThanOrEqual(0);
    expect(last).toBeGreaterThan(deploying);
    expect(advanced).toBeTruthy();
  });

  it.each(["operator", "viewer"] as const)("is not there for a %s", async (who) => {
    role = who;
    mount();
    await loaded();
    // The section is hidden while the permissions load too: wait until they are known, and then look.
    await waitFor(() => expect(client.getQueryState(["me"])?.status).toBe("success"));
    await screen.findByRole("region", { name: "Webhook" });
    expect(screen.queryByRole("button", { name: "Advanced" })).toBeNull();
    expect(screen.queryByText("Detach from git")).toBeNull();
  });

  it("asks first, names the revision and the repository, and focuses keeping the tracking", async () => {
    mount();
    fireEvent.click(await openAdvanced());
    const dialog = await screen.findByRole("alertdialog", { name: "Detach web from git?" });
    expect(
      within(dialog).getByText(
        "Revision #3 (commit 0123456) is copied into an editable draft. Bayplan stops checking stacks repo, removes the webhook and turns off auto-deploy. Nothing running changes. You can point the stack at a repository again later.",
      ),
    ).toBeTruthy();
    await waitFor(() =>
      expect(document.activeElement).toBe(
        within(dialog).getByRole("button", { name: "Keep tracking git" }),
      ),
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep tracking git" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(detaches).toBe(0);
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("says nothing about a draft when the stack has none", async () => {
    mount();
    fireEvent.click(await openAdvanced());
    const dialog = await screen.findByRole("alertdialog", { name: "Detach web from git?" });
    expect(within(dialog).queryByText(/has a draft that hasn't been deployed/)).toBeNull();
  });

  it("warns that the draft, with its overlay files and variable edits, is replaced and lost", async () => {
    hasDraft = true;
    mount();
    fireEvent.click(await openAdvanced());
    const dialog = await screen.findByRole("alertdialog", { name: "Detach web from git?" });
    await waitFor(() =>
      expect(
        within(dialog).getByText(
          "This stack has a draft that hasn't been deployed. Detaching replaces it with the deployed files, and its overlay file contents and variable edits are lost. This can't be undone.",
        ),
      ).toBeTruthy(),
    );
    // The warning does not take the place of the rest: the question is still asked, with the choice to keep tracking git.
    expect(within(dialog).getByRole("button", { name: "Keep tracking git" })).toBeTruthy();
    expect(detaches).toBe(0);
  });

  it("does not name a revision for a stack not deployed yet", async () => {
    revision = undefined;
    mount();
    fireEvent.click(await openAdvanced());
    const dialog = await screen.findByRole("alertdialog", { name: "Detach web from git?" });
    expect(
      within(dialog).getByText(/^The files of the stack are copied into an editable draft\./),
    ).toBeTruthy();
  });

  it("detaches, says so, and leads to the Overview", async () => {
    mount();
    fireEvent.click(await openAdvanced());
    const dialog = await screen.findByRole("alertdialog", { name: "Detach web from git?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Detach from git" }));
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("Detached web from git."));
    expect(detaches).toBe(1);
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/stacks/s1`),
    );
    expect(screen.queryByText("Couldn't load the git settings.")).toBeNull();
  });

  it("keeps the dialog open with the reason when the server refuses", async () => {
    detachAnswer = () =>
      Response.json(
        { type: "internal", title: "Error", status: 500, detail: "x" },
        { status: 500 },
      );
    mount();
    fireEvent.click(await openAdvanced());
    const dialog = await screen.findByRole("alertdialog", { name: "Detach web from git?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Detach from git" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "Couldn't detach the stack. Check the connection and try again.",
    );
    expect(toasts.success).not.toHaveBeenCalled();
    expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/stacks/s1/git`);
  });

  it("words a permission refusal", async () => {
    detachAnswer = () =>
      Response.json(
        { type: "forbidden", title: "Forbidden", status: 403, detail: "x" },
        { status: 403 },
      );
    mount();
    fireEvent.click(await openAdvanced());
    const dialog = await screen.findByRole("alertdialog", { name: "Detach web from git?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Detach from git" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "You don't have permission to detach this stack.",
    );
  });

  it("treats a stack that is not from git any more as detached", async () => {
    detachAnswer = () =>
      Response.json(
        { type: "stack_not_git", title: "Conflict", status: 409, detail: "x" },
        { status: 409 },
      );
    mount();
    fireEvent.click(await openAdvanced());
    const dialog = await screen.findByRole("alertdialog", { name: "Detach web from git?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Detach from git" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(toasts.success).not.toHaveBeenCalled();
  });
});
