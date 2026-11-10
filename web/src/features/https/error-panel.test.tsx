// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSACME, HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { ERROR_CLASSES } from "@/features/https/copy";
import { HttpsPage } from "@/features/https/https-page";
import "@/i18n";
import { formatClock } from "@/lib/format";
import {
  acmeActiveRun,
  acmeLastError,
  acmeRunFrame,
  acmeSteps,
  httpsLetsEncryptStatus,
} from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

let status: () => Response;

beforeEach(() => {
  status = () => Response.json(httpsLetsEncryptStatus());
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ instanceAdmin: true }));
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/directory") {
        return Response.json({ overridden: false, profiles: [], termsOfServiceUrl: "", url: "" });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
});

function withACME(over: Partial<HTTPSACME>, top: Partial<HTTPSStatus> = {}): HTTPSStatus {
  return httpsLetsEncryptStatus(over, top);
}

async function renderPage() {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>
          <HttpsPage />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await screen.findByText("Last renewed");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

const cert = () => within(screen.getByRole("region", { name: "Certificate in use" }));
const panel = () => document.getElementById("lets-encrypt-last-run");

describe("Last-run error panel: the path from the status to the page", () => {
  it("explains a failed renewal on the card row and in the panel, with the raw document under Details", async () => {
    status = () =>
      Response.json(
        withACME({
          failures: 2,
          lastError: acmeLastError(),
          nextAttemptAt: new Date(Date.now() + 3_600_000).toISOString(),
        }),
      );
    await renderPage();

    // The card's Last error row names the class and links to the panel.
    const dd = cert().getByText("Last error").nextElementSibling as HTMLElement;
    expect(dd.textContent).toContain(
      "Let's Encrypt couldn't reach http://tierplan.example.com/.well-known/acme-challenge/.",
    );
    const link = within(dd).getByRole("link", { name: "Details below" });
    expect(link.getAttribute("href")).toBe("#lets-encrypt-last-run");

    // The panel is what the link points at, a note because it comes from the stored ledger.
    const el = panel();
    expect(el).toBeTruthy();
    expect(el?.getAttribute("role")).toBe("note");
    const p = within(el as HTMLElement);
    expect(p.getByText(/Port 80 on tierplan.example.com must reach TierPlan/)).toBeTruthy();
    expect(el?.textContent).toContain("2 failed attempts in a row.");
    expect(el?.textContent).toContain("Next automatic attempt");

    // The CA's document is behind Details.
    expect(p.queryByTestId("last-run-detail")).toBeNull();
    fireEvent.click(p.getByRole("button", { name: "Details" }));
    expect(p.getByTestId("last-run-detail").textContent).toContain("Timeout during connect");
    expect(p.getByTestId("last-run-detail").className).toContain("max-h-64");
  });
});

// A fixed time for the next attempt, so the sentence it ends is the same on every run.
const NEXT = "2027-01-05T09:30:00Z";
const nextTime = () => formatClock(NEXT) as string;

function failing(error: Parameters<typeof acmeLastError>[0], over: Partial<HTTPSACME> = {}) {
  return withACME({
    failures: 2,
    lastError: acmeLastError(error),
    nextAttemptAt: NEXT,
    ...over,
  });
}

// What each class says with its parameters filled (UI-SPEC "Last-run error panel").
const CLASSES: {
  code: (typeof ERROR_CLASSES)[number];
  error: Parameters<typeof acmeLastError>[0];
  headline: string;
  fix: string;
}[] = [
  {
    code: "rate_limited",
    error: {},
    headline: "Let's Encrypt is limiting requests.",
    fix: `TierPlan waits until ${nextTime()} before it tries again. There is nothing to do.`,
  },
  {
    code: "validation_unreachable",
    error: { host: "tp.example.org" },
    headline: "Let's Encrypt couldn't reach http://tp.example.org/.well-known/acme-challenge/.",
    fix: "Port 80 on tp.example.org must reach TierPlan, for example with -p 80:7480. Check your firewall and router, then try again.",
  },
  {
    code: "validation_dns",
    error: { host: "tp.example.org" },
    headline: "The DNS records for tp.example.org don't point at this host.",
    fix: "Make the A and AAAA records for tp.example.org point at this host. Remove an old AAAA record if you have no IPv6. Then try again.",
  },
  {
    code: "caa_forbids",
    error: { host: "tp.example.org" },
    headline: "A CAA record forbids Let's Encrypt for tp.example.org.",
    fix: "Add a CAA record that allows letsencrypt.org, or remove the one that blocks it.",
  },
  {
    code: "identifier_rejected",
    error: { host: "tp.example.org" },
    headline: "Let's Encrypt won't issue for tp.example.org.",
    fix: "Use a public DNS name under a domain you own as the public URL.",
  },
  {
    code: "contact_invalid",
    error: {},
    headline: "Let's Encrypt rejected the contact email.",
    fix: "Register again with a valid address, or leave it empty.",
  },
  {
    code: "tos_action_required",
    error: { termsUrl: "https://letsencrypt.org/terms-v2.pdf" },
    headline: "Let's Encrypt changed its terms.",
    fix: "Read the current terms at https://letsencrypt.org/terms-v2.pdf, then try again.",
  },
  {
    code: "account_gone",
    error: {},
    headline: "Let's Encrypt doesn't know this account.",
    fix: "Register the account again in the Account section.",
  },
  {
    code: "profile_invalid",
    error: { profile: "shortlived" },
    headline: "Let's Encrypt doesn't offer the profile shortlived.",
    fix: "Choose another profile under Advanced, or go back to the CA default.",
  },
  {
    code: "ca_unavailable",
    error: {},
    headline: "Let's Encrypt couldn't be reached.",
    fix: `TierPlan tries again on its own schedule, next at ${nextTime()}. If it keeps failing, check this host's internet access.`,
  },
  {
    code: "ca_untrusted",
    error: {},
    headline: "TierPlan doesn't trust the certificate authority's certificate.",
    fix: "This only happens with a test directory. Check TIERPLAN_ACME_CA_FILE.",
  },
  {
    code: "internal",
    error: {},
    headline: "Something went wrong inside TierPlan.",
    fix: `Open Details and check the server log. TierPlan tries again at ${nextTime()}.`,
  },
];

describe("Last-run error panel: every class", () => {
  it("has a case for each of the twelve classes the server sends", () => {
    expect(CLASSES.map((c) => c.code)).toEqual([...ERROR_CLASSES]);
    expect(CLASSES).toHaveLength(12);
  });

  it.each(CLASSES)(
    "explains $code with its headline and fix, on the card row and in the panel",
    async ({ code, error, headline, fix }) => {
      status = () => Response.json(failing({ code, ...error }));
      await renderPage();
      const el = panel() as HTMLElement;
      expect(el.querySelector('[data-slot="alert-title"]')?.textContent).toBe(headline);
      expect(el.querySelector('[data-slot="alert-description"]')?.textContent).toBe(fix);
      const row = cert().getByText("Last error").nextElementSibling as HTMLElement;
      expect(row.textContent).toContain(headline);
    },
  );

  it("falls back to the internal copy for a class this build does not know", async () => {
    status = () => Response.json(failing({ code: "quota_exceeded_in_a_future_version" as never }));
    await renderPage();
    const el = panel() as HTMLElement;
    expect(el.querySelector('[data-slot="alert-title"]')?.textContent).toBe(
      "Something went wrong inside TierPlan.",
    );
    expect(el.querySelector('[data-slot="alert-description"]')?.textContent).toContain(
      "Open Details and check the server log.",
    );
  });

  it("uses the public host when the failure names none, and the sentence without a time when none is known", async () => {
    status = () =>
      Response.json(
        withACME({
          failures: 1,
          lastError: acmeLastError({ code: "ca_unavailable", host: undefined }),
          nextAttemptAt: undefined,
        }),
      );
    await renderPage();
    const el = panel() as HTMLElement;
    expect(el.querySelector('[data-slot="alert-description"]')?.textContent).toBe(
      "TierPlan tries again on its own schedule. If it keeps failing, check this host's internet access.",
    );
    expect(el.textContent).not.toContain("Next automatic attempt");
  });
});

describe("Last-run error panel: the meta line", () => {
  const meta = () => (panel() as HTMLElement).querySelector("p.text-xs") as HTMLElement;

  it("says 'failed attempt' for one failure", async () => {
    status = () => Response.json(failing({}, { failures: 1 }));
    await renderPage();
    expect(meta().textContent).toMatch(
      /^Failed .+\. 1 failed attempt in a row\. Next automatic attempt /,
    );
  });

  it("says 'failed attempts' for several, with the time of the next attempt", async () => {
    status = () => Response.json(failing({}, { failures: 3 }));
    await renderPage();
    expect(meta().textContent).toMatch(/^Failed .+\. 3 failed attempts in a row\. /);
    expect(meta().textContent).toContain(`Next automatic attempt ${nextTime()}.`);
  });

  it("leaves out the next attempt while renewals are paused", async () => {
    status = () => Response.json(failing({}, { failures: 3, pausedReason: "restore" }));
    await renderPage();
    expect(meta().textContent).toContain("3 failed attempts in a row.");
    expect(meta().textContent).not.toContain("Next automatic attempt");
  });

  it("leaves out the next attempt while an attempt is refused", async () => {
    status = () =>
      Response.json(
        failing(
          {},
          {
            failures: 3,
            blocked: {
              reason: "failure_guard",
              until: new Date(Date.now() + 3_600_000).toISOString(),
            },
          },
        ),
      );
    await renderPage();
    expect(meta().textContent).not.toContain("Next automatic attempt");
  });
});

describe("Last-run error panel: Details", () => {
  it("draws the CA's document as literal text and never as markup", async () => {
    const detail = `<script>window.__pwned = true</script><img src=x onerror="window.__pwned = true">${"x".repeat(65_000)}`;
    status = () => Response.json(failing({ detail }));
    await renderPage();
    const el = panel() as HTMLElement;
    fireEvent.click(within(el).getByRole("button", { name: "Details" }));
    const block = within(el).getByTestId("last-run-detail");
    expect(block.textContent).toBe(detail);
    expect(block.querySelector("script")).toBeNull();
    expect(block.querySelector("img")).toBeNull();
    expect(el.querySelector("script")).toBeNull();
    expect((window as { __pwned?: boolean }).__pwned).toBeUndefined();
  });

  it("scrolls inside max-h-64 and wraps anywhere, so a long document never widens the column", async () => {
    status = () => Response.json(failing({ detail: "A".repeat(3000) }));
    await renderPage();
    const el = panel() as HTMLElement;
    fireEvent.click(within(el).getByRole("button", { name: "Details" }));
    const cls = within(el).getByTestId("last-run-detail").className;
    for (const c of [
      "max-h-64",
      "overflow-auto",
      "whitespace-pre-wrap",
      "break-all",
      "font-mono",
      "text-xs",
      "bg-muted",
      "rounded-lg",
      "p-4",
    ]) {
      expect(cls).toContain(c);
    }
  });

  it("is closed until it is opened and closes again", async () => {
    status = () => Response.json(failing({}));
    await renderPage();
    const el = panel() as HTMLElement;
    const trigger = within(el).getByRole("button", { name: "Details" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(trigger);
    expect(within(el).queryByTestId("last-run-detail")).toBeNull();
  });

  it("has no Details when the failure carries no document", async () => {
    status = () => Response.json(failing({ detail: undefined }));
    await renderPage();
    expect(within(panel() as HTMLElement).queryByRole("button", { name: "Details" })).toBeNull();
  });
});

describe("Last-run error panel: role and place", () => {
  it("is a note when it comes from the stored ledger, and sits at #lets-encrypt-last-run", async () => {
    status = () => Response.json(failing({}));
    await renderPage();
    const el = panel() as HTMLElement;
    expect(el.getAttribute("role")).toBe("note");
    expect(el.id).toBe("lets-encrypt-last-run");
    expect(screen.getByRole("region", { name: "Let's Encrypt" }).contains(el)).toBe(true);
  });

  it("is not drawn when Let's Encrypt has no failure", async () => {
    await renderPage();
    expect(panel()).toBeNull();
  });

  it("is an alert right after a run of this session fails, and only then", async () => {
    // A run is going when the page opens, so the page follows it on its stream.
    status = () => Response.json(withACME({ activeRun: acmeActiveRun() }));
    await renderPage();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    const stream = FakeEventSource.instances[0] as FakeEventSource;
    act(() => stream.sendFrame("snapshot", acmeRunFrame()));
    expect(panel()).toBeNull();

    // It fails: the status now holds the error, and the panel announces it.
    status = () => Response.json(failing({}, { failures: 1 }));
    act(() =>
      stream.sendFrame(
        "done",
        acmeRunFrame({ state: "failed", items: acmeSteps({ account: "done", order: "failed" }) }),
        "9",
      ),
    );
    await waitFor(() => expect(panel()).toBeTruthy());
    await waitFor(() => expect(panel()?.getAttribute("role")).toBe("alert"));
    expect(screen.getByText("Issuing failed.")).toBeTruthy();
  });
});
