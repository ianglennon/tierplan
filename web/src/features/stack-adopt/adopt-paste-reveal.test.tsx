// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AdoptPage } from "@/features/stack-adopt/adopt-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The paste source compares the person's own file with the values that run, so the server answers it only to a caller
// who holds secrets.reveal on the environment (T-08-52). The page offers it to nobody else, and says why.

const ENV = "e1";
const ADOPT_CHECK = `/api/env/${ENV}/unmanaged-stacks/legacy/adopt-check`;

let me: ReturnType<typeof makeMe>;
let checkAnswer: { status: number; body: unknown };
let sent: { method: string; path: string; body?: Record<string, unknown> }[];
let client: QueryClient;

const rebuildCheck = {
  fingerprint: "fp-1",
  source: "rebuild",
  clean: true,
  matches: false,
  blocked: false,
  gaps: [],
  gapCount: 0,
  ownershipOnly: [],
  objects: [],
  counts: { networks: 0, volumes: 0, secrets: 0, configs: 0 },
  policy: { kinds: [], entries: [] },
  portainer: false,
  orphan: false,
  notes: [],
  problems: [],
  variables: [],
};

beforeEach(() => {
  sent = [];
  checkAnswer = { status: 200, body: rebuildCheck };
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
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/unmanaged-stacks/legacy`) {
        return Response.json({
          name: "legacy",
          target: "swarm",
          state: "running",
          servicesRunning: 1,
          servicesTotal: 1,
          services: [],
          foundBy: { label: "com.docker.stack.namespace", value: "legacy" },
          orphan: false,
          portainer: false,
          owner: "",
          adoptable: true,
          reason: "",
          fingerprint: "fp-1",
        });
      }
      if (path === ADOPT_CHECK && req.method === "POST") {
        if (body?.source === "paste") {
          return Response.json(checkAnswer.body, { status: checkAnswer.status });
        }
        return Response.json(rebuildCheck);
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

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AdoptPage envId={ENV} name="legacy" onAdopted={vi.fn()} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// Only the requests to this exact path count: the SPA sends others on its own.
const pasteChecks = () =>
  sent.filter((s) => s.method === "POST" && s.path === ADOPT_CHECK && s.body?.source === "paste");

describe("the paste source without secrets.reveal", () => {
  it.each([
    ["a Viewer", "viewer" as const],
    ["an Operator without the extra", "operator" as const],
  ])("is off for %s, with its reason, and the rebuild stays the choice", async (_who, role) => {
    me = makeMe({ envs: { [ENV]: role } });
    mount();

    const group = await screen.findByRole("radiogroup", { name: "Start from" });
    const paste = within(group).getByRole("radio", { name: /I have the original compose file/ });
    await waitFor(() => expect(paste.hasAttribute("disabled")).toBe(true));
    expect(within(group).getByText("Needs the reveal secrets permission")).toBeTruthy();
    const rebuild = within(group).getByRole("radio", { name: /Rebuild from the running services/ });
    expect(rebuild.hasAttribute("disabled")).toBe(false);
    expect(rebuild.getAttribute("aria-checked")).toBe("true");

    fireEvent.click(paste);
    expect(rebuild.getAttribute("aria-checked")).toBe("true");
    expect(screen.queryByLabelText("Original compose file")).toBeNull();
    expect(pasteChecks()).toEqual([]);
  });

  it("is on for an Env Admin, who holds secrets.reveal", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    mount();

    const paste = await screen.findByRole("radio", { name: /I have the original compose file/ });
    expect(paste.hasAttribute("disabled")).toBe(false);
    expect(screen.queryByText("Needs the reveal secrets permission")).toBeNull();
  });
});
