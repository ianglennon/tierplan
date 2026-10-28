// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { StackDetail } from "@/api/stacks";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DeployFailedAlert } from "@/features/stacks/deploy-failed-alert";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The warning on a stack's page after its last deploy stopped (plan 07-19, STK-07, D-14).

const ENV = "e1";

let role: "viewer" | "operator";
let rollbackInfo: { available: boolean; deployedAt: string; deployedBy: string; revision?: number };
let rollbacks: number;
let client: QueryClient;

const stackOf = (over: Partial<StackDetail> = {}): StackDetail => ({
  id: "s1",
  name: "web",
  target: "swarm",
  source: "inline",
  managed: true,
  state: "deploy_failed",
  lastDeployedAt: "2026-10-19T11:00:00Z",
  lastDeployedBy: "bob",
  hasDraft: false,
  envId: ENV,
  sourceRef: "",
  createdBy: "ada",
  createdAt: "2026-10-19T10:00:00Z",
  stopped: false,
  draftSavedAt: "",
  draftSavedBy: "",
  rollback: rollbackInfo,
  lastOperation: {
    kind: "deploy",
    state: "failed",
    code: "service_rejected",
    failedService: "web_api",
    jobId: "j1",
    startedBy: "ada",
    startedAt: "2026-10-19T12:00:00Z",
    endedAt: "2026-10-19T12:00:09Z",
  },
  ...over,
});

const withOperation = (over: Partial<NonNullable<StackDetail["lastOperation"]>>): StackDetail => {
  const base = stackOf().lastOperation as NonNullable<StackDetail["lastOperation"]>;
  return stackOf({ lastOperation: { ...base, ...over } });
};

beforeEach(() => {
  role = "operator";
  rollbackInfo = { available: true, deployedAt: "2026-01-02T10:00:00Z", deployedBy: "bob" };
  rollbacks = 0;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(stackOf());
      if (path === `/api/env/${ENV}/stacks/s1/rollback` && req.method === "POST") {
        rollbacks += 1;
        return Response.json({ jobId: "j2", revision: 4 }, { status: 202 });
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

function mount(stack: StackDetail) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <DeployFailedAlert stack={stack} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("DeployFailedAlert", () => {
  it("names the revision the one-click roll back restores, when it was deployed and by whom", async () => {
    rollbackInfo = {
      available: true,
      deployedAt: "2026-01-02T10:00:00Z",
      deployedBy: "bob",
      revision: 4,
    };
    mount(stackOf({ rollback: rollbackInfo }));
    const alert = await screen.findByRole("alert");
    expect(await within(alert).findByText(/^Revision #4, deployed .+ by bob$/)).toBeTruthy();
    expect(within(alert).queryByText(/^Last deployed/)).toBeNull();
  });

  it("says The last deploy failed and where Bayplan stopped and why", async () => {
    mount(stackOf());
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("The last deploy failed")).toBeTruthy();
    expect(
      within(alert).getByText("Bayplan stopped at web_api: Docker rejected the service."),
    ).toBeTruthy();
  });

  it("is a warning, not an error", async () => {
    mount(stackOf());
    const alert = await screen.findByRole("alert");
    expect(alert.className).toContain("border-status-warning");
    expect(alert.className).not.toContain("text-destructive");
  });

  it("offers Open console and Roll back to previous version, both outline", async () => {
    mount(stackOf());
    const alert = await screen.findByRole("alert");
    const console_ = within(alert).getByRole("link", { name: "Open console" });
    expect(console_.getAttribute("href")).toBe("/env/e1/stacks/s1/deploys/j1");
    expect(console_.getAttribute("data-variant")).toBe("outline");
    const rollback = await within(alert).findByRole("button", {
      name: "Roll back to previous version",
    });
    expect(rollback.getAttribute("data-variant")).toBe("outline");
    expect(rollback.getAttribute("aria-disabled")).toBeNull();
  });

  it("rolls back on one click, from the page, and goes to the console of the new job", async () => {
    mount(stackOf());
    fireEvent.click(await screen.findByRole("button", { name: "Roll back to previous version" }));
    await waitFor(() => expect(rollbacks).toBe(1));
  });

  it("turns the roll back off, with the reason, when there is no earlier version", async () => {
    rollbackInfo = { available: false, deployedAt: "", deployedBy: "" };
    mount(stackOf({ rollback: rollbackInfo }));
    // The button is drawn again when its reason arrives, so it is looked for again each time.
    const rollbackButton = () =>
      screen.getByRole("button", { name: "Roll back to previous version" });
    await screen.findByRole("button", { name: "Roll back to previous version" });
    await waitFor(() => expect(rollbackButton().getAttribute("aria-disabled")).toBe("true"));
    const id = rollbackButton().getAttribute("aria-describedby");
    expect(document.getElementById(id as string)?.textContent).toBe(
      "There's no earlier deployed version to roll back to.",
    );
    fireEvent.click(rollbackButton());
    expect(rollbacks).toBe(0);
  });

  it("turns the roll back off with Needs Operator for a Viewer", async () => {
    role = "viewer";
    mount(stackOf());
    const rollbackButton = () =>
      screen.getByRole("button", { name: "Roll back to previous version" });
    await screen.findByRole("button", { name: "Roll back to previous version" });
    await waitFor(() => expect(rollbackButton().getAttribute("aria-disabled")).toBe("true"));
    const id = rollbackButton().getAttribute("aria-describedby");
    expect(document.getElementById(id as string)?.textContent).toBe("Needs Operator");
  });

  it("says a deploy that was interrupted was, and that the swarm may be partly updated", async () => {
    mount(withOperation({ state: "interrupted", code: "interrupted", failedService: "" }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("The last deploy failed")).toBeTruthy();
    expect(
      within(alert).getByText(
        "The deploy was interrupted when Bayplan stopped. The swarm may be partly updated.",
      ),
    ).toBeTruthy();
    expect(
      within(alert).getByRole("button", { name: "Roll back to previous version" }),
    ).toBeTruthy();
  });

  it("says a deploy that was cancelled was", async () => {
    mount(withOperation({ state: "cancelled", code: "cancelled", failedService: "web_api" }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("The last deploy was cancelled")).toBeTruthy();
    expect(
      within(alert).getByText("Bayplan stopped at web_api. Nothing was rolled back."),
    ).toBeTruthy();
  });

  it("words a service that did not settle", async () => {
    mount(withOperation({ code: "converge_timeout" }));
    expect(
      await screen.findByText(
        "Bayplan stopped at web_api: It didn't settle in time. Docker may still finish it.",
      ),
    ).toBeTruthy();
  });

  it("words a service the swarm won't start again", async () => {
    mount(withOperation({ code: "tasks_failed" }));
    expect(
      await screen.findByText(
        "Bayplan stopped at web_api: Its tasks stopped and the swarm won't start them again.",
      ),
    ).toBeTruthy();
  });

  it("words a full network", async () => {
    mount(withOperation({ code: "network_full" }));
    expect(
      await screen.findByText(
        "Bayplan stopped at web_api: A network of the stack has no free addresses.",
      ),
    ).toBeTruthy();
  });

  it("names no service when the deploy stopped before any", async () => {
    mount(withOperation({ code: "needs_admin", failedService: "" }));
    expect(
      await screen.findByText("Bayplan stopped: The stack uses host access that needs Admin."),
    ).toBeTruthy();
  });

  it("falls back to a plain sentence for a code it does not know", async () => {
    mount(withOperation({ code: "something_new" }));
    expect(
      await screen.findByText("Bayplan stopped at web_api: The deploy didn't finish."),
    ).toBeTruthy();
  });

  it("says a failed roll back was one and offers Retry roll back", async () => {
    mount(withOperation({ kind: "rollback" }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("The last roll back failed")).toBeTruthy();
    expect(await within(alert).findByRole("button", { name: "Retry roll back" })).toBeTruthy();
    expect(
      within(alert).queryByRole("button", { name: "Roll back to previous version" }),
    ).toBeNull();
  });

  it("has no Open console when the job is not known", async () => {
    mount(withOperation({ jobId: "" }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).queryByRole("link", { name: "Open console" })).toBeNull();
  });

  it.each([
    ["a stack that is deployed", stackOf({ state: "running" })],
    ["a stack that is deploying", stackOf({ state: "deploying" })],
    ["a stack never deployed", stackOf({ state: "not_deployed", lastOperation: undefined })],
  ])("is not shown for %s", async (_name, stack) => {
    const { container } = mount(stack);
    await waitFor(() => expect(container.querySelector("[data-slot=alert]")).toBeNull());
    expect(screen.queryByText("The last deploy failed")).toBeNull();
  });

  it("draws a service name with markup in it as text", async () => {
    mount(withOperation({ failedService: "<b>x</b>" }));
    const text = await screen.findByText(/Bayplan stopped at <b>x<\/b>/);
    expect(text.querySelector("b")).toBeNull();
  });
});
