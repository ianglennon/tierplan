// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CertificatesTab } from "@/features/ingress/certificates-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let certs: () => Response;
let budget: () => Response;
let status: Json;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;

function cert(over: Json = {}): Json {
  return {
    id: "c1",
    hostname: "app.example.com",
    source: "acme",
    issuer: "production",
    staging: false,
    state: "valid",
    stateText: "Valid",
    ready: true,
    consecutiveFailures: 0,
    createdAt: "2026-11-01T10:00:00Z",
    issuedAt: "2026-11-01T10:05:00Z",
    expiresAt: "2027-01-30T10:05:00Z",
    renewAt: "2027-01-05T10:05:00Z",
    ...over,
  };
}

function serve(items: Json[]) {
  certs = () => Response.json({ items });
}

beforeEach(() => {
  setCsrfToken(null);
  serve([]);
  budget = () => Response.json({ items: [] });
  status = { enabled: true };
  me = makeMe({ envs: { [ENV]: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/certificates`) return certs();
      if (path === `/api/env/${ENV}/ingress/certificates/budget`) return budget();
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json(status);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(host?: string) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <CertificatesTab envId={ENV} host={host} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function table() {
  return screen.findByRole("table", { name: "Certificates" });
}

const rowOf = (t: HTMLElement, host: string) =>
  within(t).getByText(host).closest("tr") as HTMLElement;

describe("the Ingress page's Certificates tab", () => {
  it("draws every certificate in the server's order with its issuer, state and last error", async () => {
    serve([
      cert(),
      cert({
        id: "c2",
        hostname: "beta.example.com",
        issuer: "staging",
        staging: true,
        ready: false,
      }),
      cert({
        id: "c3",
        hostname: "corp.example.com",
        source: "upload",
        issuer: undefined,
        state: "uploaded",
        stateText: "Uploaded",
      }),
      cert({
        id: "c4",
        hostname: "dead.example.com",
        state: "failed",
        stateText: "Failed",
        ready: false,
        issuedAt: undefined,
        expiresAt: undefined,
        renewAt: undefined,
        lastError: {
          class: "dns_token_invalid",
          hint: "The DNS provider refused the token.",
          at: "2026-11-25T10:00:00Z",
        },
        nextAttemptAt: "2026-11-26T12:00:00Z",
      }),
    ]);
    mount();
    const t = await table();
    expect(
      within(t)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Hostname", "State", "Issued", "Expires", "Next renewal", "Last error", "Actions"]);
    const hosts = within(t)
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector("td")?.querySelector(".font-mono")?.textContent);
    expect(hosts).toEqual([
      "app.example.com",
      "beta.example.com",
      "corp.example.com",
      "dead.example.com",
    ]);
    const host = within(t).getByText("app.example.com");
    expect(host.className).toContain("font-mono");
    expect(host.className).toContain("break-all");

    const prod = rowOf(t, "app.example.com");
    expect(within(prod).getByText("Let's Encrypt, production")).toBeTruthy();
    expect(within(prod).getByText("Valid")).toBeTruthy();
    expect(within(prod).queryByText("Staging")).toBeNull();

    const staging = rowOf(t, "beta.example.com");
    expect(within(staging).getByText("Staging")).toBeTruthy();
    expect(within(staging).getByText("Let's Encrypt, staging")).toBeTruthy();

    const uploaded = rowOf(t, "corp.example.com");
    expect(within(uploaded).getByText("Uploaded", { selector: "p" })).toBeTruthy();
    expect(within(uploaded).getByText("Uploaded", { selector: "span" })).toBeTruthy();

    const failed = rowOf(t, "dead.example.com");
    expect(within(failed).getByText("Failed")).toBeTruthy();
    expect(within(failed).getByText("The DNS provider refused the token.")).toBeTruthy();
    expect(within(failed).getByText(/Next attempt/)).toBeTruthy();
  });

  describe("the weekly budget", () => {
    function line(over: Json = {}): Json {
      return {
        registeredDomain: "apps.example.com",
        domains: ["app.apps.example.com"],
        issuer: "production",
        cap: 45,
        limit: 50,
        used: 3,
        headroom: 42,
        shared: false,
        ...over,
      };
    }

    it("writes one line per registered domain and issuer, and the no-headroom and shared sentences", async () => {
      serve([cert()]);
      budget = () =>
        Response.json({
          items: [
            line(),
            line({
              issuer: "staging",
              used: 45,
              headroom: 0,
              nextSlotAt: "2026-11-28T12:00:00Z",
              shared: true,
            }),
            line({ registeredDomain: "other.test", used: 50, cap: 50, headroom: 0 }),
          ],
        });
      mount();
      const group = await screen.findByRole("group", { name: "Certificate budget" });
      const lines = within(group).getAllByText(/new certificates used in the last 7 days/);
      expect(lines).toHaveLength(3);
      expect(lines[0]?.textContent).toBe(
        "apps.example.com: 3 of 45 new certificates used in the last 7 days.",
      );
      expect(lines[1]?.textContent).toContain(
        "apps.example.com (staging): 45 of 45 new certificates used in the last 7 days.",
      );
      expect(lines[1]?.textContent).toContain("TierPlan orders no more until a slot opens");
      expect(lines[1]?.textContent).toContain(
        "Other names under apps.example.com share this budget.",
      );
      // No next slot is known for the third, so its no-headroom sentence is left out.
      expect(lines[2]?.textContent).not.toContain("TierPlan orders no more");
      expect(lines[0]?.textContent).not.toContain("share this budget");
    });

    it("draws no block when there are no budget rows", async () => {
      serve([cert()]);
      mount();
      await table();
      expect(screen.queryByRole("group", { name: "Certificate budget" })).toBeNull();
    });
  });

  describe("empty and partial values", () => {
    it("shows a dash in every cell a certificate has no value for", async () => {
      serve([
        cert({
          state: "queued",
          stateText: "Queued",
          ready: false,
          issuedAt: undefined,
          expiresAt: undefined,
          renewAt: undefined,
        }),
      ]);
      mount();
      const row = rowOf(await table(), "app.example.com");
      // Issued, Expires, Next renewal and Last error.
      expect(within(row).getAllByText("—")).toHaveLength(4);
      expect(within(row).queryByText("Renewal due")).toBeNull();
    });

    it("says Renewal due when the renewal time is now or past, and not before", async () => {
      vi.useFakeTimers({ toFake: ["Date"] });
      vi.setSystemTime(new Date("2026-11-26T12:00:00Z"));
      serve([
        cert({ hostname: "due.example.com", id: "a", renewAt: "2026-11-26T12:00:00Z" }),
        cert({ hostname: "past.example.com", id: "b", renewAt: "2026-11-20T12:00:00Z" }),
        cert({ hostname: "later.example.com", id: "c", renewAt: "2026-11-26T12:00:01Z" }),
      ]);
      mount();
      const t = await table();
      expect(within(rowOf(t, "due.example.com")).getByText("Renewal due")).toBeTruthy();
      expect(within(rowOf(t, "past.example.com")).getByText("Renewal due")).toBeTruthy();
      expect(within(rowOf(t, "later.example.com")).queryByText("Renewal due")).toBeNull();
    });

    it("shows the Expired state for a certificate that expired a second ago", async () => {
      vi.useFakeTimers({ toFake: ["Date"] });
      vi.setSystemTime(new Date("2026-11-26T12:00:00Z"));
      serve([
        // The server has not said so yet: the page's clock does.
        cert({ expiresAt: "2026-11-26T11:59:59Z" }),
        cert({
          id: "c2",
          hostname: "gone.example.com",
          state: "expired",
          stateText: "Expired",
          ready: false,
          expiresAt: "2026-11-26T11:59:59Z",
        }),
      ]);
      mount();
      const t = await table();
      for (const host of ["app.example.com", "gone.example.com"]) {
        const row = rowOf(t, host);
        expect(within(row).getByText("Expired", { selector: "span" })).toBeTruthy();
        expect(within(row).getByText(/^Expired .*ago$/)).toBeTruthy();
      }
    });

    it("tells why a renewal is due in a tooltip on the time", async () => {
      serve([cert({ renewBasis: "A third of the lifetime is left." })]);
      mount();
      const row = rowOf(await table(), "app.example.com");
      const trigger = within(row).getByRole("button", { name: "How the renewal time is chosen" });
      trigger.focus();
      expect(
        (await screen.findAllByText("A third of the lifetime is left.")).length,
      ).toBeGreaterThan(0);
    });

    it("words the state under it for a certificate that is not served, staging-only or waiting in the queue", async () => {
      serve([
        cert({ id: "a", hostname: "a.example.com", ready: false }),
        cert({
          id: "b",
          hostname: "b.example.com",
          state: "dns_proven_on_staging",
          stateText: "DNS proven on staging",
          ready: false,
          expiresAt: undefined,
        }),
        cert({
          id: "c",
          hostname: "c.example.com",
          state: "queued",
          stateText: "Queued",
          ready: false,
          expiresAt: undefined,
          queue: {
            code: "rate_limit_new",
            reason: "45 of 45 new certificates used this week.",
            cap: 45,
            used: 45,
          },
        }),
      ]);
      mount();
      const t = await table();
      expect(within(rowOf(t, "a.example.com")).getByText("Not served yet.")).toBeTruthy();
      expect(
        within(rowOf(t, "b.example.com")).getByText("Staging certificates are never served."),
      ).toBeTruthy();
      expect(
        within(rowOf(t, "c.example.com")).getByText("45 of 45 new certificates used this week."),
      ).toBeTruthy();
    });

    it("says the authority asked TierPlan to wait, in place of the next attempt", async () => {
      serve([
        cert({
          state: "failed",
          stateText: "Failed",
          ready: false,
          lastError: { class: "rate_limited", hint: "The authority is limiting orders." },
          retryAfterUntil: "2026-11-27T12:00:00Z",
          nextAttemptAt: "2026-11-26T12:00:00Z",
        }),
      ]);
      mount();
      const row = rowOf(await table(), "app.example.com");
      expect(within(row).getByText(/^Waiting for the authority until/)).toBeTruthy();
      expect(within(row).queryByText(/^Next attempt/)).toBeNull();
    });
  });

  describe("the authority's detail", () => {
    const failed = (detail: string) =>
      cert({
        state: "failed",
        stateText: "Failed",
        ready: false,
        lastError: {
          class: "dns_token_invalid",
          hint: "The DNS provider refused the token.",
          ...(detail ? { detail } : {}),
        },
      });

    it("opens for an Admin in mono, and a Viewer has no detail control at all", async () => {
      // The server blanks the detail for a Viewer (A-04), so the Viewer's row carries none.
      me = makeMe({ envs: { [ENV]: "admin" } });
      serve([failed("cloudflare: Invalid API token (code 9103)")]);
      const admin = mount();
      const adminRow = rowOf(await table(), "app.example.com");
      const toggle = await within(adminRow).findByRole("button", { name: "Show details" });
      expect(within(adminRow).queryByText(/Invalid API token/)).toBeNull();
      fireEvent.click(toggle);
      const detail = await within(adminRow).findByText(/Invalid API token/);
      expect(detail.className).toContain("font-mono");
      expect(detail.className).toContain("break-all");
      admin.unmount();
      cleanup();
      client.clear();

      me = makeMe({ envs: { [ENV]: "viewer" } });
      serve([failed("")]);
      mount();
      const viewerRow = rowOf(await table(), "app.example.com");
      expect(within(viewerRow).getByText("The DNS provider refused the token.")).toBeTruthy();
      expect(within(viewerRow).queryByRole("button", { name: "Show details" })).toBeNull();
      expect(within(viewerRow).queryByText(/Show details/)).toBeNull();
    });

    it("is not drawn for a Viewer even if the answer carried one", async () => {
      serve([failed("cloudflare: Invalid API token (code 9103)")]);
      mount();
      const row = rowOf(await table(), "app.example.com");
      expect(within(row).queryByRole("button", { name: "Show details" })).toBeNull();
      expect(within(row).queryByText(/Invalid API token/)).toBeNull();
    });
  });

  describe("states", () => {
    it("shows a skeleton while loading", async () => {
      certs = () => new Promise<Response>(() => {}) as unknown as Response;
      mount();
      expect(await screen.findByRole("status")).toBeTruthy();
      expect(screen.getByText("Loading certificates")).toBeTruthy();
      expect(document.querySelector(".h-48")).not.toBeNull();
    });

    it("says there are no certificates yet", async () => {
      mount();
      expect(await screen.findByText("No certificates yet")).toBeTruthy();
      expect(
        screen.getByText(
          "A certificate is ordered when a stack claims a hostname under a registered domain, or when you request one.",
        ),
      ).toBeTruthy();
      expect(screen.queryByRole("table", { name: "Certificates" })).toBeNull();
    });

    it("draws the error with a Retry that reads again", async () => {
      certs = () => Response.json({ title: "boom" }, { status: 500 });
      mount();
      expect(await screen.findByText("Couldn't read the certificates.")).toBeTruthy();
      serve([cert()]);
      fireEvent.click(screen.getByRole("button", { name: "Retry reading certificates" }));
      expect(await screen.findByText("app.example.com")).toBeTruthy();
      expect(screen.queryByText("Couldn't read the certificates.")).toBeNull();
    });

    it("cuts at 10 rows with Show all and Show fewer, in the server's order", async () => {
      serve(
        Array.from({ length: 12 }, (_, i) =>
          cert({ id: `c${i}`, hostname: `h${String(i).padStart(2, "0")}.example.com` }),
        ),
      );
      mount();
      const t = await table();
      expect(within(t).getAllByRole("row")).toHaveLength(11);
      fireEvent.click(screen.getByRole("button", { name: "Show all 12" }));
      expect(within(t).getAllByRole("row")).toHaveLength(13);
      fireEvent.click(screen.getByRole("button", { name: "Show fewer" }));
      expect(within(t).getAllByRole("row")).toHaveLength(11);
    });

    it("applies the filter before the cut, with a line and a Clear filter when nothing matches", async () => {
      serve(
        Array.from({ length: 12 }, (_, i) =>
          cert({ id: `c${i}`, hostname: `h${String(i).padStart(2, "0")}.example.com` }),
        ),
      );
      mount();
      const t = await table();
      const box = screen.getByRole("textbox", { name: "Filter certificates" });
      fireEvent.change(box, { target: { value: "H1" } });
      expect(within(t).getAllByRole("row")).toHaveLength(4);
      expect(screen.queryByRole("button", { name: /Show all/ })).toBeNull();
      fireEvent.change(box, { target: { value: "nothing" } });
      expect(await screen.findByText("No certificate matches “nothing”.")).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Clear filter" }));
      expect(within(t).getAllByRole("row")).toHaveLength(11);
    });

    it("draws Ingress is off when it is off and there are no certificates", async () => {
      status = { enabled: false };
      mount();
      expect(await screen.findByText("Ingress is off")).toBeTruthy();
      expect(screen.queryByText("No certificates yet")).toBeNull();
    });

    it("still draws the table when ingress is off and a certificate exists", async () => {
      status = { enabled: false };
      serve([cert()]);
      mount();
      await table();
      await waitFor(() => expect(screen.queryByText("Ingress is off")).toBeNull());
    });
  });
});
