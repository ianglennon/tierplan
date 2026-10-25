// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useLocation } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DriftPage } from "@/features/stack-drift/drift-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Accept live into draft (plan 08-24, STK-14, D-23): what runs becomes the stack's next draft, and the Editor opens on it.

const ENV = "e1";

type Json = Record<string, unknown>;

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

let detail: Json;
let role: "viewer" | "operator";
let draft: Json;
// How the accept route answers.
let acceptAnswer: () => Response;
let client: QueryClient;
// Every request but the reads of the stack, its drift and its draft: the accepts, with their bodies.
let posts: { path: string; body: Json }[];

function drifted(over: Json = {}): Json {
  return {
    state: "drifted",
    revision: 4,
    deployedAt: "2026-10-19T11:00:00Z",
    deployedBy: "bob",
    checkedAt: "2026-10-19T12:00:00Z",
    count: 1,
    services: [
      {
        name: "tracer_web",
        short: "web",
        change: "changed",
        current: { Mode: { Replicated: { Replicas: 1 } } },
        next: { Mode: { Replicated: { Replicas: 3 } } },
        hiddenChanges: [],
        settings: 1,
      },
    ],
    ...over,
  };
}

let checked: Json;

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
  role = "operator";
  checked = drifted();
  draft = {
    isDraft: false,
    version: 0,
    savedBy: "ada",
    savedAt: "2026-10-19T10:00:00Z",
    baseSeq: 0,
    files: [],
    vars: [],
  };
  acceptAnswer = () =>
    Response.json({
      version: 3,
      savedBy: "operator-1",
      savedAt: "2026-10-19T12:05:00Z",
      revision: 4,
      services: 1,
    });
  posts = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  for (const fn of Object.values(toasts)) fn.mockReset();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const path = url.pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(detail);
      if (path === `/api/env/${ENV}/stacks/s1/drift`) return Response.json(checked);
      if (path === `/api/env/${ENV}/stacks/s1/draft`) return Response.json(draft);
      if (path === `/api/env/${ENV}/stacks/s1/drift/accept` && req.method === "POST") {
        posts.push({ path, body: (await req.json()) as Json });
        return acceptAnswer();
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

// Where the router is, so a test can see the page the person was taken to.
function Where() {
  const location = useLocation();
  return <p data-testid="where">{`${location.pathname}${location.searchStr}`}</p>;
}

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter path="/env/e1/stacks/s1/drift">
        <TooltipProvider>
          <Where />
          <DriftPage envId={ENV} stackId="s1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// ready is the button once the page has learnt what the signed-in user may do: until then it is the held-back one.
async function ready(): Promise<HTMLElement> {
  return waitFor(() => {
    const button = screen.getByRole("button", { name: "Accept live into draft" });
    expect(button.getAttribute("aria-disabled")).not.toBe("true");
    return button;
  });
}

describe("Accept live into draft", () => {
  it("is an outline button in the drift page's footer, beside the accent Redeploy to restore", async () => {
    mount();

    const accept = await ready();
    expect(accept.closest('[data-slot="editor-footer"]')).toBeTruthy();
    expect(accept.getAttribute("data-variant")).toBe("outline");
    const restore = await screen.findByRole("link", { name: "Redeploy to restore" });
    expect(restore.closest('[data-slot="editor-footer"]')).toBe(
      accept.closest('[data-slot="editor-footer"]'),
    );
  });

  it("rebuilds the draft of a stack that has none, and opens the Editor on it with the toast", async () => {
    mount();

    fireEvent.click(await ready());

    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/editor?pane=files"),
    );
    expect(posts).toEqual([
      { path: `/api/env/${ENV}/stacks/s1/drift/accept`, body: { draftVersion: 0 } },
    ]);
    expect(toasts.success).toHaveBeenCalledWith(
      "Rebuilt the draft of tracer from what's running. Review it, then deploy.",
    );
    expect(toasts.error).not.toHaveBeenCalled();
  });

  it("sends the version of the draft as it is now", async () => {
    draft = { ...draft, isDraft: true, version: 7 };
    mount();

    fireEvent.click(await ready());

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]?.body).toEqual({ draftVersion: 7 });
  });

  it("says why the rebuild failed, and that the draft was not changed, and stays on the page", async () => {
    acceptAnswer = () =>
      Response.json(
        {
          type: "rebuild_failed",
          title: "Unprocessable Entity",
          status: 422,
          detail: "x",
          errors: [{ location: "reason", message: "no service of tracer is running" }],
        },
        { status: 422 },
      );
    mount();

    fireEvent.click(await ready());

    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith(
        "Couldn't rebuild the draft from what's running: no service of tracer is running. Your draft wasn't changed.",
      ),
    );
    expect(toasts.success).not.toHaveBeenCalled();
    expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/drift");
  });

  // heldBack waits for the button to say why it is held back: until the page has learnt what the user may do it says
  // Needs Operator, whoever they are.
  async function heldBack(reason: string): Promise<void> {
    await waitFor(() => {
      const button = screen.getByRole("button", { name: "Accept live into draft" });
      expect(button.getAttribute("aria-disabled")).toBe("true");
      expect(
        document.getElementById(button.getAttribute("aria-describedby") ?? "")?.textContent,
      ).toBe(reason);
    });
  }

  it("is held back for a Viewer with Needs Operator, and sends nothing when pressed", async () => {
    role = "viewer";
    mount();

    await heldBack("Needs Operator");
    fireEvent.click(screen.getByRole("button", { name: "Accept live into draft" }));
    expect(posts).toHaveLength(0);
  });

  it("is held back with the lock's reason while another operation runs on the environment", async () => {
    detail = {
      ...detail,
      lock: {
        stackId: "s2",
        stackName: "other",
        jobId: "j1",
        kind: "deploy",
        startedBy: "x",
        startedAt: "2026-10-19T12:00:00Z",
      },
    };
    mount();

    await heldBack("Another stack operation is running on Prod. Try again when it finishes.");
    expect(posts).toHaveLength(0);
  });

  it("is held back with No drift to restore. when what runs matches the revision", async () => {
    checked = drifted({ state: "none", count: 0, services: [] });
    mount();

    await heldBack("No drift to restore.");
    expect(posts).toHaveLength(0);
  });
});
