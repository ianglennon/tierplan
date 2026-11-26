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

let items: Json[];
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

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

beforeEach(() => {
  setCsrfToken("csrf");
  items = [cert()];
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/certificates`) return Response.json({ items });
      if (path === `/api/env/${ENV}/ingress/certificates/budget`)
        return Response.json({ items: [] });
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json({ enabled: true });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
  setCsrfToken(null);
});

async function openDetails(host = "app.example.com") {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <CertificatesTab envId={ENV} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  const button = await screen.findByRole("button", { name: `Actions for ${host}` });
  fireEvent.keyDown(button, { key: "Enter" });
  fireEvent.click(await screen.findByRole("menuitem", { name: "View details" }));
  return screen.findByRole("dialog", { name: "Certificate details" });
}

const labels = (sheet: HTMLElement) =>
  Array.from(sheet.querySelectorAll("dt")).map((dt) => dt.textContent);

describe("the certificate detail sheet", () => {
  it("opens titled Certificate details with the hostname in mono", async () => {
    const sheet = await openDetails();
    const host = within(sheet).getByText("app.example.com");
    expect(host.className).toContain("font-mono");
    expect(labels(sheet)).toEqual([
      "Source",
      "Issuer",
      "State",
      "Ready",
      "Issued",
      "Expires",
      "Next renewal",
    ]);
  });

  it("leaves out Retired and Stopped reason when the certificate has neither", async () => {
    const sheet = await openDetails();
    const names = labels(sheet);
    expect(names).not.toContain("Retired");
    expect(names).not.toContain("Retired because");
    expect(names).not.toContain("Stopped reason");
    expect(names).not.toContain("Last error");
    expect(sheet.textContent).not.toContain("—");
  });

  it("draws the retired and stopped entries when the certificate has them", async () => {
    items = [
      cert({
        state: "retired",
        stateText: "Retired",
        ready: false,
        retiredAt: "2026-11-20T10:00:00Z",
        retiredReason: "Replaced by an upload",
        stoppedReason: "No claim refers to it",
      }),
    ];
    const sheet = await openDetails();
    expect(labels(sheet)).toEqual(
      expect.arrayContaining(["Retired", "Retired because", "Stopped reason"]),
    );
    expect(within(sheet).getByText("Replaced by an upload")).toBeTruthy();
  });

  it("shows an Admin who asked for it and the error's detail", async () => {
    items = [
      cert({
        state: "failed",
        stateText: "Failed",
        ready: false,
        consecutiveFailures: 3,
        requestedAt: "2026-11-02T10:00:00Z",
        requestedBy: "user-7",
        lastError: {
          class: "dns_token_invalid",
          hint: "The DNS provider refused the token.",
          detail: "cloudflare: 9109 invalid access token",
        },
      }),
    ];
    const sheet = await openDetails();
    expect(within(sheet).getByText("by user-7")).toBeTruthy();
    expect(within(sheet).getByText("cloudflare: 9109 invalid access token")).toBeTruthy();
    expect(within(sheet).getByText("The DNS provider refused the token.")).toBeTruthy();
    // The last error closes the list.
    expect(labels(sheet).at(-1)).toBe("Last error");
    expect(labels(sheet)).toContain("Consecutive failures");
  });

  it("shows a Viewer the time it was requested and no detail", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    items = [
      cert({
        state: "failed",
        stateText: "Failed",
        ready: false,
        requestedAt: "2026-11-02T10:00:00Z",
        lastError: { class: "dns_token_invalid", hint: "The DNS provider refused the token." },
      }),
    ];
    const sheet = await openDetails();
    await waitFor(() => expect(labels(sheet)).toContain("Requested"));
    expect(sheet.textContent).not.toMatch(/ by /);
    expect(within(sheet).getByText("The DNS provider refused the token.")).toBeTruthy();
    expect(sheet.querySelector(".font-mono.break-all")?.textContent).toBe("app.example.com");
  });

  it("has no control that writes", async () => {
    const sheet = await openDetails();
    const buttons = within(sheet)
      .queryAllByRole("button")
      .map((b) => b.textContent);
    expect(buttons).toEqual(["Close"]);
    expect(sheet.querySelector("form, input, textarea")).toBeNull();
  });
});
