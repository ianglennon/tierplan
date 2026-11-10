// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSACME, HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { HttpsPage } from "@/features/https/https-page";
import "@/i18n";
import { acmeLastError, httpsLetsEncryptStatus } from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

let status: () => Response;

beforeEach(() => {
  status = () => Response.json(httpsLetsEncryptStatus());
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
