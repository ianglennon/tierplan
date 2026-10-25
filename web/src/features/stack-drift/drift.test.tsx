// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DriftPage } from "@/features/stack-drift/drift-page";
import { StackOverview } from "@/features/stacks/stack-overview";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let detail: Json;
// What the drift route answers: the newest check, and a fresh one when it is asked for.
let cached: Json | "fail";
let fresh: Json | "fail";
let client: QueryClient;
let driftCalls: { fresh: boolean }[];

function changed(short: string, over: Json = {}): Json {
  return {
    name: `tracer_${short}`,
    short,
    change: "changed",
    current: { Mode: { Replicated: { Replicas: 1 } } },
    next: { Mode: { Replicated: { Replicas: 3 } } },
    hiddenChanges: [],
    settings: 1,
    ...over,
  };
}

function drift(over: Json = {}): Json {
  return {
    state: "drifted",
    revision: 4,
    deployedAt: "2026-10-19T11:00:00Z",
    deployedBy: "bob",
    checkedAt: "2026-10-19T12:00:00Z",
    count: 1,
    services: [changed("web")],
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
    deployedRevision: 4,
    rollback: { available: false, deployedAt: "", deployedBy: "" },
  };
  cached = drift();
  fresh = drift();
  driftCalls = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const path = url.pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(detail);
      if (path === `/api/env/${ENV}/stacks/s1/services`) return Response.json({ items: [] });
      if (path === `/api/env/${ENV}/stacks/s1/drift`) {
        const isFresh = url.searchParams.get("fresh") === "true";
        driftCalls.push({ fresh: isFresh });
        const answer = isFresh ? fresh : cached;
        if (answer === "fail") {
          return Response.json(
            { type: "env_offline", title: "Bad Gateway", status: 502, detail: "down" },
            { status: 502 },
          );
        }
        return Response.json(answer);
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

function mount(ui: React.ReactNode) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>{ui}</TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("a service scaled outside the stack", () => {
  it("shows on the Overview as a status that links to the drift page", async () => {
    mount(<StackOverview envId={ENV} stackId="s1" />);

    const title = await screen.findByText("tracer has drifted from revision #4");
    const box = title.closest('[data-slot="drift-alert"]') as HTMLElement;
    expect(box.getAttribute("role")).toBe("status");
    expect(within(box).getByText("1 setting differs from what was deployed.")).toBeTruthy();
    expect(within(box).getByRole("link", { name: "View drift" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/drift",
    );
  });

  it("is checked afresh once when the Overview opens, and the fresh answer is the one shown", async () => {
    cached = drift({ count: 1 });
    fresh = drift({ count: 5 });
    mount(<StackOverview envId={ENV} stackId="s1" />);

    expect(await screen.findByText("5 settings differ from what was deployed.")).toBeTruthy();
    expect(driftCalls.filter((c) => c.fresh)).toHaveLength(1);
  });

  it("lists on the drift page what drifted, under the title, the revision it is compared with and the intro", async () => {
    fresh = drift({ services: [changed("web"), changed("api", { change: "missing" })], count: 2 });
    mount(<DriftPage envId={ENV} stackId="s1" />);

    expect(await screen.findByRole("heading", { name: "Drift in tracer" })).toBeTruthy();
    expect(screen.getByText(/compared with revision #4, deployed/)).toBeTruthy();
    expect(screen.getByText(/^Bayplan never fixes drift on its own/)).toBeTruthy();
    const list = await screen.findByRole("list", { name: "Services that drifted" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    expect(within(list).getByText("web")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Check again" })).toBeTruthy();
    await waitFor(() => expect(driftCalls.some((c) => c.fresh)).toBe(true));
  });
});
