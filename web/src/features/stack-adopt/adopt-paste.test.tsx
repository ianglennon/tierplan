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

// Adopting from the original compose file (plan 08-15, STK-13, D-12): the paste source, its check against what runs,
// the variables of the file, and what is kept of any of it (nothing, beyond the page's own state).

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const ENV = "e1";
const PATH = `/api/env/${ENV}/unmanaged-stacks/legacy`;
// The check waits this long after the last change; a test waits on what it asserts, with room for it.
const SETTLE = { timeout: 5000 };

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
// What a check of a pasted file answers: a function of the body it was sent.
let pasteAnswer: (body: Record<string, unknown>) => { status: number; body: unknown };
let adoptAnswer: { status: number; body: unknown };
let client: QueryClient;
const onAdopted = vi.fn();

const FILE = "# the stack\nservices:\n  web:\n    image: nginx:1.27\n";

const spec = (replicas: number) => ({
  Name: "legacy_web",
  Mode: { Replicated: { Replicas: replicas } },
  TaskTemplate: { ContainerSpec: { Image: "nginx:1.27" } },
});

const checkBody = (over: Record<string, unknown> = {}) => ({
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
  ...over,
});

// matching is a pasted file that differs from what runs in nothing but the ownership labels.
const matching = (over: Record<string, unknown> = {}) =>
  checkBody({ source: "paste", fingerprint: "fp-paste", matches: true, ...over });

// differing is a pasted file whose first deploy would change the replicas of web (two settings: replicas and a label).
const differing = (over: Record<string, unknown> = {}) =>
  checkBody({
    source: "paste",
    fingerprint: "fp-paste",
    clean: false,
    gapCount: 1,
    gaps: [
      {
        name: "legacy_web",
        short: "web",
        change: "changed",
        current: spec(2),
        next: spec(3),
        hiddenChanges: [],
        settings: 2,
      },
    ],
    ...over,
  });

beforeEach(() => {
  sent = [];
  pasteAnswer = () => ({ status: 200, body: matching() });
  adoptAnswer = { status: 201, body: { id: "s9", name: "legacy", envId: ENV } };
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
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === PATH) {
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
      if (path === `${PATH}/adopt-check` && req.method === "POST") {
        if (body?.source === "paste") {
          const a = pasteAnswer(body);
          return Response.json(a.body, { status: a.status });
        }
        return Response.json(checkBody());
      }
      if (path === `${PATH}/adopt` && req.method === "POST") {
        return Response.json(adoptAnswer.body, { status: adoptAnswer.status });
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

function mount(name = "legacy") {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AdoptPage envId={ENV} name={name} onAdopted={onAdopted} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// Only the requests to these exact paths count: the SPA sends others (such as the recent environments) on its own.
const pasteChecks = () =>
  sent.filter(
    (s) => s.method === "POST" && s.path === `${PATH}/adopt-check` && s.body?.source === "paste",
  );
const adoptRequests = () => sent.filter((s) => s.method === "POST" && s.path === `${PATH}/adopt`);

function adoptButton() {
  const footer = document.querySelector("[data-slot=editor-footer]") as HTMLElement;
  return within(footer).getByRole("button", { name: /Adopt stack|Adopting/ });
}

// reasonOf is the sentence the Adopt button is off for, which is its accessible description; empty when it is on.
function reasonOf(): string {
  const id = adoptButton().getAttribute("aria-describedby");
  return id ? (document.getElementById(id)?.textContent ?? "") : "";
}

// startFromPaste opens the page and chooses "I have the original compose file".
async function startFromPaste(name = "legacy") {
  mount(name);
  const radio = await screen.findByRole("radio", { name: /I have the original compose file/ });
  fireEvent.click(radio);
  return screen.findByLabelText("Original compose file");
}

const typeFile = (box: HTMLElement, text: string) =>
  fireEvent.change(box, { target: { value: text } });

// theSourceCard is the card "Start from".
function theSourceCard() {
  return screen.getByRole("heading", { name: "Start from" }).closest("section") as HTMLElement;
}

describe("Start from", () => {
  it("offers the rebuild as the default and the original file as the other choice, with their helpers", async () => {
    mount();

    const group = await screen.findByRole("radiogroup", { name: "Start from" });
    const rebuild = within(group).getByRole("radio", { name: /Rebuild from the running services/ });
    const paste = within(group).getByRole("radio", { name: /I have the original compose file/ });
    expect(rebuild.getAttribute("aria-checked")).toBe("true");
    expect(paste.getAttribute("aria-checked")).toBe("false");
    expect(screen.getByText("Bayplan builds the compose file from what's running.")).toBeTruthy();
    expect(screen.getByText("Paste it and Bayplan checks it against what's running.")).toBeTruthy();
    expect(screen.queryByLabelText("Original compose file")).toBeNull();
    expect(pasteChecks()).toEqual([]);
  });

  it("shows the empty paste field with its placeholder, and Adopt says what to do first", async () => {
    const box = (await startFromPaste()) as HTMLTextAreaElement;

    expect(box.getAttribute("placeholder")).toBe("Paste compose.yaml here");
    expect(box.value).toBe("");
    await waitFor(() =>
      expect(reasonOf()).toBe("Paste a compose file first, or rebuild from the running services."),
    );
    expect(pasteChecks()).toEqual([]);
  });
});

describe("a pasted file that matches", () => {
  it("is checked after the pause, says it matches, and is adopted as written with the fingerprint of that check", async () => {
    const box = await startFromPaste();

    typeFile(box, FILE);

    expect(
      await within(theSourceCard()).findByText("Matches what's running.", {}, SETTLE),
    ).toBeTruthy();
    expect(pasteChecks().map((r) => r.body)).toEqual([
      { source: "paste", compose: FILE, vars: [] },
    ]);
    await waitFor(() => expect(reasonOf()).toBe(""));

    fireEvent.click(adoptButton());

    await waitFor(() => expect(onAdopted).toHaveBeenCalledWith("s9"));
    // The text goes as it was typed, byte for byte, with the fingerprint of the check and no verdict.
    expect(adoptRequests().map((r) => r.body)).toEqual([
      { fingerprint: "fp-paste", source: "paste", compose: FILE, vars: [] },
    ]);
  });
});

describe("a pasted file that differs", () => {
  it("says in how many settings, may be adopted as it is, and offers the rebuild instead", async () => {
    pasteAnswer = () => ({ status: 200, body: differing() });
    const box = await startFromPaste();

    typeFile(box, FILE);

    const card = theSourceCard();
    expect(
      await within(card).findByText("Differs from what's running in 2 settings.", {}, SETTLE),
    ).toBeTruthy();
    // Differences never block (D-20): Adopt is on, and the check card lists them.
    await waitFor(() => expect(reasonOf()).toBe(""));
    expect(screen.getByText("1 difference will change on the first deploy.")).toBeTruthy();

    fireEvent.click(within(card).getByRole("button", { name: "Show differences" }));
    const gaps = screen.getByRole("region", { name: "Differences by service" });
    await waitFor(() => expect(document.activeElement).toBe(gaps));
    expect(within(gaps).getByText("web")).toBeTruthy();

    fireEvent.click(within(card).getByRole("button", { name: "Use the rebuild instead" }));

    await waitFor(() =>
      expect(
        screen
          .getByRole("radio", { name: /Rebuild from the running services/ })
          .getAttribute("aria-checked"),
      ).toBe("true"),
    );
    expect(screen.queryByLabelText("Original compose file")).toBeNull();
    await waitFor(() => expect(reasonOf()).toBe(""));
    fireEvent.click(adoptButton());
    await waitFor(() => expect(onAdopted).toHaveBeenCalledWith("s9"));
    expect(adoptRequests().map((r) => r.body)).toEqual([
      { fingerprint: "fp-1", source: "rebuild" },
    ]);
  });
});
