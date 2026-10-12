// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { PullSummary } from "@/api/pulls";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { PullsIndicator } from "@/features/pulls/pulls-indicator";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const REF = "docker.io/library/nginx:1.27";

const summary = (over: Partial<PullSummary> = {}): PullSummary => ({
  id: "j1",
  envId: "e1",
  envName: "Home lab",
  ref: REF,
  state: "running",
  done: 0,
  failed: 0,
  total: 3,
  createdAt: "2026-10-11T10:00:00Z",
  ...over,
});

let pulls: PullSummary[];
let listStatus: number;
let client: QueryClient;
let calls: string[];

beforeEach(() => {
  pulls = [];
  listStatus = 200;
  calls = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      calls.push(`${req.method} ${path}`);
      if (path === "/api/me") return Response.json(makeMe({ envs: { e1: "operator" } }));
      if (path === "/api/pulls") {
        return listStatus === 200
          ? Response.json({ pulls })
          : Response.json(
              { type: "", title: "x", status: listStatus, detail: "d" },
              { status: listStatus },
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
          <PullsIndicator />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("PullsIndicator", () => {
  it("is hidden while there is nothing to list", async () => {
    const { container } = mount();
    await waitFor(() => expect(calls).toContain("GET /api/pulls"));
    expect(screen.queryByRole("button", { name: /Pulls/ })).toBeNull();
    expect(container.querySelector("button")).toBeNull();
  });

  it("shows a neutral count badge and an accessible name with the number running", async () => {
    pulls = [
      summary(),
      summary({ id: "j2", state: "done", done: 3, finishedAt: "2026-10-11T10:01:00Z" }),
    ];
    mount();
    const button = await screen.findByRole("button", { name: "Pulls, 1 in progress" });
    expect(within(button).getByText("1")).toBeTruthy();
  });

  it("lists the pulls with ref, environment, state and time, and the starter on other people's", async () => {
    pulls = [
      summary({ id: "j1", ownerName: "sam" }),
      summary({
        id: "j2",
        ref: "docker.io/library/busybox:1",
        state: "partial",
        done: 2,
        failed: 1,
        finishedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
      }),
      summary({ id: "j3", state: "failed", failed: 3 }),
      summary({ id: "j4", state: "cancelled" }),
    ];
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Pulls, 1 in progress" }));
    const popover = await screen.findByText("Pulling on 3 of 3 nodes");
    expect(popover).toBeTruthy();
    expect(screen.getByText("by sam")).toBeTruthy();
    expect(screen.getByText("Done, 1 failed")).toBeTruthy();
    expect(screen.getByText("Failed")).toBeTruthy();
    expect(screen.getByText("Cancelled")).toBeTruthy();
    expect(screen.getAllByText("Home lab").length).toBe(4);
    expect(screen.getByText("docker.io/library/busybox:1")).toBeTruthy();
  });

  it("reopens the progress panel of a pull at once, from the stream's snapshot", async () => {
    pulls = [summary()];
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Pulls, 1 in progress" }));
    fireEvent.click(await screen.findByRole("button", { name: /nginx:1.27/ }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: `Pulling ${REF}` })).toBeTruthy();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    const src = FakeEventSource.instances[0] as FakeEventSource;
    expect(src.url).toContain("/api/pulls/j1/stream");
    act(() => {
      src.sendFrame("snapshot", {
        id: "j1",
        envId: "e1",
        ref: REF,
        credential: null,
        state: "running",
        createdAt: "2026-10-11T10:00:00Z",
        finishedAt: "",
        items: [
          {
            nodeId: "n1",
            name: "node-1",
            state: "running",
            code: "",
            message: "",
            current: 50,
            total: 100,
            unit: "bytes",
            done: 0,
            count: 1,
            layers: [],
            digest: "",
            upToDate: false,
          },
        ],
      });
    });
    expect(await within(dialog).findByText("node-1")).toBeTruthy();
    // No form: it opened on the progress.
    expect(within(dialog).queryByLabelText("Image")).toBeNull();
  });

  it("keeps the last list when a poll fails and shows no error", async () => {
    pulls = [summary()];
    mount();
    await screen.findByRole("button", { name: "Pulls, 1 in progress" });
    listStatus = 500;
    await act(async () => {
      await client.refetchQueries({ queryKey: ["pulls"] });
    });
    expect(screen.getByRole("button", { name: "Pulls, 1 in progress" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a pull that ended shows the Pulls button without a count", async () => {
    pulls = [summary({ state: "done", done: 3, finishedAt: "2026-10-11T10:01:00Z" })];
    mount();
    expect(await screen.findByRole("button", { name: "Pulls" })).toBeTruthy();
  });
});
