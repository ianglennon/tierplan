// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { stackQuery } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackJanitorCard } from "@/features/stacks/stack-janitor-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

type Json = Record<string, unknown>;

let role: "viewer" | "operator" | "admin";
let extras: string[];
let detail: Json;
let sent: Json[];
// What the stubbed server answers to the PUT instead of the usual; it may hold the answer back.
let putAnswer: (() => Response | Promise<Response>) | undefined;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  role = "admin";
  extras = [];
  detail = {
    id: "s1",
    name: "shop",
    target: "swarm",
    source: "inline",
    state: "running",
    envId: ENV,
    hasDraft: false,
    janitor: { excluded: false, declared: false },
  };
  sent = [];
  putAnswer = undefined;
  toasts.success.mockReset();
  toasts.error.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") {
        return Response.json(makeMe({ envs: { [ENV]: role }, extras: { [ENV]: extras } }));
      }
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (url.pathname === `/api/env/${ENV}/stacks/s1` && req.method === "GET") {
        return Response.json(detail);
      }
      if (url.pathname === `/api/env/${ENV}/stacks/s1/janitor` && req.method === "PUT") {
        const body = (await req.json()) as { excluded: boolean };
        sent.push(body);
        if (putAnswer) return putAnswer();
        const janitor = { ...(detail.janitor as Json), excluded: body.excluded };
        detail = { ...detail, janitor };
        return Response.json(janitor);
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

// The card as the Overview draws it: from the stack the page holds, which a change updates.
function Card() {
  const { data } = useQuery(stackQuery(ENV, "s1"));
  return data ? <StackJanitorCard envId={ENV} stack={data} /> : null;
}

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Card />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const toggle = (c: HTMLElement) =>
  within(c).getByRole("switch", { name: "Exclude this stack from the janitor" });

// The card once the person's permissions have come: until /api/me answers nothing is allowed, so the
// switch reads as unavailable for a moment, and a test must not act in that moment.
async function card() {
  const c = await screen.findByRole("region", { name: "Janitor" });
  await waitFor(() => expect(client.getQueryState(["me"])?.status).toBe("success"));
  return c;
}

describe("the stack's Janitor card", () => {
  it("explains the switch and shows it off", async () => {
    mount();
    const c = await card();
    expect(c.id).toBe("janitor");
    expect(toggle(c).getAttribute("aria-checked")).toBe("false");
    expect(
      within(c).getByText(
        "The janitor then leaves everything this stack owns alone: its containers, networks and images. Turn this on for a stack whose old containers you want to keep.",
      ),
    ).toBeTruthy();
    expect(within(c).queryByText("Excluded")).toBeNull();
  });

  it("excludes the stack at once: PUT, a spinner while it is out, the toast and the Excluded badge", async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    putAnswer = async () => {
      await gate;
      return Response.json({ excluded: true, declared: false });
    };
    mount();
    const c = await card();
    fireEvent.click(toggle(c));

    await waitFor(() => expect(sent).toEqual([{ excluded: true }]));
    expect(within(c).getByRole("status", { name: "Saving" })).toBeTruthy();
    expect(toggle(c).hasAttribute("disabled")).toBe(true);
    expect(toggle(c).getAttribute("aria-checked")).toBe("true");
    release();

    await waitFor(() =>
      expect(toasts.success).toHaveBeenCalledWith("shop is excluded from the janitor."),
    );
    await waitFor(() => expect(within(c).getByText("Excluded")).toBeTruthy());
    expect(within(c).queryByRole("status", { name: "Saving" })).toBeNull();
    expect(toggle(c).getAttribute("aria-checked")).toBe("true");
  });

  it("includes it again", async () => {
    detail = { ...detail, janitor: { excluded: true, declared: false } };
    mount();
    const c = await card();
    expect(toggle(c).getAttribute("aria-checked")).toBe("true");
    expect(within(c).getByText("Excluded")).toBeTruthy();
    fireEvent.click(toggle(c));

    await waitFor(() => expect(sent).toEqual([{ excluded: false }]));
    await waitFor(() =>
      expect(toasts.success).toHaveBeenCalledWith("shop is included in the janitor again."),
    );
    await waitFor(() => expect(within(c).queryByText("Excluded")).toBeNull());
  });

  it("puts the switch back and announces it when the change is refused", async () => {
    putAnswer = () => Response.json({ type: "x", title: "x", status: 500 }, { status: 500 });
    mount();
    const c = await card();
    fireEvent.click(toggle(c));

    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith("Couldn't change the janitor setting for shop."),
    );
    await waitFor(() => expect(toggle(c).getAttribute("aria-checked")).toBe("false"));
    expect(toggle(c).hasAttribute("disabled")).toBe(false);
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("shows a compose file's exclusion as on and not to be changed here", async () => {
    detail = { ...detail, janitor: { excluded: false, declared: true } };
    mount();
    const c = await card();
    expect(toggle(c).getAttribute("aria-checked")).toBe("true");
    expect(toggle(c).getAttribute("aria-disabled")).toBe("true");
    expect(
      within(c).getByText(
        "This stack's compose file sets tierplan.keep=true, so the janitor already skips it.",
      ),
    ).toBeTruthy();
    expect(
      document.getElementById(toggle(c).getAttribute("aria-describedby") ?? "")?.textContent,
    ).toBe(
      "This stack's compose file sets tierplan.keep=true, so the janitor already skips it. Remove the label to change this.",
    );
    fireEvent.click(toggle(c));
    expect(sent).toEqual([]);
  });

  it("keeps the declared switch on even when the switch is also set", async () => {
    detail = { ...detail, janitor: { excluded: true, declared: true } };
    mount();
    const c = await card();
    expect(toggle(c).getAttribute("aria-checked")).toBe("true");
    expect(toggle(c).getAttribute("aria-disabled")).toBe("true");
  });

  it("is off for an Operator without janitor.policy, and says what it takes", async () => {
    role = "operator";
    mount();
    const c = await card();
    expect(toggle(c).getAttribute("aria-disabled")).toBe("true");
    expect(
      document.getElementById(toggle(c).getAttribute("aria-describedby") ?? "")?.textContent,
    ).toBe("Needs the janitor policy permission on Prod");
    fireEvent.click(toggle(c));
    expect(sent).toEqual([]);
  });

  it("works for an Operator who holds janitor.policy", async () => {
    role = "operator";
    extras = ["janitor.policy"];
    mount();
    const c = await card();
    expect(toggle(c).getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(toggle(c));
    await waitFor(() => expect(sent).toEqual([{ excluded: true }]));
  });
});
