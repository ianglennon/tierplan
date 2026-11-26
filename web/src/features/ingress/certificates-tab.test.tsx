// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
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
  me = makeMe({ envs: { [ENV]: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/certificates`) return certs();
      if (path === `/api/env/${ENV}/ingress/certificates/budget`) return budget();
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json({ enabled: true });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
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
});
