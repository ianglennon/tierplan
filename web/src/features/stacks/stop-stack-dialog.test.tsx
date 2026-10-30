// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StopStackDialog } from "@/features/stacks/stop-stack-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const ENV = "e1";

let services: Array<Record<string, unknown>>;
let stopAnswer: { status: number; body: unknown };
let posts: string[];
let client: QueryClient;

beforeEach(() => {
  services = [];
  stopAnswer = { status: 202, body: { jobId: "j1", recreates: false } };
  posts = [];
  toasts.success.mockReset();
  toasts.error.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === `/api/env/${ENV}/stacks/s1/services`) return Response.json({ items: services });
      if (req.method === "POST" && path === `/api/env/${ENV}/stacks/s1/stop`) {
        posts.push(path);
        return Response.json(stopAnswer.body, { status: stopAnswer.status });
      }
      if (path === `/api/env/${ENV}/stacks/s1/deploys/j1`) {
        return Response.json({ id: "j1", state: "done", items: [] });
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

function mount(target: "swarm" | "standalone", name = "tracer") {
  const onOpenChange = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StopStackDialog
            stack={{ id: "s1", envId: ENV, name, target }}
            open
            onOpenChange={onOpenChange}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return onOpenChange;
}

const swarmRow = (mode: string) => ({ name: `tracer_${mode}`, mode, running: 1, desired: 1 });
const hostRow = (name: string, desired: number) => ({ name, mode: "", running: desired, desired });

describe("StopStackDialog", () => {
  it("counts a swarm stack's replicated and global services, each plural-aware", async () => {
    services = [swarmRow("replicated"), swarmRow("global")];
    mount("swarm");
    expect(
      await screen.findByText(
        "Swarm scales its 1 replicated service to 0 and removes 1 global service. Networks, volumes, secrets and configs stay. Start brings everything back with the same images.",
      ),
    ).toBeTruthy();
  });

  it("says a swarm stack with no global service removes none", async () => {
    services = [
      { ...swarmRow("replicated"), name: "tracer_a" },
      { ...swarmRow("replicated"), name: "tracer_b" },
    ];
    mount("swarm");
    expect(
      await screen.findByText(
        "Swarm scales its 2 replicated services to 0 and removes 0 global services. Networks, volumes, secrets and configs stay. Start brings everything back with the same images.",
      ),
    ).toBeTruthy();
  });

  it("tells a standalone host's stack how many containers it stops, plural-aware", async () => {
    services = [hostRow("web", 2), hostRow("db", 1)];
    mount("standalone");
    expect(
      await screen.findByText(
        "This stops its 3 containers. Networks and volumes stay. Start brings them back.",
      ),
    ).toBeTruthy();
    cleanup();
    services = [hostRow("web", 1)];
    client.clear();
    mount("standalone");
    expect(
      await screen.findByText(
        "This stops its 1 container. Networks and volumes stay. Start brings them back.",
      ),
    ).toBeTruthy();
  });

  it("keeps the dialog open and says why when the server refuses, with no toast over it", async () => {
    services = [swarmRow("replicated")];
    stopAnswer = {
      status: 409,
      body: {
        type: "nothing_running",
        title: "Conflict",
        status: 409,
        detail: "Nothing is running.",
      },
    };
    const onOpenChange = mount("swarm");
    await screen.findByText(/Swarm scales its 1 replicated service/);

    fireEvent.click(screen.getByRole("button", { name: "Stop stack" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "Couldn't stop tracer. Nothing is running. What already stopped stays stopped.",
    );
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
    expect(toasts.error).not.toHaveBeenCalled();
    expect(screen.getByRole("heading", { name: "Stop tracer?" })).toBeTruthy();
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Stop stack" }).getAttribute("aria-disabled"),
      ).not.toBe("true"),
    );
  });

  it("closes when the server takes the stop, and toasts when the job is done", async () => {
    services = [swarmRow("replicated")];
    const onOpenChange = mount("swarm");
    await screen.findByText(/Swarm scales its 1 replicated service/);

    fireEvent.click(screen.getByRole("button", { name: "Stop stack" }));

    await waitFor(() => expect(posts).toEqual([`/api/env/${ENV}/stacks/s1/stop`]));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("Stopped tracer."));
  });
});
