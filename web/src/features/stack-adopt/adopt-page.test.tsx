// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AdoptPage } from "@/features/stack-adopt/adopt-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Adopt page (plan 08-13, STK-13, D-14, D-15): what adopting does, the note about environment values, and a
// footer whose Adopt stack sends the fingerprint the page read and nothing else.

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const ENV = "e1";
const PATH = `/api/env/${ENV}/unmanaged-stacks/legacy`;

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let role: "viewer" | "operator" | "admin";
let sent: Sent[];
let unmanaged: Record<string, unknown>;
// What the stack's own read does: answer, 404 (with the ID of the stack that holds the name), or wait.
let notFound: { managedStackId?: string } | null;
let readHeld: Promise<void> | null;
// What the adopt check answers: its body, or a refusal.
let checkAnswer: { status: number; body: unknown };
// What the adopt route answers, and whether it waits for release() first.
let adoptAnswer: { status: number; body: unknown };
let adoptHeld: Promise<void> | null;
let client: QueryClient;
const onAdopted = vi.fn();

const problem = (
  status: number,
  type: string,
  errors: { location: string; message: string }[] = [],
) => ({
  status,
  body: { type, title: "x", status, detail: "the server's sentence", errors },
});

beforeEach(() => {
  role = "admin";
  sent = [];
  unmanaged = {
    name: "legacy",
    target: "swarm",
    state: "running",
    servicesRunning: 2,
    servicesTotal: 2,
    services: [],
    foundBy: { label: "com.docker.stack.namespace", value: "legacy" },
    orphan: false,
    portainer: false,
    owner: "",
    adoptable: true,
    reason: "",
    fingerprint: "fp-1",
  };
  notFound = null;
  readHeld = null;
  checkAnswer = {
    status: 200,
    body: { fingerprint: "fp-1", clean: true, gaps: [], gapCount: 0, ownershipOnly: [] },
  };
  adoptAnswer = { status: 201, body: { id: "s9", name: "legacy", envId: ENV } };
  adoptHeld = null;
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  onAdopted.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === PATH) {
        if (readHeld) await readHeld;
        if (notFound) {
          return Response.json(
            {
              type: "unmanaged_stack_not_found",
              title: "Not Found",
              status: 404,
              detail: "gone",
              errors: notFound.managedStackId
                ? [{ location: "managedStackId", message: notFound.managedStackId }]
                : undefined,
            },
            { status: 404 },
          );
        }
        return Response.json(unmanaged);
      }
      if (path === `${PATH}/adopt-check` && req.method === "POST") {
        return Response.json(checkAnswer.body, { status: checkAnswer.status });
      }
      if (path === `${PATH}/adopt` && req.method === "POST") {
        if (adoptHeld) await adoptHeld;
        return Response.json(adoptAnswer.body, { status: adoptAnswer.status });
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
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AdoptPage envId={ENV} name="legacy" onAdopted={onAdopted} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The adoption is the one write: the check is a POST too, but it only reads.
const writes = () => sent.filter((s) => s.method !== "GET" && s.path !== `${PATH}/adopt-check`);
const checks = () => sent.filter((s) => s.method === "POST" && s.path === `${PATH}/adopt-check`);

// adoptButton is the footer's Adopt stack, found once the page has read the stack.
async function adoptButton() {
  const footer = await waitFor(() => {
    const el = document.querySelector("[data-slot=editor-footer]");
    if (!el) throw new Error("no footer yet");
    return el as HTMLElement;
  });
  return within(footer).getByRole("button", { name: /Adopt stack|Adopting/ });
}

// reasonedAdopt waits until the button carries the reason it is off for, and returns it. The button is rebuilt when the
// reason arrives, so it is found again.
async function reasonedAdopt() {
  await waitFor(async () => {
    const b = await adoptButton();
    expect(b.getAttribute("aria-describedby")).toBeTruthy();
  });
  return adoptButton();
}

// readyAdopt waits until the page has read the stack and the permissions, so the button is live.
async function readyAdopt() {
  const button = await adoptButton();
  await waitFor(() => expect(button.getAttribute("aria-disabled")).toBeNull());
  return button;
}

describe("the Adopt page: what it says", () => {
  it("opens with the title, what adopting does, and the note about environment values", async () => {
    mount();

    expect(await screen.findByRole("heading", { name: "Adopt legacy", level: 2 })).toBeTruthy();
    expect(
      await screen.findByText(
        "Bayplan builds a compose definition from what's running on Prod, checks it, and then manages the stack. Adopting doesn't change anything that is running.",
      ),
    ).toBeTruthy();
    const note = screen
      .getByText("Environment values are copied in as they are")
      .closest("[data-slot=alert]");
    expect(note?.getAttribute("role")).toBe("status");
    expect(
      screen.getByText(
        "That includes passwords. Move secret values to Variables after adopting. Secrets and configs that already exist stay as they are and are referenced as external.",
      ),
    ).toBeTruthy();
  });

  it("has the breadcrumb Stacks / name / Adopt and a footer with Cancel back to the stack", async () => {
    mount();

    const crumb = await screen.findByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Stacks" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
    expect(within(crumb).getByRole("link", { name: "legacy" }).getAttribute("href")).toBe(
      "/env/e1/unmanaged-stacks/legacy",
    );
    expect(within(crumb).getByText("Adopt").getAttribute("aria-current")).toBe("page");
    expect(
      await screen.findByText("Nothing changes on Prod until your first deploy."),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Cancel" }).getAttribute("href")).toBe(
      "/env/e1/unmanaged-stacks/legacy",
    );
  });

  it("adds the orphan note and the Portainer advice only when they apply", async () => {
    mount();
    await screen.findByRole("heading", { name: "Adopt legacy" });
    expect(screen.queryByText("This stack looks like it was created by Portainer")).toBeNull();
    cleanup();
    unmanaged = { ...unmanaged, orphan: true, portainer: true };
    mount();
    expect(
      await screen.findByText("This stack looks like it was created by Portainer"),
    ).toBeTruthy();
    expect(screen.getByText("Created by Bayplan (another instance or a lost record)")).toBeTruthy();
  });
});

describe("the Adopt page: adopting", () => {
  it("sends the fingerprint of the check it ran and nothing else, then hands over the new stack and says nothing changed", async () => {
    // The page's own read carries another fingerprint: only the check's counts.
    checkAnswer = {
      status: 200,
      body: { fingerprint: "fp-check", clean: true, gaps: [], gapCount: 0, ownershipOnly: [] },
    };
    mount();

    fireEvent.click(await readyAdopt());

    await waitFor(() => expect(onAdopted).toHaveBeenCalledWith("s9"));
    // The one request that is not a read is the adoption, and it carries the fingerprint alone (D-14).
    expect(writes()).toEqual([
      { method: "POST", path: `${PATH}/adopt`, body: { fingerprint: "fp-check" } },
    ]);
    expect(toasts.success).toHaveBeenCalledWith("Adopted legacy. Nothing changed on Prod.");
  });

  it("shows Adopting… and does not send a second request while the first is on its way", async () => {
    let release: () => void = () => undefined;
    adoptHeld = new Promise((resolve) => {
      release = resolve;
    });
    mount();

    const button = await readyAdopt();
    fireEvent.click(button);
    expect(await screen.findByRole("button", { name: "Adopting…" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Adopting…" }));
    fireEvent.click(screen.getByRole("button", { name: "Adopting…" }));
    await act(async () => {
      release();
      await adoptHeld;
    });

    await waitFor(() => expect(onAdopted).toHaveBeenCalledTimes(1));
    expect(writes()).toHaveLength(1);
  });

  it("is off, with the role it takes, for an Operator, and sends nothing", async () => {
    role = "operator";
    mount();

    const button = await reasonedAdopt();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);

    expect(toasts.info).toHaveBeenCalledWith("Needs Admin");
    expect(writes()).toEqual([]);
    expect(onAdopted).not.toHaveBeenCalled();
  });

  it("is off, with the server's reason, for Bayplan's own stack", async () => {
    unmanaged = {
      ...unmanaged,
      owner: "bayplan",
      adoptable: false,
      reason: "This is Bayplan's own stack.",
    };
    mount();

    expect(await screen.findAllByText("This is Bayplan's own stack.")).not.toHaveLength(0);
    const button = await reasonedAdopt();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);

    expect(toasts.info).toHaveBeenCalledWith("This is Bayplan's own stack.");
    expect(writes()).toEqual([]);
  });

  it("waits for the stack to be read before it can be pressed", async () => {
    let release: () => void = () => undefined;
    readHeld = new Promise((resolve) => {
      release = resolve;
    });
    mount();

    const button = await reasonedAdopt();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(toasts.info).toHaveBeenCalledWith("Checking legacy…");
    expect(writes()).toEqual([]);
    await act(async () => {
      release();
      await readHeld;
    });
    await waitFor(async () =>
      expect((await adoptButton()).getAttribute("aria-disabled")).toBeNull(),
    );
  });
});

describe("the Adopt page: what it says when the server refuses", () => {
  it("offers Check again when what runs changed, runs the check again, and never retries the adoption", async () => {
    adoptAnswer = problem(409, "adopt_stale");
    mount();
    fireEvent.click(await readyAdopt());

    expect(
      await screen.findByText(
        "What is running on Prod changed since the check. Run the check again before you adopt.",
      ),
    ).toBeTruthy();
    expect(writes()).toHaveLength(1);
    const before = checks().length;
    checkAnswer = {
      status: 200,
      body: { fingerprint: "fp-2", clean: true, gaps: [], gapCount: 0, ownershipOnly: [] },
    };
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));

    await waitFor(() => expect(checks().length).toBeGreaterThan(before));
    await waitFor(() => expect(screen.queryByText(/changed since the check/)).toBeNull());
    expect(writes()).toHaveLength(1);
    expect(onAdopted).not.toHaveBeenCalled();
  });

  it("says the stack is already managed and opens it", async () => {
    adoptAnswer = problem(409, "stack_already_managed", [{ location: "stackId", message: "s7" }]);
    mount();
    fireEvent.click(await readyAdopt());

    expect(await screen.findByText("legacy is already managed by Bayplan.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open stack" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s7",
    );
    expect(onAdopted).not.toHaveBeenCalled();
  });

  it("names the operation that holds the environment and links to it", async () => {
    adoptAnswer = problem(409, "operation_locked", [
      { location: "stack", message: "alpha" },
      { location: "job", message: "j3" },
      { location: "started", message: "2026-10-25T10:00:00Z" },
      { location: "stackId", message: "s3" },
    ]);
    mount();
    fireEvent.click(await readyAdopt());

    const lock = await waitFor(() => {
      const el = document.querySelector("[data-slot=operation-lock]");
      if (!el) throw new Error("no lock line yet");
      return el as HTMLElement;
    });
    expect(lock.textContent).toContain(
      "Another stack operation is running on Prod: alpha, started",
    );
    expect(within(lock).getByRole("link", { name: "Open it" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s3/deploys/j3",
    );
  });

  it("says why a stack could not be rebuilt, in the server's words", async () => {
    adoptAnswer = problem(422, "rebuild_failed");
    mount();
    fireEvent.click(await readyAdopt());

    expect(
      await screen.findByText("Bayplan couldn't rebuild legacy: the server's sentence"),
    ).toBeTruthy();
  });

  it("falls back to the generic line for anything else", async () => {
    adoptAnswer = { status: 500, body: { title: "x", status: 500 } };
    mount();
    fireEvent.click(await readyAdopt());

    expect(await screen.findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(writes()).toHaveLength(1);
  });
});

describe("the Adopt page: a stack that is gone", () => {
  it("says so and offers the managed stack that holds the name", async () => {
    notFound = { managedStackId: "s7" };
    mount();

    expect(await screen.findByRole("heading", { name: "This stack is gone" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open stack" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s7",
    );
    expect(screen.getByRole("link", { name: "Back to stacks" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
    expect(screen.queryByRole("button", { name: "Adopt stack" })).toBeNull();
  });
});
