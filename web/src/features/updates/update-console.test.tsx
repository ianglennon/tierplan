// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
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
  parent?: string;
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

let state: "running" | "done" | "failed" | "cancelled" | "partial";
let kind: "service" | "container" | "bulk";
let workload: string;
let items: Item[];
let status: number;
let role: "viewer" | "operator";
let lines: string[];
let from: string;
// How the server says the update's run ended, when it says: absent for a server that does not.
let runState: string | undefined;
let runReason: string | undefined;
let client: QueryClient;

function jobBody() {
  return {
    id: "j1",
    runId: "r1",
    kind,
    workload,
    fromDigest: from,
    toDigest: B,
    startedBy: "ada",
    state,
    createdAt: "2026-11-04T12:00:00Z",
    finishedAt: state === "running" ? "" : "2026-11-04T12:00:09Z",
    items: items.map(item),
    output: lines.map((text, i) => ({ seq: i + 1, at: "2026-11-04T12:00:01Z", text })),
    ...(runState ? { runState } : {}),
    ...(runReason ? { runReason } : {}),
  };
}

beforeEach(() => {
  state = "running";
  kind = "service";
  workload = "web";
  items = [check, pull, update(), wait(), { id: "health", label: "x", state: "waiting" }];
  status = 200;
  role = "viewer";
  lines = ["Pulled nginx on 2 nodes"];
  from = A;
  runState = undefined;
  runReason = undefined;
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
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
      pull,
      {
        ...update("failed"),
        code: "service_rejected",
        message: "The swarm refused the new version. Nothing was changed.",
      },
      wait("skipped"),
    ];
    mount();
    const alert = await screen.findByText("The update failed", {
      selector: "[data-slot=alert-title]",
    });
    expect(alert.closest("[role=alert]")).toBeTruthy();
    expect(
      screen.getAllByText(/The swarm refused the new version\. Nothing was changed\./).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByText("Updated", { selector: "[data-slot=alert-title]" })).toBeNull();
    expect(screen.queryByText(/rolled back/)).toBeNull();
  });
});

// The service steps of an update whose new tasks failed, as the server writes them when it also put the
// previous version back (12-22): the failing step, then the two steps it added.
const failingWait = (code: string): Item => ({
  id: "wait",
  label: "Wait for the new tasks",
  state: "failed",
  code,
  message: "The service did not finish updating in time.",
});
const restore = (back: Item["state"] = "done", waited: Item["state"] = "done"): Item[] => [
  { id: "rollback", label: "Roll back to the previous version", state: back },
  { id: "rollback_wait", label: "Wait for the previous version to run", state: waited },
];

describe("UpdateConsole: a cancel after the swarm took the update", () => {
  it("reads as cancelled, not failed, though the job has finished steps", async () => {
    state = "partial";
    items = [
      check,
      pull,
      update("done"),
      wait("cancelled", {
        code: "wait_cancelled",
        message:
          "Cancelled while waiting for the new version to run. The update had been applied and is left as it is; it was not confirmed running.",
      }),
    ];
    mount();
    expect(
      await screen.findByText("The update was cancelled", { selector: "[data-slot=alert-title]" }),
    ).toBeTruthy();
    expect(
      screen.queryByText("The update failed", { selector: "[data-slot=alert-title]" }),
    ).toBeNull();
  });
});

describe("UpdateConsole: a failed update that was rolled back (IMG-07, D-12)", () => {
  beforeEach(() => {
    state = "failed";
  });

  it("adds the two restore steps and says the update failed and was rolled back, with the old digest", async () => {
    items = [check, pull, update("done"), failingWait("timeout"), ...restore()];
    mount();
    const title = await screen.findByText("The update failed and was rolled back", {
      selector: "[data-slot=alert-title]",
    });
    const alert = title.closest("[data-slot=update-result]");
    expect(alert?.getAttribute("role")).toBe("status");
    expect(alert?.textContent).toContain(
      `web didn't finish updating within 10 minutes. TierPlan put sha256:${"a".repeat(12)} back.`,
    );
    expect(alert?.textContent).toContain(
      "The failed version won't be offered again until a newer one appears.",
    );
    const steps = screen.getByRole("list", { name: "Steps of the deploy" }).textContent ?? "";
    expect(steps).toContain("Roll back to the previous version");
    expect(steps).toContain("Wait for the previous version to run");
    // The heading names the outcome, and nothing says the update worked.
    expect(screen.getByRole("heading", { name: /Rolled back/ })).toBeTruthy();
    expect(screen.queryByText("Updated", { selector: "[data-slot=alert-title]" })).toBeNull();
    expect(screen.getByRole("link", { name: "Open web" })).toBeTruthy();
  });

  it.each([
    ["didnt_start", [], "web didn't start."],
    ["unhealthy", [], "web didn't become healthy within 10 minutes."],
    ["converge_timeout", [], "web didn't finish updating within 10 minutes."],
    ["network_full", [], "web didn't start."],
    ["engine_error", [], "web couldn't be updated."],
  ])(
    "words the failed step %s as a reason that ends a sentence about the service",
    async (code, _x, sentence) => {
      items = [check, pull, update("done"), failingWait(code), ...restore()];
      mount();
      await screen.findByText("The update failed and was rolled back");
      expect(screen.getByText(new RegExp(sentence.replaceAll(".", "\\.")))).toBeTruthy();
    },
  );

  it("takes the window of an unhealthy wait from the health step, in seconds", async () => {
    items = [
      check,
      pull,
      update("done"),
      { id: "wait", label: "x", state: "done" },
      {
        id: "health",
        label: "x",
        state: "failed",
        code: "unhealthy",
        unit: "seconds",
        total: 120,
        message: "The new version did not become healthy.",
      },
      ...restore(),
    ];
    mount();
    expect(await screen.findByText(/web didn't become healthy within 2 minutes\./)).toBeTruthy();
  });

  it("says TierPlan restarted when the job only restores (a recovery after a restart)", async () => {
    state = "done";
    items = restore();
    mount();
    await screen.findByText("The update failed and was rolled back");
    expect(
      screen.getByText(/TierPlan restarted during this update\. TierPlan put sha256:a{12} back\./),
    ).toBeTruthy();
    expect(screen.queryByText("Updated", { selector: "[data-slot=alert-title]" })).toBeNull();
  });

  it("does not say rolled back when the recovery found the service as the update had never changed it", async () => {
    state = "partial";
    items = [
      {
        id: "rollback",
        label: "Roll back to the previous version",
        state: "done",
        code: "nothing_to_put_back",
        message: "The update had not changed the service, so nothing was put back.",
      },
      { id: "rollback_wait", label: "Wait for the previous version to run", state: "skipped" },
    ];
    mount();
    await screen.findByText("The update failed", { selector: "[data-slot=alert-title]" });
    expect(screen.queryByText(/rolled back/)).toBeNull();
    expect(screen.queryByText(/put sha256/)).toBeNull();
  });

  it("shows the destructive alert when the rollback itself failed, with why", async () => {
    items = [
      check,
      pull,
      update("done"),
      failingWait("didnt_start"),
      {
        id: "rollback",
        label: "x",
        state: "failed",
        code: "restore_refused",
        message: "The swarm would not take the previous version back.",
      },
      { id: "rollback_wait", label: "x", state: "skipped", code: "not_run" },
    ];
    mount();
    const title = await screen.findByText("The rollback failed", {
      selector: "[data-slot=alert-title]",
    });
    const alert = title.closest("[data-slot=update-result]");
    expect(alert?.getAttribute("role")).toBe("alert");
    expect(alert?.textContent).toContain(
      `web may not be running the version you expect. TierPlan couldn't restore sha256:${"a".repeat(12)}: the swarm wouldn't take it back.`,
    );
    expect(screen.queryByText("The update failed and was rolled back")).toBeNull();
  });

  it("does not call it a rollback when somebody else changed the service meanwhile", async () => {
    items = [
      check,
      pull,
      update("done"),
      failingWait("timeout"),
      {
        id: "rollback",
        label: "x",
        state: "failed",
        code: "restore_service_changed",
        message:
          "The service was changed by someone else, so the previous version was not put back.",
      },
      { id: "rollback_wait", label: "x", state: "skipped", code: "not_run" },
    ];
    mount();
    const title = await screen.findByText("The update failed", {
      selector: "[data-slot=alert-title]",
    });
    expect(title.closest("[role=alert]")).toBeTruthy();
    expect(
      screen.getAllByText(/changed by someone else, so the previous version was not put back/)
        .length,
    ).toBeGreaterThan(0);
    expect(screen.queryByText(/was rolled back/)).toBeNull();
  });

  it("announces the outcome once, and not for a job that had already ended", async () => {
    state = "running";
    items = [check, pull, update("done"), wait("running"), ...restore("waiting", "waiting")];
    mount();
    await screen.findByRole("heading", { name: "Update of web" });
    await act(async () => stream()?.sendFrame("snapshot", jobBody(), "1"));
    await screen.findByText("Live");
    const spoken = () => document.querySelector("[data-slot=update-announcer]")?.textContent;
    expect(spoken()).toBe("");
    state = "failed";
    items = [check, pull, update("done"), failingWait("timeout"), ...restore()];
    await act(async () => stream()?.sendFrame("done", jobBody(), "9"));
    await screen.findByText("The update failed and was rolled back");
    expect(spoken()).toBe("Update of web failed and was rolled back.");
  });

  it("announces a success as completed", async () => {
    state = "running";
    mount();
    await screen.findByRole("heading", { name: "Update of web" });
    await act(async () => stream()?.sendFrame("snapshot", jobBody(), "1"));
    await screen.findByText("Live");
    state = "done";
    items = [check, pull, update("done"), wait("done")];
    await act(async () => stream()?.sendFrame("done", jobBody(), "9"));
    await screen.findByText("Updated", { selector: "[data-slot=alert-title]" });
    expect(document.querySelector("[data-slot=update-announcer]")?.textContent).toBe(
      "Update of web completed.",
    );
  });
});

describe("UpdateConsole: a pull that failed changed nothing (user decision, D-14)", () => {
  beforeEach(() => {
    state = "failed";
    items = [
      check,
      {
        ...pull,
        state: "failed",
        code: "registry_unreachable",
        message: "The image could not be pulled, so nothing was changed.",
      },
      { ...update("skipped"), code: "not_run" },
      wait("skipped"),
    ];
  });

  it("says the image could not be pulled and nothing was changed, and never that it was rolled back", async () => {
    mount();
    const title = await screen.findByText("The image couldn't be pulled", {
      selector: "[data-slot=alert-title]",
    });
    const alert = title.closest("[data-slot=update-result]");
    expect(alert?.getAttribute("role")).toBe("status");
    expect(alert?.textContent).toContain(
      `Nothing was changed: web still runs sha256:${"a".repeat(12)}.`,
    );
    expect(document.body.textContent).not.toMatch(/rolled back/i);
    expect(screen.queryByText("Roll back to the previous version")).toBeNull();
    expect(screen.getByRole("heading", { name: /Not updated/ })).toBeTruthy();
  });

  it("offers Update anyway to an Operator", async () => {
    role = "operator";
    mount();
    const button = await screen.findByRole("button", { name: "Update anyway…" });
    expect(button.getAttribute("aria-disabled")).toBeNull();
  });

  it("shows Update anyway unavailable to a Viewer", async () => {
    mount();
    const button = (await screen.findByRole("button", {
      name: "Update anyway…",
    })) as HTMLButtonElement;
    expect(button.disabled || button.getAttribute("aria-disabled") === "true").toBe(true);
  });

  it("announces that the image could not be pulled", async () => {
    state = "running";
    mount();
    await screen.findByRole("heading", { name: "Update of web" });
    await act(async () => stream()?.sendFrame("snapshot", jobBody(), "1"));
    await screen.findByText("Live");
    state = "failed";
    items = [{ ...pull, state: "failed", code: "registry_unreachable" }];
    await act(async () => stream()?.sendFrame("done", jobBody(), "9"));
    await screen.findByText("The image couldn't be pulled");
    expect(document.querySelector("[data-slot=update-announcer]")?.textContent).toBe(
      "Update of web failed. The image could not be pulled and nothing was changed.",
    );
  });
});

describe("UpdateConsole: a container that failed after the old one was touched", () => {
  const step = (id: string, state: Item["state"], over: Partial<Item> = {}): Item => ({
    id,
    label: "x",
    state,
    ...over,
  });

  beforeEach(() => {
    kind = "container";
    workload = "cache";
    state = "failed";
    items = [
      step("check", "done"),
      step("pull", "done"),
      step("create", "done"),
      step("swap", "done"),
      step("start", "failed", { code: "didnt_start", message: "The new version did not start." }),
      step("remove_old", "skipped", { code: "not_run" }),
    ];
  });

  it("reads the old container's return as a rollback", async () => {
    lines = ["Create cache", "The new container did not start"];
    mount();
    await screen.findByText("The update failed and was rolled back");
    expect(screen.getByText(/cache didn't start\. TierPlan put sha256:a{12} back\./)).toBeTruthy();
  });

  it("reads a restore that did not work as a failed rollback", async () => {
    lines = [
      "The old container could not be put back. It is cache_old if it was renamed; check cache on the node.",
    ];
    mount();
    await screen.findByText("The rollback failed", { selector: "[data-slot=alert-title]" });
    expect(screen.getByText(/the old container couldn't be put back/)).toBeTruthy();
  });

  it("does not claim a rollback for a failure before the old container was touched", async () => {
    items = [
      step("check", "done"),
      step("pull", "done"),
      step("create", "failed", {
        code: "create_refused",
        message: "The new container could not be created, so nothing was changed.",
      }),
      step("swap", "skipped", { code: "not_run" }),
    ];
    mount();
    await screen.findByText("The update failed", { selector: "[data-slot=alert-title]" });
    expect(screen.queryByText(/rolled back/)).toBeNull();
  });
});

// A recovery after a restart runs one restore step, and that step reads done whether it put the old
// container back or found nothing to put back. The run's record says which (runState).
describe("UpdateConsole: a recovery after a restart reads the run, not the steps", () => {
  const restoreStep = (over: Partial<Item> = {}): Item => ({
    id: "rollback",
    label: "Put the old container back",
    state: "done",
    ...over,
  });

  beforeEach(() => {
    kind = "container";
    workload = "cache";
    state = "done";
    runReason = "server_restarted";
    lines = [
      "TierPlan restarted during this update. Putting cache back to sha256:aaaaaaaaaaaa",
      "cache runs the version it ran before the update",
    ];
  });

  it("does not say rolled back or updated for a container the update never touched", async () => {
    runState = "failed";
    items = [restoreStep()];
    mount();
    await screen.findByText("The update failed", { selector: "[data-slot=alert-title]" });
    expect(screen.getByText(/TierPlan restarted during this update\. Open cache/)).toBeTruthy();
    expect(screen.queryByText(/rolled back/i)).toBeNull();
    expect(screen.queryByText(/put sha256/)).toBeNull();
    expect(screen.queryByText("Updated", { selector: "[data-slot=alert-title]" })).toBeNull();
    // The step list does not call the restore a rollback, nor tick the update's end.
    expect(screen.queryByText("Roll back to the previous version")).toBeNull();
    expect(screen.getByText("Check what the update changed")).toBeTruthy();
    const steps = within(screen.getByRole("list", { name: "Steps of the deploy" })).getAllByRole(
      "listitem",
    );
    expect(steps.at(-1)?.textContent).toBe("DoneSkipped");
  });

  it("says rolled back when the old container really was put back", async () => {
    runState = "rolled_back";
    items = [restoreStep()];
    mount();
    await screen.findByText("The update failed and was rolled back");
    expect(screen.getByText(/TierPlan restarted during this update\. TierPlan put/)).toBeTruthy();
  });

  it("reads a service the update never wrote the same way", async () => {
    kind = "service";
    workload = "web";
    state = "partial";
    runState = "failed";
    items = [
      {
        id: "rollback",
        label: "Roll back to the previous version",
        state: "done",
        code: "nothing_to_put_back",
        message: "The update had not changed the service, so nothing was put back.",
      },
      { id: "rollback_wait", label: "Wait for the previous version to run", state: "skipped" },
    ];
    mount();
    await screen.findByText("The update failed", { selector: "[data-slot=alert-title]" });
    expect(screen.getByText(/TierPlan restarted during this update\. Open web/)).toBeTruthy();
    expect(screen.queryByText(/rolled back/i)).toBeNull();
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

// A bulk update (plan 12-31, D-11): one job for the environment with a top-level item for each workload,
// the steps of its update under it, and a summary the job ends with.
describe("UpdateConsole: a bulk update", () => {
  const top = (
    id: string,
    label: string,
    state: Item["state"],
    over: Partial<Item> = {},
  ): Item => ({
    id,
    label,
    state,
    ...over,
  });
  const summary = (result: Record<string, string>): Item =>
    top("summary", "Summary", "done", { result });

  function bulk(list: Item[], end: typeof state = "done") {
    kind = "bulk";
    workload = "3 workloads";
    from = "";
    state = end;
    items = list;
    lines = ["Updated 1 of 3. 1 rolled back. 1 skipped."];
  }

  it("ends with Partly done when one was rolled back and one skipped", async () => {
    bulk(
      [
        top("i0", "Update web", "done"),
        top("i0/pull", "Pull the image", "done", { parent: "i0" }),
        top("i1", "Update api", "failed", { code: "unhealthy" }),
        top("i1/rollback", "Roll back", "done", { parent: "i1" }),
        top("i2", "Update cache", "skipped", { code: "moved" }),
        summary({ done: "1", total: "3", rolled_back: "1", skipped: "1" }),
      ],
      "failed",
    );
    mount();
    expect(await screen.findByRole("heading", { name: /Update of 3 items/ })).toBeTruthy();
    const title = await screen.findByText("Partly done", { selector: "[data-slot=alert-title]" });
    const alert = title.closest("[role=status]") as HTMLElement;
    expect(alert).toBeTruthy();
    expect(alert.textContent).toContain("Updated 1 of 3. 1 was rolled back and 1 was skipped.");
    // Every item says what happened to it, in the console's words.
    const rows = screen.getByRole("list", { name: "Items of the update" });
    const text = rows.textContent ?? "";
    expect(text).toContain("web");
    expect(text).toContain("Updated");
    expect(text).toContain("Rolled back");
    expect(text).toContain("api didn't become healthy");
    expect(text).toContain("Skipped");
    expect(text).toContain("A newer version appeared since you confirmed, so cache was skipped.");
    // The steps of an item are not listed as items of their own.
    expect(within(rows).getAllByRole("listitem")).toHaveLength(3);
    // There is no one workload to update again, and no Update anyway.
    expect(screen.queryByRole("button", { name: /Update anyway/ })).toBeNull();
  });

  it("ends with the success alert when every item was updated", async () => {
    bulk(
      [
        top("i0", "Update web", "done"),
        top("i1", "Update api", "done"),
        summary({ done: "2", total: "2", rolled_back: "0", skipped: "0" }),
      ],
      "done",
    );
    mount();
    const title = await screen.findByText("Updated", { selector: "[data-slot=alert-title]" });
    expect(title.closest("[role=status]")?.textContent).toContain("All 2 items");
    expect(screen.queryByText("Partly done")).toBeNull();
  });

  it("shows the items that have not run as waiting while the job runs, and no result", async () => {
    bulk(
      [
        top("i0", "Update web", "running"),
        top("i1", "Update api", "waiting"),
        top("summary", "Summary", "waiting"),
      ],
      "running",
    );
    mount();
    await screen.findByRole("heading", { name: /Update of 2 items/ });
    const rows = screen.getByRole("list", { name: "Items of the update" });
    expect(within(rows).getByText("Updating…")).toBeTruthy();
    expect(within(rows).getByText("Waiting")).toBeTruthy();
    expect(screen.queryByText("Partly done")).toBeNull();
  });

  it("counts a failure that was not rolled back and names the one that was cancelled", async () => {
    bulk(
      [
        top("i0", "Update web", "failed", { code: "engine_error" }),
        top("i1", "Update api", "cancelled", { code: "cancelled" }),
        summary({ done: "0", total: "2", rolled_back: "0", skipped: "1" }),
      ],
      "cancelled",
    );
    mount();
    const title = await screen.findByText("Cancelled", { selector: "[data-slot=alert-title]" });
    expect(title.closest("[role=status]")?.textContent).toContain("Updated 0 of 2.");
  });
});
