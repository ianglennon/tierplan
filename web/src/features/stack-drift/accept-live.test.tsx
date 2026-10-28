// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useLocation } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
// When set, the read of the draft waits for it, so a test can act while the page is reading the draft.
let draftGate: Promise<void> | null;

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
  draftGate = null;
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
      if (path === `/api/env/${ENV}/stacks/s1/draft`) {
        if (draftGate) await draftGate;
        return Response.json(draft);
      }
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

  it("sends the version of the draft as it is now, after the confirm a saved draft asks for", async () => {
    draft = { ...draft, isDraft: true, version: 7 };
    mount();

    fireEvent.click(await ready());
    const dialog = await screen.findByRole("alertdialog", { name: "Replace the draft?" });
    expect(posts).toHaveLength(0);
    fireEvent.click(within(dialog).getByRole("button", { name: "Replace draft" }));

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]?.body).toEqual({ draftVersion: 7 });
  });

  it("sends one accept for two quick clicks, even while the draft is still being read", async () => {
    let release = () => {};
    draftGate = new Promise<void>((resolve) => {
      release = resolve;
    });
    mount();
    const accept = await ready();

    fireEvent.click(accept);
    fireEvent.click(accept);
    release();

    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/editor?pane=files"),
    );
    expect(posts.filter((p) => p.path === `/api/env/${ENV}/stacks/s1/drift/accept`)).toHaveLength(
      1,
    );
    expect(toasts.error).not.toHaveBeenCalled();
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

  it("says a stopped stack has nothing running to keep when the server refuses with stack_stopped", async () => {
    acceptAnswer = () =>
      Response.json(
        { type: "stack_stopped", title: "Conflict", status: 409, detail: "x" },
        { status: 409 },
      );
    mount();

    fireEvent.click(await ready());

    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith(
        "This stack is stopped, so there's nothing running to keep. Start it first.",
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

describe("Accept live when a saved draft exists", () => {
  beforeEach(() => {
    detail = { ...detail, hasDraft: true };
    draft = {
      isDraft: true,
      version: 5,
      savedBy: "ada",
      savedAt: "2026-10-19T10:30:00Z",
      baseSeq: 4,
      files: [],
      vars: [],
    };
  });

  // dialog waits for the confirm: the page learns the stack has a saved draft a moment after it opens.
  async function openConfirm() {
    fireEvent.click(await ready());
    return screen.findByRole("alertdialog", { name: "Replace the draft?" });
  }

  it("asks Replace the draft? and names who saved it and when, with Keep draft focused", async () => {
    mount();

    const dialog = await openConfirm();

    expect(dialog.textContent).toMatch(
      /This rebuilds the draft from what's running\. The saved draft by ada, .+, is replaced, including files and variables\. It can't be undone\./,
    );
    const keep = within(dialog).getByRole("button", { name: "Keep draft" });
    await waitFor(() => expect(document.activeElement).toBe(keep));
    expect(
      within(dialog).getByRole("button", { name: "Replace draft" }).getAttribute("data-variant"),
    ).toBe("destructive");
    expect(posts).toHaveLength(0);
  });

  it("sends nothing and keeps the draft on Keep draft", async () => {
    mount();
    const dialog = await openConfirm();

    fireEvent.click(within(dialog).getByRole("button", { name: "Keep draft" }));

    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(posts).toHaveLength(0);
    expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/drift");
  });

  it("rebuilds the draft on Replace draft, at the version the dialog showed, and opens the Editor", async () => {
    mount();
    const dialog = await openConfirm();

    fireEvent.click(within(dialog).getByRole("button", { name: "Replace draft" }));

    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/editor?pane=files"),
    );
    expect(posts.map((p) => p.body)).toEqual([{ draftVersion: 5 }]);
    expect(toasts.success).toHaveBeenCalledWith(
      "Rebuilt the draft of tracer from what's running. Review it, then deploy.",
    );
  });

  it("shows the Phase 7 conflict wording when the draft moved, and stays on the page", async () => {
    acceptAnswer = () =>
      Response.json(
        {
          type: "draft_conflict",
          title: "Conflict",
          status: 409,
          detail: "x",
          errors: [
            { location: "savedBy", message: "bob" },
            { location: "version", message: "6" },
          ],
        },
        { status: 409 },
      );
    mount();
    const dialog = await openConfirm();

    fireEvent.click(within(dialog).getByRole("button", { name: "Replace draft" }));

    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith(
        "bob saved this draft while you were editing. Reloading shows their version and drops your unsaved changes.",
      ),
    );
    expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/drift");
  });

  it("asks when the fresh read finds a saved draft although the cached stack says it has none", async () => {
    // A colleague saved a draft after the stack was last polled: the stack still reads hasDraft false.
    detail = { ...detail, hasDraft: false };
    mount();

    const dialog = await openConfirm();

    expect(dialog.textContent).toMatch(/The saved draft by ada, /);
    expect(posts).toHaveLength(0);
  });

  it("does not ask when the draft read finds no saved draft", async () => {
    detail = { ...detail, hasDraft: true };
    draft = { ...draft, isDraft: false, version: 0 };
    mount();

    fireEvent.click(await ready());

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]?.body).toEqual({ draftVersion: 0 });
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});

describe("the drift page's footer on a narrow screen", () => {
  it("holds the revision above the footer as a caption, and keeps the two buttons in it", async () => {
    mount();
    await ready();

    const footer = document.querySelector('[data-slot="editor-footer"]') as HTMLElement;
    const caption = screen.getAllByText("Revision #4").find((el) => !footer.contains(el));
    expect(caption).toBeTruthy();
    expect(caption?.className).toContain("md:hidden");
    expect(footer.contains(caption ?? null)).toBe(false);
    expect(within(footer).getByRole("button", { name: "Accept live into draft" })).toBeTruthy();
    expect(within(footer).getByRole("link", { name: "Redeploy to restore" })).toBeTruthy();
    // The one in the footer is the wide screens' and goes when the caption shows.
    expect(within(footer).getByText("Revision #4").className).toContain("max-md:hidden");
  });
});
