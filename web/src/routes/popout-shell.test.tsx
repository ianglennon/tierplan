// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { FakeWebSocket } from "@/features/terminal/fake-web-socket";
import type { TerminalProps } from "@/features/terminal/terminal";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The shell pop-out window (plan 04-24, D-32, PLAT-05): a top-level page outside the app
// layout, behind the same session guard, with the launcher first. xterm needs a real
// layout, so the terminal is a stand-in and the socket a fake.

vi.mock("@/features/terminal/terminal", () => ({
  Terminal: (props: TerminalProps) => <section aria-label={`Terminal in ${props.name}`} />,
}));

const PATH = "/popout/e1/containers/c1/shell";

type Who = "operator" | "signed-out" | "no-access";

interface Sent {
  method: string;
  path: string;
  query: string;
  body?: Record<string, unknown>;
}
let sent: Sent[];

function stub(who: Who) {
  const me =
    who === "operator"
      ? makeMe({ csrfToken: "c", envs: { e1: "operator" }, extras: { e1: ["containers.exec"] } })
      : makeMe({ csrfToken: "c", envs: {} });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const path = url.pathname;
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, query: url.search, body });
      if (path === "/api/instance") {
        return Response.json({
          instanceName: "TierPlan Home",
          needsSetup: false,
          rememberDays: 30,
        });
      }
      if (path === "/api/me") {
        if (who === "signed-out") return Response.json({ status: 401 }, { status: 401 });
        return Response.json(me);
      }
      if (path === "/api/environments") {
        return Response.json({
          items: [{ id: "e1", name: "Homelab", status: { state: "online" } }],
        });
      }
      if (path === "/api/env/e1/containers/c1") {
        return Response.json({
          id: "c1",
          name: "web-1",
          state: "running",
          image: "nginx:1.27",
          nodeId: "n1",
          stack: "",
          labels: [],
        });
      }
      if (path === "/api/me/shell-preferences") {
        return Response.json({ code: "shell_preference_not_found" }, { status: 404 });
      }
      if (path === "/api/env/e1/containers/c1/shell") {
        return Response.json(
          {
            sessionId: "SID1",
            wsPath: "/api/env/e1/shell-sessions/SID1/ws",
            command: ["/bin/sh"],
            user: "",
            workingDir: "",
          },
          { status: 201 },
        );
      }
      if (path === "/api/version") {
        return Response.json({
          version: "",
          commit: "abc1234",
          display: "dev (abc1234)",
          sourceUrl: "https://github.com/ianglennon/tierplan",
          sourceTreeUrl: "https://github.com/ianglennon/tierplan/tree/abc1234",
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
  sent = [];
  FakeWebSocket.reset();
  setCsrfToken(null);
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  vi.stubGlobal("WebSocket", FakeWebSocket);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  localStorage.clear();
});

describe("the shell pop-out", () => {
  it("sends a signed-out visitor to sign-in with the pop-out remembered in next", async () => {
    stub("signed-out");
    const router = visit(`${PATH}?node=n1`);
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    const next = (router.state.location.search as { next?: string }).next;
    expect(next?.startsWith(PATH)).toBe(true);
    expect(next).toContain("node=n1");
  });

  it("opens outside the app chrome with the container, its state, the environment and the version footer", async () => {
    stub("operator");
    visit(PATH);
    expect(await screen.findByRole("heading", { level: 1, name: "web-1" })).toBeTruthy();
    expect(await screen.findByText("Homelab")).toBeTruthy();
    expect(screen.getByText("Running")).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: /main/i })).toBeNull();
    expect(document.querySelector('[data-slot="sidebar"]')).toBeNull();
    expect(await screen.findByRole("link", { name: /dev \(abc1234\)/ })).toBeTruthy();
  });

  it("shows the launcher first, when no session is open in this window", async () => {
    stub("operator");
    visit(PATH);
    expect(await screen.findByRole("heading", { name: "Open a shell in web-1" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open shell" })).toBeTruthy();
    expect(screen.queryByLabelText("Terminal in web-1")).toBeNull();
    expect(FakeWebSocket.instances).toHaveLength(0);
    expect(sent.filter((s) => s.method === "POST")).toHaveLength(0);
  });

  it("titles the window with the container and the instance", async () => {
    stub("operator");
    visit(PATH);
    await waitFor(() => expect(document.title).toBe("web-1 shell · TierPlan Home"));
  });

  it("opens a shell in its own window on the node named, with the toolbar but no second pop-out", async () => {
    stub("operator");
    visit(`${PATH}?node=n7`);
    const open = await screen.findByRole("button", { name: "Open shell" });
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
    await act(async () => {});
    fireEvent.click(open);
    await waitFor(() => expect(sent.filter((s) => s.method === "POST")).toHaveLength(1));
    expect(sent.find((s) => s.method === "POST")?.body).toMatchObject({
      nodeId: "n7",
      shell: "auto",
    });
    expect(
      new URLSearchParams(sent.find((s) => s.path === "/api/env/e1/containers/c1")?.query).get(
        "nodeId",
      ),
    ).toBe("n7");
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
    const ws = FakeWebSocket.instances[0] as FakeWebSocket;
    act(() => ws.open());
    const bar = await screen.findByRole("toolbar", { name: "Terminal controls" });
    expect(within(bar).getByText("Connected")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "End session" })).toBeTruthy();
    expect(within(bar).queryByRole("button", { name: "Open in new window" })).toBeNull();
  });

  it("says there is no access for a user who cannot read the container, and opens nothing", async () => {
    stub("no-access");
    visit(PATH);
    expect(await screen.findByText(/don't have permission|not allowed|permission/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open shell" })).toBeNull();
    expect(sent.filter((s) => s.path === "/api/env/e1/containers/c1")).toHaveLength(0);
    expect(FakeWebSocket.instances).toHaveLength(0);
  });
});
