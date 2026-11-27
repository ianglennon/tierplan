// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { SelfRouteCard } from "@/features/ingress/self-route-card";
import { SettingsTab } from "@/features/ingress/settings-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

// TierPlan's own route on the Settings tab (D-16, D-17, D-11, UI-SPEC S1): drawn only where TierPlan runs, every state
// from one map, the snippet as the server sent it, the safeguards that keep the direct address in view, the reasons a
// reader cannot switch it, and the faster refetch while the route settles.

const ENV = "e1";
const OTHER = "e2";
const BASE = `/api/env/${ENV}/ingress`;
const DIRECT = "https://tierplan.example.org:7480";
const HOST = "tierplan.apps.example.org";

type Json = Record<string, unknown>;
type Write = { method: string; path: string; body: Json };

let envType: "swarm" | "standalone";
let ingressOn: boolean;
let selfRoute: Json | undefined;
let statusAnswer: (() => Promise<Response> | Response) | null;
let statusCalls: number;
let writes: Write[];
let previewAnswer: (body: Json) => Response;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

// route is the ingress status's selfRoute as the server sends it for the environment TierPlan runs in.
function route(over: Json = {}): Json {
  return {
    here: "yes",
    on: false,
    host: "",
    state: "off",
    reason: "",
    directAddress: DIRECT,
    kind: "swarm_service",
    attached: false,
    network: "tierplan-self",
    carveOut: false,
    ...over,
  };
}

// The switch-on preview's items, in the order the server sends them (D-17).
const ITEMS = [
  { code: "self_hostname", state: "ok", message: `${HOST} is free and under apps.example.org.` },
  {
    code: "self_certificate",
    state: "warning",
    reason: "no_certificate",
    message: `No certificate yet. TierPlan orders one when you turn the route on. ${HOST} isn't served until it is ready.`,
  },
  { code: "self_network", state: "ok", message: "TierPlan is on tierplan-self." },
  {
    code: "self_origin",
    state: "ok",
    message: `TierPlan will also accept requests from https://${HOST}. Your public URL isn't changed. Cookies are separate, so you sign in on each address.`,
  },
];

function statusBody(env: string): Json {
  return {
    enabled: ingressOn,
    enableFailed: false,
    disablingIncomplete: false,
    paused: false,
    pendingWipes: 0,
    redeploy: [],
    accessLogs: false,
    tokenDisabled: false,
    nodes: { expected: 1, enrolled: 1, ready: 1 },
    ...(env === ENV && selfRoute ? { selfRoute } : {}),
  };
}

function job(id: string, kind: string): Json {
  return {
    id,
    kind,
    state: "done",
    items: [],
    output: [],
    createdAt: "2026-11-27T10:00:00Z",
    finishedAt: "2026-11-27T10:00:05Z",
    startedBy: "u1",
  };
}

beforeEach(() => {
  vi.useFakeTimers({
    toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
  });
  setCsrfToken(null);
  envType = "swarm";
  ingressOn = true;
  selfRoute = route();
  statusAnswer = null;
  statusCalls = 0;
  writes = [];
  previewAnswer = (body) =>
    Response.json({ host: String(body.hostname ?? ""), directAddress: DIRECT, items: ITEMS });
  me = makeMe({ instanceAdmin: true });
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") {
        return Response.json({
          items: [
            { id: ENV, name: "Prod", envType },
            { id: OTHER, name: "Edge", envType },
          ],
        });
      }
      const env = path.split("/")[3] ?? "";
      if (path === `/api/env/${env}/ingress/status`) {
        if (env === ENV) statusCalls++;
        if (env === ENV && statusAnswer) return statusAnswer();
        return Response.json(statusBody(env));
      }
      if (path === `/api/env/${env}/ingress/vip`) {
        return Response.json(
          { title: "Problem", status: 404, type: "vip_not_enabled", detail: "No virtual IP." },
          { status: 404, headers: { "Content-Type": "application/problem+json" } },
        );
      }
      if (path === `/api/env/${env}/ingress/join-token`) {
        return Response.json({ exists: false, generation: 0, disabled: false });
      }
      if (req.method !== "GET") {
        const body = (await req.json().catch(() => ({}))) as Json;
        writes.push({ method: req.method, path, body });
        if (path === `${BASE}/self/preview`) return previewAnswer(body);
        if (path === `${BASE}/self/off/preview`) {
          return Response.json({
            host: HOST,
            directAddress: DIRECT,
            items: [
              {
                code: "self_origin_removed",
                state: "ok",
                message: `TierPlan stops accepting requests from https://${HOST}.`,
              },
              {
                code: "self_network_removed",
                state: "ok",
                message:
                  "The tierplan-self network is removed, because the port-80 carve-out doesn't need it.",
              },
            ],
          });
        }
        if (path === `${BASE}/self` && req.method === "POST") {
          return Response.json({ jobId: "j1" }, { status: 202 });
        }
        if (path === `${BASE}/self` && req.method === "DELETE") {
          return Response.json({ jobId: "j2" }, { status: 202 });
        }
      }
      if (path === `${BASE}/jobs/j1`) return Response.json(job("j1", "ingress_self_route_enable"));
      if (path === `${BASE}/jobs/j2`) return Response.json(job("j2", "ingress_self_route_disable"));
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  client.clear();
});

function wrap(children: React.ReactNode) {
  return (
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          {children}
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>
  );
}

function mountCard() {
  return render(wrap(<SelfRouteCard envId={ENV} />));
}

async function card(): Promise<HTMLElement> {
  return waitUnderFakeClock(() => screen.getByRole("region", { name: "TierPlan's own route" }));
}

async function button(name: string): Promise<HTMLButtonElement> {
  return waitUnderFakeClock(() => screen.getByRole("button", { name }) as HTMLButtonElement);
}

function itemHeadings(dialog: HTMLElement): string[] {
  const list = dialog.querySelector("ol") as HTMLElement;
  return Array.from(list.querySelectorAll(":scope > li")).map(
    (li) => (li.querySelector("span") as HTMLElement).textContent ?? "",
  );
}

describe("where the card is drawn", () => {
  it("draws the card only where TierPlan runs, second on that environment's Settings tab", async () => {
    render(
      wrap(
        <>
          <div data-testid={ENV}>
            <SettingsTab envId={ENV} />
          </div>
          <div data-testid={OTHER}>
            <SettingsTab envId={OTHER} />
          </div>
        </>,
      ),
    );
    const here = screen.getByTestId(ENV);
    const there = screen.getByTestId(OTHER);
    await waitUnderFakeClock(() => {
      expect(within(here).getByText("Virtual IP")).toBeTruthy();
      expect(within(there).getByText("Virtual IP")).toBeTruthy();
      expect(within(here).getByText("Join token")).toBeTruthy();
      expect(within(there).getByText("Join token")).toBeTruthy();
      expect(within(here).getByRole("button", { name: "Turn on own route" })).toBeTruthy();
    });
    const titles = (root: HTMLElement) =>
      within(root)
        .getAllByRole("heading", { level: 3 })
        .map((h) => h.textContent);
    expect(titles(here)).toEqual([
      "Ingress",
      "TierPlan's own route",
      "Virtual IP",
      "Proxy access log",
      "Join token",
    ]);
    expect(titles(there)).toEqual(["Ingress", "Virtual IP", "Proxy access log", "Join token"]);
    expect(within(here).getByRole("region", { name: "TierPlan's own route" }).id).toBe(
      "tierplan-own-route",
    );
    expect(within(there).queryByRole("region", { name: "TierPlan's own route" })).toBeNull();
  });
});

describe("turning the route on", () => {
  it("turns the route on through the review", async () => {
    mountCard();
    fireEvent.click(await button("Turn on own route"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("dialog", { name: "Turn on TierPlan's own route?" }),
    );
    expect(dialog.textContent).toContain(`Direct access on ${DIRECT} keeps working`);
    // The form first: nothing to check yet, nothing asked of the server.
    const check = within(dialog).getByRole("button", {
      name: "Check hostname",
    }) as HTMLButtonElement;
    expect(check.disabled).toBe(true);
    const field = within(dialog).getByLabelText("Hostname") as HTMLInputElement;
    expect(field.getAttribute("autocomplete")).toBe("off");
    expect(field.getAttribute("spellcheck")).toBe("false");
    expect(within(dialog).getByRole("link", { name: "Domains tab" }).getAttribute("href")).toBe(
      `/env/${ENV}/ingress/domains`,
    );
    expect(writes).toEqual([]);

    fireEvent.change(field, { target: { value: HOST } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Check hostname" }));
    await waitUnderFakeClock(() => expect(dialog.querySelector("ol")).toBeTruthy());
    expect(itemHeadings(dialog)).toEqual([
      "Hostname",
      "Domain and certificate",
      "Self network",
      "Origin",
    ]);
    expect(writes).toEqual([
      { method: "POST", path: `${BASE}/self/preview`, body: { hostname: HOST } },
    ]);

    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on own route" }));
    await waitUnderFakeClock(() =>
      expect(within(dialog).getByText("TierPlan's own route is on.")).toBeTruthy(),
    );
    expect(within(dialog).getByRole("heading", { name: "Turning own route on…" })).toBeTruthy();
    expect(writes.filter((w) => w.path === `${BASE}/self`)).toEqual([
      { method: "POST", path: `${BASE}/self`, body: { hostname: HOST } },
    ]);
  });
});
