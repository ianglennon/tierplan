// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
  useParams,
} from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { stackJobQuery } from "@/api/stacks";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { DeployConsole } from "@/features/stack-deploy/deploy-console";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

// The deploy console (plan 07-17, STK-07, D-13): the job as the server holds it, read once over
// HTTP and then followed over server-sent events, with the HTTP read as the fallback while the stream
// is down. The server is a stubbed fetch and a FakeEventSource the test drives.

const ENV = "e1";
const STACK = "s1";

interface Item {
  id: string;
  label: string;
  state: "waiting" | "running" | "done" | "failed" | "skipped" | "cancelled";
  parent?: string;
  kind?: string;
  message?: string;
  detail?: string;
  code?: string;
  current?: number;
  total?: number;
  unit?: string;
  result?: Record<string, string>;
}

interface Line {
  seq: number;
  at: string;
  text: string;
}

let job: { state: string; items: Item[] };
let output: Line[];
let status: number;
let reads: number;
let client: QueryClient;
// What the caller may do on the environment, and what the server answers a cancel with.
let role: "viewer" | "operator";
let cancelStatus: number;
let cancels: Request[];
// What the stack's page says of the way back, what the server answers a roll back with (202 starts
// job j2; any other status is a problem of that type), and the rolls back that were sent.
let rollbackInfo: { available: boolean; deployedAt: string; deployedBy: string };
let rollbackAnswer: { status: number; type?: string };
let rollbacks: Request[];
// The mode and state of the job j2 a roll back starts.
let rollbackJob: { state: string; items: Item[] };

const item = (over: Item): Record<string, unknown> => ({
  code: "",
  current: 0,
  total: 0,
  unit: "",
  message: "",
  detail: "",
  parent: "",
  kind: "step",
  result: {},
  ...over,
});

function jobBody(over: Partial<{ state: string; items: Item[]; id: string; mode: string }> = {}) {
  const j = { ...job, ...over };
  return {
    id: over.id ?? "j1",
    stackId: STACK,
    mode: over.mode ?? "deploy",
    startedBy: "ada",
    state: j.state,
    createdAt: "2026-10-19T12:00:00Z",
    finishedAt: j.state === "running" ? "" : "2026-10-19T12:00:09Z",
    items: j.items.map(item),
    output,
  };
}

const check: Item = { id: "check", label: "Check the stack", state: "done" };
const lookup: Item = { id: "lookup", label: "Look up images", state: "running" };
const services: Item = { id: "services", label: "Deploy services", state: "waiting", total: 2 };
const row = (name: string, state: Item["state"], over: Partial<Item> = {}): Item => ({
  id: `service:s1_${name}`,
  label: name,
  state,
  parent: "services",
  kind: "service",
  ...over,
});

beforeEach(() => {
  job = { state: "running", items: [check, lookup, services] };
  output = [];
  status = 200;
  reads = 0;
  role = "viewer";
  cancelStatus = 202;
  cancels = [];
  rollbackInfo = { available: true, deployedAt: "2026-01-02T10:00:00Z", deployedBy: "ada" };
  rollbackAnswer = { status: 202 };
  rollbacks = [];
  rollbackJob = { state: "running", items: [check, services] };
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  // The job query keeps its own retry rule; a zero delay makes its three tries instant.
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}`) {
        return Response.json({
          id: STACK,
          name: "web",
          target: "swarm",
          source: "inline",
          state: "deploying",
          lastDeployedAt: "",
          lastDeployedBy: "",
          hasDraft: false,
          envId: ENV,
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: false,
          rollback: rollbackInfo,
        });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/rollback` && req.method === "POST") {
        rollbacks.push(req);
        if (rollbackAnswer.status === 202) {
          return Response.json({ jobId: "j2", revision: 4 }, { status: 202 });
        }
        return Response.json(
          {
            type: rollbackAnswer.type,
            title: "x",
            status: rollbackAnswer.status,
            detail: "d",
          },
          { status: rollbackAnswer.status },
        );
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/deploys/j2`) {
        return Response.json(jobBody({ ...rollbackJob, id: "j2", mode: "rollback" }));
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/deploys/j1/cancel` && req.method === "POST") {
        cancels.push(req);
        if (cancelStatus === 202) return Response.json({ jobId: "j1" }, { status: 202 });
        return Response.json(
          { type: "job_finished", title: "Conflict", status: cancelStatus, detail: "ended" },
          { status: cancelStatus },
        );
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/deploys/j1`) {
        reads += 1;
        if (status === 404) {
          return Response.json(
            { type: "stack_job_not_found", title: "Not Found", status: 404, detail: "gone" },
            { status: 404 },
          );
        }
        if (status !== 200) return Response.json({ title: "x", status }, { status });
        return Response.json(jobBody());
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <DeployConsole envId={ENV} stackId={STACK} jobId="j1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The stream of the console, once it has opened.
const source = (n = 0): FakeEventSource => {
  const s = FakeEventSource.instances[n];
  if (!s) throw new Error(`no stream ${n} is open`);
  return s;
};

// Opens the console and lets the stream say where the job stands.
async function mountLive(body = jobBody()) {
  mount();
  await screen.findByRole("heading", { name: "Deploying web" });
  act(() => source().sendFrame("snapshot", body));
}

const stepLines = (): string[] =>
  within(screen.getByRole("list", { name: "Steps of the deploy" }))
    .getAllByRole("listitem")
    .filter((li) => li.getAttribute("data-slot") !== "deploy-service-row")
    .map((li) => li.firstElementChild?.textContent ?? "");

describe("a running deploy", () => {
  it("is titled for the stack and lists the steps in order with the state in words", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Deploying web" })).toBeTruthy();
    expect(stepLines()).toEqual([
      "Check the stackDone",
      "Look up imagesWorking",
      "Deploy servicesWaiting",
    ]);
    // No result until the job ends.
    expect(screen.queryByText("Deploy complete")).toBeNull();
    expect(screen.queryByText("Deploy failed")).toBeNull();
  });

  it("says who started it and when, and how long it has run", async () => {
    mount();
    await screen.findByRole("heading", { name: "Deploying web" });
    expect(screen.getByText(/^Started .* by ada\./)).toBeTruthy();
    expect(screen.getByText(/^Running for .+\.$/).getAttribute("aria-live")).toBe("off");
  });

  it("shows a skeleton until the first snapshot", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>(() => {})),
    );
    mount();
    expect(await screen.findByRole("status", { name: "Loading the deploy…" })).toBeTruthy();
  });

  it("moves the focus to the title once, when the console opens", async () => {
    mount();
    const title = await screen.findByRole("heading", { name: "Deploying web" });
    expect(document.activeElement).toBe(title);
  });

  it("draws a step label with markup in it as text", async () => {
    job.items[0] = { id: "check", label: "<b>Check</b>", state: "done" };
    mount();
    const label = await screen.findByText("<b>Check</b>");
    expect(label.querySelector("b")).toBeNull();
  });
});

describe("the steps and the service rows", () => {
  it("lists a network, a config and the services in run order whatever order they were added in", async () => {
    // The server adds the objects after the steps it starts with, so the items arrive out of run
    // order; the group is not a line, its steps stand in its place.
    job = {
      state: "running",
      items: [
        check,
        lookup,
        { id: "networks", label: "Networks", state: "running", kind: "group" },
        { id: "objects", label: "Secrets and configs", state: "waiting", kind: "group" },
        services,
        { id: "cleanup", label: "Remove services no longer in the stack", state: "waiting" },
        {
          id: "network:s1_default",
          label: "Create network s1_default",
          state: "running",
          parent: "networks",
        },
        {
          id: "config:s1_conf",
          label: "Create config conf",
          state: "waiting",
          parent: "objects",
        },
        row("api", "waiting"),
        row("web", "waiting"),
        {
          id: "remove-secret:s1_old",
          label: "Remove old secret old",
          state: "waiting",
          parent: "cleanup",
        },
      ],
    };
    await mountLive(jobBody());
    expect(stepLines()).toEqual([
      "Check the stackDone",
      "Look up imagesWorking",
      "Create network s1_defaultWorking",
      "Create config confWaiting",
      "Deploy servicesWaiting",
      "Remove services no longer in the stackWaiting",
      "Remove old secret oldWaiting",
    ]);
    const rows = within(screen.getByRole("list", { name: "Services" })).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual(["Waitingapi", "Waitingweb"]);
  });

  it("reads each service row's state in words", async () => {
    job = {
      state: "running",
      items: [
        check,
        { ...services, state: "running" },
        row("api", "done"),
        row("web", "running"),
        row("worker", "waiting"),
        row("cron", "skipped"),
      ],
    };
    await mountLive(jobBody());
    const text = within(screen.getByRole("list", { name: "Services" }))
      .getAllByRole("listitem")
      .map((r) => r.textContent);
    expect(text).toEqual(["Runningapi", "Workingweb", "Waitingworker", "Skippedcron"]);
  });

  it("counts the services done against the services there are, plural-aware, and moves the bar", async () => {
    job = { state: "running", items: [check, services, row("api", "done"), row("web", "waiting")] };
    await mountLive(jobBody());
    expect(screen.getByText("1 of 2 services done")).toBeTruthy();
    const bar = screen.getByRole("progressbar", { name: "Deploy progress" });
    expect(bar.getAttribute("aria-valuenow")).toBe("50");
    cleanup();

    job = { state: "running", items: [check, services, row("api", "waiting")] };
    FakeEventSource.reset();
    await mountLive(jobBody());
    expect(screen.getByText("0 of 1 service done")).toBeTruthy();
  });

  it("lists the steps as done for a stack with nothing to converge", async () => {
    job = {
      state: "done",
      items: [
        check,
        { ...lookup, state: "done" },
        { ...services, state: "done" },
        { id: "cleanup", label: "Remove services no longer in the stack", state: "done" },
      ],
    };
    mount();
    expect(await screen.findByText("Deploy complete")).toBeTruthy();
    expect(stepLines()).toEqual([
      "Check the stackDone",
      "Look up imagesDone",
      "Deploy servicesDone",
      "Remove services no longer in the stackDone",
    ]);
    expect(screen.getByText("0 of 0 services done")).toBeTruthy();
  });

  it("draws a service name with markup in it as text", async () => {
    job = { state: "running", items: [check, services, row("<i>web</i>", "waiting")] };
    await mountLive(jobBody());
    const name = await screen.findByText("<i>web</i>");
    expect(name.querySelector("i")).toBeNull();
  });
});

// A task of a service, as the server lists it under the service's row.
const task = (service: string, slot: number, state: string, node: string, detail = ""): Item => ({
  id: `task:t${service}${slot}`,
  label: `s1_${service}.${slot}`,
  state: "running",
  parent: `service:s1_${service}`,
  kind: "task",
  detail,
  result: { state, node },
});

describe("a service coming up (plan 07-18, D-13)", () => {
  const converging = (running: number, total: number, over: Partial<Item> = {}): Item =>
    row("api", "running", {
      current: running,
      total,
      unit: "tasks",
      result: { action: "updated" },
      ...over,
    });

  it("reads Converging k/n with a bar named for the service and the task count beside it", async () => {
    job = { state: "running", items: [check, { ...services, state: "running" }, converging(1, 3)] };
    await mountLive(jobBody());
    const list = within(screen.getByRole("list", { name: "Services" }));
    expect(list.getByText("Converging 1/3")).toBeTruthy();
    const bar = list.getByRole("progressbar", { name: "Convergence of api" });
    expect(bar.getAttribute("aria-valuenow")).toBe("33");
    expect(list.getByText("1/3 tasks")).toBeTruthy();
  });

  it("reads Pulling image while a task prepares its image", async () => {
    job = {
      state: "running",
      items: [
        check,
        { ...services, state: "running" },
        converging(0, 2, { result: { action: "created", phase: "pulling" } }),
      ],
    };
    await mountLive(jobBody());
    expect(screen.getByText("Pulling image")).toBeTruthy();
    expect(screen.queryByText("Converging 0/2")).toBeNull();
  });

  it("follows the tasks coming up from the stream: Converging 0/2, then 2/2 and Running", async () => {
    job = { state: "running", items: [check, { ...services, state: "running" }, converging(0, 2)] };
    await mountLive(jobBody());
    expect(screen.getByText("Converging 0/2")).toBeTruthy();
    act(() => {
      source().sendFrame("item", item(converging(1, 2)), "6");
    });
    expect(screen.getByText("Converging 1/2")).toBeTruthy();
    act(() => {
      source().sendFrame(
        "item",
        item(
          row("api", "done", {
            current: 2,
            total: 2,
            unit: "tasks",
            result: { action: "updated", settled: "true", running: "2", desired: "2" },
          }),
        ),
        "7",
      );
    });
    expect(screen.getByText("Running")).toBeTruthy();
    expect(screen.queryByText(/^Converging/)).toBeNull();
  });

  it("lists the tasks under the row, with their state, node and error, when it is opened", async () => {
    job = {
      state: "running",
      items: [
        check,
        { ...services, state: "running" },
        converging(1, 2),
        task("api", 1, "running", "mgr-1"),
        task("api", 2, "rejected", "wrk-1", "No such image: api:2"),
      ],
    };
    await mountLive(jobBody());
    // Collapsed to begin with: the table is not drawn until the row opens.
    expect(screen.queryByRole("table", { name: "Tasks of api" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Show what happened to api/ }));
    const table = within(screen.getByRole("table", { name: "Tasks of api" }));
    expect(table.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Task",
      "State",
      "Node",
      "Error",
    ]);
    const rows = table.getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      "s1_api.1Runningmgr-1",
      "s1_api.2Rejectedwrk-1No such image: api:2",
    ]);
  });

  it("reads Timed out with the sentence, the minutes and Docker's last words when the service never settled", async () => {
    job = {
      state: "failed",
      items: [
        check,
        { ...services, state: "failed" },
        row("web", "failed", {
          code: "converge_timeout",
          message: "The service didn't settle in time. Docker may still finish it.",
          detail: "No such image: web:2",
          current: 0,
          total: 2,
          unit: "tasks",
          result: { action: "updated", minutes: "10" },
        }),
      ],
    };
    mount();
    expect(await screen.findByText("Timed out")).toBeTruthy();
    // A failed row starts open.
    expect(
      screen.getAllByText(
        "web didn't settle within 10 minutes. Docker may still finish it. Check the service, or roll back.",
      ).length,
    ).toBeGreaterThan(0);
    expect(screen.getByText("No such image: web:2")).toBeTruthy();
    // The row says Timed out, not Failed; the step above it is the one that failed.
    expect(within(screen.getByRole("list", { name: "Services" })).queryByText("Failed")).toBeNull();
  });

  it("says one minute, not one minutes", async () => {
    job = {
      state: "failed",
      items: [
        check,
        { ...services, state: "failed" },
        row("web", "failed", { code: "converge_timeout", result: { minutes: "1" } }),
      ],
    };
    mount();
    expect(
      (await screen.findAllByText(/web didn't settle within 1 minute\. /)).length,
    ).toBeGreaterThan(0);
  });

  it("reads Updated, not Running, for a service the deploy took but did not wait for", async () => {
    job = {
      state: "failed",
      items: [
        check,
        { ...services, state: "failed" },
        row("api", "done", { result: { action: "updated", settled: "false" } }),
        row("web", "done", { result: { action: "created", settled: "false" } }),
      ],
    };
    mount();
    await screen.findByText("Updated");
    expect(screen.getByText("Created")).toBeTruthy();
    expect(screen.queryByText("Running", { selector: "span" })).toBeNull();
  });

  it("draws a task's error, node and name as text", async () => {
    job = {
      state: "running",
      items: [
        check,
        { ...services, state: "running" },
        converging(0, 1),
        { ...task("api", 1, "rejected", "<b>n</b>", "<i>bad</i>"), label: "<u>t</u>" },
      ],
    };
    await mountLive(jobBody());
    fireEvent.click(screen.getByRole("button", { name: /Show what happened to api/ }));
    for (const markup of ["<b>n</b>", "<i>bad</i>", "<u>t</u>"]) {
      expect(screen.getByText(markup).children).toHaveLength(0);
    }
  });
});

describe("following the stream", () => {
  it("shows Live, takes the stream's snapshot at once and updates a row from an event", async () => {
    job = { state: "running", items: [check, services, row("api", "waiting")] };
    await mountLive(jobBody());
    expect(screen.getByText("Live")).toBeTruthy();
    expect(source().url).toMatch(/\/api\/env\/e1\/stacks\/s1\/deploys\/j1\/stream$/);
    act(() => {
      source().sendFrame("item", item(row("api", "running")), "4");
    });
    expect(screen.getByText("Working", { selector: "span" })).toBeTruthy();
    act(() => {
      source().sendFrame("item", item(row("api", "done")), "5");
    });
    expect(screen.getByText("Running")).toBeTruthy();
    expect(screen.getByText("1 of 1 service done")).toBeTruthy();
  });

  it("adds a service row an event brings, in its place under Deploy services", async () => {
    await mountLive(jobBody({ items: [check, services] }));
    expect(screen.queryByRole("list", { name: "Services" })).toBeNull();
    act(() => {
      source().sendFrame("item", item(row("api", "waiting")), "3");
    });
    const rows = within(screen.getByRole("list", { name: "Services" })).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual(["Waitingapi"]);
  });

  it("ends on the done frame: the result, no chip, and the stream closed", async () => {
    await mountLive(jobBody({ items: [check, services, row("api", "running")] }));
    const done = jobBody({
      state: "done",
      items: [{ ...check }, { ...services, state: "done" }, row("api", "done")],
    });
    act(() => source().sendFrame("done", done, "9"));
    expect(screen.getByRole("heading", { name: "Deploy complete" })).toBeTruthy();
    expect(screen.getByText("web is running 1 service on Prod.")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
    expect(source().closed).toBe(true);
  });

  it("says Reconnecting… on a dropped stream, keeps the last state and asks for the missed events", async () => {
    vi.useFakeTimers({
      toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
    });
    mount();
    await waitUnderFakeClock(() => {
      expect(screen.getByRole("heading", { name: "Deploying web" })).toBeTruthy();
    });
    act(() => {
      source().sendFrame("snapshot", jobBody({ items: [check, services, row("api", "waiting")] }));
      source().sendFrame("item", item(row("api", "running")), "7");
    });
    expect(screen.getByText("Live")).toBeTruthy();
    act(() => source().fail());
    expect(screen.getByText("Reconnecting…")).toBeTruthy();
    // The last state stays on screen.
    expect(screen.getByRole("heading", { name: "Deploying web" })).toBeTruthy();
    expect(screen.getByText("api")).toBeTruthy();
    // After one second a new connection names the last event it saw.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(source(1).url).toMatch(/stream\?lastEventId=7$/);
    act(() => source(1).sendFrame("snapshot", jobBody()));
    expect(screen.getByText("Live")).toBeTruthy();
    expect(screen.queryByText("Reconnecting…")).toBeNull();
  });
});

describe("polling", () => {
  it("asks again every second while the stream is not live and stops once it is or the job ended", async () => {
    // Timers are faked before the page opens, so the interval it starts is the fake one.
    vi.useFakeTimers({
      toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
    });
    mount();
    await waitUnderFakeClock(() => {
      expect(screen.getByRole("heading", { name: "Deploying web" })).toBeTruthy();
    });
    const before = reads;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });
    expect(reads).toBeGreaterThan(before);

    // Once the stream is live, nothing is polled.
    act(() => source().sendFrame("snapshot", jobBody()));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100);
    });
    const live = reads;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(reads).toBe(live);

    // A stream that drops goes back to asking.
    act(() => source().fail());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });
    expect(reads).toBeGreaterThan(live);
  });

  it("follows a job over HTTP alone when the browser has no EventSource", async () => {
    vi.stubGlobal("EventSource", undefined);
    job = { state: "done", items: [{ ...check }, { ...services, state: "done" }] };
    mount();
    expect(await screen.findByText("Deploy complete")).toBeTruthy();
  });

  it("asks every second while running, never while done, and never again for a job that is gone", () => {
    const options = stackJobQuery(ENV, STACK, "j1");
    const interval = options.refetchInterval as (q: unknown) => number | false;
    const state = (data: unknown, error: unknown = null) => ({ state: { data, error } });
    expect(interval(state({ state: "running" }))).toBe(1000);
    expect(interval(state(undefined))).toBe(1000);
    for (const ended of ["done", "partial", "failed", "cancelled"]) {
      expect(interval(state({ state: ended }))).toBe(false);
    }
  });
});

describe("a deploy that ended", () => {
  it("says Deploy complete with what is running and a way to open the stack", async () => {
    job = {
      state: "done",
      items: [
        { ...services, state: "done", current: 3, total: 3 },
        row("a", "done"),
        row("b", "done"),
        row("c", "done"),
      ],
    };
    mount();
    expect(await screen.findByText("Deploy complete")).toBeTruthy();
    expect(screen.getByText("web is running 3 services on Prod.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open stack" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1",
    );
  });

  it("says service, not services, for one", async () => {
    job = { state: "done", items: [{ ...services, state: "done" }, row("a", "done")] };
    mount();
    expect(await screen.findByText("web is running 1 service on Prod.")).toBeTruthy();
  });

  it("says Deploy failed with the reason of the step that failed", async () => {
    job = {
      state: "failed",
      items: [
        check,
        {
          ...services,
          state: "failed",
          message: "Couldn't update web: no such image.",
        },
      ],
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Deploy failed")).toBeTruthy();
    expect(within(alert).getByText("Couldn't update web: no such image.")).toBeTruthy();
    expect(within(alert).getByRole("link", { name: "Back to editing" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/editor",
    );
    expect(within(alert).getByRole("link", { name: "Open stack" })).toBeTruthy();
    expect(screen.queryByText("Deploy complete")).toBeNull();
  });

  it("names the step it stopped at when the server gave no sentence", async () => {
    job = { state: "failed", items: [{ ...lookup, state: "failed" }] };
    mount();
    expect(await screen.findByText("Bayplan stopped at Look up images.")).toBeTruthy();
  });

  it("reads a partly done job as failed", async () => {
    job = {
      state: "partial",
      items: [{ ...services, state: "failed", message: "web failed to start." }],
    };
    mount();
    expect(await screen.findByText("Deploy failed")).toBeTruthy();
  });

  it("says Deploy cancelled", async () => {
    job = { state: "cancelled", items: [{ ...services, state: "cancelled" }] };
    mount();
    expect(await screen.findByText("Deploy cancelled")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open stack" })).toBeTruthy();
  });
});

describe("a job that is not listed", () => {
  it("says so and offers the stack, and does not ask again", async () => {
    status = 404;
    vi.useFakeTimers({
      toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
    });
    mount();
    await waitUnderFakeClock(() => {
      expect(screen.getByRole("heading", { name: "This deploy is no longer listed" })).toBeTruthy();
    });
    expect(
      screen.getByText(
        "Finished deploys stay here for a short while. Open the stack to see what is running now.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open stack" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1",
    );
    const after = reads;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(reads).toBe(after);
  });

  it("says so when the stream is refused with a 404 after it dropped", async () => {
    vi.useFakeTimers({
      toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
    });
    mount();
    await waitUnderFakeClock(() => {
      expect(screen.getByRole("heading", { name: "Deploying web" })).toBeTruthy();
    });
    act(() => source().sendFrame("snapshot", jobBody()));
    status = 404;
    // The browser gave up on a refused stream (readyState 2); the probe learns it is a 404.
    act(() => source().fail(true));
    await waitUnderFakeClock(() => {
      expect(screen.getByRole("heading", { name: "This deploy is no longer listed" })).toBeTruthy();
    });
  });

  it("says it could not load the deploy, with a way to try again", async () => {
    status = 500;
    mount();
    expect(await screen.findByText("Couldn't load the deploy.")).toBeTruthy();
    status = 200;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading the deploy" }));
    expect(await screen.findByRole("heading", { name: "Deploying web" })).toBeTruthy();
  });
});

const line = (seq: number, text: string, at = "2026-10-19T12:00:03Z"): Line => ({ seq, at, text });
const CLOCK = /^\d{2}:\d{2}:\d{2} /;

describe("the output", () => {
  it("is collapsed while the deploy runs, with a trigger named Output", async () => {
    output = [line(1, "Created network web_default")];
    await mountLive(jobBody());
    const trigger = screen.getByRole("button", { name: "Output" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("log")).toBeNull();
  });

  it("stays collapsed after a deploy that worked", async () => {
    job = { state: "done", items: [{ ...services, state: "done" }, row("api", "done")] };
    output = [line(1, "Created service s1_api")];
    mount();
    await screen.findByText("Deploy complete");
    expect(screen.getByRole("button", { name: "Output" }).getAttribute("aria-expanded")).toBe(
      "false",
    );
    expect(screen.queryByRole("log")).toBeNull();
  });

  it("opens by itself when the deploy fails", async () => {
    job = {
      state: "failed",
      items: [{ ...services, state: "failed" }, row("api", "failed", { code: "service_rejected" })],
    };
    output = [line(1, "Failed service s1_api: no such image")];
    mount();
    await screen.findByText("Deploy failed");
    expect(screen.getByRole("button", { name: "Output" }).getAttribute("aria-expanded")).toBe(
      "true",
    );
    expect(screen.getByRole("log")).toBeTruthy();
  });

  it("opens when the stream says the deploy failed, and a person can close it again", async () => {
    await mountLive(jobBody({ items: [check, services, row("api", "running")] }));
    expect(screen.queryByRole("log")).toBeNull();
    act(() =>
      source().sendFrame(
        "done",
        {
          ...jobBody({
            state: "failed",
            items: [check, { ...services, state: "failed" }, row("api", "failed")],
          }),
          output: [line(2, "Failed service s1_api: no such image")],
        },
        "9",
      ),
    );
    expect(screen.getByRole("log")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Output" }));
    expect(screen.queryByRole("log")).toBeNull();
  });

  it("is a log region that does not announce, with each line as a time and its text", async () => {
    output = [
      line(1, "Created network web_default", "2026-10-19T12:00:03Z"),
      line(2, "Created service s1_api", "2026-10-19T12:00:04Z"),
    ];
    await mountLive(jobBody());
    fireEvent.click(screen.getByRole("button", { name: "Output" }));
    const log = screen.getByRole("log", { name: "Output" });
    expect(log.getAttribute("aria-live")).toBe("off");
    const lines = (log.textContent ?? "").split("\n").filter(Boolean);
    expect(lines).toHaveLength(2);
    expect(lines[0]).toMatch(CLOCK);
    expect(lines[0]).toMatch(/ Created network web_default$/);
    expect(lines[1]).toMatch(/ Created service s1_api$/);
  });

  it("adds a line an output frame brings, once, however often it is sent", async () => {
    await mountLive(jobBody());
    fireEvent.click(screen.getByRole("button", { name: "Output" }));
    expect(screen.getByText("No output yet.")).toBeTruthy();
    act(() => {
      source().sendFrame("output", line(3, "Created network web_default"), "3");
      source().sendFrame("output", line(3, "Created network web_default"), "3");
    });
    const log = screen.getByRole("log");
    expect(log.textContent?.match(/Created network web_default/g)).toHaveLength(1);
    expect(screen.queryByText("No output yet.")).toBeNull();
  });

  it("draws a line with markup in it, and a hidden secret, as text", async () => {
    output = [line(1, "<b>TOKEN=<hidden></b>")];
    await mountLive(jobBody());
    fireEvent.click(screen.getByRole("button", { name: "Output" }));
    const log = screen.getByRole("log");
    expect(log.querySelector("b")).toBeNull();
    expect(log.textContent).toContain("<b>TOKEN=<hidden></b>");
  });

  it("wraps long lines when asked to, and not otherwise", async () => {
    output = [line(1, "x".repeat(300))];
    await mountLive(jobBody());
    fireEvent.click(screen.getByRole("button", { name: "Output" }));
    const wrap = screen.getByRole("button", { name: "Wrap" });
    expect(wrap.getAttribute("aria-pressed")).toBe("false");
    expect(screen.getByRole("log").className).toContain("whitespace-pre");
    expect(screen.getByRole("log").className).not.toContain("whitespace-pre-wrap");
    fireEvent.click(wrap);
    expect(wrap.getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("log").className).toContain("whitespace-pre-wrap");
  });

  it("copies the output as it reads on the screen", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    output = [line(1, "Created network web_default"), line(2, "Created service s1_api")];
    await mountLive(jobBody());
    fireEvent.click(screen.getByRole("button", { name: "Output" }));
    fireEvent.click(screen.getByRole("button", { name: "Copy output" }));
    expect(writeText).toHaveBeenCalledTimes(1);
    const text = (writeText.mock.calls[0] as unknown as [string])[0];
    expect(text.split("\n")).toHaveLength(2);
    expect(text).toMatch(
      /^\d{2}:\d{2}:\d{2} Created network web_default\n\d{2}:\d{2}:\d{2} Created service s1_api$/,
    );
  });
});

describe("why a service failed", () => {
  const failedJob = (code: string, detail: string, message = ""): typeof job => ({
    state: "partial",
    items: [
      check,
      { ...services, state: "failed" },
      row("api", "failed", { code, detail, message }),
      row("web", "skipped", {
        code: "not_run",
        message: "Not run because an earlier step failed.",
      }),
    ],
  });

  it("starts the failed row open, with Docker's words under it", async () => {
    job = failedJob(
      "service_rejected",
      "no such image: nginx:bad",
      "The swarm rejected the service.",
    );
    mount();
    await screen.findByText("Deploy failed");
    const trigger = screen.getByRole("button", { name: /Show what happened to api/ });
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("Docker rejected api: no such image: nginx:bad")).toBeTruthy();
  });

  it("reads later services as Skipped", async () => {
    job = failedJob("service_rejected", "no such image");
    mount();
    await screen.findByText("Deploy failed");
    const rows = within(screen.getByRole("list", { name: "Services" })).getAllByRole("listitem");
    expect(rows[1]?.textContent).toContain("Skipped");
    expect(rows[1]?.textContent).toContain("web");
    // A skipped row has nothing to open.
    expect(within(rows[1] as HTMLElement).queryByRole("button")).toBeNull();
  });

  it("words another code with the server's sentence and Docker's words", async () => {
    job = failedJob("agent_policy_denied", "denied by policy", "The agent refused the request.");
    mount();
    await screen.findByText("Deploy failed");
    expect(screen.getAllByText("The agent refused the request.").length).toBeGreaterThan(0);
    expect(screen.getByText("denied by policy")).toBeTruthy();
  });

  it("draws Docker's words as text, wrapped", async () => {
    job = failedJob("service_rejected", "<script>alert(1)</script>".repeat(5));
    mount();
    await screen.findByText("Deploy failed");
    const text = screen.getByText(/^Docker rejected api: <script>/);
    expect(text.querySelector("script")).toBeNull();
    expect(text.className).toContain("break-all");
  });
});

describe("the result", () => {
  it("offers Open stack and Open logs when the deploy worked", async () => {
    job = { state: "done", items: [{ ...services, state: "done" }, row("api", "done")] };
    mount();
    await screen.findByText("Deploy complete");
    expect(screen.getByRole("link", { name: "Open stack" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1",
    );
    expect(screen.getByRole("link", { name: "Open logs" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/logs",
    );
  });

  it("says where it stopped and how many services were updated, in an alert", async () => {
    job = {
      state: "partial",
      items: [
        check,
        { ...services, state: "failed" },
        row("api", "done"),
        row("web", "failed", { code: "service_rejected", detail: "no such image" }),
      ],
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Deploy failed")).toBeTruthy();
    expect(
      within(alert).getByText(
        "Bayplan stopped at web. 1 of 2 services were updated before it stopped, and nothing was rolled back.",
      ),
    ).toBeTruthy();
    expect(within(alert).getByRole("link", { name: "Back to editing" })).toBeTruthy();
  });
});

// A deploy that stopped (plan 07-18, D-14): where it stopped, what the deploy cleaned up, and what it
// could not.
describe("the result of a deploy that stopped", () => {
  const cleanup1 = (kind: string, name: string, over: Partial<Item> = {}): Item => ({
    id: `clean-${kind}:${name}`,
    label: `Remove ${kind} ${name}`,
    state: "done",
    parent: "cleanup",
    kind: "step",
    result: { cleaned: "true", kind, name },
    ...over,
  });
  const stopped: Item[] = [
    check,
    { ...services, state: "failed" },
    row("api", "done", { result: { action: "updated", settled: "false" } }),
    row("web", "failed", { code: "service_rejected", detail: "no such image" }),
    row("db", "skipped", { code: "not_run" }),
    {
      id: "cleanup",
      label: "Remove services no longer in the stack",
      state: "skipped",
      code: "not_run",
    },
  ];

  it("counts the services the swarm took, and says the objects nothing uses were removed", async () => {
    job = {
      state: "partial",
      items: [...stopped, cleanup1("network", "s1_front"), cleanup1("secret", "s1_tok_ab12")],
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(
      within(alert).getByText(
        "Bayplan stopped at web. 1 of 3 services were updated before it stopped, and nothing was rolled back.",
      ),
    ).toBeTruthy();
    expect(
      within(alert).getByText(
        "The 2 objects created for this deploy that nothing uses were removed.",
      ),
    ).toBeTruthy();
    expect(within(alert).queryByText(/couldn't remove/)).toBeNull();
  });

  it("lists each removal in the steps as Cleaned up", async () => {
    job = { state: "partial", items: [...stopped, cleanup1("network", "s1_front")] };
    mount();
    await screen.findByRole("alert");
    const lines = within(screen.getByRole("list", { name: "Steps of the deploy" }))
      .getAllByRole("listitem")
      .filter((li) => li.getAttribute("data-slot") !== "deploy-service-row")
      .map((li) => li.firstElementChild?.textContent ?? "");
    expect(lines).toContain("Remove network s1_frontCleaned up");
  });

  it("names what it could not remove and where to remove it", async () => {
    job = {
      state: "partial",
      items: [
        ...stopped,
        cleanup1("secret", "s1_tok_ab12"),
        cleanup1("network", "s1_front", {
          state: "failed",
          code: "remove_failed",
          result: { cleaned: "false", left: "failed", kind: "network", name: "s1_front" },
        }),
        cleanup1("config", "s1_conf_cd34", {
          state: "failed",
          code: "remove_failed",
          result: { cleaned: "false", left: "failed", kind: "config", name: "s1_conf_cd34" },
        }),
      ],
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(
      within(alert).getByText(
        "Bayplan couldn't remove network s1_front, config s1_conf_cd34. Remove them under Networks, Secrets or Configs.",
      ),
    ).toBeTruthy();
  });

  it("says nothing of a network that stays because a service uses it", async () => {
    job = {
      state: "partial",
      items: [
        ...stopped,
        cleanup1("network", "s1_front", {
          result: { cleaned: "false", left: "in_use", kind: "network", name: "s1_front" },
        }),
      ],
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(within(alert).queryByText(/couldn't remove/)).toBeNull();
    expect(within(alert).queryByText(/nothing uses were removed/)).toBeNull();
  });

  it("carries the timeout state of a service that did not settle", async () => {
    job = {
      state: "partial",
      items: [
        check,
        { ...services, state: "failed", code: "converge_timeout" },
        row("web", "failed", {
          code: "converge_timeout",
          detail: "No such image: web:2",
          result: { action: "updated", minutes: "10" },
        }),
      ],
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(
      within(alert).getByText(
        "web didn't settle within 10 minutes. Docker may still finish it. Check the service, or roll back.",
      ),
    ).toBeTruthy();
    expect(
      within(alert).getByText(
        "Bayplan stopped at web. 1 of 1 service was updated before it stopped, and nothing was rolled back.",
      ),
    ).toBeTruthy();
  });

  it("reads cancelled with how far it got and what was cleaned up", async () => {
    job = {
      state: "cancelled",
      items: [
        check,
        { ...services, state: "cancelled", code: "cancelled" },
        row("api", "done", { result: { action: "created", settled: "false" } }),
        row("web", "cancelled", { code: "not_run" }),
        cleanup1("secret", "s1_tok_ab12"),
      ],
    };
    mount();
    expect(await screen.findByText("Deploy cancelled")).toBeTruthy();
    expect(
      screen.getByText(
        "Bayplan stopped at Deploy services. 1 of 2 services were already updated. Objects created for this deploy that nothing uses were removed.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to editing" })).toBeTruthy();
  });

  it("leaves the cleanup sentence out of a cancel that removed nothing", async () => {
    job = {
      state: "cancelled",
      items: [
        check,
        { ...services, state: "cancelled", code: "cancelled" },
        row("api", "cancelled", { code: "not_run" }),
      ],
    };
    mount();
    await screen.findByText("Deploy cancelled");
    expect(
      screen.getByText("Bayplan stopped at Deploy services. 0 of 1 service was already updated."),
    ).toBeTruthy();
    expect(screen.queryByText(/nothing uses were removed/)).toBeNull();
  });
});

// Cancel deploy (plan 07-18): asks first, sends one cancel, says Cancelling… until the job reports
// it.
describe("cancelling a deploy", () => {
  const dialog = () => screen.getByRole("alertdialog");

  it("offers Cancel deploy to an Operator while the deploy runs and not after it ended", async () => {
    role = "operator";
    job = { state: "running", items: [check, services, row("api", "running")] };
    await mountLive(jobBody());
    expect(await screen.findByRole("button", { name: "Cancel deploy" })).toBeTruthy();
    act(() => {
      source().sendFrame(
        "done",
        jobBody({
          state: "done",
          items: [check, { ...services, state: "done" }, row("api", "done")],
        }),
        "9",
      );
    });
    expect(screen.queryByRole("button", { name: "Cancel deploy" })).toBeNull();
  });

  it("is off with Needs Operator for a Viewer", async () => {
    job = { state: "running", items: [check, services, row("api", "running")] };
    await mountLive(jobBody());
    const button = await screen.findByRole("button", { name: "Cancel deploy" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    const id = button.getAttribute("aria-describedby");
    expect(document.getElementById(id as string)?.textContent).toBe("Needs Operator");
  });

  it("asks Cancel this deploy? with Keep deploying focused, and sends nothing when it is kept", async () => {
    role = "operator";
    await mountLive(jobBody({ items: [check, services, row("api", "running")] }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel deploy" }));
    expect(within(dialog()).getByText("Cancel this deploy?")).toBeTruthy();
    expect(
      within(dialog()).getByText(
        "Bayplan stops at the step it has reached. Services already updated stay updated, and objects created for this deploy that nothing uses are removed.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(
      within(dialog()).getByRole("button", { name: "Keep deploying" }),
    );
    fireEvent.click(within(dialog()).getByRole("button", { name: "Keep deploying" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(cancels).toHaveLength(0);
  });

  it("sends one cancel on confirming and says Cancelling… until the job reports cancelled", async () => {
    role = "operator";
    await mountLive(jobBody({ items: [check, services, row("api", "running")] }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel deploy" }));
    fireEvent.click(within(dialog()).getByRole("button", { name: "Cancel deploy" }));
    await screen.findByText("Cancelling…");
    expect(cancels).toHaveLength(1);
    expect(new URL(cancels[0]?.url ?? "").pathname).toBe(
      `/api/env/${ENV}/stacks/${STACK}/deploys/j1/cancel`,
    );
    // No second cancel can be sent while one is on its way.
    expect(screen.queryByRole("button", { name: "Cancel deploy" })).toBeNull();

    act(() => {
      source().sendFrame(
        "done",
        jobBody({
          state: "cancelled",
          items: [
            check,
            { ...services, state: "cancelled", code: "cancelled" },
            row("api", "done"),
          ],
        }),
        "9",
      );
    });
    // The result's title, and the polite region saying it once.
    expect(screen.getAllByText("Deploy cancelled")).toHaveLength(2);
    expect(screen.queryByText("Cancelling…")).toBeNull();
  });

  it("goes back to the job as it is when the deploy had already ended", async () => {
    role = "operator";
    cancelStatus = 409;
    await mountLive(jobBody({ items: [check, services, row("api", "running")] }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel deploy" }));
    fireEvent.click(within(dialog()).getByRole("button", { name: "Cancel deploy" }));
    await waitFor(() => expect(cancels).toHaveLength(1));
    // Nothing was cancelled, so the console does not claim it is cancelling.
    await waitFor(() => expect(screen.queryByText("Cancelling…")).toBeNull());
  });
});

describe("the announcements", () => {
  const spoken = () =>
    Array.from(document.querySelectorAll('[data-slot="deploy-announcer"] p')).map(
      (p) => p.textContent,
    );

  it("is one polite region, empty until something happens", async () => {
    await mountLive(jobBody({ items: [check, services, row("api", "waiting")] }));
    const region = document.querySelector('[data-slot="deploy-announcer"]');
    expect(region?.getAttribute("aria-live")).toBe("polite");
    expect(spoken()).toEqual([]);
  });

  it("announces a service that never settled with its sentence, and one that was only taken as nothing", async () => {
    await mountLive(
      jobBody({ items: [check, services, row("api", "waiting"), row("web", "waiting")] }),
    );
    act(() => {
      source().sendFrame(
        "item",
        item(row("api", "done", { result: { action: "updated", settled: "false" } })),
        "4",
      );
      source().sendFrame(
        "item",
        item(row("web", "failed", { code: "converge_timeout", result: { minutes: "10" } })),
        "5",
      );
    });
    expect(spoken()).toEqual([
      "web failed: web didn't settle within 10 minutes. Docker may still finish it. Check the service, or roll back.",
    ]);
  });

  it("announces a service that runs and a service that fails once each, and leaves the failed deploy to its alert", async () => {
    await mountLive(
      jobBody({ items: [check, services, row("api", "waiting"), row("web", "waiting")] }),
    );
    act(() => {
      source().sendFrame("item", item(row("api", "running")), "4");
    });
    expect(spoken()).toEqual([]); // working is not an outcome
    act(() => {
      source().sendFrame("item", item(row("api", "done")), "5");
      source().sendFrame("item", item(row("api", "done")), "5");
    });
    act(() => {
      source().sendFrame(
        "item",
        item(row("web", "failed", { code: "service_rejected", detail: "no such image" })),
        "6",
      );
    });
    act(() =>
      source().sendFrame(
        "done",
        jobBody({
          state: "partial",
          items: [
            check,
            { ...services, state: "failed" },
            row("api", "done"),
            row("web", "failed", { code: "service_rejected", detail: "no such image" }),
          ],
        }),
        "7",
      ),
    );
    // The end of a failed deploy is announced by its alert, once; the polite region does not say it again.
    expect(spoken()).toEqual(["api is running", "web failed: no such image"]);
    expect(screen.getByRole("alert")).toBeTruthy();
  });

  it("announces Deploy complete and Deploy cancelled", async () => {
    await mountLive(jobBody({ items: [check, services, row("api", "running")] }));
    act(() =>
      source().sendFrame(
        "done",
        jobBody({
          state: "done",
          items: [check, { ...services, state: "done" }, row("api", "done")],
        }),
        "8",
      ),
    );
    expect(spoken()).toEqual(["api is running", "Deploy complete"]);
    cleanup();
    FakeEventSource.reset();
    await mountLive(jobBody({ items: [check, services, row("api", "running")] }));
    act(() =>
      source().sendFrame(
        "done",
        jobBody({
          state: "cancelled",
          items: [check, { ...services, state: "cancelled" }, row("api", "cancelled")],
        }),
        "8",
      ),
    );
    expect(spoken()).toEqual(["Deploy cancelled"]);
  });

  it("says nothing about a deploy that had already ended when the console opened", async () => {
    job = { state: "done", items: [{ ...services, state: "done" }, row("api", "done")] };
    mount();
    await screen.findByText("Deploy complete");
    expect(spoken()).toEqual([]);
  });

  it("does not move the focus when it announces", async () => {
    await mountLive(jobBody({ items: [check, services, row("api", "waiting")] }));
    const title = screen.getByRole("heading", { name: "Deploying web" });
    expect(document.activeElement).toBe(title);
    act(() => {
      source().sendFrame("item", item(row("api", "done")), "5");
    });
    expect(document.activeElement).toBe(title);
  });
});

// Rolling back (plan 07-19, STK-07, D-14): after a deploy that stopped, one click redeploys what last
// ran well, and the console becomes the console of the new job.
describe("rolling back", () => {
  const stopped: Item[] = [
    check,
    { ...services, state: "failed" },
    row("web", "failed", { code: "service_rejected", detail: "no such image" }),
  ];

  // The console in a router that can follow the roll back to its job, as the app's route does.
  function mountRouted() {
    function ConsoleRoute() {
      const { envId, stackId, jobId } = useParams({ strict: false }) as {
        envId: string;
        stackId: string;
        jobId: string;
      };
      return <DeployConsole key={jobId} envId={envId} stackId={stackId} jobId={jobId} />;
    }
    const root = createRootRoute();
    const router = createRouter({
      routeTree: root.addChildren([
        createRoute({
          getParentRoute: () => root,
          path: "/env/$envId/stacks/$stackId/deploys/$jobId",
          component: ConsoleRoute,
        }),
        createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId/editor" }),
        createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId" }),
      ]),
      history: createMemoryHistory({ initialEntries: [`/env/${ENV}/stacks/${STACK}/deploys/j1`] }),
    });
    render(
      <QueryClientProvider client={client}>
        <TooltipProvider>
          <RouterProvider router={router} />
          <Toaster />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    return router;
  }

  it("offers Roll back to previous version after a failed deploy, naming when it was deployed and by whom", async () => {
    role = "operator";
    job = { state: "failed", items: stopped };
    mount();
    const alert = await screen.findByRole("alert");
    const button = await within(alert).findByRole("button", {
      name: "Roll back to previous version",
    });
    expect(button.getAttribute("aria-disabled")).toBeNull();
    expect(within(alert).getByText(/^Last deployed .+ by ada$/)).toBeTruthy();
    // It is the accent of the surface, ahead of the ways out.
    expect(button.getAttribute("data-variant")).toBe("default");
    const names = within(alert)
      .getAllByRole("button")
      .concat(within(alert).getAllByRole("link"))
      .map((el) => el.textContent);
    expect(names).toEqual(["Roll back to previous version", "Back to editing", "Open stack"]);
    // Nothing is sent until it is clicked.
    expect(rollbacks).toHaveLength(0);
  });

  it("offers it after a cancelled deploy too", async () => {
    role = "operator";
    job = { state: "cancelled", items: [check, { ...services, state: "cancelled" }] };
    mount();
    expect(
      await screen.findByRole("button", { name: "Roll back to previous version" }),
    ).toBeTruthy();
  });

  it("is off with the reason when there is no earlier deployed version", async () => {
    role = "operator";
    rollbackInfo = { available: false, deployedAt: "", deployedBy: "" };
    job = { state: "failed", items: stopped };
    mount();
    const button = await screen.findByRole("button", { name: "Roll back to previous version" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    const id = button.getAttribute("aria-describedby");
    expect(document.getElementById(id as string)?.textContent).toBe(
      "There's no earlier deployed version to roll back to.",
    );
    expect(screen.queryByText(/^Last deployed/)).toBeNull();
    fireEvent.click(button);
    expect(rollbacks).toHaveLength(0);
  });

  it("is off with Needs Operator for a Viewer", async () => {
    job = { state: "failed", items: stopped };
    mount();
    const button = await screen.findByRole("button", { name: "Roll back to previous version" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    const id = button.getAttribute("aria-describedby");
    expect(document.getElementById(id as string)?.textContent).toBe("Needs Operator");
    fireEvent.click(button);
    expect(rollbacks).toHaveLength(0);
  });

  it("is not offered after a deploy that completed", async () => {
    role = "operator";
    job = { state: "done", items: [check, { ...services, state: "done" }, row("web", "done")] };
    mount();
    await screen.findByText("Deploy complete");
    expect(screen.queryByRole("button", { name: "Roll back to previous version" })).toBeNull();
  });

  it("starts one roll back on a click and replaces the console with the new job's, titled Rolling back", async () => {
    role = "operator";
    job = { state: "failed", items: stopped };
    const router = mountRouted();
    const button = await screen.findByRole("button", { name: "Roll back to previous version" });
    fireEvent.click(button);
    fireEvent.click(button);
    expect(await screen.findByRole("heading", { name: "Rolling back web" })).toBeTruthy();
    // One request, with nothing chosen by the browser.
    expect(rollbacks).toHaveLength(1);
    expect(rollbacks[0]?.method).toBe("POST");
    expect(await rollbacks[0]?.text()).toBe("");
    expect(router.state.location.pathname).toBe("/env/e1/stacks/s1/deploys/j2");
    // Replaced, not pushed: Back leaves the console.
    expect(router.history.canGoBack()).toBe(false);
    // The old job's result is gone with its console.
    expect(screen.queryByText("Deploy failed")).toBeNull();
  });

  it("says Roll back failed with Retry roll back when the roll back itself failed", async () => {
    role = "operator";
    rollbackJob = { state: "failed", items: stopped };
    job = { state: "failed", items: stopped };
    const router = mountRouted();
    fireEvent.click(await screen.findByRole("button", { name: "Roll back to previous version" }));
    await screen.findByRole("heading", { name: "Rolling back web" });
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Roll back failed")).toBeTruthy();
    expect(within(alert).queryByText("Deploy failed")).toBeNull();
    expect(
      within(alert).queryByRole("button", { name: "Roll back to previous version" }),
    ).toBeNull();
    const retry = within(alert).getByRole("button", { name: "Retry roll back" });
    // Retrying sends a new request and moves to its job, which is a console like any other.
    fireEvent.click(retry);
    await waitFor(() => expect(rollbacks).toHaveLength(2));
    expect(router.state.location.pathname).toBe("/env/e1/stacks/s1/deploys/j2");
  });

  it("says Roll back complete when it worked", async () => {
    role = "operator";
    rollbackJob = {
      state: "done",
      items: [check, { ...services, state: "done" }, row("web", "done")],
    };
    job = { state: "failed", items: stopped };
    mountRouted();
    fireEvent.click(await screen.findByRole("button", { name: "Roll back to previous version" }));
    await screen.findByRole("heading", { name: "Rolling back web" });
    expect(await screen.findByText("Roll back complete")).toBeTruthy();
    expect(screen.queryByText("Deploy complete")).toBeNull();
  });

  it.each([
    [
      "operation_locked",
      409,
      "Another stack operation started on Prod just now. Try again when it finishes.",
    ],
    [
      "privileged_compose",
      403,
      "The version to roll back to uses host access, which needs Admin. Ask an administrator to roll it back.",
    ],
    ["no_previous_version", 409, "There's no earlier deployed version to roll back to."],
    [
      "pins_invalid",
      422,
      "That version can't be deployed again as it is. Open the editor to fix it, then deploy.",
    ],
    ["something_else", 500, "Couldn't start the roll back. Try again."],
  ])(
    "stays on the console and says why when the server refuses with %s",
    async (type, status, message) => {
      role = "operator";
      rollbackAnswer = { status, type };
      job = { state: "failed", items: stopped };
      const router = mountRouted();
      fireEvent.click(await screen.findByRole("button", { name: "Roll back to previous version" }));
      expect(await screen.findByText(message)).toBeTruthy();
      expect(router.state.location.pathname).toBe("/env/e1/stacks/s1/deploys/j1");
      // The person can ask again: nothing was retried for them.
      expect(rollbacks).toHaveLength(1);
      await waitFor(() =>
        expect(
          screen
            .getByRole("button", { name: "Roll back to previous version" })
            .hasAttribute("disabled"),
        ).toBe(false),
      );
    },
  );
});
