// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The logs pop-out window (plan 04-21, D-22, PLAT-05, T-04-74): a top-level page outside
// the app layout, behind the same session guard.

const PATH = "/popout/e1/containers/c1/logs";

type Who = "viewer" | "signed-out" | "no-access";

// The state the stack of the stack pop-out tests is in.
let stackState = "running";

function stub(who: Who) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Bayplan Home", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") {
        if (who === "signed-out") return Response.json({ status: 401 }, { status: 401 });
        return Response.json(
          makeMe({ csrfToken: "c", envs: who === "viewer" ? { e1: "viewer" } : {} }),
        );
      }
      if (path === "/api/environments") {
        return Response.json({
          items: [{ id: "e1", name: "Homelab", status: { state: "online" } }],
        });
      }
      if (path === "/api/env/e1/containers/c1") {
        return Response.json({ id: "c1", name: "web-1", state: "running", stack: "", labels: [] });
      }
      if (path === "/api/env/e1/services/web") {
        return Response.json({
          id: "svc1",
          name: "web",
          mode: "replicated",
          running: 2,
          desired: 2,
          updateStatus: "",
          labels: [],
        });
      }
      if (path === "/api/env/e1/stacks/s1") {
        return Response.json({
          id: "s1",
          name: "tracer",
          target: "swarm",
          source: "inline",
          state: stackState,
          lastDeployedAt: "2026-10-19T11:00:00Z",
          lastDeployedBy: "bob",
          hasDraft: false,
          envId: "e1",
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: stackState === "stopped",
          servicesRunning: 2,
          servicesTotal: 2,
          rollback: { available: false, deployedAt: "", deployedBy: "" },
        });
      }
      if (path === "/api/env/e1/stacks/s1/services") return Response.json({ items: [] });
      if (path === "/api/env/e1/stacks/s1/resources") {
        return Response.json({
          tasks: { items: [], error: "" },
          networks: { items: [], error: "" },
          volumes: { items: [], error: "", notReporting: [] },
          secrets: { items: [], error: "" },
          configs: { items: [], error: "" },
        });
      }
      if (path === "/api/version") {
        return Response.json({
          version: "",
          commit: "abc1234",
          display: "dev (abc1234)",
          sourceUrl: "https://github.com/ianglennon/bayplan",
          sourceTreeUrl: "https://github.com/ianglennon/bayplan/tree/abc1234",
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
}

function visit(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

beforeEach(() => {
  stackState = "running";
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  localStorage.clear();
});

describe("the logs pop-out", () => {
  it("sends a signed-out visitor to sign-in with the pop-out remembered in next", async () => {
    stub("signed-out");
    const router = visit(`${PATH}?q=boom&ts=true`);
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    const next = (router.state.location.search as { next?: string }).next;
    expect(next).toBeDefined();
    expect(next?.startsWith(PATH)).toBe(true);
    expect(next).toContain("q=boom");
  });

  it("opens outside the app chrome with the container, its state, the environment and the version footer", async () => {
    stub("viewer");
    visit(PATH);
    expect(await screen.findByRole("heading", { level: 1, name: "web-1" })).toBeTruthy();
    expect(await screen.findByText("Homelab")).toBeTruthy();
    expect(screen.getByText("Running")).toBeTruthy();
    // No sidebar, no user menu: the app layout is not around it.
    expect(screen.queryByRole("navigation", { name: /main/i })).toBeNull();
    expect(document.querySelector('[data-slot="sidebar"]')).toBeNull();
    expect(await screen.findByRole("link", { name: /dev \(abc1234\)/ })).toBeTruthy();
    expect(screen.getByRole("log", { name: "Logs for web-1" })).toBeTruthy();
  });

  it("titles the window with the container and the instance", async () => {
    stub("viewer");
    visit(PATH);
    await waitFor(() => expect(document.title).toBe("web-1 logs · Bayplan Home"));
  });

  it("opens the stream for the container on the node named, and reads the settings from its address", async () => {
    stub("viewer");
    visit(`${PATH}?node=n7&q=boom&ts=true&wrap=true&since=1h`);
    await screen.findByRole("log", { name: "Logs for web-1" });
    const url = new URL((FakeEventSource.instances[0] as FakeEventSource).url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/containers/c1/logs/stream");
    expect(url.searchParams.get("nodeId")).toBe("n7");
    expect(url.searchParams.has("since")).toBe(true);
    expect((screen.getByRole("searchbox", { name: "Search logs" }) as HTMLInputElement).value).toBe(
      "boom",
    );
    expect(screen.getByRole("button", { name: "Timestamps", pressed: true })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Wrap", pressed: true })).toBeTruthy();
    // The pop-out does not offer a pop-out.
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
  });

  it("says so, and opens no stream, for a person who may not read containers there", async () => {
    stub("no-access");
    visit(PATH);
    expect(await screen.findByText(/don't have permission|not allowed|permission/i)).toBeTruthy();
    expect(FakeEventSource.instances).toHaveLength(0);
  });
});

// The logs pop-out of a service (plan 06-31, OBS-02, D-35): the same viewer, chrome-less.
describe("the service logs pop-out", () => {
  const SERVICE_PATH = "/popout/e1/services/web/logs";

  it("sends a signed-out visitor to sign-in with the pop-out remembered in next", async () => {
    stub("signed-out");
    const router = visit(`${SERVICE_PATH}?task=t1&stopped=true`);
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    const next = (router.state.location.search as { next?: string }).next;
    expect(next?.startsWith(SERVICE_PATH)).toBe(true);
    expect(next).toContain("task=t1");
  });

  it("opens outside the app chrome with the service, its state, the environment and the version footer", async () => {
    stub("viewer");
    visit(SERVICE_PATH);
    expect(await screen.findByRole("heading", { level: 1, name: "web" })).toBeTruthy();
    expect(await screen.findByText("Homelab")).toBeTruthy();
    expect(await screen.findByText("Running")).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: /main/i })).toBeNull();
    expect(document.querySelector('[data-slot="sidebar"]')).toBeNull();
    expect(await screen.findByRole("link", { name: /dev \(abc1234\)/ })).toBeTruthy();
    expect(screen.getByRole("log", { name: "Logs for web" })).toBeTruthy();
  });

  it("titles the window with the service and the instance", async () => {
    stub("viewer");
    visit(SERVICE_PATH);
    await waitFor(() => expect(document.title).toBe("web logs · Bayplan Home"));
  });

  it("opens the stream of the service narrowed as the address says, and reads the settings from it", async () => {
    stub("viewer");
    visit(
      `${SERVICE_PATH}?task=t1&node=alpha&stopped=true&prefix=false&q=boom&ts=true&wrap=true&since=1h`,
    );
    await screen.findByRole("log", { name: "Logs for web" });
    const url = new URL((FakeEventSource.instances[0] as FakeEventSource).url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/services/web/logs/stream");
    expect(url.searchParams.get("task")).toBe("t1");
    expect(url.searchParams.get("node")).toBe("alpha");
    expect(url.searchParams.get("stopped")).toBe("1");
    expect(url.searchParams.has("since")).toBe(true);
    expect((screen.getByRole("searchbox", { name: "Search logs" }) as HTMLInputElement).value).toBe(
      "boom",
    );
    expect(screen.getByRole("button", { name: "Timestamps", pressed: true })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Wrap", pressed: true })).toBeTruthy();
    // The pop-out does not offer a pop-out.
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
  });

  it("says so, and opens no stream, for a person who may not read services there", async () => {
    stub("no-access");
    visit(SERVICE_PATH);
    expect(await screen.findByText(/don't have permission|not allowed|permission/i)).toBeTruthy();
    expect(FakeEventSource.instances).toHaveLength(0);
  });
});

// The logs pop-out of a stack (plan 07-29, STK-10, D-22): the same viewer, chrome-less.
describe("the stack logs pop-out", () => {
  const STACK_PATH = "/popout/e1/stacks/s1/logs";

  it("sends a signed-out visitor to sign-in with the pop-out remembered in next", async () => {
    stub("signed-out");
    const router = visit(`${STACK_PATH}?service=web&stopped=true`);
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    const next = (router.state.location.search as { next?: string }).next;
    expect(next?.startsWith(STACK_PATH)).toBe(true);
    expect(next).toContain("service=web");
  });

  it("opens outside the app chrome with the stack, its state, the environment and the version footer", async () => {
    stub("viewer");
    visit(STACK_PATH);
    expect(await screen.findByRole("heading", { level: 1, name: "tracer" })).toBeTruthy();
    expect(await screen.findByText("Homelab")).toBeTruthy();
    expect(await screen.findByText("Running")).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: /main/i })).toBeNull();
    expect(document.querySelector('[data-slot="sidebar"]')).toBeNull();
    expect(await screen.findByRole("link", { name: /dev \(abc1234\)/ })).toBeTruthy();
    expect(await screen.findByRole("log", { name: "Logs for tracer" })).toBeTruthy();
  });

  it("titles the window with the stack and the instance", async () => {
    stub("viewer");
    visit(STACK_PATH);
    await waitFor(() => expect(document.title).toBe("tracer logs · Bayplan Home"));
  });

  it("opens the stream of the stack narrowed as the address says, and reads the settings from it", async () => {
    stub("viewer");
    visit(
      `${STACK_PATH}?service=web,api&task=t1&node=alpha&stopped=true&prefix=false&q=boom&ts=true&wrap=true&since=1h`,
    );
    await screen.findByRole("log", { name: "Logs for tracer" });
    const url = new URL((FakeEventSource.instances[0] as FakeEventSource).url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/stacks/s1/logs/stream");
    expect(url.searchParams.get("services")).toBe("web,api");
    expect(url.searchParams.get("tasks")).toBe("t1");
    expect(url.searchParams.get("nodes")).toBe("alpha");
    expect(url.searchParams.get("stopped")).toBe("1");
    expect(url.searchParams.has("since")).toBe(true);
    expect((screen.getByRole("searchbox", { name: "Search logs" }) as HTMLInputElement).value).toBe(
      "boom",
    );
    expect(screen.getByRole("button", { name: "Timestamps", pressed: true })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Wrap", pressed: true })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Task names", pressed: false })).toBeTruthy();
    // The pop-out does not offer a pop-out.
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
  });

  it("opens no stream, and says the stack isn't running, for a stack that was stopped", async () => {
    stackState = "stopped";
    stub("viewer");
    visit(STACK_PATH);
    expect(await screen.findByText("tracer isn't running")).toBeTruthy();
    expect(screen.getByText("Start it to see its logs.")).toBeTruthy();
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("says so, and opens no stream, for a person who may not read stacks there", async () => {
    stub("no-access");
    visit(STACK_PATH);
    expect(await screen.findByText(/don't have permission|not allowed|permission/i)).toBeTruthy();
    expect(FakeEventSource.instances).toHaveLength(0);
  });
});
