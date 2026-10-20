// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import {
  STACK_RESOURCES_POLL_BUSY_MS,
  STACK_RESOURCES_POLL_MS,
  stackResourcesQuery,
} from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackDetailLayout } from "@/features/stacks/stack-detail-layout";
import { StackResources } from "@/features/stacks/stack-resources";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let detail: Json;
let resources: Json | "fail" | "hang";
let resourceCalls: number;
let client: QueryClient;

function task(over: Json = {}): Json {
  return {
    task: {
      id: "tttttttttttttttttttttttt1",
      slot: 2,
      nodeId: "node-1",
      node: "mgr-1",
      nodeKnown: true,
      targetNodeId: "",
      state: "running",
      dockerState: "running",
      desiredState: "running",
      error: "",
      message: "started",
      exitCode: null,
      containerId: "c1",
      createdAt: "2026-10-20T10:00:00Z",
      updatedAt: "2026-10-20T11:00:00Z",
      stateSince: "2026-10-20T10:30:00Z",
    },
    serviceId: "svc-1",
    service: "web",
    global: false,
    ...over,
  };
}

function network(over: Json = {}): Json {
  return {
    id: "net-1",
    name: "tracer_default",
    driver: "overlay",
    scope: "swarm",
    ownership: "stack",
    ...over,
  };
}

beforeEach(() => {
  detail = {
    id: "s1",
    name: "tracer",
    target: "swarm",
    source: "inline",
    state: "running",
    lastDeployedAt: "2026-10-19T11:00:00Z",
    lastDeployedBy: "bob",
    hasDraft: false,
    envId: ENV,
    sourceRef: "",
    createdBy: "ada",
    createdAt: "2026-10-19T10:00:00Z",
    stopped: false,
    rollback: { available: false, deployedAt: "", deployedBy: "" },
  };
  resources = {
    tasks: { items: [task()], error: "" },
    networks: { items: [network()], error: "" },
  };
  resourceCalls = 0;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(detail);
      if (path === `/api/env/${ENV}/stacks/s1/resources`) {
        resourceCalls++;
        if (resources === "hang") return new Promise<Response>(() => {});
        if (resources === "fail") {
          return Response.json(
            { type: "env_offline", title: "Bad Gateway", status: 502, detail: "down" },
            { status: 502 },
          );
        }
        return Response.json(resources);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackResources envId={ENV} stackId="s1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("StackResources of a swarm stack", () => {
  it("shows the Tasks card then the Networks card, each with its count in words", async () => {
    mount();

    const tasks = await screen.findByRole("region", { name: "Tasks" });
    const networks = await screen.findByRole("region", { name: "Networks" });
    expect(tasks.compareDocumentPosition(networks) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(tasks).getByText(/^1 task\b/)).toBeTruthy();
    expect(within(networks).getByText("1 network")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Containers" })).toBeNull();
  });

  it("lists a task with its slot name and short ID, its service and node as links, its state and its updated time", async () => {
    mount();

    const card = await screen.findByRole("region", { name: "Tasks" });
    const table = await within(card).findByRole("table");
    const headers = within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(headers).toEqual(["Task", "Service", "State", "Node", "Updated", "Error"]);
    expect(within(table).getByText("web.2")).toBeTruthy();
    expect(within(table).getByText("tttttttttttt")).toBeTruthy();
    expect(within(table).getByRole("link", { name: "web" }).getAttribute("href")).toBe(
      "/env/e1/services/svc-1",
    );
    expect(within(table).getByRole("link", { name: "mgr-1" }).getAttribute("href")).toBe(
      "/env/e1/nodes/node-1",
    );
    expect(within(table).getByText("Running")).toBeTruthy();
    expect(
      within(card).getByText(/Showing current tasks\. Open a service for its history\./),
    ).toBeTruthy();
  });

  it("names the task of a global service by its service alone", async () => {
    resources = {
      tasks: { items: [task({ global: true, service: "agent" })], error: "" },
      networks: { items: [], error: "" },
    };
    mount();

    const table = await within(await screen.findByRole("region", { name: "Tasks" })).findByRole(
      "table",
    );
    expect(within(table).getByRole("link", { name: "agent" })).toBeTruthy();
    expect(within(table).queryByText("agent.2")).toBeNull();
  });

  it("shows a task's error in the offline colour with the whole text available", async () => {
    resources = {
      tasks: {
        items: [
          task({
            task: {
              ...(task().task as Json),
              state: "rejected",
              dockerState: "rejected",
              error: "no suitable node (scheduling constraints not satisfied)",
            },
          }),
        ],
        error: "",
      },
      networks: { items: [], error: "" },
    };
    mount();

    const table = await within(await screen.findByRole("region", { name: "Tasks" })).findByRole(
      "table",
    );
    expect(
      within(table).getByText("no suitable node (scheduling constraints not satisfied)"),
    ).toBeTruthy();
  });

  it("marks a network the stack owns Stack", async () => {
    mount();

    const card = await screen.findByRole("region", { name: "Networks" });
    const table = await within(card).findByRole("table");
    const headers = within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(headers).toEqual(["Name", "Driver", "Scope", "Ownership"]);
    expect(within(table).getByRole("link", { name: "tracer_default" }).getAttribute("href")).toBe(
      "/env/e1/networks/net-1",
    );
    expect(within(table).getByText("overlay")).toBeTruthy();
    expect(within(table).getByText("swarm")).toBeTruthy();
    expect(within(table).getByText("Stack")).toBeTruthy();
    expect(within(table).queryByText("External")).toBeNull();
  });

  it("marks an external network External, with the sentence that Bayplan never changes it", async () => {
    resources = {
      tasks: { items: [], error: "" },
      networks: {
        items: [network({ id: "net-x", name: "proxy", ownership: "external" })],
        error: "",
      },
    };
    mount();

    const card = await screen.findByRole("region", { name: "Networks" });
    const badge = await within(card).findByText("External");
    expect(within(card).queryByText("Stack")).toBeNull();
    fireEvent.focus(badge.closest("button") as HTMLElement);
    expect(
      (
        await screen.findAllByText(
          "Created outside this stack. Bayplan never changes or removes it.",
        )
      ).length,
    ).toBeGreaterThan(0);
  });

  it("says nothing is running on a stopped stack and a muted line for an empty kind, never hiding a card", async () => {
    detail = { ...detail, state: "stopped" };
    resources = { tasks: { items: [], error: "" }, networks: { items: [], error: "" } };
    mount();

    const tasks = await screen.findByRole("region", { name: "Tasks" });
    const networks = await screen.findByRole("region", { name: "Networks" });
    expect(await within(tasks).findByText("Nothing is running.")).toBeTruthy();
    expect(within(networks).getByText("No networks.")).toBeTruthy();
    expect(within(tasks).getByText(/^0 tasks\b/)).toBeTruthy();
    expect(within(tasks).queryByRole("table")).toBeNull();
  });

  it("says No tasks. for a running stack that has none", async () => {
    resources = { tasks: { items: [], error: "" }, networks: { items: [], error: "" } };
    mount();

    const tasks = await screen.findByRole("region", { name: "Tasks" });
    expect(await within(tasks).findByText("No tasks.")).toBeTruthy();
  });

  it("shows three skeleton rows in each card while the read is on its way", async () => {
    resources = "hang";
    mount();

    const tasks = await screen.findByTestId("skeleton-tasks");
    const networks = await screen.findByTestId("skeleton-networks");
    expect(tasks.children).toHaveLength(3);
    expect(networks.children).toHaveLength(3);
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("draws Docker's text as text", async () => {
    resources = {
      tasks: { items: [], error: "" },
      networks: {
        items: [network({ name: "<img src=x onerror=alert(1)>", driver: "<b>x</b>" })],
        error: "",
      },
    };
    mount();

    const card = await screen.findByRole("region", { name: "Networks" });
    const table = await within(card).findByRole("table");
    expect(within(table).getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(table.querySelector("img")).toBeNull();
    expect(table.querySelector("b")).toBeNull();
  });

  it("shows each card's failure with a Retry when the whole read fails, and reads again on Retry", async () => {
    resources = "fail";
    mount();

    const tasks = await screen.findByRole("region", { name: "Tasks" });
    expect(await within(tasks).findByText("Couldn't load tasks.")).toBeTruthy();
    const networks = screen.getByRole("region", { name: "Networks" });
    expect(within(networks).getByText("Couldn't load networks.")).toBeTruthy();

    resources = { tasks: { items: [task()], error: "" }, networks: { items: [], error: "" } };
    const before = resourceCalls;
    fireEvent.click(within(tasks).getByRole("button", { name: "Retry loading tasks" }));

    expect(await within(tasks).findByRole("table")).toBeTruthy();
    expect(resourceCalls).toBeGreaterThan(before);
  });
});

describe("StackResources of a standalone stack", () => {
  beforeEach(() => {
    detail = { ...detail, target: "standalone" };
    resources = {
      containers: {
        items: [
          {
            id: "c-web",
            name: "tracer-web-1",
            service: "web",
            state: "running",
            health: "healthy",
            image: "nginx:1.27@sha256:aaaa",
            ports: ["8080->80/tcp"],
            created: "2026-10-20T10:00:00Z",
          },
        ],
        error: "",
      },
      networks: {
        items: [network({ id: "net-b", name: "tracer_default", driver: "bridge", scope: "local" })],
        error: "",
      },
    };
  });

  it("shows the Containers card first, with the container linking to its page", async () => {
    mount();

    const containers = await screen.findByRole("region", { name: "Containers" });
    const table = await within(containers).findByRole("table");
    const headers = within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(headers).toEqual(["Name", "Service", "State", "Image", "Ports", "Updated"]);
    expect(within(table).getByRole("link", { name: "tracer-web-1" }).getAttribute("href")).toBe(
      "/env/e1/containers/c-web",
    );
    expect(within(table).getByText("8080->80/tcp")).toBeTruthy();
    expect(within(table).getByText("Running")).toBeTruthy();
    expect(within(containers).getByText("1 container")).toBeTruthy();
    const networks = screen.getByRole("region", { name: "Networks" });
    expect(
      containers.compareDocumentPosition(networks) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Tasks" })).toBeNull();
  });
});

describe("stackResourcesQuery", () => {
  function interval(o: { target: "swarm" | "standalone"; busy?: boolean }) {
    const opts = stackResourcesQuery(ENV, "s1", o);
    const fn = opts.refetchInterval as (q: unknown) => number | false;
    return fn({});
  }

  it("reads a swarm every 5 s, every 2 s during an operation, and a standalone host at the lists' pace", () => {
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");

    expect(interval({ target: "swarm" })).toBe(STACK_RESOURCES_POLL_MS);
    expect(interval({ target: "swarm", busy: true })).toBe(STACK_RESOURCES_POLL_BUSY_MS);
    expect(STACK_RESOURCES_POLL_MS).toBe(5_000);
    expect(STACK_RESOURCES_POLL_BUSY_MS).toBe(2_000);
    expect(interval({ target: "standalone" })).toBe(30_000);
  });

  it("stops reading while the page is hidden", () => {
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");

    expect(interval({ target: "swarm" })).toBe(false);
    expect(interval({ target: "swarm", busy: true })).toBe(false);
    expect(interval({ target: "standalone" })).toBe(false);
  });
});

describe("the Resources tab of the stack page", () => {
  it("is the third tab, after Overview and Editor", async () => {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <StackDetailLayout envId={ENV} stackId="s1">
              <div />
            </StackDetailLayout>
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );

    await waitFor(() => expect(screen.getAllByRole("tab")).toHaveLength(3));
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Overview",
      "Editor",
      "Resources",
    ]);
  });
});
