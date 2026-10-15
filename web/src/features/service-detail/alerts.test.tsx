// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { serviceRetry } from "@/api/services";
import { ServiceAlerts } from "@/features/service-detail/alerts";
import "@/i18n";
import { TestRouter } from "@/test-router";
import {
  bareServiceDetail,
  rollout,
  SERVICE_ENV,
  SERVICE_ID,
  type ServiceDetailBody,
  type ServiceRolloutBody,
} from "@/test-service";

// The alerts under the header of a service page, below the rollout region (plan 06-09, RES-02,
// D-12, UI-SPEC "Service detail").

let answer: ServiceRolloutBody;
let reads: number;

beforeEach(() => {
  answer = rollout();
  reads = 0;
  serviceRetry.attempts = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === `/api/env/${SERVICE_ENV}/services/${SERVICE_ID}/rollout`) {
        reads++;
        return Response.json(answer);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  serviceRetry.attempts = 1;
});

async function mount(detail: ServiceDetailBody) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <ServiceAlerts envId={SERVICE_ENV} detail={detail} />
      </TestRouter>
    </QueryClientProvider>,
  );
  // The rollout is read once before the alerts decide.
  await waitFor(() => expect(reads).toBe(1));
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 20));
  });
}

const short = (extra: Partial<ServiceDetailBody> = {}) =>
  bareServiceDetail({ running: 1, desired: 3, ...extra });

describe("the replicas-short alert", () => {
  it("shows the newest task error", async () => {
    answer = rollout({ latestTaskError: "task: non-zero exit (1)" });
    await mount(short());
    expect(await screen.findByText("2 of 3 tasks aren't running")).toBeTruthy();
    expect(screen.getByText("Latest task error: task: non-zero exit (1)")).toBeTruthy();
  });

  it("says Swarm is still starting them when no task has failed", async () => {
    await mount(short());
    expect(await screen.findByText("2 of 3 tasks aren't running")).toBeTruthy();
    expect(screen.getByText("Swarm is still starting them.")).toBeTruthy();
  });

  it("draws the error as text, never as markup", async () => {
    answer = rollout({ latestTaskError: "<b>boom</b>" });
    await mount(short());
    await screen.findByText("2 of 3 tasks aren't running");
    expect(document.querySelector("b")).toBeNull();
    expect(screen.getByText("Latest task error: <b>boom</b>")).toBeTruthy();
  });

  it.each([
    ["every task runs", { running: 3, desired: 3 }],
    ["the service is scaled to 0", { running: 0, desired: 0 }],
    ["the service is a job", { mode: "replicated-job" as const, running: 0, desired: 0 }],
  ])("is not shown when %s", async (_name, extra) => {
    await mount(bareServiceDetail(extra));
    expect(screen.queryByText(/tasks aren't running/)).toBeNull();
  });

  it.each([
    "updating",
    "rollback_started",
    "paused",
    "rollback_paused",
    "rollback_completed",
  ] as const)("is not shown while the rollout region shows %s", async (state) => {
    answer = rollout({ state, latestTaskError: "boom" });
    await mount(short());
    expect(screen.queryByText(/tasks aren't running/)).toBeNull();
  });

  it("is shown again once the last update completed", async () => {
    answer = rollout({ state: "completed" });
    await mount(short());
    expect(await screen.findByText("2 of 3 tasks aren't running")).toBeTruthy();
  });

  it("is not shown while the restart-loop alert is", async () => {
    await mount(short({ restartLoop: LOOP }));
    expect(screen.getByText("api is in a restart loop")).toBeTruthy();
    expect(screen.queryByText(/tasks aren't running/)).toBeNull();
  });
});

const LOOP = {
  state: "flagged" as const,
  count: 4,
  windowMinutes: 10,
  latestError: "task: non-zero exit (1)",
  since: "2026-10-14T09:00:00Z",
};

describe("the restart-loop alert", () => {
  it("names the service, counts the failed tasks in the window and gives the latest error", async () => {
    await mount(bareServiceDetail({ restartLoop: LOOP }));
    expect(await screen.findByText("api is in a restart loop")).toBeTruthy();
    expect(
      screen.getByText(
        "4 tasks failed in the last 10 minutes. Latest error: task: non-zero exit (1)",
      ),
    ).toBeTruthy();
  });

  it("offers View failed tasks, which opens the Tasks tab with only the failed ones", async () => {
    await mount(bareServiceDetail({ restartLoop: LOOP }));
    const link = await screen.findByRole("link", { name: "View failed tasks" });
    const href = link.getAttribute("href") ?? "";
    expect(href).toContain(`/env/${SERVICE_ENV}/services/${SERVICE_ID}/tasks`);
    expect(href).toContain("failed=1");
  });

  it("is a warning with the offline icon, not a destructive alert", async () => {
    await mount(bareServiceDetail({ restartLoop: LOOP }));
    const alert = (await screen.findByText("api is in a restart loop")).closest(
      "[data-slot='alert']",
    );
    expect(alert?.className).toContain("text-status-warning-text");
    expect(alert?.querySelector("svg.text-status-offline-text")).not.toBeNull();
  });

  it("wraps a long error as text, never as markup", async () => {
    await mount(bareServiceDetail({ restartLoop: { ...LOOP, latestError: "<b>boom</b>" } }));
    expect(await screen.findByText(/Latest error: <b>boom<\/b>/)).toBeTruthy();
    expect(document.querySelector("b")).toBeNull();
    const body = screen.getByText(/Latest error/);
    expect(body.className).toContain("break-words");
  });

  it("leaves out the error when none is known", async () => {
    await mount(bareServiceDetail({ restartLoop: { ...LOOP, latestError: "" } }));
    expect(await screen.findByText("4 tasks failed in the last 10 minutes.")).toBeTruthy();
    expect(screen.queryByText(/Latest error/)).toBeNull();
  });

  it("comes before the stack and agent notices, and is absent for a service not in a loop", async () => {
    await mount(bareServiceDetail({ restartLoop: LOOP, stack: "shop", agent: true }));
    const title = await screen.findByText("api is in a restart loop");
    const stack = screen.getByText("Managed by stack shop");
    expect(title.compareDocumentPosition(stack) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    cleanup();
    await mount(bareServiceDetail({ restartLoop: null }));
    expect(screen.queryByText(/restart loop/)).toBeNull();
  });
});

describe("the order of the alerts", () => {
  it("is restart loop, replicas short, stack-owned, agent service", async () => {
    await mount(short({ stack: "shop", agent: true, restartLoop: LOOP }));
    // The restart-loop alert hides replicas-short, so check the two pairs in turn.
    const loop = screen.getByText("api is in a restart loop");
    const stack = screen.getByText("Managed by stack shop");
    const agent = screen.getByText(/This is Bayplan's agent service/);
    expect(loop.compareDocumentPosition(stack) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(stack.compareDocumentPosition(agent) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("puts replicas short before the stack-owned notice", async () => {
    await mount(short({ stack: "shop" }));
    const shortAlert = await screen.findByText("2 of 3 tasks aren't running");
    const stack = screen.getByText("Managed by stack shop");
    expect(
      shortAlert.compareDocumentPosition(stack) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("shows neither the stack notice nor the agent warning for a plain service", async () => {
    await mount(bareServiceDetail({ running: 3, desired: 3 }));
    expect(screen.queryByText(/Managed by stack/)).toBeNull();
    expect(screen.queryByText(/agent service/)).toBeNull();
  });
});
