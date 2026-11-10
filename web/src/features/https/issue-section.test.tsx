// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ACMEAccount, HTTPSACME, HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { AcmeCard } from "@/features/https/acme-card";
import { formatDate } from "@/features/https/copy";
import "@/i18n";
import { formatClock } from "@/lib/format";
import {
  acmeActiveRun,
  acmeLastError,
  acmeRunFrame,
  acmeStep,
  acmeSteps,
  httpsACME,
  httpsStatus,
} from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

// Issue certificate and Renew now on the Let's Encrypt card: the refusals, the waits, the reused
// answer and the run panel they lead to (plan 14-16, ACME-04, ACME-06, ACME-07, ACME-10).

interface Sent {
  path: string;
  method: string;
}

const REGISTERED_AT = "2026-11-10T08:00:00Z";
const account: ACMEAccount = {
  contact: "ops@example.com",
  registeredAt: REGISTERED_AT,
  termsOfServiceUrl: "",
};

let sent: Sent[];
let status: () => HTTPSStatus;
let me: () => Response;
// What a press of Issue certificate or Renew now is answered with.
let issueAnswer: () => Response | Promise<Response>;
let renewAnswer: () => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));
const viewer = () => Response.json(makeMe({ allEnvironments: "viewer" }));

function withACME(over: Partial<HTTPSACME>, top: Partial<HTTPSStatus> = {}): HTTPSStatus {
  return httpsStatus({ acme: httpsACME({ account, ...over }), ...top });
}

// A Let's Encrypt certificate that is served now, expiring in 60 days.
const NOT_AFTER = new Date(Date.now() + 60 * 86_400_000).toISOString();
function served(over: Partial<HTTPSACME> = {}): HTTPSStatus {
  return withACME(
    {
      mode: "active",
      hasState: true,
      certificate: {
        issuedAt: new Date(Date.now() - 86_400_000).toISOString(),
        names: ["tierplan.example.com"],
        notAfter: NOT_AFTER,
        notBefore: new Date(Date.now() - 86_400_000).toISOString(),
        renewed: false,
      },
      ...over,
    },
    {
      certificate: {
        source: "lets_encrypt",
        issuer: "R11",
        names: ["tierplan.example.com"],
        notBefore: new Date(Date.now() - 86_400_000).toISOString(),
        notAfter: NOT_AFTER,
        testCA: false,
      },
    },
  );
}

const problem = (status: number, type: string, errors: unknown[] = [], headers = {}) =>
  new Response(JSON.stringify({ status, type, title: type, errors }), {
    status,
    headers: { "Content-Type": "application/problem+json", ...headers },
  });

// A refusal as the server's 422 carries it: errors[].message is the code, errors[].value the data.
const refusalError = (code: string, value: Record<string, unknown> = {}) => ({
  location: "refusals",
  message: code,
  value: { code, ...value },
});

// Every refusal the server has words for, with parameters.
const ALL_REFUSALS: NonNullable<HTTPSACME["refusals"]> = [
  { code: "names_ip", host: "192.0.2.10" },
  { code: "agent_url_ip", host: "198.51.100.7" },
  { code: "names_wildcard", host: "*.example.com" },
  { code: "names_not_public", host: "tierplan.lan" },
  { code: "base_path", path: "/tierplan" },
  { code: "agent_dial_host", count: 2, hosts: ["a.example.net", "b.example.net"] },
];

const HOUR = 3_600_000;
const inAnHour = () => new Date(Date.now() + HOUR).toISOString();

beforeEach(() => {
  sent = [];
  status = () => withACME({});
  me = instanceAdmin;
  issueAnswer = () => Response.json({ jobId: "job-1", reused: false }, { status: 202 });
  renewAnswer = () => Response.json({ jobId: "job-1", reused: false }, { status: 202 });
  successToast = vi.spyOn(toast, "success");
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method });
      if (path === "/api/me") return me();
      if (path === "/api/https/status") return Response.json(status());
      if (path === "/api/https/acme/directory") {
        return Response.json({ overridden: false, profiles: [], termsOfServiceUrl: "", url: "" });
      }
      if (path === "/api/https/acme/issue" && req.method === "POST") return issueAnswer();
      if (path === "/api/https/acme/renew" && req.method === "POST") return renewAnswer();
      if (path === "/api/https/acme/stop" && req.method === "POST") {
        return new Response(null, { status: 204 });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
});

function renderCard() {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>
          <AcmeCard />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "Let's Encrypt" }));

// The card, once the status and who is signed in have both arrived.
async function ready() {
  renderCard();
  await card().findByText("Names on the certificate");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

const issueButton = () => card().queryByRole("button", { name: /^(Issue certificate|Issuing)/ });
// The text of each success toast, which toastSuccess passes as an element.
const toasts = () =>
  successToast.mock.calls.map(
    (c: unknown[]) => (c[0] as { props: { children: string } }).props.children,
  );
// Lets every promise the page has started run to its end: a request that would be sent has been
// sent when this returns (a macrotask turn, so no clock is waited on).
const flush = () =>
  act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
const renewButton = () => card().queryByRole("button", { name: /^(Renew now|Renewing)/ });
const posts = () => sent.filter((s) => s.method === "POST").map((s) => s.path);
const stream = () => FakeEventSource.instances.at(-1) as FakeEventSource;

describe("issuing a certificate and watching it", () => {
  it("presses Issue certificate, follows the six steps live and shows the new certificate", async () => {
    await ready();
    fireEvent.click(issueButton() as HTMLElement);

    // The press became a POST, and the page followed the run it started on its stream.
    await waitFor(() => expect(posts()).toEqual(["/api/https/acme/issue"]));
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    expect(new URL(stream().url, "http://x").pathname).toBe("/api/https/acme/runs/job-1/stream");

    status = () => withACME({ activeRun: acmeActiveRun() });
    act(() => stream().sendFrame("snapshot", acmeRunFrame()));
    const panel = await card().findByRole("list", { name: "Steps" });
    const step = (id: string) => panel.querySelector(`[data-step="${id}"]`);
    expect(card().getByText("Issuing certificate")).toBeTruthy();
    expect(panel.querySelectorAll("li")).toHaveLength(6);
    expect(step("account")?.getAttribute("data-state")).toBe("running");

    act(() => {
      stream().sendFrame(
        "item",
        acmeStep({ id: "account", label: "Account", state: "skipped" }),
        "1",
      );
      stream().sendFrame("item", acmeStep({ id: "order", label: "Order", state: "running" }), "2");
    });
    expect(step("account")?.getAttribute("data-state")).toBe("skipped");
    expect(step("order")?.getAttribute("data-state")).toBe("running");

    // The run ends: the status is read again and shows the certificate it stored.
    status = () => served();
    act(() =>
      stream().sendFrame(
        "done",
        acmeRunFrame({
          state: "done",
          items: acmeSteps({
            account: "skipped",
            order: "done",
            challenge: "done",
            validate: "done",
            finalize: "done",
            stored: "done",
          }),
        }),
        "9",
      ),
    );
    await card().findByText("Certificate issued.");
    await waitFor(() => expect(toasts()).toEqual(["Certificate issued."]));
    await card().findByText(/Now serving a certificate valid until/);
    expect(stream().closed).toBe(true);
  });
});

describe("a first issue that fails (W-CR-01)", () => {
  it("shows the classified error panel, announced once, though Let's Encrypt stays off", async () => {
    await ready();
    fireEvent.click(issueButton() as HTMLElement);
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() => stream().sendFrame("snapshot", acmeRunFrame()));
    await card().findByText("Issuing certificate");

    // The run fails: the server records the error and the ledger, and leaves the mode off.
    status = () =>
      withACME({
        hasState: true,
        mode: "off",
        failures: 1,
        nextAttemptAt: new Date(Date.now() + 600_000).toISOString(),
        lastError: acmeLastError(),
      });
    act(() =>
      stream().sendFrame(
        "done",
        acmeRunFrame({
          state: "failed",
          items: acmeSteps({
            account: "done",
            order: "done",
            challenge: "done",
            validate: "failed",
          }),
        }),
        "9",
      ),
    );
    await card().findByText("Issuing failed.");
    const headline = await card().findByText(
      "Let's Encrypt couldn't reach http://tierplan.example.com/.well-known/acme-challenge/.",
    );
    const panel = headline.closest('[data-slot="alert"]') as HTMLElement;
    await waitFor(() => expect(panel.getAttribute("role")).toBe("alert"));
    expect(panel.textContent).not.toContain("Next automatic attempt");
    expect(toasts()).toEqual([]);
  });
});

// The alert "Can't issue yet" and its items, reason and fix each.
const refusalAlert = () =>
  card().getByText("Can't issue yet").closest('[data-slot="alert"]') as HTMLElement;
const refusalItems = () =>
  Array.from(refusalAlert().querySelectorAll("li")).map((li) =>
    Array.from(li.querySelectorAll("span")).map((span) => span.textContent),
  );
const noRefusals = () => card().queryByText("Can't issue yet");
const isDisabled = (button: HTMLElement | null) => button?.getAttribute("aria-disabled") === "true";
const describedBy = (button: HTMLElement | null) => {
  const id = button?.getAttribute("aria-describedby");
  return id ? (document.getElementById(id)?.textContent ?? "") : "";
};
const filledButtons = () =>
  Array.from(card().getAllByRole("button")).filter((b) => b.className.includes("bg-primary"));

const REASONS = {
  names_ip: "The public URL uses an IP address (192.0.2.10).",
  agent_url_ip: "The agent URL uses an IP address (198.51.100.7).",
  names_wildcard: "*.example.com is a wildcard name.",
  names_not_public:
    "tierplan.lan isn't a public DNS name. It has no dot, or ends in a private suffix such as .lan or .home.arpa.",
  base_path: "The public URL has a path (/tierplan).",
  agent_dial_host:
    "2 connected agents reach TierPlan at a.example.net, b.example.net, which isn't on the certificate.",
} as const;

describe("E4: what stops an order", () => {
  it("renders nothing and keeps Issue certificate enabled when nothing is refused (empty)", async () => {
    await ready();
    expect(noRefusals()).toBeNull();
    expect(isDisabled(issueButton())).toBe(false);
  });

  it("renders nothing until the status has arrived (loading)", async () => {
    status = () => withACME({ refusals: ALL_REFUSALS });
    renderCard();
    expect(noRefusals()).toBeNull();
    expect(issueButton()).toBeNull();
    await card().findByText("Can't issue yet");
  });

  it("shows every refusal at once as its own item, with its reason then its fix (populated, zero-one-many, overflow)", async () => {
    status = () => withACME({ refusals: ALL_REFUSALS });
    await ready();
    const items = refusalItems();
    // All six, in the order the server gave them: no cap, no collapse.
    expect(items).toHaveLength(6);
    expect(items.map((i) => i[0])).toEqual(Object.values(REASONS));
    expect(items.map((i) => i[1])).toEqual([
      "Set the public URL in Settings > General to a DNS name that points to this host.",
      "Set the agent URL to a DNS name, or clear it to use the public URL.",
      "Use a plain host name. Wildcard certificates aren't supported.",
      "Use a name under a domain you own, such as tierplan.example.com. Nothing was sent to Let's Encrypt.",
      "HTTP-01 needs TierPlan at the root of its host. Remove the path from the public URL, or give TierPlan its own host name.",
      "Reinstall those agents with a new install command, or set the agent URL to the host they use. Only agents that are connected now are checked.",
    ]);
    // The reason is the heavier line, the fix the plain one.
    const first = refusalAlert().querySelector("li") as HTMLElement;
    expect((first.children[0] as HTMLElement).className).toContain("font-semibold");
    expect((first.children[1] as HTMLElement).className).toContain("font-normal");
    // It sits above the buttons.
    const button = issueButton() as HTMLElement;
    expect(
      refusalAlert().compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("words one connected agent in the singular and names the public host when a host is empty", async () => {
    status = () =>
      withACME({
        refusals: [
          { code: "agent_dial_host", count: 1, hosts: ["a.example.net"] },
          { code: "names_not_public", host: "" },
        ],
      });
    await ready();
    expect(refusalItems().map((i) => i[0])).toEqual([
      "1 connected agent reaches TierPlan at a.example.net, which isn't on the certificate.",
      "the public host isn't a public DNS name. It has no dot, or ends in a private suffix such as .lan or .home.arpa.",
    ]);
  });

  it("keeps Issue certificate aria-disabled, linked to the list, and sends nothing when it is pressed", async () => {
    status = () => withACME({ refusals: ALL_REFUSALS.slice(0, 2) });
    await ready();
    const button = issueButton();
    expect(isDisabled(button)).toBe(true);
    expect(button?.hasAttribute("disabled")).toBe(false);
    expect(describedBy(button)).toContain("The public URL uses an IP address");
    fireEvent.click(button as HTMLElement);
    await flush();
    expect(posts()).toEqual([]);
  });

  it("renders the same list from a 422 as from the status (error)", async () => {
    status = () => withACME({ refusals: ALL_REFUSALS });
    await ready();
    const fromStatus = refusalItems();
    cleanup();
    queryClient.clear();

    status = () => withACME({});
    issueAnswer = () =>
      problem(
        422,
        "acme_refused",
        ALL_REFUSALS.map((r) => refusalError(r.code, r)),
      );
    await ready();
    expect(noRefusals()).toBeNull();
    fireEvent.click(issueButton() as HTMLElement);
    await card().findByText("Can't issue yet");
    expect(refusalItems()).toEqual(fromStatus);
    // A fresh refusal is announced; the page did not start a run.
    expect(refusalAlert().getAttribute("role")).toBe("alert");
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("never invents a refusal: a 422 with a code it has no words for shows the general failure", async () => {
    issueAnswer = () => problem(422, "acme_refused", [refusalError("names_unheard_of")]);
    await ready();
    fireEvent.click(issueButton() as HTMLElement);
    await card().findByText("Something went wrong. Try again.");
    expect(noRefusals()).toBeNull();
  });

  it("breaks long hosts anywhere and lets the action row buttons fill a narrow screen", async () => {
    const host = `${"very-long-label.".repeat(8)}example.com`;
    status = () => withACME({ refusals: [{ code: "names_wildcard", host }] });
    await ready();
    const item = refusalAlert().querySelector("li") as HTMLElement;
    expect(item.className).toContain("break-all");
    expect(item.className).toContain("min-w-0");
    expect(item.textContent).toContain(host);
    expect((issueButton() as HTMLElement).className).toContain("max-md:w-full");
  });

  it("draws a host as text, never as markup", async () => {
    status = () =>
      withACME({ refusals: [{ code: "names_wildcard", host: "<img src=x onerror=alert(1)>" }] });
    await ready();
    expect(refusalAlert().querySelector("img")).toBeNull();
    expect(refusalAlert().textContent).toContain("<img src=x onerror=alert(1)>");
  });
});

describe("the buttons of each state", () => {
  const expiredAt = new Date(Date.now() - 86_400_000).toISOString();
  const stored: NonNullable<HTTPSACME["certificate"]> = {
    issuedAt: expiredAt,
    names: ["tierplan.example.com"],
    notAfter: expiredAt,
    notBefore: expiredAt,
    renewed: false,
  };

  it("shows no button and asks for an account first before one is registered (A)", async () => {
    status = () => httpsStatus();
    await ready();
    expect(issueButton()).toBeNull();
    expect(renewButton()).toBeNull();
    expect(card().getByText("Register a Let's Encrypt account first.")).toBeTruthy();
  });

  it.each([
    ["B", () => withACME({}), true, false],
    ["D", () => served(), false, true],
    [
      "E",
      () => served({ namesMismatch: true, names: [{ name: "new.example.com", refused: false }] }),
      true,
      true,
    ],
    [
      "K",
      () => withACME({ certificate: stored, fallback: { expiredAt }, hasState: true }),
      true,
      true,
    ],
    ["L", () => withACME({ certificate: stored, hasState: true }), true, false],
  ])(
    "draws the buttons of state %s, with at most one filled",
    async (state, make, issue, renew) => {
      status = make;
      await ready();
      // The status really is the state the row names.
      expect(document.querySelector(`div[data-state="${state}"]`)).not.toBeNull();
      expect(Boolean(issueButton())).toBe(issue);
      expect(Boolean(renewButton())).toBe(renew);
      // Issue certificate is the card's one filled button; Renew now is an outline.
      expect(filledButtons().map((b) => b.textContent)).toEqual(issue ? ["Issue certificate"] : []);
      if (renew) expect(filledButtons().includes(renewButton() as HTMLElement)).toBe(false);
    },
  );

  it("draws no button and no helper without settings.write", async () => {
    me = viewer;
    status = () => served();
    renderCard();
    await card().findByText("Names on the certificate");
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
    expect(renewButton()).toBeNull();
    expect(card().queryByText(/switches to the new certificate/)).toBeNull();
  });

  it("tells a viewer what is refused, with no button", async () => {
    me = viewer;
    status = () => withACME({ refusals: ALL_REFUSALS.slice(0, 1) });
    renderCard();
    await card().findByText("Can't issue yet");
    expect(issueButton()).toBeNull();
  });
});

describe("waits (ACME-07)", () => {
  it("disables both buttons and links the wait when Let's Encrypt asked TierPlan to wait", async () => {
    const until = inAnHour();
    status = () =>
      served({
        namesMismatch: true,
        names: [{ name: "new.example.com", refused: false }],
        blocked: { reason: "told_to_wait", until },
      });
    await ready();
    const wait = `Let's Encrypt asked TierPlan to wait. You can try again at ${formatClock(until)}.`;
    for (const button of [issueButton(), renewButton()]) {
      expect(isDisabled(button)).toBe(true);
      expect(describedBy(button)).toBe(wait);
    }
    fireEvent.click(issueButton() as HTMLElement);
    fireEvent.click(renewButton() as HTMLElement);
    await flush();
    expect(posts()).toEqual([]);
  });

  it("words three failures in an hour in state G, with Renew now alone", async () => {
    const until = inAnHour();
    status = () => served({ blocked: { reason: "failure_guard", until }, failures: 3 });
    await ready();
    expect(issueButton()).toBeNull();
    expect(isDisabled(renewButton())).toBe(true);
    expect(describedBy(renewButton())).toBe(
      `Three attempts failed in the last hour. TierPlan waits so it doesn't use up Let's Encrypt's limits. You can try again at ${formatClock(until)}.`,
    );
  });

  it("takes a 429 as the wait, with the instant the server gave", async () => {
    const until = inAnHour();
    issueAnswer = () =>
      problem(429, "acme_blocked", [{ location: "body", message: "told_to_wait", value: until }], {
        "Retry-After": "3600",
      });
    await ready();
    fireEvent.click(issueButton() as HTMLElement);
    await card().findByText(
      `Let's Encrypt asked TierPlan to wait. You can try again at ${formatClock(until)}.`,
    );
    expect(isDisabled(issueButton())).toBe(true);
  });

  it("reads the status again when the page was out of date (409 run active)", async () => {
    issueAnswer = () => problem(409, "acme_run_active");
    await ready();
    const before = sent.filter((s) => s.path === "/api/https/status").length;
    status = () => withACME({ activeRun: acmeActiveRun({ jobId: "other" }) });
    fireEvent.click(issueButton() as HTMLElement);
    await waitFor(() =>
      expect(sent.filter((s) => s.path === "/api/https/status").length).toBeGreaterThan(before),
    );
    await card().findByText("A run is already in progress.");
    expect(card().queryByText("Something went wrong. Try again.")).toBeNull();
  });

  it("shows the general failure when the server fails", async () => {
    issueAnswer = () => problem(500, "internal");
    await ready();
    fireEvent.click(issueButton() as HTMLElement);
    await card().findByText("Something went wrong. Try again.");
    expect(isDisabled(issueButton())).toBe(false);
  });
});

describe("a run in progress (state C)", () => {
  it("disables both buttons with a spinner on the one that started it", async () => {
    await ready();
    status = () => withACME({ activeRun: acmeActiveRun() });
    fireEvent.click(issueButton() as HTMLElement);
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    await waitFor(() => expect(card().getByText("A run is already in progress.")).toBeTruthy());
    const issue = issueButton() as HTMLElement;
    const renew = renewButton() as HTMLElement;
    expect(issue.textContent).toBe("Issuing...");
    expect(issue.querySelector('[data-slot="spinner"]')).not.toBeNull();
    expect(renew.textContent).toBe("Renew now");
    expect(renew.querySelector('[data-slot="spinner"]')).toBeNull();
    expect(isDisabled(issue)).toBe(true);
    expect(isDisabled(renew)).toBe(true);
    expect(describedBy(renew)).toBe("A run is already in progress.");
    // No filled button while it runs.
    expect(filledButtons()).toEqual([]);
  });

  it("puts the spinner on Renew now when that started the run", async () => {
    status = () => served();
    await ready();
    status = () => served({ activeRun: acmeActiveRun({ kind: "renew" }) });
    fireEvent.click(renewButton() as HTMLElement);
    await waitFor(() => expect(posts()).toEqual(["/api/https/acme/renew"]));
    await waitFor(() => expect(renewButton()?.textContent).toBe("Renewing..."));
    expect(issueButton()?.querySelector('[data-slot="spinner"]') ?? null).toBeNull();
  });

  it("starts a renewal the same way and tells it when it is done (ACME-06)", async () => {
    status = () => served();
    await ready();
    expect(filledButtons()).toEqual([]);
    fireEvent.click(renewButton() as HTMLElement);
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() => stream().sendFrame("snapshot", acmeRunFrame({ kind: "renew" })));
    await card().findByText("Renewing certificate");
    act(() =>
      stream().sendFrame(
        "done",
        acmeRunFrame({ kind: "renew", state: "done", items: acmeSteps({ stored: "done" }) }),
        "9",
      ),
    );
    await card().findByText("Certificate renewed.");
    await waitFor(() => expect(toasts()).toEqual(["Certificate renewed."]));
  });

  it("answers an unchanged name set with the reused note and orders nothing", async () => {
    status = () => served();
    issueAnswer = () => Response.json({ reused: true, validUntil: NOT_AFTER }, { status: 200 });
    // Names changed, so Issue certificate is drawn next to Renew now.
    status = () =>
      served({ namesMismatch: true, names: [{ name: "new.example.com", refused: false }] });
    await ready();
    fireEvent.click(issueButton() as HTMLElement);
    const note = await card().findByRole("status");
    expect(note.textContent).toBe(
      `These names already have a valid certificate until ${formatDate(NOT_AFTER)}. Nothing was ordered. Use Renew now to get a new one.`,
    );
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(toasts()).toEqual([]);
  });

  it("closes the stream and sends no cancel when the page is left", async () => {
    await ready();
    fireEvent.click(issueButton() as HTMLElement);
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() => stream().sendFrame("snapshot", acmeRunFrame()));
    cleanup();
    expect(stream().closed).toBe(true);
    expect(posts()).toEqual(["/api/https/acme/issue"]);
  });
});

describe("a run started elsewhere", () => {
  it("is picked up on page load from the status and followed live", async () => {
    status = () => withACME({ activeRun: acmeActiveRun({ jobId: "job-7" }) });
    await ready();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    expect(new URL(stream().url, "http://x").pathname).toBe("/api/https/acme/runs/job-7/stream");
    act(() => stream().sendFrame("snapshot", acmeRunFrame({ jobId: "job-7" })));
    await card().findByText("Issuing certificate");
    expect(posts()).toEqual([]);
  });

  it("joins a resume after a restart midway: earlier steps done, the account skipped", async () => {
    status = () => withACME({ hasState: true, activeRun: acmeActiveRun({ trigger: "resume" }) });
    await ready();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() =>
      stream().sendFrame(
        "snapshot",
        acmeRunFrame({
          trigger: "resume",
          items: acmeSteps({
            account: "skipped",
            order: "done",
            challenge: "done",
            validate: "running",
          }),
        }),
      ),
    );
    const panel = await card().findByRole("list", { name: "Steps" });
    const states = Array.from(panel.querySelectorAll("li")).map((li) =>
      li.getAttribute("data-state"),
    );
    expect(states).toEqual(["skipped", "done", "done", "running", "waiting", "waiting"]);
    expect(card().getByText("Account already registered.")).toBeTruthy();
    expect(
      card().getByText("Waiting for Let's Encrypt to check tierplan.example.com."),
    ).toBeTruthy();
  });

  it("says a renewal the scheduler started is automatic", async () => {
    status = () => served({ activeRun: acmeActiveRun({ kind: "renew", trigger: "schedule" }) });
    await ready();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() => stream().sendFrame("snapshot", acmeRunFrame({ kind: "renew", trigger: "schedule" })));
    await card().findByText("Renewing certificate automatically");
  });

  it("keeps the finished panel until the next run starts", async () => {
    status = () => withACME({ activeRun: acmeActiveRun() });
    await ready();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    status = () => served();
    act(() =>
      stream().sendFrame(
        "done",
        acmeRunFrame({ state: "done", items: acmeSteps({ stored: "done" }) }),
      ),
    );
    await card().findByText("Certificate issued.");
    // The status moved on, and the panel stays.
    await waitFor(() =>
      expect(
        card().queryByText("Now serving a certificate valid until", { exact: false }),
      ).toBeTruthy(),
    );
    expect(card().getByText("Certificate issued.")).toBeTruthy();
  });
});

describe("after Stop (W-WR-01)", () => {
  // Stop using Let's Encrypt through the card's own button and dialog. The server goes back to the
  // self-signed certificate and keeps the Let's Encrypt one (state L).
  async function stopLetsEncrypt() {
    status = () => {
      const kept = served({ mode: "off" });
      return { ...kept, certificate: httpsStatus().certificate };
    };
    fireEvent.click(card().getByRole("button", { name: "Stop using Let's Encrypt" }));
    const dialog = within(await screen.findByRole("alertdialog"));
    fireEvent.click(dialog.getByRole("button", { name: "Stop using Let's Encrypt" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  }

  it("puts away the last renewal's panel, which no longer describes what is served", async () => {
    status = () => served();
    await ready();
    fireEvent.click(renewButton() as HTMLElement);
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() => stream().sendFrame("snapshot", acmeRunFrame({ kind: "renew" })));
    act(() =>
      stream().sendFrame(
        "done",
        acmeRunFrame({ kind: "renew", state: "done", items: acmeSteps({ stored: "done" }) }),
        "9",
      ),
    );
    await card().findByText("Certificate renewed.");
    await card().findByText(/Now serving a certificate valid until/);

    await stopLetsEncrypt();
    await waitFor(() => expect(card().queryByText("Certificate renewed.")).toBeNull());
    expect(card().queryByText(/Now serving a certificate valid until/)).toBeNull();
    expect(document.querySelector("[data-run-state]")).toBeNull();
    // State L: Issue certificate is offered again, with no spinner left from the renewal.
    expect(issueButton()?.textContent).toBe("Issue certificate");
  });

  it("puts away a failed renewal's panel too", async () => {
    status = () => served();
    await ready();
    fireEvent.click(renewButton() as HTMLElement);
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    status = () => served({ failures: 1, lastError: acmeLastError() });
    act(() =>
      stream().sendFrame(
        "done",
        acmeRunFrame({ kind: "renew", state: "failed", items: acmeSteps({ order: "failed" }) }),
        "9",
      ),
    );
    await card().findByText("Renewing failed.");

    await stopLetsEncrypt();
    await waitFor(() => expect(card().queryByText("Renewing failed.")).toBeNull());
    expect(document.querySelector("[data-run-state]")).toBeNull();
  });
});

describe("a dropped stream", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("says Reconnecting... and reads the status every 5 seconds until the stream returns", async () => {
    status = () => withACME({ activeRun: acmeActiveRun() });
    await ready();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() => stream().sendFrame("snapshot", acmeRunFrame()));
    await card().findByText("Issuing certificate");
    expect(card().queryByText("Reconnecting...")).toBeNull();
    const reads = () => sent.filter((s) => s.path === "/api/https/status").length;

    vi.useFakeTimers({ toFake: ["setTimeout", "setInterval", "clearTimeout", "clearInterval"] });
    act(() => stream().fail());
    await waitUnderFakeClock(() => expect(card().getByText("Reconnecting...")).toBeTruthy());

    const before = reads();
    await vi.advanceTimersByTimeAsync(5_000);
    await waitUnderFakeClock(() => expect(reads()).toBeGreaterThan(before));
    // The stream is opened again after a second, and says where it was.
    expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(2);

    // It returns: the header settles and the polling stops.
    act(() => stream().sendFrame("snapshot", acmeRunFrame()));
    await waitUnderFakeClock(() => expect(card().queryByText("Reconnecting...")).toBeNull());
    const settled = reads();
    await vi.advanceTimersByTimeAsync(20_000);
    expect(reads()).toBe(settled);
  });
});
