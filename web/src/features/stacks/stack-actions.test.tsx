// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useLocation } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { StackDetail } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackActions } from "@/features/stacks/stack-actions";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const toasts = vi.hoisted(() => ({
  success: vi.fn(),
  error: vi.fn(),
  info: vi.fn(),
}));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const ENV = "e1";

let role: "viewer" | "operator" | "admin" = "operator";
let posts: string[];
let jobState: string;
let jobItems: Array<{ id: string; state: string; message: string }>;
let services: Array<Record<string, unknown>>;
let answer: { status: number; body: unknown };
let client: QueryClient;

function stack(over: Partial<StackDetail> = {}) {
  return {
    id: "s1",
    envId: ENV,
    name: "tracer",
    target: "swarm",
    state: "running",
    ...over,
  } as StackDetail;
}

beforeEach(() => {
  role = "operator";
  posts = [];
  jobState = "done";
  jobItems = [];
  services = [
    { name: "tracer_web", mode: "replicated" },
    { name: "tracer_api", mode: "replicated" },
    { name: "tracer_agent", mode: "global" },
  ];
  answer = { status: 202, body: { jobId: "j1", recreates: false } };
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === `/api/env/${ENV}/stacks/s1/services`) {
        return Response.json({ items: services.map((s) => ({ ...s, running: 1, desired: 1 })) });
      }
      if (path === `/api/env/${ENV}/stacks/s1/removal-preview`) {
        return Response.json({
          services: ["tracer_web"],
          containers: [],
          networks: [],
          secrets: [],
          configs: [],
          volumes: [],
          externals: [],
          notReporting: [],
          hasDraft: false,
        });
      }
      if (req.method === "POST" && /\/stacks\/s1\/(stop|start)$/.test(path)) {
        posts.push(path);
        return Response.json(answer.body, { status: answer.status });
      }
      if (path === `/api/env/${ENV}/stacks/s1/deploys/j1`) {
        return Response.json({ id: "j1", state: jobState, items: jobItems });
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

// Shows where the router is, so a test can see that a start opened the console.
function Where() {
  return <p data-testid="where">{useLocation().pathname}</p>;
}

function mount(s: StackDetail) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackActions stack={s} />
          <Where />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("StackActions", () => {
  it("offers Stop stack while there is something to stop and Start stack while the stack is stopped", async () => {
    mount(stack({ state: "degraded" }));
    expect(await screen.findByRole("button", { name: "Stop stack…" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Start stack" })).toBeNull();
    cleanup();
    mount(stack({ state: "stopped" }));
    expect(await screen.findByRole("button", { name: "Start stack" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Stop stack…" })).toBeNull();
  });

  it.each([
    "deploying",
    "rolling_back",
    "starting",
    "stopping",
    "removing",
    "not_deployed",
  ] as const)("offers neither while the stack reads %s", async (state) => {
    mount(stack({ state }));
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Stop stack…" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Start stack" })).toBeNull();
  });

  it("stops the stack from a dialog that names what a swarm stop does in the stack's own counts", async () => {
    mount(stack());
    fireEvent.click(await screen.findByRole("button", { name: "Stop stack…" }));

    expect(await screen.findByRole("heading", { name: "Stop tracer?" })).toBeTruthy();
    expect(
      await screen.findByText(
        "Swarm scales its 2 replicated services to 0 and removes 1 global service. Networks, volumes, secrets and configs stay. Start brings everything back with the same images.",
      ),
    ).toBeTruthy();
    expect(posts).toEqual([]);

    fireEvent.click(screen.getByRole("button", { name: "Stop stack" }));

    await waitFor(() => expect(posts).toEqual(["/api/env/e1/stacks/s1/stop"]));
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("Stopped tracer."));
    expect(screen.queryByRole("heading", { name: "Stop tracer?" })).toBeNull();
  });

  it("starts the stack at once with no dialog and says Started when the job is done", async () => {
    mount(stack({ state: "stopped" }));

    fireEvent.click(await screen.findByRole("button", { name: "Start stack" }));

    await waitFor(() => expect(posts).toEqual(["/api/env/e1/stacks/s1/start"]));
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("Started tracer."));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("explains in a Tooltip that Start brings back the last deployed version and not the draft", async () => {
    mount(stack({ state: "stopped" }));

    fireEvent.focus(await screen.findByRole("button", { name: "Start stack" }));

    expect(
      (
        await screen.findAllByText(
          "Start brings back the last deployed version, not your saved draft.",
        )
      ).length,
    ).toBeGreaterThan(0);
  });

  it("says what failed when the job ends without finishing a start", async () => {
    jobState = "failed";
    jobItems = [{ id: "services", state: "failed", message: "The swarm rejected the service." }];
    mount(stack({ state: "stopped" }));

    fireEvent.click(await screen.findByRole("button", { name: "Start stack" }));

    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith(
        "Couldn't start tracer. The swarm rejected the service.",
      ),
    );
  });

  it("opens Update images in {name} from the header for a stack that runs", async () => {
    services = [
      {
        name: "tracer_web",
        short: "web",
        mode: "replicated",
        image: "nginx:1.27",
        pinned: `nginx:1.27@sha256:${"a".repeat(64)}`,
        pinState: "pinned",
      },
    ];
    mount(stack());

    fireEvent.click(await screen.findByRole("button", { name: "Update images…" }));

    expect(await screen.findByRole("heading", { name: "Update images in tracer" })).toBeTruthy();
    expect(await screen.findByRole("checkbox", { name: "Update the image of web" })).toBeTruthy();
    expect(posts).toEqual([]);
  });

  it.each(["not_deployed", "stopped"] as const)(
    "shows Update images… off with the reason while the stack reads %s",
    async (state) => {
      mount(stack({ state }));

      const update = await screen.findByRole("button", { name: "Update images…" });
      expect(update.getAttribute("aria-disabled")).toBe("true");
      fireEvent.click(update);

      expect(toasts.info).toHaveBeenCalledWith("Nothing is running to update.");
      expect(screen.queryByRole("heading", { name: "Update images in tracer" })).toBeNull();
    },
  );

  it("does not offer Update images… while an operation is under way, and shows a viewer it off", async () => {
    mount(stack({ state: "deploying" }));
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Update images…" })).toBeNull();
    cleanup();
    client.clear();

    role = "viewer";
    mount(stack());
    const update = await screen.findByRole("button", { name: "Update images…" });
    expect(update.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(update);
    expect(toasts.info).toHaveBeenCalledWith("Needs Operator");
  });

  it("shows a viewer the action it applies to off, with the role it takes, and sends nothing", async () => {
    role = "viewer";
    mount(stack());

    const stop = await screen.findByRole("button", { name: "Stop stack…" });
    expect(stop.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(stop);

    expect(toasts.info).toHaveBeenCalledWith("Needs Operator");
    expect(posts).toEqual([]);
    expect(screen.queryByRole("heading", { name: "Stop tracer?" })).toBeNull();
  });

  it("opens the console for a start that creates services again, and says nothing else", async () => {
    answer = { status: 202, body: { jobId: "j1", recreates: true } };
    mount(stack({ state: "stopped" }));

    fireEvent.click(await screen.findByRole("button", { name: "Start stack" }));

    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/deploys/j1"),
    );
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("says why when the server refuses a start, in a toast", async () => {
    answer = {
      status: 409,
      body: { type: "operation_locked", title: "Conflict", status: 409, detail: "busy" },
    };
    mount(stack({ state: "stopped" }));

    fireEvent.click(await screen.findByRole("button", { name: "Start stack" }));

    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith(
        "Couldn't start tracer. Another stack operation is running on this environment. Try again when it finishes.",
      ),
    );
  });

  it("says what failed when a stop's job ends without finishing", async () => {
    jobState = "failed";
    jobItems = [{ id: "services", state: "failed", message: "The swarm rejected the service." }];
    mount(stack());

    fireEvent.click(await screen.findByRole("button", { name: "Stop stack…" }));
    fireEvent.click(await screen.findByRole("button", { name: "Stop stack" }));

    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith(
        "Couldn't stop tracer. The swarm rejected the service. What already stopped stays stopped.",
      ),
    );
  });
});

describe("StackActions: the More menu", () => {
  async function openMore() {
    const trigger = await screen.findByRole("button", { name: "More actions for tracer" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
  }

  it("offers Copy stack name and, to an Env Admin, Remove… which opens the dialog", async () => {
    role = "admin";
    mount(stack());
    await openMore();

    expect(await screen.findByRole("menuitem", { name: "Copy stack name" })).toBeTruthy();
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove…" }));

    expect(await screen.findByRole("heading", { name: "Remove tracer?" })).toBeTruthy();
    expect(posts).toEqual([]);
  });

  it("shows an Operator Remove… off with Needs Admin and opens nothing", async () => {
    role = "operator";
    mount(stack());
    await openMore();

    const remove = await screen.findByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(remove);

    expect(toasts.info).toHaveBeenCalledWith("Needs Admin");
    expect(screen.queryByRole("heading", { name: "Remove tracer?" })).toBeNull();
  });

  it("keeps Remove… off while an operation is under way on the stack", async () => {
    role = "admin";
    mount(stack({ state: "deploying" }));
    await openMore();

    const remove = await screen.findByRole("menuitem", { name: "Remove…" });
    expect(remove.getAttribute("data-disabled")).not.toBeNull();
  });
});
