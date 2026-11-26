// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { IngressPageAlerts } from "@/features/ingress/page-alerts";
import "@/i18n";
import { makeMe } from "@/test-me";

// The page-level alerts (UI-SPEC S1): the environment cannot be reached, and ingress is paused after a restore
// with Resume ingress for an Admin.

const ENV = "e1";
const UNREACHABLE =
  "TierPlan can't reach this environment just now. What you see may be out of date.";
const PAUSED =
  "Ingress is paused after a restore. Nothing is written to the swarm until an Admin resumes it.";

type Json = Record<string, unknown>;

let status: Json;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;
let posts: Request[];
let resumeAnswer: () => Response;

function statusBody(over: Json = {}): Json {
  return {
    accessLogs: false,
    disablingIncomplete: false,
    enableFailed: false,
    enabled: true,
    lastEnsure: { at: "", class: "" },
    nodes: { enrolled: 0, expected: 0, ready: 0 },
    objects: { network: true, secret: false, service: true },
    objectsKnown: true,
    paused: false,
    pendingWipes: 0,
    redeploy: [],
    tokenDisabled: false,
    ...over,
  };
}

beforeEach(() => {
  setCsrfToken(null);
  status = statusBody();
  me = makeMe({ envs: { [ENV]: "admin" } });
  posts = [];
  resumeAnswer = () => Response.json({ resumed: true });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json(status);
      if (path === `/api/env/${ENV}/ingress/resume` && req.method === "POST") {
        posts.push(req.clone());
        const answer = resumeAnswer();
        if (answer.ok) status = statusBody();
        return answer;
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
      <TooltipProvider>
        <IngressPageAlerts envId={ENV} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

describe("the Ingress page's alerts", () => {
  it("draws nothing for an environment that is reachable and not paused", async () => {
    mount();
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.length).toBeGreaterThan(0));
    await client.refetchQueries({ queryKey: ["env", ENV, "ingress"] });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("draws nothing when the status cannot be read, since each tab reports its own read", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json({ title: "boom", status: 500 }, { status: 500 })),
    );
    mount();
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.length).toBeGreaterThan(0));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("draws one neutral alert when the objects are unknown", async () => {
    status = statusBody({ objectsKnown: false });
    mount();
    expect(await screen.findByText(UNREACHABLE)).toBeTruthy();
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Resume ingress" })).toBeNull();
  });

  it("draws the same alert when the last pass ended unreachable", async () => {
    status = statusBody({ lastEnsure: { at: "2026-11-26T10:00:00Z", class: "unreachable" } });
    mount();
    expect(await screen.findByText(UNREACHABLE)).toBeTruthy();
  });

  it("draws the paused alert with Resume ingress, which an Admin presses once", async () => {
    status = statusBody({ paused: true });
    mount();
    expect(await screen.findByText(PAUSED)).toBeTruthy();
    const button = screen.getByRole("button", { name: "Resume ingress" });
    expect(button.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(button);
    fireEvent.click(button);
    await waitFor(() => expect(screen.queryByText(PAUSED)).toBeNull());
    expect(posts).toHaveLength(1);
    expect(new URL(posts[0]?.url ?? "").pathname).toBe(`/api/env/${ENV}/ingress/resume`);
  });

  it("draws Resume ingress disabled for a reader, with the reason, and posts nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    status = statusBody({ paused: true });
    mount();
    expect(await screen.findByText(PAUSED)).toBeTruthy();
    const button = screen.getByRole("button", { name: "Resume ingress" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(button.className).toContain("cursor-not-allowed");
    expect(screen.getByText("Needs Admin", { selector: "span.sr-only" })).toBeTruthy();
    fireEvent.click(button);
    expect(posts).toHaveLength(0);
    expect(screen.getByText(PAUSED)).toBeTruthy();
  });

  it("shows the server's words under the button when the resume is refused", async () => {
    status = statusBody({ paused: true });
    resumeAnswer = () =>
      Response.json(
        { title: "Conflict", status: 409, detail: "Another resume is already running." },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Resume ingress" }));
    const refusal = await screen.findByText("Another resume is already running.");
    expect(refusal.closest("[role=alert]")?.className).toContain("text-destructive");
    expect(screen.getByText(PAUSED)).toBeTruthy();
    // The button is pressable again.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Resume ingress" }).hasAttribute("disabled")).toBe(
        false,
      ),
    );
  });

  it("draws both alerts, each once, when the environment is paused and unreachable", async () => {
    status = statusBody({ paused: true, objectsKnown: false });
    mount();
    expect(await screen.findByText(PAUSED)).toBeTruthy();
    expect(screen.getByText(UNREACHABLE)).toBeTruthy();
    const alerts = screen.getAllByRole("alert");
    expect(alerts).toHaveLength(2);
    expect(within(alerts[0] as HTMLElement).getByText(UNREACHABLE)).toBeTruthy();
  });
});
