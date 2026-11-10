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
import "@/i18n";
import {
  acmeActiveRun,
  acmeRunFrame,
  acmeStep,
  acmeSteps,
  httpsACME,
  httpsStatus,
} from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

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
