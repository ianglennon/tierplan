// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { SelfRouteCard, selfRouteHeadline } from "@/features/ingress/self-route-card";
import { SettingsTab } from "@/features/ingress/settings-tab";
import { routeHeadline } from "@/features/stacks/route-state";
import i18n from "@/i18n";
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
let enableJob: Json;
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
  enableJob = job("j1", "ingress_self_route_enable");
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
      if (path === `${BASE}/jobs/j1`) return Response.json(enableJob);
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

// region is the card as it is now, loading or not.
async function region(): Promise<HTMLElement> {
  return waitUnderFakeClock(() => screen.getByRole("region", { name: "TierPlan's own route" }));
}

// card is the card once the status has been read.
async function card(): Promise<HTMLElement> {
  return waitUnderFakeClock(() => {
    const found = screen.getByRole("region", { name: "TierPlan's own route" });
    expect(found.querySelector('[data-slot="skeleton"]')).toBeNull();
    return found;
  });
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

// UI-SPEC S1b: a malformed value gets an inline error under the Hostname field, in plain text. The server judges the
// name; its sentence for ingress_host_invalid is drawn under the field as sent. Other refusals stay above the form.
describe("a malformed hostname (UI-SPEC S1b)", () => {
  const BAD = "tier plan.example.org";
  const INVALID = `The hostname “${BAD}” is not valid: the label “tier plan” may hold only letters, digits and hyphens.`;

  function refusing(reason: string, message: string) {
    previewAnswer = (body) =>
      Response.json({
        host: String(body.hostname ?? ""),
        directAddress: DIRECT,
        items: [{ code: "self_hostname", state: "refused", reason, message }, ...ITEMS.slice(1)],
      });
  }

  async function checkHost(
    value: string,
  ): Promise<{ dialog: HTMLElement; field: HTMLInputElement }> {
    mountCard();
    fireEvent.click(await button("Turn on own route"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("dialog", { name: "Turn on TierPlan's own route?" }),
    );
    const field = within(dialog).getByLabelText("Hostname") as HTMLInputElement;
    fireEvent.change(field, { target: { value } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Check hostname" }));
    await waitUnderFakeClock(() => expect(dialog.querySelector("ol")).toBeTruthy());
    return { dialog, field };
  }

  function inlineError(dialog: HTMLElement): HTMLElement | null {
    return dialog.querySelector<HTMLElement>("[data-slot=field-error]");
  }

  it("draws the server's sentence under the field, tied to it, and marks the field invalid", async () => {
    refusing("ingress_host_invalid", INVALID);
    const { dialog, field } = await checkHost(BAD);
    const error = inlineError(dialog);
    expect(error?.textContent).toBe(INVALID);
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(field.getAttribute("aria-describedby")?.split(" ")).toContain(error?.id);
    // Plain text: no link, no list, no alert box.
    expect(error?.querySelector("a, ul, [data-slot=alert]")).toBeNull();
    // The field stays in the form, below what the server refused with.
    expect(
      field.compareDocumentPosition(error as Node) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("clears the inline error once the hostname is edited", async () => {
    refusing("ingress_host_invalid", INVALID);
    const { dialog, field } = await checkHost(BAD);
    expect(inlineError(dialog)).toBeTruthy();
    fireEvent.change(field, { target: { value: HOST } });
    expect(inlineError(dialog)).toBeNull();
    expect(field.getAttribute("aria-invalid")).not.toBe("true");
  });

  it("keeps a refusal of a well-formed name above the form, not under the field", async () => {
    refusing("ingress_host_taken", `${HOST} is already held by the stack web.`);
    const { dialog, field } = await checkHost(HOST);
    expect(inlineError(dialog)).toBeNull();
    expect(field.getAttribute("aria-invalid")).not.toBe("true");
    expect(dialog.textContent).toContain(`${HOST} is already held by the stack web.`);
  });
});

describe("the switch-on's Done alert when TierPlan is left off the network (UI-SPEC S1b)", () => {
  function leftOff(attached: string): Json {
    return {
      ...job("j1", "ingress_self_route_enable"),
      items: [
        {
          id: "self_network",
          label: "Self network",
          state: "done",
          kind: "step",
          parent: "",
          code: "",
          message: "",
          detail: "",
          result: { network: "tierplan-self", attached },
        },
      ],
    };
  }

  async function turnOn(): Promise<HTMLElement> {
    mountCard();
    fireEvent.click(await button("Turn on own route"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("dialog", { name: "Turn on TierPlan's own route?" }),
    );
    fireEvent.change(within(dialog).getByLabelText("Hostname"), { target: { value: HOST } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Check hostname" }));
    await waitUnderFakeClock(() => expect(dialog.querySelector("ol")).toBeTruthy());
    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on own route" }));
    await waitUnderFakeClock(() =>
      expect(within(dialog).getByText("TierPlan's own route is on.")).toBeTruthy(),
    );
    return dialog;
  }

  const note = "Add the lines on the card to your stack file to start serving.";

  it("adds where the lines are on a swarm when the job left TierPlan off tierplan-self", async () => {
    enableJob = leftOff("false");
    const dialog = await turnOn();
    expect(within(dialog).getByText(note)).toBeTruthy();
  });

  it("adds nothing once TierPlan is on the network", async () => {
    enableJob = leftOff("true");
    const dialog = await turnOn();
    expect(within(dialog).queryByText(note)).toBeNull();
  });

  it("adds nothing on a standalone host, which connects TierPlan itself", async () => {
    selfRoute = route({ kind: "standalone" });
    envType = "standalone";
    enableJob = leftOff("false");
    const dialog = await turnOn();
    expect(within(dialog).queryByText(note)).toBeNull();
  });
});

const STACK_LINES =
  "services:\n  tierplan:\n    networks:\n      - tierplan-self\nnetworks:\n  tierplan-self:\n    external: true\n";
const COMMAND = "docker service update --network-add tierplan-self tierplan_tierplan";

function words(key: string): string {
  return i18n.t(key, { ns: key.split(".")[0] });
}

// variantOf reads an Alert's variant from the classes it draws with.
function variantOf(alert: HTMLElement): "warning" | "destructive" | "neutral" {
  if (alert.className.includes("border-status-warning")) return "warning";
  if (alert.className.includes("text-destructive")) return "destructive";
  return "neutral";
}

// statusBlock is the card's first part: the dot with its word, then the detail line when there is one.
function statusBlock(region: HTMLElement): HTMLElement {
  return region.querySelector('[data-slot="card-content"] > div') as HTMLElement;
}

describe("every state", () => {
  const on = { on: true, host: HOST, attached: true };
  const cases: { name: string; route: Json; word: string; detail: string | null }[] = [
    { name: "off", route: {}, word: "Off", detail: null },
    { name: "serving", route: { ...on, state: "serving" }, word: "Serving", detail: null },
    {
      name: "pending",
      route: { ...on, state: "pending" },
      word: "Rolling out",
      detail: "The proxies are still picking up the route.",
    },
    {
      name: "waiting_certificate with a reason",
      route: { ...on, state: "waiting_certificate", reason: "no_certificate" },
      word: "Waiting for certificate",
      detail: `No certificate has been issued for this hostname yet. Nothing is served on ${HOST} until it is ready. ${DIRECT} keeps working.`,
    },
    {
      name: "waiting_certificate with no reason",
      route: { ...on, state: "waiting_certificate", reason: "" },
      word: "Waiting for certificate",
      detail: `Nothing is served on ${HOST} until it is ready. ${DIRECT} keeps working.`,
    },
    {
      name: "self_not_attached on a swarm",
      route: { ...on, attached: false, state: "self_not_attached", reason: "self_not_attached" },
      word: "Not attached",
      detail: `TierPlan's own service isn't on the tierplan-self network yet. Nothing is served on ${HOST} until it is. Add the lines below to your stack file and deploy it again.`,
    },
    {
      name: "self_not_attached on a standalone host",
      route: {
        ...on,
        attached: false,
        kind: "standalone",
        state: "self_not_attached",
        reason: "self_not_attached",
      },
      word: "Not attached",
      detail:
        "TierPlan's container isn't on the tierplan-self network yet. TierPlan connects it itself on the next check.",
    },
    {
      name: "unknown",
      route: { ...on, state: "unknown" },
      word: "Unknown",
      detail: "TierPlan couldn't read this just now.",
    },
  ];

  it.each(cases)("draws each state from one map: $name", async ({ route: over, word, detail }) => {
    selfRoute = route(over);
    mountCard();
    const region = await card();
    const block = statusBlock(region);
    const lines = Array.from(block.children).map((el) => el.textContent);
    expect(lines).toEqual(detail === null ? [word] : [word, detail]);
    // No error banner for any state, unknown included.
    const alerts = Array.from(region.querySelectorAll<HTMLElement>('[data-slot="alert"]'));
    expect(alerts.filter((a) => variantOf(a) === "destructive")).toEqual([]);
  });

  it("takes every state word a route also has from the route headline", () => {
    for (const state of ["serving", "pending", "waiting_certificate", "unknown"] as const) {
      expect(selfRouteHeadline(state)).toEqual(routeHeadline(state, false));
    }
    expect(selfRouteHeadline("self_not_attached")).toEqual(routeHeadline("not_attached", false));
    expect(words(selfRouteHeadline("self_not_attached").labelKey)).toBe("Not attached");
    expect(selfRouteHeadline("off").dot).toBe("pending");
    expect(words(selfRouteHeadline("off").labelKey)).toBe("Off");
  });

  it("draws the off state as its body sentence and the direct address only", async () => {
    mountCard();
    const region = await card();
    expect(
      within(region).getByText(
        "Serve TierPlan itself on a hostname through ingress, with a certificate. It uses its own network, so the other routes can't reach TierPlan.",
      ),
    ).toBeTruthy();
    const terms = Array.from(region.querySelectorAll("dt")).map((dt) => dt.textContent);
    expect(terms).toEqual(["Direct address"]);
    expect(within(region).getByText(DIRECT)).toBeTruthy();
    expect(
      within(region).getByText(
        "Keeps working. You can always reach TierPlan here, and turn the route off from here.",
      ),
    ).toBeTruthy();
    expect(region.textContent).not.toContain("You sign in on each address separately");
  });

  it("lists the hostname, the direct address and the network while the route is on, never truncated", async () => {
    const long = `a-very-long-name-that-goes-on-and-on-for-a-while.${HOST}`;
    selfRoute = route({ on: true, host: long, state: "serving", attached: true });
    mountCard();
    const region = await card();
    const terms = Array.from(region.querySelectorAll("dt")).map((dt) => dt.textContent);
    expect(terms).toEqual(["Hostname", "Direct address", "Network"]);
    const host = within(region).getByText(long);
    expect(host.className).toContain("font-mono");
    expect(host.className).toContain("break-all");
    expect(host.className).not.toContain("truncate");
    expect(within(region).getByText("tierplan-self").className).toContain("font-mono");
    expect(within(region).getByText("Attached")).toBeTruthy();
    expect(
      within(region).getByText(
        "You sign in on each address separately. Single sign-on still returns to the public URL.",
      ),
    ).toBeTruthy();
    expect(within(region).queryByText(/Serve TierPlan itself/)).toBeNull();
  });
});

describe("the snippet", () => {
  const notAttached = {
    on: true,
    host: HOST,
    attached: false,
    state: "self_not_attached",
    reason: "self_not_attached",
  };
  const shared = `TierPlan makes the tierplan-self network when you turn the route on. Run this after that. Direct access on ${DIRECT} isn't affected either way.`;

  it("draws exactly one snippet form: the stack file lines", async () => {
    selfRoute = route({ ...notAttached, snippet: { kind: "stack_file", text: STACK_LINES } });
    mountCard();
    const region = await card();
    expect(within(region).getByText("Add TierPlan to the self network")).toBeTruthy();
    expect(
      within(region).getByText(
        "TierPlan can't change its own service: an update would stop the task that sent it. Add these lines to your TierPlan stack file and deploy it again.",
      ),
    ).toBeTruthy();
    const block = within(region).getByRole("region", { name: "the stack file lines" });
    expect(block.querySelector("code")?.textContent).toBe(STACK_LINES);
    expect(region.querySelectorAll("pre")).toHaveLength(1);
    expect(within(region).getByText(/If the service has no `networks` entry yet/)).toBeTruthy();
    expect(within(region).getByText(shared)).toBeTruthy();
    expect(within(region).queryByRole("region", { name: "the service update command" })).toBeNull();
    expect(region.textContent).not.toMatch(/token|password/i);
  });

  it("draws exactly one snippet form: the service update command", async () => {
    selfRoute = route({ ...notAttached, snippet: { kind: "service_update", text: COMMAND } });
    mountCard();
    const region = await card();
    expect(
      within(region).getByText(
        "TierPlan's service wasn't deployed from a stack file, so add the network with this command on a manager.",
      ),
    ).toBeTruthy();
    const block = within(region).getByRole("region", { name: "the service update command" });
    expect(block.querySelector("code")?.textContent).toBe(COMMAND);
    expect(region.querySelectorAll("pre")).toHaveLength(1);
    expect(within(region).getByText(shared)).toBeTruthy();
    expect(within(region).queryByText(/If the service has no `networks` entry yet/)).toBeNull();
  });

  it("draws no snippet on a standalone host", async () => {
    selfRoute = route({ ...notAttached, kind: "standalone" });
    mountCard();
    const region = await card();
    expect(within(region).getByText(/TierPlan connects it itself on the next check/)).toBeTruthy();
    expect(within(region).queryByText("Add TierPlan to the self network")).toBeNull();
    expect(region.querySelectorAll("pre")).toHaveLength(0);
  });
});

describe("the safeguards", () => {
  it("warns when the page is used through the route", async () => {
    selfRoute = route({ on: true, host: window.location.host, state: "serving", attached: true });
    mountCard();
    const region = await card();
    const alert = within(region)
      .getByText(/You're using TierPlan through this route/)
      .closest('[data-slot="alert"]') as HTMLElement;
    expect(alert.textContent).toBe(
      `You're using TierPlan through this route. If you turn it off, this page may stop answering. Open ${DIRECT} to carry on.`,
    );
    expect(variantOf(alert)).toBe("neutral");
    expect(within(alert).getByText(DIRECT).className).toContain("font-mono");
  });

  it("does not warn when the page comes from another address", async () => {
    selfRoute = route({ on: true, host: HOST, state: "serving", attached: true });
    mountCard();
    const region = await card();
    expect(within(region).queryByText(/You're using TierPlan through this route/)).toBeNull();
  });

  it("shows refusals known before the switch: TierPlan needs to run as a service", async () => {
    selfRoute = route({
      kind: "swarm_container",
      refusal: { code: "self_route_needs_service", message: "The server's sentence." },
    });
    mountCard();
    const region = await card();
    const alert = within(region)
      .getByText(/TierPlan runs as a plain container on a swarm node/)
      .closest('[data-slot="alert"]') as HTMLElement;
    expect(variantOf(alert)).toBe("warning");
    expect(alert.textContent).toBe(
      "TierPlan runs as a plain container on a swarm node, and the route needs it to run as a swarm service. See install-in-swarm.md.",
    );
    // The file name in mono, and not a link: the app does not link the docs.
    expect(within(alert).getByText("install-in-swarm.md").className).toContain("font-mono");
    expect(within(alert).queryByRole("link")).toBeNull();
  });

  it("shows refusals known before the switch: TierPlan can't tell where it runs, and the button stays enabled", async () => {
    selfRoute = route({
      here: "unknown",
      kind: "",
      state: "unknown",
      refusal: { code: "self_route_not_here", message: "The server's sentence." },
    });
    mountCard();
    const region = await card();
    expect(statusBlock(region).textContent).toBe("Off");
    const alert = within(region)
      .getByText(/TierPlan can't tell whether it runs in this environment/)
      .closest('[data-slot="alert"]') as HTMLElement;
    expect(variantOf(alert)).toBe("warning");
    expect(alert.textContent).toBe(
      `TierPlan can't tell whether it runs in this environment, so it won't switch its route on here. Direct access on ${DIRECT} isn't affected.`,
    );
    const turnOn = await button("Turn on own route");
    expect(turnOn.getAttribute("aria-disabled")).toBeNull();
    expect(turnOn.disabled).toBe(false);
  });

  it("draws a refusal with no words of its own as the server's sentence", async () => {
    selfRoute = route({ refusal: { code: "something_new", message: "The server's sentence." } });
    mountCard();
    const region = await card();
    expect(within(region).getByText("The server's sentence.")).toBeTruthy();
  });

  it("turns the route off without a destructive button", async () => {
    selfRoute = route({ on: true, host: HOST, state: "serving", attached: true });
    mountCard();
    fireEvent.click(await button("Turn off own route"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("dialog", { name: "Turn off TierPlan's own route?" }),
    );
    expect(dialog.textContent).toContain(
      `TierPlan stops serving ${HOST}. Direct access on ${DIRECT} keeps working.`,
    );
    // The off-preview runs as the dialog opens: there is no form.
    await waitUnderFakeClock(() => expect(dialog.querySelector("ol")).toBeTruthy());
    expect(writes).toEqual([{ method: "POST", path: `${BASE}/self/off/preview`, body: {} }]);
    expect(itemHeadings(dialog)).toEqual(["Origin", "Self network"]);
    expect(within(dialog).queryByText(/You're using TierPlan through this route/)).toBeNull();
    const confirm = within(dialog).getByRole("button", {
      name: "Turn off own route",
    }) as HTMLButtonElement;
    expect(confirm.getAttribute("data-variant")).toBe("default");
    fireEvent.click(confirm);
    await waitUnderFakeClock(() =>
      expect(within(dialog).getByText("TierPlan's own route is off.")).toBeTruthy(),
    );
    expect(within(dialog).getByRole("heading", { name: "Turning own route off…" })).toBeTruthy();
    expect(writes.filter((w) => w.path === `${BASE}/self`)).toEqual([
      { method: "DELETE", path: `${BASE}/self`, body: {} },
    ]);
  });

  it("warns in the switch-off review when the page is used through the route, and still lets it go", async () => {
    selfRoute = route({ on: true, host: window.location.host, state: "pending", attached: true });
    mountCard();
    fireEvent.click(await button("Turn off own route"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("dialog", { name: "Turn off TierPlan's own route?" }),
    );
    await waitUnderFakeClock(() => expect(dialog.querySelector("ol")).toBeTruthy());
    const warning = within(dialog)
      .getByText(
        `You're using TierPlan through this route. This page may stop answering as the route goes. The change still goes through. Open ${DIRECT} to see the result.`,
      )
      .closest('[data-slot="alert"]') as HTMLElement;
    expect(variantOf(warning)).toBe("warning");
    expect(
      (within(dialog).getByRole("button", { name: "Turn off own route" }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);
  });

  it("draws a failed job's sentence, then the hint with the direct address", async () => {
    enableJob = {
      ...job("j1", "ingress_self_route_enable"),
      state: "failed",
      items: [
        {
          id: "self_check",
          kind: "step",
          label: "Check the hostname",
          state: "failed",
          message: `${HOST} is already held by the stack shop.`,
          detail: "",
        },
      ],
    };
    mountCard();
    fireEvent.click(await button("Turn on own route"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("dialog", { name: "Turn on TierPlan's own route?" }),
    );
    fireEvent.change(within(dialog).getByLabelText("Hostname"), { target: { value: HOST } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Check hostname" }));
    await waitUnderFakeClock(() => expect(dialog.querySelector("ol")).toBeTruthy());
    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on own route" }));
    const sentence = await waitUnderFakeClock(() =>
      within(dialog).getByText(`${HOST} is already held by the stack shop.`),
    );
    const alert = sentence.closest('[data-slot="alert"]') as HTMLElement;
    expect(variantOf(alert)).toBe("destructive");
    expect(alert.textContent).toContain(
      `Nothing is lost. Turn it on again to carry on, or off to remove what was made. Direct access on ${DIRECT} still works.`,
    );
  });
});

describe("loading and errors", () => {
  it("draws a skeleton while the status loads", async () => {
    statusAnswer = () => new Promise<Response>(() => {});
    mountCard();
    const loading = await region();
    expect(loading.querySelector('[data-slot="skeleton"]')?.className).toContain("h-16");
    expect(within(loading).queryByRole("button")).toBeNull();
  });

  it("draws the read error with a retry that reads again", async () => {
    statusAnswer = () =>
      Response.json(
        { title: "Problem", status: 500, type: "internal", detail: "internal error" },
        { status: 500, headers: { "Content-Type": "application/problem+json" } },
      );
    mountCard();
    const region = await card();
    await waitUnderFakeClock(() =>
      expect(within(region).getByText("Couldn't read the route status.")).toBeTruthy(),
    );
    const before = statusCalls;
    statusAnswer = null;
    fireEvent.click(within(region).getByRole("button", { name: "Retry reading the route status" }));
    await button("Turn on own route");
    expect(statusCalls).toBe(before + 1);
    expect(within(region).queryByText("Couldn't read the route status.")).toBeNull();
  });
});

// reasonOf is what a reader is told about a control they cannot use: the text NeedsTooltip points at with
// aria-describedby. Undefined when the control is usable.
function reasonOf(control: HTMLElement): string | undefined {
  if (control.getAttribute("aria-disabled") !== "true") return undefined;
  const id = control.getAttribute("aria-describedby") ?? "";
  return document.getElementById(id)?.textContent ?? "";
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

const HELPER =
  "Turning this on changes which origins and proxy addresses TierPlan trusts, so it needs an instance admin.";

describe("who can switch it", () => {
  const readers: { who: string; me: () => ReturnType<typeof makeMe>; reason?: string }[] = [
    { who: "Viewer", me: () => makeMe({ envs: { [ENV]: "viewer" } }), reason: "Needs Admin" },
    { who: "Operator", me: () => makeMe({ envs: { [ENV]: "operator" } }), reason: "Needs Admin" },
    {
      who: "environment Admin",
      me: () => makeMe({ envs: { [ENV]: "admin" } }),
      reason: "Only an instance admin can do this",
    },
    { who: "instance Admin", me: () => makeMe({ instanceAdmin: true }) },
  ];

  it("gives each reader the right reason, in the order ingress.manage first", async () => {
    const seen: (string | undefined)[] = [];
    for (const reader of readers) {
      me = reader.me();
      selfRoute = route({
        on: true,
        host: HOST,
        state: "self_not_attached",
        reason: "self_not_attached",
        snippet: { kind: "service_update", text: COMMAND },
      });
      const view = mountCard();
      const region = await card();
      const turnOff = await button("Turn off own route");
      // Everyone sees the whole card: the state, the hostname, the direct address, the snippet and the helper line.
      expect(within(region).getByText(HOST)).toBeTruthy();
      expect(within(region).getByText(DIRECT)).toBeTruthy();
      expect(
        within(region).getByRole("region", { name: "the service update command" }),
      ).toBeTruthy();
      expect(within(region).getByText(HELPER)).toBeTruthy();
      await waitUnderFakeClock(() => expect(reasonOf(turnOff)).toBe(reader.reason));
      seen.push(reasonOf(turnOff));
      if (reader.reason) {
        // A disabled control is never answered with a dialog.
        fireEvent.click(turnOff);
        expect(screen.queryByRole("dialog")).toBeNull();
      }
      view.unmount();
      client.clear();
    }
    expect(seen).toEqual([
      "Needs Admin",
      "Needs Admin",
      "Only an instance admin can do this",
      undefined,
    ]);
    expect(writes).toEqual([]);
  });

  it("is disabled while ingress is off, with a plain reason", async () => {
    ingressOn = false;
    mountCard();
    const region = await card();
    const turnOn = await button("Turn on own route");
    expect(reasonOf(turnOn)).toBe("Turn ingress on first. The route is served by the proxies.");
    expect(within(region).getByText(HELPER)).toBeTruthy();
  });

  it("puts a permission reason before the ingress-off one", async () => {
    ingressOn = false;
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mountCard();
    await card();
    const turnOn = await button("Turn on own route");
    await waitUnderFakeClock(() => expect(reasonOf(turnOn)).toBe("Needs Admin"));
  });

  it("never holds the release back by the route's state or ingress", async () => {
    ingressOn = false;
    selfRoute = route({ on: true, host: HOST, state: "unknown", attached: false });
    mountCard();
    await card();
    const turnOff = await button("Turn off own route");
    expect(reasonOf(turnOff)).toBeUndefined();
    expect(turnOff.disabled).toBe(false);
  });
});

describe("how often it reads", () => {
  beforeEach(() => {
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  });

  it("refetches faster while settling: waiting for a certificate reads again within 10 s", async () => {
    selfRoute = route({ on: true, host: HOST, state: "waiting_certificate", attached: true });
    mountCard();
    await card();
    const before = statusCalls;
    await advance(10_000);
    expect(statusCalls).toBeGreaterThan(before);
  });

  it("reads every 30 s while serving", async () => {
    selfRoute = route({ on: true, host: HOST, state: "serving", attached: true });
    mountCard();
    await card();
    const before = statusCalls;
    await advance(15_000);
    expect(statusCalls).toBe(before);
    await advance(16_000);
    expect(statusCalls).toBe(before + 1);
  });
});
