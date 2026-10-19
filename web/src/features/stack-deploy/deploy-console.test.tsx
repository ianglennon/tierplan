// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { stackJobQuery } from "@/api/stacks";
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
}

let job: { state: string; items: Item[] };
let status: number;
let reads: number;
let client: QueryClient;

const item = (over: Item): Record<string, unknown> => ({
  code: "",
  current: 0,
  total: 0,
  message: "",
  detail: "",
  parent: "",
  kind: "step",
  result: {},
  ...over,
});

function jobBody(over: Partial<{ state: string; items: Item[] }> = {}) {
  const j = { ...job, ...over };
  return {
    id: "j1",
    stackId: STACK,
    mode: "deploy",
    startedBy: "ada",
    state: j.state,
    createdAt: "2026-10-19T12:00:00Z",
    finishedAt: j.state === "running" ? "" : "2026-10-19T12:00:09Z",
    items: j.items.map(item),
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
  status = 200;
  reads = 0;
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  // The job query keeps its own retry rule; a zero delay makes its three tries instant.
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
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
        });
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
    expect(screen.getByText("Deploy complete")).toBeTruthy();
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
