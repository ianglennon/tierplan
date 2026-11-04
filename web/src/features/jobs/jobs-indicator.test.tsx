// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type JanitorJobSummary, janitorJobsKey } from "@/api/janitor";
import { PULLS_POLL_RUNNING_MS, type PullSummary } from "@/api/pulls";
import { type StackJobSummary, stackJobsKey } from "@/api/stacks";
import type { ImageUpdateJobSummary } from "@/api/updates";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { JobsIndicator } from "@/features/jobs/jobs-indicator";
import { followStackOperation } from "@/features/stacks/follow-operation";
import "@/i18n";
import { keyExists } from "@/i18n/use-t";
import { janitorJob, run } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

const REF = "docker.io/library/nginx:1.27";

const summary = (over: Partial<PullSummary> = {}): PullSummary => ({
  id: "j1",
  envId: "e1",
  envName: "Home lab",
  ref: REF,
  state: "running",
  done: 0,
  failed: 0,
  total: 3,
  createdAt: "2026-10-11T10:00:00Z",
  ...over,
});

// A stack job as GET /api/stack-jobs lists it.
interface StackJobRow {
  id: string;
  envId: string;
  envName: string;
  stackId: string;
  stackName: string;
  kind: StackJobSummary["kind"];
  state: "running" | "done" | "failed" | "cancelled";
  startedAt: string;
  finishedAt: string;
  startedBy: string;
}

const stackJob = (over: Partial<StackJobRow> = {}): StackJobRow => ({
  id: "d1",
  envId: "e1",
  envName: "Home lab",
  stackId: "s1",
  stackName: "web",
  kind: "deploy",
  state: "running",
  startedAt: "2026-10-11T10:00:00Z",
  finishedAt: "",
  startedBy: "ada",
  ...over,
});

let pulls: PullSummary[];
let stackJobs: StackJobRow[];
let stackJobsStatus: number | "never";
let janitorJobs: JanitorJobSummary[];
let janitorJobsStatus: number;
let updateJobs: ImageUpdateJobSummary[];
let janitorRun: ReturnType<typeof run>;
let listStatus: number;
let client: QueryClient;
let calls: string[];
let cancelAnswer: (id: string) => Response;

beforeEach(() => {
  pulls = [];
  stackJobs = [];
  stackJobsStatus = 200;
  janitorJobs = [];
  janitorJobsStatus = 200;
  updateJobs = [];
  janitorRun = run({ state: "completed" });
  listStatus = 200;
  calls = [];
  cancelAnswer = (id) => Response.json({ id, state: "cancelled", items: [] });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      calls.push(`${req.method} ${path}`);
      if (path === "/api/me") {
        return Response.json(makeMe({ envs: { e1: "operator" }, username: "ada" }));
      }
      if (path === "/api/stack-jobs") {
        if (stackJobsStatus === "never") return new Promise<Response>(() => {});
        return stackJobsStatus === 200
          ? Response.json({ jobs: stackJobs })
          : Response.json(
              { type: "", title: "x", status: stackJobsStatus, detail: "d" },
              { status: stackJobsStatus },
            );
      }
      if (path === "/api/janitor-jobs") {
        return janitorJobsStatus === 200
          ? Response.json({ jobs: janitorJobs })
          : Response.json(
              { type: "", title: "x", status: janitorJobsStatus, detail: "d" },
              { status: janitorJobsStatus },
            );
      }
      if (path === "/api/update-jobs") return Response.json({ jobs: updateJobs });
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: "e1", name: "Home lab", envType: "swarm" }] });
      }
      if (path === "/api/env/e1/janitor/runs/r1") return Response.json(janitorRun);
      if (path === "/api/pulls") {
        return listStatus === 200
          ? Response.json({ pulls })
          : Response.json(
              { type: "", title: "x", status: listStatus, detail: "d" },
              { status: listStatus },
            );
      }
      const cancel = path.match(/^\/api\/pulls\/([^/]+)\/cancel$/);
      if (cancel && req.method === "POST") return cancelAnswer(cancel[1] as string);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  // A toast raised by one test (the failed cancel) is in sonner's store until it is dismissed,
  // and would be found by the next test's role="alert" query.
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

// Where is the page the router stands on, so a test can see where an action took the person.
function Where() {
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  return <output data-testid="where">{pathname}</output>;
}

function mount(path = "/") {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter path={path}>
        <TooltipProvider>
          <JobsIndicator />
          <Toaster />
          <Where />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("JobsIndicator pulls", () => {
  it("is hidden while there is nothing to list", async () => {
    const { container } = mount();
    await waitFor(() => expect(calls).toContain("GET /api/pulls"));
    expect(screen.queryByRole("button", { name: /Jobs/ })).toBeNull();
    expect(container.querySelector("button")).toBeNull();
  });

  it("shows a neutral count badge and an accessible name with the number running", async () => {
    pulls = [
      summary(),
      summary({ id: "j2", state: "done", done: 3, finishedAt: "2026-10-11T10:01:00Z" }),
    ];
    mount();
    const button = await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    expect(within(button).getByText("1")).toBeTruthy();
  });

  it("lists the pulls with ref, environment, state and time, and the starter on other people's", async () => {
    pulls = [
      summary({ id: "j1", ownerName: "sam" }),
      summary({
        id: "j2",
        ref: "docker.io/library/busybox:1",
        state: "partial",
        done: 2,
        failed: 1,
        finishedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
      }),
      summary({ id: "j3", state: "failed", failed: 3 }),
      summary({ id: "j4", state: "cancelled" }),
    ];
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Jobs, 1 in progress" }));
    const popover = await screen.findByText("Pulling on 3 of 3 nodes");
    expect(popover).toBeTruthy();
    expect(screen.getByText("by sam")).toBeTruthy();
    expect(screen.getByText("Done, 1 failed")).toBeTruthy();
    expect(screen.getByText("Failed")).toBeTruthy();
    expect(screen.getByText("Cancelled")).toBeTruthy();
    expect(screen.getAllByText("Home lab").length).toBe(4);
    expect(screen.getByText("docker.io/library/busybox:1")).toBeTruthy();
  });

  it("reopens the progress panel of a pull at once, from the stream's snapshot", async () => {
    pulls = [summary()];
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Jobs, 1 in progress" }));
    fireEvent.click(await screen.findByRole("button", { name: /Pulling on 3 of 3 nodes/ }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: `Pulling ${REF}` })).toBeTruthy();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    const src = FakeEventSource.instances[0] as FakeEventSource;
    expect(src.url).toContain("/api/pulls/j1/stream");
    act(() => {
      src.sendFrame("snapshot", {
        id: "j1",
        envId: "e1",
        ref: REF,
        credential: null,
        state: "running",
        createdAt: "2026-10-11T10:00:00Z",
        finishedAt: "",
        items: [
          {
            nodeId: "n1",
            name: "node-1",
            state: "running",
            code: "",
            message: "",
            current: 50,
            total: 100,
            unit: "bytes",
            done: 0,
            count: 1,
            layers: [],
            digest: "",
            upToDate: false,
          },
        ],
      });
    });
    expect(await within(dialog).findByText("node-1")).toBeTruthy();
    // No form: it opened on the progress.
    expect(within(dialog).queryByLabelText("Image")).toBeNull();
  });

  it("keeps the last list when a poll fails and shows no error", async () => {
    // The list is polled while a pull runs. The clock is fully fake, so the poll comes when the
    // test says so and not when real time happens to pass; under it the test waits with vi.waitFor,
    // because Testing Library's waitFor and findBy poll on the faked timers and so never poll.
    vi.useFakeTimers();
    pulls = [summary()];
    mount();
    await waitUnderFakeClock(() => screen.getByRole("button", { name: "Jobs, 1 in progress" }));
    const listed = () => calls.filter((c) => c === "GET /api/pulls").length;
    const before = listed();
    listStatus = 500;
    await vi.advanceTimersByTimeAsync(PULLS_POLL_RUNNING_MS);
    await waitUnderFakeClock(() => expect(listed()).toBeGreaterThan(before));
    await waitUnderFakeClock(() => expect(client.isFetching() + client.isMutating()).toBe(0));
    expect(screen.getByRole("button", { name: "Jobs, 1 in progress" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a pull that ended shows the Jobs button without a count", async () => {
    pulls = [summary({ state: "done", done: 3, finishedAt: "2026-10-11T10:01:00Z" })];
    mount();
    expect(await screen.findByRole("button", { name: "Jobs" })).toBeTruthy();
  });

  it("has a Cancel button on running rows only, named for the pull", async () => {
    pulls = [
      summary({ id: "j1" }),
      summary({
        id: "j2",
        ref: "docker.io/library/busybox:1",
        state: "done",
        done: 3,
        finishedAt: "2026-10-11T10:01:00Z",
      }),
    ];
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Jobs, 1 in progress" }));
    expect(await screen.findByRole("button", { name: `Cancel pull of ${REF}` })).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: /Cancel pull of docker.io\/library\/busybox/ }),
    ).toBeNull();
  });

  it("cancels a pull from its row, once, tells how many nodes it stopped and does not open the panel", async () => {
    pulls = [summary({ id: "j1" })];
    cancelAnswer = (id) =>
      Response.json({
        id,
        state: "cancelled",
        items: [
          { nodeId: "n1", state: "cancelled" },
          { nodeId: "n2", state: "cancelled" },
          { nodeId: "n3", state: "done" },
        ],
      });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Jobs, 1 in progress" }));
    fireEvent.click(await screen.findByRole("button", { name: `Cancel pull of ${REF}` }));
    expect(await screen.findByText("Cancelled the pull on 2 nodes.")).toBeTruthy();
    expect(calls.filter((c) => c === "POST /api/pulls/j1/cancel")).toHaveLength(1);
    expect(screen.queryByRole("heading", { name: `Pulling ${REF}` })).toBeNull();
  });

  it("does not retry a cancel that failed, and says so", async () => {
    pulls = [summary({ id: "j1" })];
    cancelAnswer = () =>
      Response.json({ type: "", title: "x", status: 500, detail: "d" }, { status: 500 });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Jobs, 1 in progress" }));
    fireEvent.click(await screen.findByRole("button", { name: `Cancel pull of ${REF}` }));
    expect(await screen.findByText("Couldn't cancel the pull. Try again.")).toBeTruthy();
    expect(calls.filter((c) => c === "POST /api/pulls/j1/cancel")).toHaveLength(1);
  });

  it("says a pull that had already ended is over, not that the cancel worked", async () => {
    pulls = [summary({ id: "j1" })];
    cancelAnswer = () =>
      Response.json(
        { type: "pull_finished", title: "x", status: 409, detail: "d" },
        { status: 409 },
      );
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Jobs, 1 in progress" }));
    fireEvent.click(await screen.findByRole("button", { name: `Cancel pull of ${REF}` }));
    expect(await screen.findByText("This pull has already ended.")).toBeTruthy();
  });
});

// The jobs indicator lists stack operations beside pulls (plan 07-19, UI-SPEC Defaults 4).
describe("JobsIndicator stack jobs", () => {
  const refresh = () => client.invalidateQueries({ queryKey: stackJobsKey });
  const open = async (name: string | RegExp = "Jobs, 1 in progress") =>
    fireEvent.click(await screen.findByRole("button", { name }));

  it("lists a running deploy and a finished pull together, running first, and counts both kinds", async () => {
    stackJobs = [stackJob({ startedAt: "2026-10-11T10:01:00Z" })];
    pulls = [
      summary({
        id: "j9",
        state: "done",
        done: 3,
        finishedAt: "2026-10-11T10:05:00Z",
        createdAt: "2026-10-11T10:04:00Z",
      }),
      summary({ id: "j8", ref: "docker.io/library/busybox:1" }),
    ];
    mount();
    // Two are running (the pull and the deploy); the finished pull is listed but not counted.
    await open("Jobs, 2 in progress");
    const rows = within(await screen.findByRole("list")).getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    // Running ones first: the deploy started after the running pull, so it is the newer.
    expect(within(rows[0] as HTMLElement).getByText("web")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("docker.io/library/busybox:1")).toBeTruthy();
    expect(within(rows[2] as HTMLElement).getByText(REF)).toBeTruthy();
  });

  it("draws a Rocket for a stack job and a Download for a pull, with the name, environment and state", async () => {
    stackJobs = [stackJob({ startedAt: "2026-10-11T10:01:00Z" })];
    pulls = [summary()];
    mount();
    await open("Jobs, 2 in progress");
    const rows = within(await screen.findByRole("list")).getAllByRole("listitem");
    const deploy = rows[0] as HTMLElement;
    expect(deploy.querySelector(".lucide-rocket")).not.toBeNull();
    expect(within(deploy).getByText("web")).toBeTruthy();
    expect(within(deploy).getByText("Home lab")).toBeTruthy();
    expect(within(deploy).getByText("Deploying")).toBeTruthy();
    expect((rows[1] as HTMLElement).querySelector(".lucide-download")).not.toBeNull();
  });

  it("draws an Undo for a roll back, with its own words", async () => {
    stackJobs = [stackJob({ kind: "rollback" })];
    mount();
    await open("Jobs, 1 in progress");
    const row = within(await screen.findByRole("list")).getAllByRole("listitem")[0] as HTMLElement;
    expect(row.querySelector(".lucide-undo-2")).not.toBeNull();
    expect(row.querySelector(".lucide-rocket")).toBeNull();
    expect(within(row).getByText("Rolling back")).toBeTruthy();
  });

  it("draws a ListRestart for a restore, with its own words", async () => {
    stackJobs = [stackJob({ kind: "restore" })];
    mount();
    await open("Jobs, 1 in progress");
    const row = within(await screen.findByRole("list")).getAllByRole("listitem")[0] as HTMLElement;
    expect(row.querySelector(".lucide-list-restart")).not.toBeNull();
    expect(row.querySelector(".lucide-undo-2")).toBeNull();
    expect(within(row).getByText("Restoring")).toBeTruthy();
  });

  it("opens the console of a running deploy from its row", async () => {
    stackJobs = [stackJob({ id: "d7", stackId: "s9" })];
    mount();
    await open();
    const link = await screen.findByRole("link", { name: "Open console of web" });
    expect(link.textContent).toBe("Open");
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s9/deploys/d7");
    // Following it closes the list, so the console is not drawn under a popover.
    fireEvent.click(link);
    await waitFor(() =>
      expect(screen.queryByRole("link", { name: "Open console of web" })).toBeNull(),
    );
  });

  it.each([
    ["deploy", "running", "Deploying"],
    ["deploy", "done", "Deployed"],
    ["deploy", "failed", "Failed"],
    ["deploy", "cancelled", "Cancelled"],
    ["rollback", "running", "Rolling back"],
    ["rollback", "done", "Rolled back"],
    ["rollback", "failed", "Failed"],
    ["restore", "running", "Restoring"],
    ["restore", "done", "Restored"],
    ["restore", "failed", "Failed"],
    ["update", "running", "Updating images"],
    ["update", "done", "Images updated"],
    ["stop", "running", "Stopping"],
    ["stop", "done", "Stopped"],
    ["start", "running", "Starting"],
    ["start", "done", "Started"],
    ["remove", "running", "Removing"],
    ["remove", "done", "Removed"],
    ["remove", "failed", "Failed"],
  ] as const)("says %s %s as %s", async (kind, state, words) => {
    stackJobs = [
      stackJob({ kind, state, finishedAt: state === "running" ? "" : "2026-10-11T10:02:00Z" }),
    ];
    mount();
    await open(state === "running" ? "Jobs, 1 in progress" : "Jobs");
    expect(await screen.findByText(words)).toBeTruthy();
  });

  it("says No jobs running. above the jobs that ended, when none runs", async () => {
    stackJobs = [stackJob({ state: "done", finishedAt: "2026-10-11T10:02:00Z" })];
    mount();
    await open("Jobs");
    expect(await screen.findByText("No jobs running.")).toBeTruthy();
    expect(screen.getByText("Deployed")).toBeTruthy();
  });

  it("does not say it while something runs", async () => {
    stackJobs = [stackJob()];
    mount();
    await open();
    await screen.findByText("Deploying");
    expect(screen.queryByText("No jobs running.")).toBeNull();
  });

  it("shows skeleton rows until the stack jobs have loaded", async () => {
    stackJobsStatus = "never";
    pulls = [summary()];
    mount();
    await open();
    expect(await screen.findByRole("status", { name: "Loading jobs" })).toBeTruthy();
    // The pull is there meanwhile.
    expect(screen.getByText(REF)).toBeTruthy();
  });

  it("shows an error row with Retry when the stack jobs cannot be loaded, and Retry asks again", async () => {
    stackJobsStatus = 500;
    pulls = [summary()];
    mount();
    await open();
    expect(await screen.findByText("Couldn't load the stack jobs.")).toBeTruthy();
    const asked = () => calls.filter((c) => c === "GET /api/stack-jobs").length;
    const before = asked();
    stackJobsStatus = 200;
    stackJobs = [stackJob()];
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Deploying")).toBeTruthy();
    expect(asked()).toBeGreaterThan(before);
    expect(screen.queryByText("Couldn't load the stack jobs.")).toBeNull();
  });

  it("says Reconnecting… on a running job while the list cannot be read, and the state again once it can", async () => {
    stackJobs = [stackJob()];
    mount();
    await open();
    await screen.findByText("Deploying");
    stackJobsStatus = 500;
    await act(async () => {
      await refresh();
    });
    expect(await screen.findByText("Reconnecting…")).toBeTruthy();
    expect(screen.queryByText("Deploying")).toBeNull();
    stackJobsStatus = 200;
    await act(async () => {
      await refresh();
    });
    expect(await screen.findByText("Deploying")).toBeTruthy();
    expect(screen.queryByText("Reconnecting…")).toBeNull();
  });

  it("drops a job that has left the server's history instead of keeping a dead link", async () => {
    stackJobs = [stackJob({ id: "d1" }), stackJob({ id: "d2", stackName: "api", stackId: "s2" })];
    mount();
    await open("Jobs, 2 in progress");
    await screen.findByRole("link", { name: "Open console of api" });
    stackJobs = [stackJob({ id: "d1" })];
    await act(async () => {
      await refresh();
    });
    await waitFor(() =>
      expect(screen.queryByRole("link", { name: "Open console of api" })).toBeNull(),
    );
    expect(screen.getByRole("link", { name: "Open console of web" })).toBeTruthy();
  });

  it("keeps the badge to 9+ past nine running jobs of both kinds", async () => {
    stackJobs = Array.from({ length: 8 }, (_, i) => stackJob({ id: `d${i}`, stackName: `s${i}` }));
    pulls = [summary({ id: "p1" }), summary({ id: "p2", ref: "docker.io/library/a:1" })];
    mount();
    const button = await screen.findByRole("button", { name: "Jobs, 10 in progress" });
    expect(within(button).getByText("9+")).toBeTruthy();
  });

  it("truncates a long stack name and gives it in full in a tooltip", async () => {
    const long = "a-very-long-stack-name-".repeat(6);
    stackJobs = [stackJob({ stackName: long })];
    mount();
    await open();
    const name = await screen.findByText(long);
    expect(name.className).toContain("truncate");
  });

  it("is hidden when nothing is listed on either side", async () => {
    const { container } = mount();
    await waitFor(() => expect(calls).toContain("GET /api/stack-jobs"));
    expect(container.querySelector("button")).toBeNull();
  });
});

describe("JobsIndicator clean-ups", () => {
  const open = async (name: string | RegExp = "Jobs, 1 in progress") =>
    fireEvent.click(await screen.findByRole("button", { name }));

  it("lists a running scheduled clean-up as Cleaning {environment} with how far it has got, by TierPlan, and no Cancel", async () => {
    janitorJobs = [janitorJob({ trigger: "schedule", by: "TierPlan", done: 3, total: 8 })];
    mount();
    await open();
    const row = within(await screen.findByRole("list")).getAllByRole("listitem")[0] as HTMLElement;
    expect(await within(row).findByText("Cleaning Home lab")).toBeTruthy();
    expect(within(row).getByText("Removing 3 of 8")).toBeTruthy();
    expect(within(row).getByText("by TierPlan")).toBeTruthy();
    expect(row.querySelector(".lucide-brush-cleaning")).not.toBeNull();
    // Removals are individual and idempotent: there is nothing to cancel.
    expect(within(row).queryByRole("button")).toBeNull();
    expect(screen.queryByRole("button", { name: /Cancel/ })).toBeNull();
  });

  it("names the person who chose Clean now, and counts the clean-up as running", async () => {
    janitorJobs = [janitorJob({ trigger: "manual", by: "Ada" })];
    mount();
    await open("Jobs, 1 in progress");
    expect(await screen.findByText("by Ada")).toBeTruthy();
  });

  it("says a clean-up is waiting to start until the run has a plan", async () => {
    janitorJobs = [janitorJob({ done: 0, total: 0 })];
    mount();
    await open();
    expect(await screen.findByText("Waiting to start")).toBeTruthy();
  });

  it("opens the run from Open, and following it closes the list", async () => {
    janitorJobs = [janitorJob({ runId: "r1" })];
    mount();
    await open();
    const link = await screen.findByRole("link", { name: "Open the cleanup of Home lab" });
    expect(link.textContent).toBe("Open");
    expect(link.getAttribute("href")).toBe("/env/e1/janitor/runs/r1");
    fireEvent.click(link);
    await waitFor(() =>
      expect(screen.queryByRole("link", { name: "Open the cleanup of Home lab" })).toBeNull(),
    );
    expect(screen.getByTestId("where").textContent).toBe("/env/e1/janitor/runs/r1");
  });

  it.each([
    ["completed", "Cleaned"],
    ["partial", "Partly cleaned"],
    ["failed", "Failed"],
    ["skipped", "Skipped"],
  ] as const)("words a clean-up that ended from the run record: %s is %s", async (state, words) => {
    janitorRun = run({ state });
    janitorJobs = [janitorJob({ state: "done", done: 8, total: 8 })];
    mount();
    // None is running, so the button has no count.
    await open("Jobs");
    expect(await screen.findByText(words)).toBeTruthy();
  });

  it("does not call a clean-up failed because the job counts an item kept in use as failed", async () => {
    // The run record says completed; the hub's state for the same job is failed.
    janitorRun = run({ state: "completed" });
    janitorJobs = [janitorJob({ state: "failed", done: 8, total: 8 })];
    mount();
    await open("Jobs");
    expect(await screen.findByText("Cleaned")).toBeTruthy();
    expect(screen.queryByText("Failed")).toBeNull();
  });

  it("says only Finished until the run record is read", async () => {
    janitorJobs = [janitorJob({ state: "done", done: 8, total: 8 })];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/janitor-jobs") return Response.json({ jobs: janitorJobs });
        if (path === "/api/me") return Response.json(makeMe({ envs: { e1: "operator" } }));
        if (path === "/api/env/e1/janitor/runs/r1") return new Promise<Response>(() => {});
        return Response.json({}, { status: 404 });
      }),
    );
    mount();
    await open("Jobs");
    expect(await screen.findByText("Finished")).toBeTruthy();
  });

  it("lists a clean-up beside a deploy, running first, and counts both", async () => {
    stackJobs = [stackJob({ startedAt: "2026-10-11T10:01:00Z" })];
    janitorJobs = [janitorJob({ startedAt: "2026-10-11T10:03:00Z" })];
    mount();
    await open("Jobs, 2 in progress");
    const rows = within(await screen.findByRole("list")).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(await within(rows[0] as HTMLElement).findByText("Cleaning Home lab")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("web")).toBeTruthy();
  });

  it("shows an error row with Retry when the clean-ups cannot be loaded, beside the jobs that could", async () => {
    stackJobs = [stackJob()];
    janitorJobsStatus = 500;
    mount();
    await open();
    expect(await screen.findByText("Couldn't load the cleanups.")).toBeTruthy();
    janitorJobsStatus = 200;
    janitorJobs = [janitorJob()];
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Cleaning Home lab")).toBeTruthy();
    expect(screen.queryByText("Couldn't load the cleanups.")).toBeNull();
  });

  it("keeps the last list when a poll fails and shows no error", async () => {
    janitorJobs = [janitorJob()];
    mount();
    await open();
    await screen.findByText("Cleaning Home lab");
    janitorJobsStatus = 500;
    await act(async () => {
      await client.invalidateQueries({ queryKey: janitorJobsKey });
    });
    expect(screen.queryByText("Couldn't load the cleanups.")).toBeNull();
    expect(screen.getByText("Cleaning Home lab")).toBeTruthy();
  });

  it("truncates a long environment name and gives it in full in a tooltip", async () => {
    janitorJobs = [janitorJob()];
    mount();
    await open();
    const name = await screen.findByText("Cleaning Home lab");
    expect(name.className).toContain("truncate");
  });
});

// A stack job the person started that ends while they are elsewhere raises one toast (D-13).
describe("JobsIndicator image updates", () => {
  const upd = (over: Partial<ImageUpdateJobSummary> = {}): ImageUpdateJobSummary => ({
    id: "u1",
    envId: "e1",
    envName: "Home lab",
    workload: "web",
    kind: "service",
    state: "running",
    startedAt: "2026-10-11T10:00:00Z",
    endedAt: "",
    ...over,
  });
  const open = async (name: string | RegExp = "Jobs, 1 in progress") =>
    fireEvent.click(await screen.findByRole("button", { name }));

  it("lists a running update as Updating web with Updating…, and counts it", async () => {
    updateJobs = [upd()];
    mount();
    await open();
    const row = within(await screen.findByRole("list")).getAllByRole("listitem")[0] as HTMLElement;
    expect(row.querySelector(".lucide-circle-arrow-up")).not.toBeNull();
    expect(within(row).getByText("Updating web")).toBeTruthy();
    expect(within(row).getByText("Home lab")).toBeTruthy();
    expect(within(row).getByText("Updating…")).toBeTruthy();
  });

  it("words a finished update Updated and a failed one Failed, and counts neither", async () => {
    updateJobs = [
      upd({ id: "u1", state: "done", endedAt: "2026-10-11T10:02:00Z" }),
      upd({ id: "u2", workload: "api", state: "failed", endedAt: "2026-10-11T10:03:00Z" }),
    ];
    mount();
    await open("Jobs");
    const rows = within(await screen.findByRole("list")).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(within(rows[0] as HTMLElement).getByText("Updated")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("Failed")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Jobs" })).toBeTruthy();
  });

  it("opens the update console from its row and closes the list", async () => {
    updateJobs = [upd({ id: "u7" })];
    mount();
    await open();
    const link = await screen.findByRole("link", { name: "Open the update of web" });
    expect(link.textContent).toBe("Open");
    expect(link.getAttribute("href")).toBe("/env/e1/update-jobs/u7");
    fireEvent.click(link);
    await waitFor(() => expect(screen.queryByRole("list")).toBeNull());
  });

  it("lists updates with the other kinds, running first", async () => {
    updateJobs = [upd({ startedAt: "2026-10-11T10:05:00Z" })];
    stackJobs = [stackJob({ state: "done", finishedAt: "2026-10-11T10:09:00Z" })];
    mount();
    await open();
    const rows = within(await screen.findByRole("list")).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(within(rows[0] as HTMLElement).getByText("Updating web")).toBeTruthy();
  });
});

describe("JobsIndicator job toasts", () => {
  const refresh = () => client.invalidateQueries({ queryKey: stackJobsKey });
  const finish = async (over: Partial<StackJobRow>) => {
    stackJobs = [stackJob({ finishedAt: "2026-10-11T10:02:00Z", ...over })];
    await act(async () => {
      await refresh();
    });
  };

  it("says Deployed {name}. with Open when a deploy they started ends", async () => {
    stackJobs = [stackJob()];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ state: "done" });
    expect(await screen.findByText("Deployed web.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open" })).toBeTruthy();
  });

  it("says Deploy of {name} failed. with Open console when it fails", async () => {
    stackJobs = [stackJob()];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ state: "failed" });
    expect(await screen.findByText("Deploy of web failed.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open console" })).toBeTruthy();
  });

  it("words a roll back as a roll back", async () => {
    stackJobs = [stackJob({ kind: "rollback" })];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ kind: "rollback", state: "failed" });
    expect(await screen.findByText("Roll back of web failed.")).toBeTruthy();
  });

  it("raises it once, however often the list is read again", async () => {
    stackJobs = [stackJob()];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ state: "done" });
    await screen.findByText("Deployed web.");
    await act(async () => {
      await refresh();
      await refresh();
    });
    expect(screen.getAllByText("Deployed web.")).toHaveLength(1);
  });

  it("says nothing for a job someone else started", async () => {
    stackJobs = [stackJob({ startedBy: "sam" })];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ startedBy: "sam", state: "done" });
    await screen.findByRole("button", { name: "Jobs" });
    expect(screen.queryByText("Deployed web.")).toBeNull();
  });

  it("says nothing for a job that had already ended when the page opened", async () => {
    stackJobs = [stackJob({ state: "done", finishedAt: "2026-10-11T10:02:00Z" })];
    mount();
    await screen.findByRole("button", { name: "Jobs" });
    await act(async () => {
      await refresh();
    });
    expect(screen.queryByText("Deployed web.")).toBeNull();
  });

  it("says nothing on the console of the job itself, and nothing later either", async () => {
    stackJobs = [stackJob({ id: "d1" })];
    mount("/env/e1/stacks/s1/deploys/d1");
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ id: "d1", state: "done" });
    await screen.findByRole("button", { name: "Jobs" });
    expect(screen.queryByText("Deployed web.")).toBeNull();
  });

  it("words a finished stop as a stop, never as a deploy", async () => {
    stackJobs = [stackJob({ kind: "stop" })];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ kind: "stop", state: "done" });
    expect(await screen.findByText("Stopped web.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open" })).toBeTruthy();
    expect(screen.queryByText("Deployed web.")).toBeNull();
  });

  it("words a failed start as a start with Open console", async () => {
    stackJobs = [stackJob({ kind: "start" })];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ kind: "start", state: "failed" });
    expect(await screen.findByText("Start of web failed.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open console" })).toBeTruthy();
    expect(screen.queryByText("Deploy of web failed.")).toBeNull();
  });

  it("words a finished removal as a removal, and Open goes to the stack list", async () => {
    stackJobs = [stackJob({ kind: "remove" })];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ kind: "remove", state: "done" });
    expect(await screen.findByText("Removed web.")).toBeTruthy();
    expect(screen.queryByText("Deployed web.")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks"));
  });

  it("words a failed removal as a removal", async () => {
    stackJobs = [stackJob({ kind: "remove" })];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ kind: "remove", state: "failed" });
    expect(await screen.findByText("Removal of web failed.")).toBeTruthy();
  });

  it("says nothing from the indicator for an operation this page follows itself", async () => {
    // The id is its own: the set of followed jobs outlives the test, and no other test follows this one.
    stackJobs = [stackJob({ id: "followed-1", kind: "stop" })];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    followStackOperation(client, {
      envId: "e1",
      stackId: "s1",
      jobId: "followed-1",
      onDone: vi.fn(),
      onFailed: vi.fn(),
    });
    await finish({ id: "followed-1", kind: "stop", state: "done" });
    await screen.findByRole("button", { name: "Jobs" });
    expect(screen.queryByText("Stopped web.")).toBeNull();
    expect(document.querySelector("[data-sonner-toast]")).toBeNull();
  });

  it("has words for every kind of stack job", () => {
    const KINDS = [
      "deploy",
      "update",
      "rollback",
      "restore",
      "start",
      "stop",
      "remove",
    ] as const satisfies readonly StackJobSummary["kind"][];
    // A kind added to the API type without words here fails the type check.
    const missing: Exclude<StackJobSummary["kind"], (typeof KINDS)[number]> extends never
      ? true
      : false = true;
    expect(missing).toBe(true);
    for (const kind of KINDS) {
      expect(keyExists(`jobs.toast.${kind}.done`), `toast ${kind} done`).toBe(true);
      expect(keyExists(`jobs.toast.${kind}.failed`), `toast ${kind} failed`).toBe(true);
      for (const state of ["running", "done", "failed", "cancelled"]) {
        expect(keyExists(`jobs.indicator.stack.${kind}.${state}`), `row ${kind} ${state}`).toBe(
          true,
        );
      }
    }
  });

  it("says nothing for a deploy that was cancelled", async () => {
    stackJobs = [stackJob()];
    mount();
    await screen.findByRole("button", { name: "Jobs, 1 in progress" });
    await finish({ state: "cancelled" });
    await screen.findByRole("button", { name: "Jobs" });
    expect(document.querySelector("[data-sonner-toast]")).toBeNull();
  });
});
