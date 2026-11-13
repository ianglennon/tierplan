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
  acmeSwitchLock,
  httpsEnvOnStatus,
  httpsLetsEncryptStatus,
  lastErrorDNS,
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

async function renderPage(ready = "Last renewed") {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>
          <HttpsPage />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await screen.findByText(ready);
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
    fix: "This only happens with a test directory. Check TIERPLAN_ACME_CA_FILE. A server clock that is badly wrong can cause it too.",
  },
  {
    code: "internal",
    error: {},
    headline: "Something went wrong inside TierPlan.",
    fix: `Open Details and check the server log. TierPlan tries again at ${nextTime()}.`,
  },
  {
    code: "dns_token_invalid",
    error: {},
    headline: "Cloudflare didn't accept the API token.",
    fix: "The token is wrong, expired or revoked. Create a new token and enter it again under Validation method.",
  },
  {
    code: "dns_token_scope",
    error: { host: "tp.example.org", zone: "example.org" },
    headline: "The Cloudflare token can't manage DNS records for tp.example.org.",
    fix: "The token needs both Zone:DNS:Edit and Zone:Zone:Read, and it must include the zone for tp.example.org. Fix it in Cloudflare, or enter a new token.",
  },
  {
    code: "dns_zone_not_found",
    error: { host: "tp.example.org" },
    headline: "Cloudflare has no zone for tp.example.org.",
    fix: "Add the zone to the Cloudflare account the token belongs to, and check that the token includes it. Or make _acme-challenge.tp.example.org a CNAME to a record in a Cloudflare zone you can edit.",
  },
  {
    code: "dns_delegation_target",
    error: { host: "tp.example.org", target: "tp.validation.example.net" },
    headline:
      "_acme-challenge.tp.example.org is delegated to tp.validation.example.net, which TierPlan can't edit.",
    fix: "The target must be in a Cloudflare zone this token can edit. Add that zone to the token, or change the CNAME.",
  },
  {
    code: "dns_not_visible",
    error: { host: "tp.example.org", resolvers: ["1.1.1.1", "192.168.1.1"] },
    headline: "The TXT record for tp.example.org didn't appear on 1.1.1.1, 192.168.1.1.",
    fix: "Check that this host can reach 1.1.1.1, 192.168.1.1 on port 53. If you set resolvers under Advanced DNS, they must answer for public records. Clear them to ask the zone's own nameservers.",
  },
  {
    code: "dns_rate_limited",
    error: {},
    headline: "Cloudflare is limiting requests.",
    fix: `TierPlan waits until ${nextTime()} before it tries again. There is nothing to do.`,
  },
  {
    code: "dns_unavailable",
    error: {},
    headline: "Cloudflare couldn't be reached.",
    fix: `TierPlan tries again on its own schedule, next at ${nextTime()}. If it keeps failing, check this host's internet access.`,
  },
  {
    code: "validation_txt",
    error: { host: "tp.example.org" },
    headline: "Let's Encrypt didn't find the right TXT record for _acme-challenge.tp.example.org.",
    fix: "TierPlan saw the record, but Let's Encrypt didn't. Check that the zone's nameservers at your registrar are Cloudflare's, and that nothing else at _acme-challenge.tp.example.org gets in the way, such as a conflicting CNAME. Then try again.",
  },
];

describe("Last-run error panel: every class", () => {
  it("has a case for each of the twenty classes the server sends", () => {
    expect(CLASSES.map((c) => c.code)).toEqual([...ERROR_CLASSES]);
    expect(CLASSES).toHaveLength(20);
    expect(ERROR_CLASSES).toHaveLength(20);
    for (const code of [
      "dns_token_invalid",
      "dns_token_scope",
      "dns_zone_not_found",
      "dns_delegation_target",
      "dns_not_visible",
      "dns_rate_limited",
      "dns_unavailable",
      "validation_txt",
    ] as const) {
      expect(CLASSES.map((c) => c.code)).toContain(code);
    }
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

  it("explains a DNS-01 failure from the status with the resolvers it asked", async () => {
    status = () =>
      Response.json(withACME({ failures: 1, lastError: lastErrorDNS({ host: "tp.example.org" }) }));
    await renderPage();
    const el = panel() as HTMLElement;
    expect(el.querySelector('[data-slot="alert-title"]')?.textContent).toBe(
      "The TXT record for tp.example.org didn't appear on 1.1.1.1, 192.168.1.1.",
    );
    expect(el.querySelector('[data-slot="alert-description"]')?.textContent).toContain(
      "reach 1.1.1.1, 192.168.1.1 on port 53",
    );
  });

  it("names the zone's own nameservers when a propagation failure asked no resolver list", async () => {
    status = () =>
      Response.json(
        withACME({
          failures: 1,
          lastError: lastErrorDNS({ host: "tp.example.org", resolvers: undefined }),
        }),
      );
    await renderPage();
    expect((panel() as HTMLElement).querySelector('[data-slot="alert-title"]')?.textContent).toBe(
      "The TXT record for tp.example.org didn't appear on the zone's nameservers.",
    );
  });

  it("draws a server-supplied target as text, never as markup", async () => {
    const target = '<img src=x onerror="window.__pwned = true">.example.net';
    status = () =>
      Response.json(
        withACME({
          failures: 1,
          lastError: lastErrorDNS({
            code: "dns_delegation_target",
            host: "tp.example.org",
            target,
          }),
        }),
      );
    await renderPage();
    const el = panel() as HTMLElement;
    expect(el.querySelector("img")).toBeNull();
    expect(el.querySelector('[data-slot="alert-title"]')?.textContent).toContain(target);
  });

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

describe("Last-run error panel: TIERPLAN_ACME=on (D-07)", () => {
  const NOTE =
    "TierPlan keeps trying because TIERPLAN_ACME is on. A restart doesn't make it try sooner.";

  it("keeps the Phase 14 panel, with the next automatic attempt, and adds the keeps-trying note under it", async () => {
    status = () =>
      Response.json(
        httpsEnvOnStatus({
          failures: 1,
          lastError: acmeLastError(),
          nextAttemptAt: NEXT,
        }),
      );
    await renderPage("Mode");
    const el = panel() as HTMLElement;
    expect(el.textContent).toContain("Next automatic attempt");
    expect(el.textContent).toContain(nextTime());
    const note = [...el.querySelectorAll("p")].find((n) => n.textContent === NOTE) as HTMLElement;
    expect(note).toBeTruthy();
    expect(note.className).toContain("text-xs");
    expect(note.className).toContain("text-muted-foreground");
    expect([...note.querySelectorAll("code")].map((c) => c.textContent)).toEqual(["TIERPLAN_ACME"]);
    // It is the meta line's follower, not part of it.
    expect(note.previousElementSibling?.textContent).toContain("Next automatic attempt");
  });

  it("is absent without the variable, and while the card is paused", async () => {
    status = () =>
      Response.json(withACME({ failures: 1, lastError: acmeLastError(), nextAttemptAt: NEXT }));
    await renderPage();
    expect((panel() as HTMLElement).textContent).not.toContain("keeps trying");
    cleanup();
    queryClient.clear();

    status = () =>
      Response.json(
        withACME({
          failures: 1,
          lastError: acmeLastError(),
          locks: acmeSwitchLock("on"),
          pausedReason: "restore",
        }),
      );
    await renderPage();
    expect((panel() as HTMLElement).textContent).not.toContain("keeps trying");
  });
});

// The clock hint: one line after the fix while the status carries a clock offset (D-07, D-29, UI-SPEC J5).
describe("Last-run error panel: the clock hint", () => {
  const hint = () => panel()?.querySelector('[data-testid="clock-hint"]') as HTMLElement | null;
  const FIX =
    "Port 80 on tp.example.org must reach TierPlan, for example with -p 80:7480. Check your firewall and router, then try again.";

  it("says how far ahead the clock is, after the fix, and changes neither the headline nor the fix", async () => {
    status = () =>
      Response.json(failing({ code: "validation_unreachable", host: "tp.example.org" }));
    await renderPage();
    const baseline = {
      title: panel()?.querySelector('[data-slot="alert-title"]')?.textContent,
      fix: panel()?.querySelector('[data-slot="alert-description"] > p')?.textContent,
    };
    expect(hint()).toBeNull();
    cleanup();
    queryClient.clear();

    status = () =>
      Response.json(
        failing(
          { code: "validation_unreachable", host: "tp.example.org" },
          { clockOffsetSeconds: 600 },
        ),
      );
    await renderPage();
    const el = hint() as HTMLElement;
    expect(el.textContent).toBe(
      "TierPlan's clock is 10 minutes ahead of Let's Encrypt's. Fix NTP on this host so expiry and renewal times are right.",
    );
    const desc = panel()?.querySelector('[data-slot="alert-description"]') as HTMLElement;
    expect(el.previousElementSibling?.textContent).toBe(baseline.fix);
    expect(desc.children).toHaveLength(2);
    expect(panel()?.querySelector('[data-slot="alert-title"]')?.textContent).toBe(baseline.title);
    expect(baseline.fix).toBe(FIX);
  });

  it("says how far behind the clock is", async () => {
    status = () => Response.json(failing({ code: "internal" }, { clockOffsetSeconds: -10800 }));
    await renderPage();
    expect(hint()?.textContent).toBe(
      "TierPlan's clock is 3 hours behind Let's Encrypt's. Fix NTP on this host so expiry and renewal times are right.",
    );
  });

  it("is absent without an offset", async () => {
    status = () => Response.json(failing({ code: "internal" }));
    await renderPage();
    expect(hint()).toBeNull();
  });

  it.each(["validation_unreachable", "rate_limited", "internal"] as const)(
    "shows for the class %s",
    async (code) => {
      status = () =>
        Response.json(failing({ code, host: "tp.example.org" }, { clockOffsetSeconds: 600 }));
      await renderPage();
      expect(hint()?.textContent).toContain("10 minutes ahead");
    },
  );

  it("shows beside a failed first Issue while Let's Encrypt is off", async () => {
    status = () =>
      Response.json(
        failing(
          { code: "validation_unreachable", host: "tp.example.org" },
          { mode: "off", certificate: undefined, clockOffsetSeconds: 600 },
        ),
      );
    await renderPage("Last error");
    expect(hint()?.textContent).toContain("10 minutes ahead");
  });

  it("keeps the fixed clock sentence on ca_untrusted with or without a clock", async () => {
    const SENTENCE =
      "This only happens with a test directory. Check TIERPLAN_ACME_CA_FILE. A server clock that is badly wrong can cause it too.";
    status = () => Response.json(failing({ code: "ca_untrusted" }));
    await renderPage();
    expect(panel()?.querySelector('[data-slot="alert-description"]')?.textContent).toBe(SENTENCE);
    expect(hint()).toBeNull();
    cleanup();
    queryClient.clear();

    status = () => Response.json(failing({ code: "ca_untrusted" }, { clockOffsetSeconds: 600 }));
    await renderPage();
    expect(
      panel()?.querySelector('[data-slot="alert-description"] > p:first-child')?.textContent,
    ).toBe(SENTENCE);
    expect(hint()?.textContent).toContain("10 minutes ahead");
  });

  it.each([
    [301, "5 minutes ahead"],
    [7199, "120 minutes ahead"],
    [7200, "2 hours ahead"],
    [172_799, "48 hours ahead"],
    [200_000, "2 days ahead"],
    [-200_000, "2 days behind"],
  ])(
    "words an offset of %i seconds as %s, with no raw seconds and no empty placeholder",
    async (secs, words) => {
      status = () => Response.json(failing({ code: "internal" }, { clockOffsetSeconds: secs }));
      await renderPage();
      const text = hint()?.textContent ?? "";
      expect(text).toContain(`clock is ${words} `.trimEnd());
      expect(text).not.toMatch(/second/);
      expect(text).not.toContain("{{");
    },
  );
});
