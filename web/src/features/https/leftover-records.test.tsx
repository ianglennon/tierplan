// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AcmeCard } from "@/features/https/acme-card";
import { LeftOverRecords } from "@/features/https/leftover-records";
import "@/i18n";
import { formatClock } from "@/lib/format";
import {
  acmeDNSStatus,
  acmeLastError,
  httpsLetsEncryptStatus,
  httpsStatus,
  leftOverRecord,
} from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The left-over TXT records list (plan 15-11, CHAL-04, D-16, D-17, D-18, D-23, UI-SPEC E7).

afterEach(cleanup);

function draw(leftOver: ReturnType<typeof leftOverRecord>[] | null) {
  return render(
    <TooltipProvider>
      <LeftOverRecords dns={acmeDNSStatus({ leftOver })} />
    </TooltipProvider>,
  );
}

const alert = () => screen.getByRole("note");
const items = () => within(alert()).getAllByRole("listitem");

describe("E7: the left-over list on its own", () => {
  it("renders no alert when the status lists no records (empty)", () => {
    const { container, unmount } = draw([]);
    expect(container.innerHTML).toBe("");
    unmount();
    const none = draw(null);
    expect(none.container.innerHTML).toBe("");
  });

  it("words one record in the singular and two in the plural, oldest first as given (zero-one-many)", () => {
    const { unmount } = draw([leftOverRecord()]);
    expect(screen.getByText("A TXT record is left in Cloudflare")).toBeTruthy();
    expect(items()).toHaveLength(1);
    unmount();
    draw([
      leftOverRecord({ recordId: "rec-old", name: "_acme-challenge.old.example.com" }),
      leftOverRecord({ recordId: "rec-new", name: "_acme-challenge.new.example.com" }),
    ]);
    expect(screen.getByText("2 TXT records are left in Cloudflare")).toBeTruthy();
    expect(items().map((li) => li.textContent)).toEqual([
      expect.stringContaining("_acme-challenge.old.example.com"),
      expect.stringContaining("_acme-challenge.new.example.com"),
    ]);
  });

  it("explains itself with the intro and the footer naming the tierplan: comment", () => {
    draw([leftOverRecord()]);
    expect(
      screen.getByText(
        "TierPlan couldn't delete these records it created. It keeps trying and removes them as soon as it can. The order itself was not affected.",
      ),
    ).toBeTruthy();
    const footer = screen.getByText(/You can also delete a record yourself/);
    expect(footer.textContent).toBe(
      "You can also delete a record yourself in the Cloudflare dashboard. TierPlan's records have a comment that starts with tierplan:.",
    );
    expect(within(footer).getByText("tierplan:").tagName).toBe("CODE");
  });

  it("is a warning note with the triangle mark", () => {
    draw([leftOverRecord()]);
    const el = alert();
    expect(el.getAttribute("data-variant") ?? el.className).toMatch(/warning/);
    expect(el.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
  });

  it("names each record and its id in mono text (populated)", () => {
    draw([leftOverRecord({ recordId: "cf-9f8e7d" })]);
    const li = items()[0] as HTMLElement;
    expect(li.textContent).toContain(
      "TXT record left at _acme-challenge.tierplan.example.com (id cf-9f8e7d)",
    );
    const codes = Array.from(li.querySelectorAll("code")).map((c) => c.textContent);
    expect(codes).toEqual(["_acme-challenge.tierplan.example.com", "cf-9f8e7d"]);
  });

  it("shows the classified headline of the delete error as the reason (error)", () => {
    draw([leftOverRecord({ code: "dns_token_scope" })]);
    expect(
      screen.getByText("The Cloudflare token can't manage DNS records for tierplan.example.com."),
    ).toBeTruthy();
  });

  it("falls back to the internal headline for a class it has no words for (error)", () => {
    draw([leftOverRecord({ code: "from_a_future_version" })]);
    expect(screen.getByText("Something went wrong inside TierPlan.")).toBeTruthy();
  });

  it("gives no reason when no delete was tried yet", () => {
    draw([leftOverRecord({ code: undefined, lastTryAt: undefined })]);
    expect(screen.queryByText("Something went wrong inside TierPlan.")).toBeNull();
    expect(items()[0]?.textContent).not.toContain("Last tried");
  });

  it("says when it was last tried and when it tries next", () => {
    const next = new Date(Date.now() + 600_000).toISOString();
    draw([leftOverRecord({ nextTryAt: next })]);
    const li = items()[0] as HTMLElement;
    expect(li.textContent).toContain("Last tried 2 min ago.");
    expect(li.textContent).toContain(`Next try ${formatClock(next)}.`);
  });

  it("shows the restore pause in place of the next try (partial, D-23)", () => {
    draw([leftOverRecord({ retryPaused: true, pausedReason: "restore", nextTryAt: undefined })]);
    const li = items()[0] as HTMLElement;
    expect(li.textContent).toContain("Retries are paused until you resume Let's Encrypt.");
    expect(li.textContent).not.toContain("Next try");
  });

  it("shows the certificate-files pause in place of the next try, naming the variables in mono (partial, D-23)", () => {
    draw([leftOverRecord({ retryPaused: true, pausedReason: "cert_files", nextTryAt: undefined })]);
    const li = items()[0] as HTMLElement;
    expect(li.textContent).toContain(
      "Retries are paused while TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE are set.",
    );
    expect(li.textContent).not.toContain("Next try");
    expect(Array.from(li.querySelectorAll("code")).map((c) => c.textContent)).toEqual(
      expect.arrayContaining(["TIERPLAN_TLS_CERT_FILE", "TIERPLAN_TLS_KEY_FILE"]),
    );
  });

  it("wraps a long name, id and reason anywhere (long-text)", () => {
    const name = `_acme-challenge.${"long-label.".repeat(12)}example.com`;
    draw([leftOverRecord({ name, recordId: "r".repeat(120) })]);
    const li = items()[0] as HTMLElement;
    for (const code of Array.from(li.querySelectorAll("code"))) {
      expect(code.className).toContain("break-all");
    }
    expect(li.className).toContain("min-w-0");
    expect(li.className).toContain("break-words");
  });

  it("draws a name the server sent as text, never as markup", () => {
    draw([leftOverRecord({ name: "<img src=x onerror=alert(1)>" })]);
    expect(alert().querySelector("img")).toBeNull();
    expect(alert().textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it("lists every record with no cap and no collapse (overflow)", () => {
    draw(
      Array.from({ length: 12 }, (_, i) =>
        leftOverRecord({ recordId: `rec-${i}`, name: `_acme-challenge.h${i}.example.com` }),
      ),
    );
    expect(screen.getByText("12 TXT records are left in Cloudflare")).toBeTruthy();
    expect(items()).toHaveLength(12);
    expect(within(alert()).queryByRole("button")).toBeNull();
  });

  it("offers no button and no link: the page never deletes a record by its name (T-15-37)", () => {
    draw([leftOverRecord(), leftOverRecord({ recordId: "rec-2" })]);
    expect(within(alert()).queryAllByRole("button")).toEqual([]);
    expect(within(alert()).queryAllByRole("link")).toEqual([]);
  });
});

// The list on the card: it comes with the status, in every state and under either saved method.
let status: () => Response | Promise<Response>;

beforeEach(() => {
  status = () => Response.json(httpsStatus());
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
  vi.unstubAllGlobals();
  queryClient.clear();
});

function renderCard() {
  return render(
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
const LEFT = { leftOver: [leftOverRecord()] };

const cardStates: [string, (dns: HTTPSStatus["acme"]) => HTTPSStatus][] = [
  ["D (served, healthy)", () => httpsLetsEncryptStatus({ dns: acmeDNSStatus(LEFT) })],
  [
    "F (last run failed)",
    () => httpsLetsEncryptStatus({ dns: acmeDNSStatus(LEFT), lastError: acmeLastError() }),
  ],
  [
    "H (paused after a restore)",
    () =>
      httpsLetsEncryptStatus({
        dns: acmeDNSStatus({
          leftOver: [leftOverRecord({ retryPaused: true, pausedReason: "restore" })],
        }),
        pausedReason: "restore",
      }),
  ],
  [
    "J (certificate files)",
    () =>
      httpsLetsEncryptStatus(
        {
          dns: acmeDNSStatus({
            leftOver: [leftOverRecord({ retryPaused: true, pausedReason: "cert_files" })],
          }),
          pausedReason: "cert_files",
        },
        { certFiles: true },
      ),
  ],
];

describe("E7: the left-over list on the Let's Encrypt card", () => {
  it.each(cardStates)("is drawn in state %s", async (_name, make) => {
    status = () => Response.json(make({} as never));
    renderCard();
    expect(await card().findByText("A TXT record is left in Cloudflare")).toBeTruthy();
  });

  it.each(["http-01", "dns-01"] as const)(
    "is drawn when the saved method is %s",
    async (method) => {
      status = () =>
        Response.json(httpsLetsEncryptStatus({ challengeType: method, dns: acmeDNSStatus(LEFT) }));
      renderCard();
      expect(await card().findByText("A TXT record is left in Cloudflare")).toBeTruthy();
    },
  );

  it("sits at the top of Run and actions, above the state alerts", async () => {
    status = () =>
      Response.json(httpsLetsEncryptStatus({ dns: acmeDNSStatus(LEFT), pausedReason: "restore" }));
    renderCard();
    const title = await card().findByText("A TXT record is left in Cloudflare");
    const restore = await card().findByText("Paused after restore");
    const order = title.compareDocumentPosition(restore);
    expect(order & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("is not drawn when the status lists no record", async () => {
    status = () => Response.json(httpsLetsEncryptStatus());
    renderCard();
    await card().findByText("Names on the certificate");
    expect(card().queryByText(/TXT records? (is|are) left/)).toBeNull();
  });

  it("renders nothing until the status has arrived (loading)", async () => {
    let release: (r: Response) => void = () => {};
    const pending = new Promise<Response>((resolve) => {
      release = resolve;
    });
    status = () => pending;
    renderCard();
    expect(card().queryByText(/TXT records? (is|are) left/)).toBeNull();
    release(Response.json(httpsLetsEncryptStatus({ dns: acmeDNSStatus(LEFT) })));
    await waitFor(() =>
      expect(card().getByText("A TXT record is left in Cloudflare")).toBeTruthy(),
    );
  });
});
