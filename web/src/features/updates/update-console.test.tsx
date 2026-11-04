// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { HealthWait, UpdateConsole } from "@/features/updates/update-console";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The update console (plan 12-17, IMG-06, D-05): an image update's job as the server holds it, read
// once over HTTP and followed over its stream, in the deploy console's parts.

const ENV = "e1";
const A = `sha256:${"a".repeat(64)}`;
const B = `sha256:${"b".repeat(64)}`;

interface Item {
  id: string;
  label: string;
  state: "waiting" | "running" | "done" | "failed" | "skipped" | "cancelled";
  code?: string;
  message?: string;
  current?: number;
  total?: number;
  unit?: string;
  result?: Record<string, string>;
}

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

const check: Item = { id: "check", label: "Check the new version", state: "done" };
const pull: Item = {
  id: "pull",
  label: "Pull the image",
  state: "done",
  result: { nodes: "2" },
};
const update = (state: Item["state"] = "running"): Item => ({
  id: "update",
  label: "Update web",
  state,
});
const wait = (state: Item["state"] = "waiting", over: Partial<Item> = {}): Item => ({
  id: "wait",
  label: "Wait for the new tasks",
  state,
  ...over,
});

let state: "running" | "done" | "failed" | "cancelled";
let kind: "service" | "container";
let workload: string;
let items: Item[];
let status: number;
let client: QueryClient;

function jobBody() {
  return {
    id: "j1",
    runId: "r1",
    kind,
    workload,
    fromDigest: A,
    toDigest: B,
    startedBy: "ada",
    state,
    createdAt: "2026-11-04T12:00:00Z",
    finishedAt: state === "running" ? "" : "2026-11-04T12:00:09Z",
    items: items.map(item),
    output: [{ seq: 1, at: "2026-11-04T12:00:01Z", text: "Pulled nginx on 2 nodes" }],
  };
}

beforeEach(() => {
  state = "running";
  kind = "service";
  workload = "web";
  items = [check, pull, update(), wait(), { id: "health", label: "x", state: "waiting" }];
  status = 200;
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
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
      if (path === `/api/env/${ENV}/update-jobs/j1`) {
        return status === 200
          ? Response.json(jobBody())
          : Response.json(
              { type: "update_job_not_found", title: "Not found", status: 404 },
              { status: 404 },
            );
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
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <UpdateConsole envId={ENV} jobId="j1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const stream = () => FakeEventSource.instances[0];

describe("UpdateConsole: a running update", () => {
  it("re-attaches to a running job: the title, the breadcrumb and the steps as they stand", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Update of web" })).toBeTruthy();
    expect(screen.getByText("Prod")).toBeTruthy();
    expect(screen.getByText("Update of web", { selector: "li" })).toBeTruthy();
    expect(screen.getByText("Started by ada", { exact: false })).toBeTruthy();
    const steps = screen.getByRole("list", { name: "Steps of the deploy" });
    const text = steps.textContent ?? "";
    for (const label of [
      "Check the new version",
      "Pull the image on 2 nodes",
      "Update the service",
      "Wait for the new tasks",
      "Wait for a healthy check",
      "Done",
    ]) {
      expect(text).toContain(label);
    }
    // No result yet, and the stream opens at the update's own address.
    expect(screen.queryByText("Updated", { selector: "[data-slot=alert-title]" })).toBeNull();
    expect(stream()?.url).toContain(`/api/env/${ENV}/update-jobs/j1/stream`);
  });

  it("shows the live chip once the stream is live, and no result", async () => {
    mount();
    await screen.findByRole("heading", { name: "Update of web" });
    expect(screen.queryByText("Live")).toBeNull();
    await act(async () => stream()?.sendFrame("snapshot", jobBody(), "1"));
    expect(await screen.findByText("Live")).toBeTruthy();
    expect(screen.queryByText("The update failed")).toBeNull();
  });

  it("follows the steps and ends in the success alert, with no live chip left", async () => {
    mount();
    await screen.findByRole("heading", { name: "Update of web" });
    await act(async () => stream()?.sendFrame("snapshot", jobBody(), "1"));
    await screen.findByText("Live");
    items = [
      check,
      pull,
      update("done"),
      wait("done"),
      { id: "health", label: "x", state: "done" },
    ];
    state = "done";
    await act(async () => stream()?.sendFrame("done", jobBody(), "9"));
    const title = await screen.findByText("Updated", { selector: "[data-slot=alert-title]" });
    expect(title.closest("[role=status]")).toBeTruthy();
    expect(screen.getByText(`web now runs sha256:${"b".repeat(12)}.`)).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
    expect(screen.getByRole("link", { name: "Open web" })).toBeTruthy();
  });
});

describe("UpdateConsole: a finished update", () => {
  it("shows a job that already ended without the live chip, the success alert and the link", async () => {
    state = "done";
    items = [check, pull, update("done"), wait("done")];
    mount();
    expect(
      await screen.findByText("Updated", { selector: "[data-slot=alert-title]" }),
    ).toBeTruthy();
    expect(screen.getByText(`web now runs sha256:${"b".repeat(12)}.`)).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
    expect(screen.getByRole("link", { name: "Open web" }).getAttribute("href")).toContain(
      "/env/e1/services",
    );
    // The health step is listed only when the server added it.
    expect(screen.queryByText("Wait for a healthy check")).toBeNull();
    expect(screen.getByText(/Took/)).toBeTruthy();
  });

  it("says the update failed and why, from the step that stopped", async () => {
    state = "failed";
    items = [
      check,
      {
        ...pull,
        state: "failed",
        code: "pull_failed",
        message: "The image could not be pulled, so nothing was changed.",
      },
      { ...update("skipped"), code: "not_run" },
      wait("skipped"),
    ];
    mount();
    const alert = await screen.findByText("The update failed", {
      selector: "[data-slot=alert-title]",
    });
    expect(alert.closest("[role=alert]")).toBeTruthy();
    expect(
      screen.getAllByText(/The image could not be pulled, so nothing was changed\./).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByText("Updated", { selector: "[data-slot=alert-title]" })).toBeNull();
  });
});

describe("UpdateConsole: a job that is gone", () => {
  it("says finished updates stay only a short while, and offers the services", async () => {
    status = 404;
    mount();
    expect(await screen.findByText("This update is no longer listed")).toBeTruthy();
    await waitFor(() => expect(screen.getByRole("link", { name: "Open services" })).toBeTruthy());
  });
});

describe("UpdateConsole: a container update", () => {
  const step = (id: string, label: string, state: Item["state"]): Item => ({ id, label, state });

  beforeEach(() => {
    kind = "container";
    workload = "cache";
  });

  it("lists the steps of a recreate, in the console's words, with the old container's removal last", async () => {
    items = [
      step("check", "x", "done"),
      step("pull", "x", "done"),
      step("create", "x", "done"),
      step("swap", "x", "done"),
      step("start", "x", "done"),
      step("health", "x", "running"),
      step("remove_old", "x", "waiting"),
    ];
    mount();
    expect(await screen.findByRole("heading", { name: "Update of cache" })).toBeTruthy();
    const steps = screen.getByRole("list", { name: "Steps of the deploy" });
    const labels = Array.from(steps.querySelectorAll("li")).map((li) => li.textContent ?? "");
    const order = [
      "Check the new version",
      "Pull the image",
      "Create the new container",
      "Stop and rename the old container",
      "Start the new container",
      "Wait for a healthy check",
      "Remove the old container",
      "Done",
    ];
    let from = 0;
    for (const label of order) {
      const at = labels.findIndex((text, i) => i >= from && text.includes(label));
      expect(at, label).toBeGreaterThanOrEqual(from);
      from = at + 1;
    }
  });

  it("says how long the new container may still take to be healthy, with a progress bar", async () => {
    items = [
      step("check", "x", "done"),
      step("pull", "x", "done"),
      step("create", "x", "done"),
      step("swap", "x", "done"),
      step("start", "x", "done"),
      step("health", "x", "running"),
      step("remove_old", "x", "waiting"),
    ];
    mount();
    expect(
      await screen.findByText("Waiting for cache to be healthy. Up to 5 minutes left."),
    ).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Time left to become healthy" })).toBeTruthy();
  });

  it("shows no health wait for a container without a health check, nor once the update ended", async () => {
    state = "done";
    items = [
      step("check", "x", "done"),
      step("pull", "x", "done"),
      step("create", "x", "done"),
      step("swap", "x", "done"),
      step("start", "x", "done"),
      step("remove_old", "x", "done"),
    ];
    mount();
    expect(
      await screen.findByText("Updated", { selector: "[data-slot=alert-title]" }),
    ).toBeTruthy();
    expect(screen.queryByText(/to be healthy/)).toBeNull();
    expect(screen.queryByText("Wait for a healthy check")).toBeNull();
    // The link goes to the containers, not the services.
    expect(screen.getByRole("link", { name: "Open cache" }).getAttribute("href")).toContain(
      "/env/e1/containers",
    );
  });
});

describe("HealthWait", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const bar = () => screen.getByRole("progressbar");
  const polite = () =>
    Array.from(document.querySelectorAll("[aria-live=polite] p")).map((p) => p.textContent);
  const tick = (ms: number) =>
    act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
    });

  it("counts down in the line, but the bar and the polite region change once per quarter", async () => {
    render(<HealthWait name="web" windowSeconds={80} />);
    expect(screen.getByText("Waiting for web to be healthy. Up to 1 minute left.")).toBeTruthy();
    expect(bar().getAttribute("aria-valuetext")).toBe(
      "Waiting for web to be healthy, 1 minute left",
    );

    // Part-way through the first quarter the line has moved on, and nothing was spoken.
    await tick(10_000);
    expect(screen.getByText("Waiting for web to be healthy. Up to 1 minute left.")).toBeTruthy();
    await tick(10_000);
    expect(polite()).toEqual(["Waiting for web to be healthy, 1 minute left"]);
    await tick(10_000);
    expect(screen.getByText("Waiting for web to be healthy. Up to 50 seconds left.")).toBeTruthy();
    // The bar still reads what it read when the quarter began.
    expect(bar().getAttribute("aria-valuetext")).toBe(
      "Waiting for web to be healthy, 1 minute left",
    );
    expect(polite()).toHaveLength(1);

    await tick(10_000);
    expect(bar().getAttribute("aria-valuetext")).toBe(
      "Waiting for web to be healthy, 40 seconds left",
    );
    expect(polite()).toEqual([
      "Waiting for web to be healthy, 1 minute left",
      "Waiting for web to be healthy, 40 seconds left",
    ]);
  });

  it("stops at zero and never speaks more than four times", async () => {
    render(<HealthWait name="web" windowSeconds={80} />);
    for (let i = 0; i < 6; i++) await tick(20_000);
    expect(screen.getByText("Waiting for web to be healthy. Up to 0 seconds left.")).toBeTruthy();
    expect(polite()).toHaveLength(4);
    expect(bar().getAttribute("aria-valuenow")).toBe("100");
  });
});
