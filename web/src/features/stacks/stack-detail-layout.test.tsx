// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackDetailLayout } from "@/features/stacks/stack-detail-layout";
import { StackOverview } from "@/features/stacks/stack-overview";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let role: "viewer" | "operator" = "viewer";
let detail: Record<string, unknown>;
let status: number;
let client: QueryClient;

beforeEach(() => {
  role = "viewer";
  detail = {
    id: "s1",
    name: "tracer",
    target: "swarm",
    source: "inline",
    state: "not_deployed",
    lastDeployedAt: "",
    lastDeployedBy: "",
    hasDraft: false,
    envId: ENV,
    sourceRef: "",
    createdBy: "ada",
    createdAt: "2026-10-19T10:00:00Z",
    stopped: false,
  };
  status = 200;
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
      if (path === `/api/env/${ENV}/stacks/s1`) {
        if (status === 404) {
          return Response.json(
            { type: "stack_not_found", title: "Not Found", status: 404, detail: "gone" },
            { status: 404 },
          );
        }
        return Response.json(detail);
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

function mount(flow?: "review" | "deploy") {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackDetailLayout envId={ENV} stackId="s1" flow={flow}>
            <StackOverview envId={ENV} stackId="s1" />
          </StackDetailLayout>
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("StackDetailLayout", () => {
  it("shows the breadcrumb, the name, the state, the badges and the never-deployed line", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "tracer", level: 2 })).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Stacks" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
    expect(within(crumb).getByText("tracer")).toBeTruthy();
    expect(screen.getByText("Not deployed")).toBeTruthy();
    expect(screen.getByText("Swarm")).toBeTruthy();
    expect(screen.getByText("Inline")).toBeTruthy();
    expect(await screen.findByText("Prod · Never deployed")).toBeTruthy();
  });

  it("has an Overview tab with the Not deployed yet state for a stack that was never deployed", async () => {
    mount();
    expect(await screen.findByRole("tab", { name: "Overview" })).toBeTruthy();
    expect(await screen.findByRole("heading", { name: "Not deployed yet", level: 2 })).toBeTruthy();
    expect(
      screen.getByText(
        "tracer has a definition, but nothing is running. Review and deploy it when you're ready.",
      ),
    ).toBeTruthy();
  });

  it("shows the details and the last deploy of a deployed stack", async () => {
    detail = {
      ...detail,
      state: "running",
      lastDeployedAt: "2026-10-19T11:00:00Z",
      lastDeployedBy: "bob",
      hasDraft: true,
    };
    mount();
    expect(await screen.findByText("Running")).toBeTruthy();
    expect(screen.getByText("Draft")).toBeTruthy();
    expect(await screen.findByRole("heading", { name: "Details" })).toBeTruthy();
    expect(screen.queryByText("Not deployed yet")).toBeNull();
    expect(screen.getAllByText(/by bob/).length).toBeGreaterThan(0);
  });

  it("renders a name with markup in it as text", async () => {
    detail = { ...detail, name: "<b>bold</b>" };
    mount();
    const heading = await screen.findByRole("heading", { name: "<b>bold</b>", level: 2 });
    expect(heading.querySelector("b")).toBeNull();
  });

  it("becomes the gone state when the server says the stack does not exist", async () => {
    status = 404;
    mount();
    expect(
      await screen.findByRole("heading", { name: "This stack is gone", level: 2 }),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to stacks" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
  });
});

describe("the flow caption", () => {
  const stepsOf = () =>
    within(screen.getByRole("navigation", { name: "Deploy steps" }))
      .getAllByRole("listitem")
      .map((li) => [li.textContent, li.getAttribute("aria-current")]);

  it("replaces the tab bar on the Review, with Step 2 of 3: Review as the current step", async () => {
    mount("review");
    await screen.findByRole("heading", { name: "tracer", level: 2 });
    expect(stepsOf()).toEqual([
      ["1 Edit", null],
      ["Step 2 of 3: Review", "step"],
      ["3 Deploy", null],
    ]);
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
    expect(screen.queryByRole("tablist")).toBeNull();
  });

  it("makes Step 3 of 3: Deploy the current step on the console", async () => {
    mount("deploy");
    await screen.findByRole("heading", { name: "tracer", level: 2 });
    expect(stepsOf()).toEqual([
      ["1 Edit", null],
      ["2 Review", null],
      ["Step 3 of 3: Deploy", "step"],
    ]);
  });

  it("keeps the breadcrumb and the header, and shows the tabs and no caption without a flow", async () => {
    mount("review");
    expect(await screen.findByText("Not deployed")).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toBeTruthy();
    cleanup();
    mount();
    expect(await screen.findByRole("tab", { name: "Overview" })).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: "Deploy steps" })).toBeNull();
  });
});

describe("the Deploy… action", () => {
  it("is a link to the review for a user who may deploy", async () => {
    role = "operator";
    mount();
    const link = await screen.findByRole("link", { name: "Deploy…" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s1/review");
  });

  it("is off with Needs Operator for a Viewer, and the reason is in its description", async () => {
    mount();
    const button = await screen.findByRole("button", { name: "Deploy…" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    const id = button.getAttribute("aria-describedby");
    expect(document.getElementById(id as string)?.textContent).toBe("Needs Operator");
    expect(screen.queryByRole("link", { name: "Deploy…" })).toBeNull();
  });

  it("is left out on the review and the console, where the flow caption is", async () => {
    role = "operator";
    mount("review");
    await screen.findByRole("heading", { name: "tracer", level: 2 });
    expect(screen.queryByRole("link", { name: "Deploy…" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deploy…" })).toBeNull();
    cleanup();
    mount("deploy");
    await screen.findByRole("heading", { name: "tracer", level: 2 });
    expect(screen.queryByRole("link", { name: "Deploy…" })).toBeNull();
  });

  it("waits for the stack, so it is not offered for a stack that is gone", async () => {
    role = "operator";
    status = 404;
    mount();
    await screen.findByRole("heading", { name: "This stack is gone", level: 2 });
    expect(screen.queryByRole("link", { name: "Deploy…" })).toBeNull();
  });
});

describe("the failed deploy alert", () => {
  const failed = () => ({
    ...detail,
    state: "deploy_failed",
    rollback: { available: true, deployedAt: "2026-10-19T11:00:00Z", deployedBy: "bob" },
    lastOperation: {
      kind: "deploy",
      state: "failed",
      code: "service_rejected",
      failedService: "tracer_web",
      jobId: "j1",
      startedBy: "ada",
      startedAt: "2026-10-19T12:00:00Z",
      endedAt: "2026-10-19T12:00:09Z",
    },
  });

  it("is shown above the tabs while the last deploy has failed, until a deploy succeeds", async () => {
    detail = failed();
    mount();
    const alert = await screen.findByText("The last deploy failed");
    const tabs = screen.getByRole("tablist");
    // Above the tabs, in the page's order.
    expect(alert.compareDocumentPosition(tabs) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(
      screen.getByText("Bayplan stopped at tracer_web: Docker rejected the service."),
    ).toBeTruthy();
  });

  it("is not shown once the stack is deployed", async () => {
    detail = { ...failed(), state: "running" };
    mount();
    await screen.findByRole("heading", { name: "tracer", level: 2 });
    expect(screen.queryByText("The last deploy failed")).toBeNull();
  });

  it("is shown on the review, where the person is about to deploy again", async () => {
    detail = failed();
    mount("review");
    expect(await screen.findByText("The last deploy failed")).toBeTruthy();
  });

  it("is left out of the console, which is that deploy's own page", async () => {
    detail = failed();
    mount("deploy");
    await screen.findByRole("heading", { name: "tracer", level: 2 });
    expect(screen.queryByText("The last deploy failed")).toBeNull();
  });

  it("comes before the operation lock when both are there", async () => {
    detail = {
      ...failed(),
      lock: {
        stackId: "s2",
        stackName: "other",
        jobId: "j9",
        kind: "deploy",
        startedAt: "2026-10-19T12:00:00Z",
        startedBy: "bob",
      },
    };
    mount();
    const alert = await screen.findByText("The last deploy failed");
    const lock = await screen.findByText(/Another stack operation is running/);
    expect(alert.compareDocumentPosition(lock) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

describe("the operation lock", () => {
  const held = () => ({
    stackId: "s9",
    stackName: "other",
    jobId: "j9",
    kind: "deploy",
    startedAt: new Date(Date.now() - 2 * 60_000).toISOString(),
    startedBy: "ada",
  });

  it("is shown above the tabs on every page of the environment while an operation runs", async () => {
    detail = { ...detail, lock: held() };
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(
      /^Another stack operation is running on Prod: other, started/,
    );
    expect(within(alert).getByRole("link", { name: "Open it" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s9/deploys/j9",
    );
    // Above the tab bar.
    const tabs = screen.getByRole("tablist");
    expect(alert.compareDocumentPosition(tabs) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("is shown on the review, which is where Deploy waits for it", async () => {
    detail = { ...detail, lock: held() };
    mount("review");
    expect(await screen.findByRole("alert")).toBeTruthy();
  });

  it("is left out of the console: that page is the operation", async () => {
    detail = { ...detail, lock: held() };
    mount("deploy");
    await screen.findByRole("heading", { name: "tracer", level: 2 });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("is not there when nothing holds the environment", async () => {
    mount();
    await screen.findByRole("heading", { name: "tracer", level: 2 });
    expect(screen.queryByText(/Another stack operation/)).toBeNull();
  });
});
