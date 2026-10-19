// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { stackJobQuery } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackJobView } from "@/features/stack-deploy/job-view";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

// The first view of a deploy (plan 07-15, D-26): the job as the server holds it, asked for every
// second while it runs. The server is a stubbed fetch.

const ENV = "e1";
const STACK = "s1";

interface Item {
  id: string;
  label: string;
  state: "waiting" | "running" | "done" | "failed" | "cancelled";
  message?: string;
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
  result: {},
  ...over,
});

function jobBody() {
  return {
    id: "j1",
    stackId: STACK,
    mode: "deploy",
    state: job.state,
    createdAt: "2026-10-19T12:00:00Z",
    finishedAt: job.state === "running" ? "" : "2026-10-19T12:00:09Z",
    items: job.items.map(item),
  };
}

beforeEach(() => {
  job = {
    state: "running",
    items: [
      { id: "check", label: "Check the stack", state: "done" },
      { id: "lookup", label: "Look up images", state: "running" },
      { id: "services", label: "Services", state: "waiting", total: 2 },
    ],
  };
  status = 200;
  reads = 0;
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
          <StackJobView envId={ENV} stackId={STACK} jobId="j1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("a running deploy", () => {
  it("is titled for the stack and lists the steps in order with the state in words", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Deploying web" })).toBeTruthy();
    const steps = screen.getByRole("list", { name: "Steps of the deploy" });
    const rows = within(steps).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      "Check the stackDone",
      "Look up imagesWorking",
      "ServicesWaiting",
    ]);
    // No result until the job ends.
    expect(screen.queryByText("Deploy complete")).toBeNull();
    expect(screen.queryByText("Deploy failed")).toBeNull();
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

describe("polling", () => {
  it("asks again every second while the job runs and stops once it has ended", async () => {
    // Timers are faked before the page opens, so the interval it starts is the fake one.
    vi.useFakeTimers({
      toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
    });
    mount();
    await waitUnderFakeClock(() => {
      expect(screen.getByRole("heading", { name: "Deploying web" })).toBeTruthy();
    });
    const before = reads;
    job = {
      state: "done",
      items: [
        { id: "check", label: "Check the stack", state: "done" },
        { id: "services", label: "Services", state: "done", current: 2, total: 2 },
      ],
    };
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });
    expect(reads).toBeGreaterThan(before);
    expect(screen.getByText("Deploy complete")).toBeTruthy();
    const after = reads;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(reads).toBe(after);
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
      items: [{ id: "services", label: "Services", state: "done", current: 3, total: 3 }],
    };
    mount();
    expect(await screen.findByText("Deploy complete")).toBeTruthy();
    expect(screen.getByText("web is running 3 services on Prod.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open stack" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1",
    );
  });

  it("says service, not services, for one", async () => {
    job = {
      state: "done",
      items: [{ id: "services", label: "Services", state: "done", current: 1, total: 1 }],
    };
    mount();
    expect(await screen.findByText("web is running 1 service on Prod.")).toBeTruthy();
  });

  it("says Deploy failed with the reason of the step that failed", async () => {
    job = {
      state: "failed",
      items: [
        { id: "check", label: "Check the stack", state: "done" },
        {
          id: "services",
          label: "Services",
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
    job = {
      state: "failed",
      items: [{ id: "lookup", label: "Look up images", state: "failed" }],
    };
    mount();
    expect(await screen.findByText("Bayplan stopped at Look up images.")).toBeTruthy();
  });

  it("reads a partly done job as failed", async () => {
    job = {
      state: "partial",
      items: [
        { id: "services", label: "Services", state: "failed", message: "web failed to start." },
      ],
    };
    mount();
    expect(await screen.findByText("Deploy failed")).toBeTruthy();
  });

  it("says Deploy cancelled", async () => {
    job = {
      state: "cancelled",
      items: [{ id: "services", label: "Services", state: "cancelled" }],
    };
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

  it("says it could not load the deploy, with a way to try again", async () => {
    status = 500;
    mount();
    expect(await screen.findByText("Couldn't load the deploy.")).toBeTruthy();
    status = 200;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading the deploy" }));
    expect(await screen.findByRole("heading", { name: "Deploying web" })).toBeTruthy();
  });
});
