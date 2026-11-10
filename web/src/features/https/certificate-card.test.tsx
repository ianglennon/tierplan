// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSCertificate, HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CertificateCard } from "@/features/https/certificate-card";
import { HttpsPage } from "@/features/https/https-page";
import "@/i18n";
import { httpsStatus } from "@/test-https";

let answer: () => Response | Promise<Response>;

beforeEach(() => {
  answer = () => Response.json(httpsStatus());
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/https/status") return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
});

function renderCard() {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <CertificateCard />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

function withCertificate(over: Partial<HTTPSCertificate>): HTTPSStatus {
  const base = httpsStatus();
  return { ...base, certificate: { ...base.certificate, ...over } };
}

const card = () => within(screen.getByRole("region", { name: "Certificate in use" }));

// The dd that follows the dt with this label.
function rowValue(label: string): HTMLElement {
  const dt = card().getByText(label, { selector: "dt" });
  const dd = dt.nextElementSibling;
  if (!(dd instanceof HTMLElement)) throw new Error(`no value for ${label}`);
  return dd;
}

describe("Certificate in use card", () => {
  it("shows three skeleton rows while the status is pending, and no certificate", () => {
    answer = () => new Promise<Response>(() => {});
    renderCard();
    const loading = screen.getByTestId("certificate-loading");
    expect(loading.children).toHaveLength(3);
    expect(card().queryByText("Source")).toBeNull();
    expect(card().queryByText("Self-signed")).toBeNull();
  });

  it("shows the unreachable alert inside the card when the status cannot be read", async () => {
    answer = () => Response.json({}, { status: 500 });
    renderCard();
    const alert = await card().findByRole("alert");
    expect(alert.textContent).toBe("Can't reach TierPlan. Check your connection and try again.");
    expect(screen.queryByTestId("certificate-loading")).toBeNull();
    expect(card().queryByText("Source")).toBeNull();
  });

  it("renders a self-signed certificate with its badge, rows and note", async () => {
    // Half a day over, so the whole days left stay 364 whatever the clock reads while the test runs.
    const notAfter = new Date(Date.now() + 364.5 * 86_400_000).toISOString();
    answer = () => Response.json(withCertificate({ notAfter }));
    renderCard();
    await card().findByText("Source");
    expect(within(rowValue("Source")).getByText("Self-signed")).toBeTruthy();
    expect(rowValue("Issuer").textContent).toBe("TierPlan");
    expect(rowValue("Expires").textContent).toContain("Expires in 364 days");
    expect(
      card().getByText(
        "TierPlan made this certificate for this install. Browsers warn about it until you use Let's Encrypt or your own certificate files.",
      ),
    ).toBeTruthy();
  });

  it("renders a certificate-files certificate with its own badge and note", async () => {
    answer = () => Response.json(withCertificate({ source: "cert_files", issuer: "Example CA" }));
    renderCard();
    await card().findByText("Source");
    expect(within(rowValue("Source")).getByText("Certificate files")).toBeTruthy();
    expect(rowValue("Issuer").textContent).toBe("Example CA");
    expect(
      card().getByText(
        "TierPlan serves the files named by TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE.",
      ),
    ).toBeTruthy();
    expect(card().queryByText(/made this certificate/)).toBeNull();
  });

  it("shows Unknown when the issuer is empty", async () => {
    answer = () => Response.json(withCertificate({ issuer: "" }));
    renderCard();
    await card().findByText("Source");
    expect(rowValue("Issuer").textContent).toBe("Unknown");
  });

  it("lists the IP addresses of a certificate that has only IP names", async () => {
    answer = () => Response.json(withCertificate({ names: ["192.168.1.20", "::1"] }));
    renderCard();
    await card().findByText("Source");
    const items = within(rowValue("Names")).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual(["192.168.1.20", "::1"]);
  });

  it("lists every one of six names with no 'and N more'", async () => {
    const names = ["a.example.com", "b.example.com", "localhost", "127.0.0.1", "::1", "10.0.0.5"];
    answer = () => Response.json(withCertificate({ names }));
    renderCard();
    await card().findByText("Source");
    const items = within(rowValue("Names")).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual(names);
    expect(card().queryByText(/more/)).toBeNull();
  });

  it("renders one name as the same list", async () => {
    answer = () => Response.json(withCertificate({ names: ["tierplan.example.com"] }));
    renderCard();
    await card().findByText("Source");
    const list = within(rowValue("Names")).getByRole("list");
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(list.textContent).toBe("tierplan.example.com");
  });

  it("wraps a 120 character host and a long issuer instead of widening the column", async () => {
    const host = `${"a".repeat(55)}.${"b".repeat(55)}.example.com`;
    const issuer = "The Very Long Certificate Authority Name ".repeat(5).trim();
    answer = () => Response.json(withCertificate({ names: [host], issuer }));
    renderCard();
    await card().findByText("Source");
    expect(host.length).toBeGreaterThanOrEqual(120);
    expect(rowValue("Names").className).toContain("break-all");
    expect(rowValue("Names").className).toContain("min-w-0");
    expect(rowValue("Issuer").className).toContain("break-all");
    expect(rowValue("Issuer").className).toContain("font-mono");
    expect(within(rowValue("Names")).getByText(host).className).toContain("font-mono");
    expect(rowValue("Issuer").textContent).toBe(issuer);
  });

  it("shows a punycode name as the certificate carries it, and markup as text", async () => {
    const punycode = "xn--bcher-kva.example";
    const markup = "<img src=x onerror=alert(1)>";
    answer = () => Response.json(withCertificate({ names: [punycode], issuer: markup }));
    renderCard();
    await card().findByText("Source");
    expect(within(rowValue("Names")).getByText(punycode)).toBeTruthy();
    expect(card().queryByText("bücher.example")).toBeNull();
    expect(rowValue("Issuer").textContent).toBe(markup);
    expect(rowValue("Issuer").querySelector("img")).toBeNull();
  });

  it("shows no renewal or error rows outside Let's Encrypt", async () => {
    renderCard();
    await card().findByText("Source");
    expect(card().queryByText("Last renewed")).toBeNull();
    expect(card().queryByText("Next renewal")).toBeNull();
    expect(card().queryByText("Last error")).toBeNull();
    expect(
      card()
        .getAllByRole("term")
        .map((dt) => dt.textContent),
    ).toEqual(["Source", "Issuer", "Names", "Expires"]);
  });
});

describe("HTTPS page", () => {
  it("puts the heading and description above the certificate card", async () => {
    render(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <HttpsPage />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    expect(screen.getByRole("heading", { level: 2, name: "HTTPS" })).toBeTruthy();
    expect(
      screen.getByText(
        "TierPlan always serves HTTPS. See the certificate it is using, or get a free one from Let's Encrypt that browsers trust.",
      ),
    ).toBeTruthy();
    await card().findByText("Source");
  });
});
