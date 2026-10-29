// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { StackGit } from "@/api/stack-git";
import type { StackRevision } from "@/api/stack-history";
import type { StackDetail } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { JobsIndicator } from "@/features/jobs/jobs-indicator";
import { DeployConsole } from "@/features/stack-deploy/deploy-console";
import { AutoDeployPausedAlert } from "@/features/stack-git/auto-deploy-paused-alert";
import { GitTab } from "@/features/stack-git/git-tab";
import { RevisionTable } from "@/features/stack-history/revision-table";
import { LastDeployedCell } from "@/features/stacks/stack-columns";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Auto-deploy in the UI (plan 09-27, GIT-07, D-18 to D-20): the switch on the Git tab with its confirm, and an automatic
// deploy told apart from a person's in the console. The server is a stubbed fetch.

const ENV = "e1";
const DEPLOYED = "0123456789abcdef0123456789abcdef01234567";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

function makeGit(over: Partial<StackGit> = {}): StackGit {
  return {
    repoId: "r1",
    repoName: "stacks repo",
    repoHost: "github.com",
    repoPath: "acme/stacks",
    refKind: "branch",
    ref: "main",
    folder: "stacks/web",
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
    ...over,
  };
}

function makeStack(git: StackGit): StackDetail {
  return {
    id: "s1",
    name: "web",
    target: "standalone",
    source: "git",
    state: "running",
    lastDeployedAt: "",
    lastDeployedBy: "",
    hasDraft: false,
    envId: ENV,
    sourceRef: "",
    createdBy: "ada",
    createdAt: "2026-10-19T10:00:00Z",
    stopped: false,
    git,
  } as StackDetail;
}

let git: StackGit;
// The stack jobs the jobs indicator lists.
let stackJobs: Record<string, unknown>[];
let role: "viewer" | "operator" | "admin";
let instanceAdmin: boolean;
// The bodies the auto-deploy route was sent, and what it answers (a status other than 200 is a refusal).
let puts: { enabled: boolean }[];
let putStatus: number;
// The job the console reads.
let jobExtra: Record<string, unknown>;
let client: QueryClient;

beforeEach(() => {
  git = makeGit();
  role = "operator";
  instanceAdmin = false;
  puts = [];
  stackJobs = [];
  putStatus = 200;
  jobExtra = {};
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  setCsrfToken(null);
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") {
        return Response.json(
          makeMe(instanceAdmin ? { instanceAdmin: true } : { envs: { [ENV]: role } }),
        );
      }
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Homelab" }] });
      }
      if (path === "/api/stack-jobs") return Response.json({ jobs: stackJobs });
      if (path === "/api/pulls") return Response.json({ pulls: [] });
      if (path === `/api/env/${ENV}/stacks/s1/git/auto-deploy` && req.method === "PUT") {
        const body = (await req.json()) as { enabled: boolean };
        puts.push(body);
        if (putStatus !== 200) {
          return Response.json(
            { type: "owner_lacks_permission", title: "x", status: putStatus, detail: "d" },
            { status: putStatus },
          );
        }
        git = body.enabled
          ? {
              ...git,
              autoDeploy: true,
              autoDeployPaused: false,
              autoDeployOwner: { id: "u1", name: "Ada" },
            }
          : { ...git, autoDeploy: false, autoDeployPaused: false, autoDeployOwner: undefined };
        return Response.json(git);
      }
      if (path === `/api/env/${ENV}/stacks/s1/git`) return Response.json(git);
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(makeStack(git));
      if (path === `/api/env/${ENV}/stacks/s1/deploys/j1`) {
        return Response.json({
          id: "j1",
          stackId: "s1",
          mode: "deploy",
          startedBy: "system:gitsync",
          state: "running",
          createdAt: "2026-10-29T12:00:00Z",
          finishedAt: "",
          items: [],
          output: [],
          ...jobExtra,
        });
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

function mountTab() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter path={`/env/${ENV}/stacks/s1/git`}>
        <TooltipProvider>
          <GitTab envId={ENV} stackId="s1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

function mountConsole() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <DeployConsole envId={ENV} stackId="s1" jobId="j1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The Deploying card once the git settings have loaded.
async function card() {
  return screen.findByRole("region", { name: "Deploying" });
}

// The switch once the permissions are known (an unknown permission leaves it disabled).
async function autoSwitch(c: HTMLElement) {
  const sw = await within(c).findByRole("switch", { name: "Deploy changes automatically" });
  await waitFor(() => expect((sw as HTMLButtonElement).disabled).toBe(false));
  return sw;
}

describe("the Deploying card", () => {
  it("has the switch off by default, with the helper for waiting", async () => {
    mountTab();
    const c = await card();
    expect(
      within(c).getByText("Choose whether new commits deploy by themselves or wait for you."),
    ).toBeTruthy();
    const sw = await autoSwitch(c);
    expect(sw.getAttribute("aria-checked")).toBe("false");
    expect(
      within(c).getByText(
        "A new commit that changes stacks/web makes web show as behind. You review and deploy it.",
      ),
    ).toBeTruthy();
    expect(within(c).queryByText("Runs as")).toBeNull();
  });

  it("opens the confirm and leaves the switch reading the saved state until it succeeds", async () => {
    mountTab();
    const c = await card();
    const sw = await autoSwitch(c);
    fireEvent.click(sw);

    const dialog = await screen.findByRole("alertdialog", { name: "Turn on auto-deploy?" });
    expect(
      within(dialog).getByText(
        "New commits to main that change stacks/web will deploy by themselves, without a review, as you. If you lose permission to deploy on Homelab, auto-deploy pauses. Changes that fail a check are skipped and shown as an error.",
      ),
    ).toBeTruthy();
    // Nothing is applied yet: the switch still reads off, and focus starts on the safe button.
    expect(sw.getAttribute("aria-checked")).toBe("false");
    expect(puts).toEqual([]);
    await waitFor(() =>
      expect(document.activeElement).toBe(
        within(dialog).getByRole("button", { name: "Keep waiting for review" }),
      ),
    );

    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on auto-deploy" }));
    await waitFor(() => expect(puts).toEqual([{ enabled: true }]));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(toasts.success).toHaveBeenCalledWith("Auto-deploy is on for web.");
    await waitFor(() =>
      expect(within(c).getByRole("switch").getAttribute("aria-checked")).toBe("true"),
    );
    expect(
      within(c).getByText(
        "A new commit that changes stacks/web deploys without review, as Ada. A blocked change is skipped and shown as an error.",
      ),
    ).toBeTruthy();
    expect(within(c).getByText("Runs as")).toBeTruthy();
    expect(within(c).getByText("Ada")).toBeTruthy();
    // An Operator may not transfer: the control is for someone who can manage users.
    expect(within(c).queryByRole("button", { name: /^Transfer/ })).toBeNull();
  });

  it("leaves the switch off and sends nothing when the confirm is cancelled", async () => {
    mountTab();
    const c = await card();
    const sw = await autoSwitch(c);
    fireEvent.click(sw);
    const dialog = await screen.findByRole("alertdialog", { name: "Turn on auto-deploy?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep waiting for review" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(puts).toEqual([]);
    expect(sw.getAttribute("aria-checked")).toBe("false");
  });

  it("says so inside the confirm when the server refuses, and keeps the switch off", async () => {
    putStatus = 403;
    mountTab();
    const c = await card();
    const sw = await autoSwitch(c);
    fireEvent.click(sw);
    const dialog = await screen.findByRole("alertdialog", { name: "Turn on auto-deploy?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on auto-deploy" }));
    expect(
      await within(dialog).findByText(
        "You don't hold the permission to deploy here, so you can't turn on auto-deploy.",
      ),
    ).toBeTruthy();
    expect(sw.getAttribute("aria-checked")).toBe("false");
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("turns off with no confirm and says the stack now waits for review", async () => {
    git = makeGit({ autoDeploy: true, autoDeployOwner: { id: "u1", name: "Ada" } });
    mountTab();
    const c = await card();
    const sw = await autoSwitch(c);
    expect(sw.getAttribute("aria-checked")).toBe("true");
    fireEvent.click(sw);
    await waitFor(() => expect(puts).toEqual([{ enabled: false }]));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("web now waits for review."));
    await waitFor(() =>
      expect(within(c).getByRole("switch").getAttribute("aria-checked")).toBe("false"),
    );
  });

  it("is off for a Viewer, with the reason", async () => {
    role = "viewer";
    mountTab();
    const c = await card();
    // The switch is drawn again once the permissions are known, so it is read again each time.
    const read = () => within(c).getByRole("switch", { name: "Deploy changes automatically" });
    await waitFor(() => expect(read().getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(read());
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(puts).toEqual([]);
  });
});

describe("the console of an automatic deploy", () => {
  it("is titled Auto-deploying with the commit, what started it and whom it ran for", async () => {
    jobExtra = { trigger: "poll", onBehalfOf: "Ada", commit: "0123456" };
    mountConsole();
    expect(await screen.findByRole("heading", { name: "Auto-deploying web" })).toBeTruthy();
    expect(screen.getByText("Commit 0123456, started by a poll check for Ada.")).toBeTruthy();
    // The system actor's name is not shown as a person who pressed Deploy.
    expect(screen.queryByText(/system:gitsync/)).toBeNull();
  });

  it("names a webhook", async () => {
    jobExtra = { trigger: "webhook", onBehalfOf: "Ada", commit: "89abcde" };
    mountConsole();
    await screen.findByRole("heading", { name: "Auto-deploying web" });
    expect(screen.getByText("Commit 89abcde, started by a webhook for Ada.")).toBeTruthy();
  });

  it("keeps the plain title and the person for a deploy somebody pressed", async () => {
    jobExtra = { startedBy: "ada" };
    mountConsole();
    expect(await screen.findByRole("heading", { name: "Deploying web" })).toBeTruthy();
    expect(screen.getByText(/^Started .* by ada\./)).toBeTruthy();
    expect(screen.queryByText(/started by a poll check/)).toBeNull();
  });
});

// The words under the Deploying card when something is unusual about the automation.
describe("the Deploying card, owner and paused", () => {
  it("names who it runs as and offers Transfer… to someone who can manage users", async () => {
    git = makeGit({ autoDeploy: true, autoDeployOwner: { id: "u1", name: "Ada" } });
    instanceAdmin = true;
    mountTab();
    const c = await card();
    expect(
      await within(c).findByRole("button", { name: "Transfer auto-deploy of web…" }),
    ).toBeTruthy();
    expect(within(c).getByText("Runs as")).toBeTruthy();
    expect(within(c).getByText("Ada")).toBeTruthy();
    expect(within(c).queryByText("Paused")).toBeNull();
  });

  it("shows the Paused badge and Resume… for a paused automation, and resumes after the confirm", async () => {
    git = makeGit({
      autoDeploy: true,
      autoDeployPaused: true,
      autoDeployPausedReason: "owner",
      autoDeployOwner: { id: "u1", name: "Ada" },
    });
    mountTab();
    const c = await card();
    expect(await within(c).findByText("Paused")).toBeTruthy();
    const resume = await within(c).findByRole("button", { name: "Resume…" });
    await waitFor(() => expect((resume as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(resume);
    const dialog = await screen.findByRole("alertdialog", { name: "Turn on auto-deploy?" });
    expect(puts).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on auto-deploy" }));
    await waitFor(() => expect(puts).toEqual([{ enabled: true }]));
    await waitFor(() => expect(within(c).queryByText("Paused")).toBeNull());
  });

  it("hides the switch for a fixed commit and says there is nothing to deploy", async () => {
    git = makeGit({ refKind: "commit", ref: DEPLOYED });
    mountTab();
    const c = await card();
    expect(await within(c).findByText("A fixed commit has nothing to deploy.")).toBeTruthy();
    expect(within(c).queryByRole("switch")).toBeNull();
  });
});

function mountAlert(stack: StackDetail) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter path={`/env/${ENV}/stacks/s1`}>
        <TooltipProvider>
          <AutoDeployPausedAlert stack={stack} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("the auto-deploy paused alert", () => {
  const pausedGit = {
    autoDeploy: true,
    autoDeployPaused: true,
    autoDeployPausedReason: "owner" as const,
    autoDeployOwner: { id: "u1", name: "Ada" },
  };

  it("says the owner lost permission and leads to the git settings", async () => {
    git = makeGit(pausedGit);
    mountAlert(makeStack(git));
    expect(await screen.findByText("Auto-deploy is paused")).toBeTruthy();
    expect(
      await screen.findByText(
        "Ada set it up and no longer has permission to deploy on Homelab. Transfer it to someone who can, or turn it off.",
      ),
    ).toBeTruthy();
    const link = screen.getByRole("link", { name: "Open git settings" });
    expect(link.getAttribute("href")).toBe(`/env/${ENV}/stacks/s1/git`);
  });

  it("is not shown while the sync error alert is, or when nothing is paused", async () => {
    git = makeGit({ ...pausedGit, status: "error", reasonCode: "fetch_failed" });
    const view = mountAlert(makeStack(git));
    await waitFor(() => expect(view.container.querySelector("[data-slot=alert]")).toBeNull());
    view.unmount();

    git = makeGit({ autoDeploy: true });
    const second = mountAlert(makeStack(git));
    await waitFor(() => expect(second.container.querySelector("[data-slot=alert]")).toBeNull());
  });

  it("leaves a rollback pause to the alert the rollback plan adds", async () => {
    git = makeGit({ ...pausedGit, autoDeployPausedReason: "rollback" });
    const view = mountAlert(makeStack(git));
    await waitFor(() => expect(view.container.querySelector("[data-slot=alert]")).toBeNull());
  });
});

describe("the jobs indicator", () => {
  const job = (over: Record<string, unknown>) => ({
    id: "d1",
    envId: ENV,
    envName: "Homelab",
    stackId: "s1",
    stackName: "web",
    kind: "deploy",
    state: "running",
    startedAt: "2026-10-29T10:00:00Z",
    finishedAt: "",
    startedBy: "system:gitsync",
    ...over,
  });

  async function open() {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <JobsIndicator />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: /^Jobs/ }));
    return screen.findByRole("list");
  }

  it.each([
    ["running", "Auto-deploying"],
    ["done", "Auto-deployed"],
    ["failed", "Failed"],
  ])("reads %s as %s for an automatic deploy, with the branch icon", async (state, words) => {
    stackJobs = [
      job({
        state,
        trigger: "poll",
        finishedAt: state === "running" ? "" : "2026-10-29T10:01:00Z",
      }),
    ];
    const list = await open();
    expect(within(list).getByText(words)).toBeTruthy();
    expect(list.querySelector("svg.lucide-git-branch")).not.toBeNull();
    expect(list.querySelector("svg.lucide-rocket")).toBeNull();
  });

  it("keeps Deploying and the rocket for a deploy a person pressed", async () => {
    stackJobs = [job({ startedBy: "ada" })];
    const list = await open();
    expect(within(list).getByText("Deploying")).toBeTruthy();
    expect(list.querySelector("svg.lucide-rocket")).not.toBeNull();
    expect(list.querySelector("svg.lucide-git-branch")).toBeNull();
  });
});

const revisionOf = (over: Partial<StackRevision> = {}): StackRevision =>
  ({
    seq: 3,
    origin: "deploy",
    restoredFrom: 0,
    outcome: "good",
    createdBy: "Ada",
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
  }) as StackRevision;

describe("History", () => {
  function mountTable(items: StackRevision[]) {
    return render(
      <QueryClientProvider client={client}>
        <TestRouter path={`/env/${ENV}/stacks/s1/history`}>
          <TooltipProvider>
            <RevisionTable
              envId={ENV}
              stackId="s1"
              name="web"
              items={items}
              selected={undefined}
              fresh={new Set()}
            />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  it("reads Auto-deploy for the user for an automatic revision, with a Tooltip naming the trigger", async () => {
    mountTable([
      revisionOf({ seq: 4, syncTrigger: "poll" }),
      revisionOf({ seq: 3, syncTrigger: "webhook" }),
      revisionOf({ seq: 2, createdBy: "bob" }),
    ]);
    const rows = await screen.findAllByRole("row");
    // The header row, then one per revision, newest first.
    const poll = rows[1] as HTMLElement;
    const webhook = rows[2] as HTMLElement;
    const manual = rows[3] as HTMLElement;
    expect(within(poll).getByText("Auto-deploy")).toBeTruthy();
    expect(within(poll).getByText("for Ada")).toBeTruthy();
    expect(within(webhook).getByText("Auto-deploy")).toBeTruthy();
    expect(within(manual).getByText("bob")).toBeTruthy();
    expect(within(manual).queryByText("Auto-deploy")).toBeNull();

    fireEvent.pointerMove(within(poll).getByText("Auto-deploy"));
    fireEvent.mouseEnter(within(poll).getByText("Auto-deploy"));
    expect((await screen.findByRole("tooltip")).textContent).toBe(
      "Started by a poll check on behalf of Ada.",
    );
  });

  it("names a webhook in the Tooltip", async () => {
    mountTable([revisionOf({ seq: 3, syncTrigger: "webhook" })]);
    const label = await screen.findByText("Auto-deploy");
    fireEvent.pointerMove(label);
    fireEvent.mouseEnter(label);
    expect((await screen.findByRole("tooltip")).textContent).toBe(
      "Started by a webhook on behalf of Ada.",
    );
  });
});

describe("the stack lists", () => {
  const stackOf = (over: Record<string, unknown>) =>
    ({
      ...makeStack(makeGit()),
      lastDeployedAt: "2026-10-29T10:00:00Z",
      lastDeployedBy: "system:gitsync",
      ...over,
    }) as StackDetail;

  function mountCell(stack: StackDetail) {
    return render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <LastDeployedCell stack={stack} />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  it("reads the short commit and auto-deploy for an automatic deploy", async () => {
    mountCell(stackOf({ lastDeployedTrigger: "webhook" }));
    expect(await screen.findByText("0123456 · auto-deploy")).toBeTruthy();
    expect(screen.queryByText(/system:gitsync/)).toBeNull();
  });

  it("keeps the commit and the person for a deploy somebody pressed", async () => {
    mountCell(stackOf({ lastDeployedBy: "ada" }));
    expect(await screen.findByText("0123456 · by ada")).toBeTruthy();
  });
});
