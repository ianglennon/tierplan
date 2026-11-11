// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSACME, HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AcmeCard } from "@/features/https/acme-card";
import "@/i18n";
import {
  acmeDNSStatus,
  httpsACME,
  httpsStatus,
  tokenTestName,
  tokenTestResult,
} from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The DNS-01 controls beside Save: Test token, and Advanced DNS with the resolvers (plan 15-10,
// CHAL-02, CHAL-05).

interface Sent {
  path: string;
  method: string;
  body?: Record<string, unknown>;
}

const TOKEN = "cf-secret-token-0123456789";
const REGISTERED_AT = "2026-11-10T08:00:00Z";

let sent: Sent[];
let status: () => Response | Promise<Response>;
let testToken: (body: Record<string, unknown>) => Response | Promise<Response>;
let put: (body: Record<string, unknown>) => Response | Promise<Response>;

function withACME(over: Partial<HTTPSACME>): HTTPSStatus {
  return httpsStatus({
    acme: httpsACME({
      account: { contact: "ops@example.com", registeredAt: REGISTERED_AT, termsOfServiceUrl: "" },
      ...over,
    }),
  });
}

beforeEach(() => {
  sent = [];
  status = () => Response.json(withACME({}));
  testToken = () => Response.json(tokenTestResult());
  put = () =>
    Response.json({ challengeType: "dns-01", resolvers: [], saved: true, tokenSaved: true });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "PUT" || req.method === "POST") {
        const text = await req.clone().text();
        if (text !== "") entry.body = JSON.parse(text);
      }
      sent.push(entry);
      if (path === "/api/me") return Response.json(makeMe({ instanceAdmin: true }));
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/directory") {
        return Response.json({ overridden: false, profiles: [], termsOfServiceUrl: "", url: "" });
      }
      if (path === "/api/https/acme/dns/test" && req.method === "POST") {
        return testToken(entry.body ?? {});
      }
      if (path === "/api/https/acme/validation" && req.method === "PUT")
        return put(entry.body ?? {});
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

const card = () => within(screen.getByRole("region", { name: "Let's Encrypt" }));
const puts = () => sent.filter((s) => s.method === "PUT");
const tests = () => sent.filter((s) => s.path === "/api/https/acme/dns/test");

async function ready() {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>
          <AcmeCard />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await card().findByText("Names on the certificate");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

const pickDNS = () => fireEvent.click(card().getByRole("radio", { name: /DNS-01/ }));
const tokenInput = () => card().getByLabelText("Cloudflare API token") as HTMLInputElement;
const testButton = () => card().getByRole("button", { name: /Test token|Testing token/ });
const saveButton = () => card().getByRole("button", { name: /Save validation settings|Saving/ });
const type = (input: HTMLInputElement, value: string) =>
  fireEvent.change(input, { target: { value } });

describe("Test token", () => {
  it("is a button of its own before Save, with the helper under the row", async () => {
    await ready();
    pickDNS();
    const buttons = card()
      .getAllByRole("button")
      .filter((b) => /Test token|Save validation settings/.test(b.textContent ?? ""));
    expect(buttons.map((b) => b.textContent)).toEqual(["Test token", "Save validation settings"]);
    expect(
      card().getByText(
        "Test token checks the names above without placing an order. It adds a test TXT record at each name and deletes it again.",
      ),
    ).toBeTruthy();
  });

  it("is not offered under an HTTP-01 draft", async () => {
    await ready();
    expect(card().queryByRole("button", { name: "Test token" })).toBeNull();
  });

  it("sends the typed token and shows the per-name result, storing nothing", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    const statusReads = sent.filter((s) => s.path === "/api/https/status").length;
    fireEvent.click(testButton());

    await card().findByText("Token test passed");
    expect(tests()).toHaveLength(1);
    expect(tests()[0]?.body).toEqual({ token: TOKEN });
    expect(card().getByText("Read back.")).toBeTruthy();
    expect(card().getByText(/Seen by/)).toBeTruthy();
    expect(card().queryByText("Nothing was saved. The settings in use are unchanged.")).toBeNull();
    expect(puts()).toHaveLength(0);
    expect(sent.filter((s) => s.path === "/api/https/status")).toHaveLength(statusReads);
    // The typed token stays in the field: a test is not a save.
    expect(tokenInput().value).toBe(TOKEN);
  });

  it("tests the saved token when none is typed", async () => {
    status = () =>
      Response.json(
        withACME({ challengeType: "dns-01", dns: acmeDNSStatus({ tokenSaved: true }) }),
      );
    await ready();
    fireEvent.click(testButton());

    await card().findByText("Token test passed");
    expect(tests()).toHaveLength(1);
    expect(tests()[0]?.body).toEqual({});
  });

  it("shows the failing name's result when the token fails", async () => {
    testToken = () =>
      Response.json(
        tokenTestResult({
          passed: false,
          names: [
            tokenTestName({
              steps: [
                { id: "zone", state: "done" },
                { id: "created", state: "failed" },
                { id: "read", state: "skipped" },
                { id: "deleted", state: "skipped" },
              ],
              error: { code: "dns_token_scope", host: "tierplan.example.com" },
              info: { resolvers: null, seconds: 0, state: "not_checked" },
            }),
          ],
        }),
      );
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(testButton());
    await card().findByText("Token test failed");
    expect(puts()).toHaveLength(0);
  });

  it("says there is nothing to test, and sends nothing, with no token typed or saved", async () => {
    await ready();
    pickDNS();
    fireEvent.click(testButton());
    expect(await card().findByText("Enter a Cloudflare API token to test.")).toBeTruthy();
    expect(tests()).toHaveLength(0);
  });

  it("shows 'Testing token...', the hint and both buttons aria-disabled while it runs, with the previous result cleared", async () => {
    let answer: (r: Response) => void = () => {};
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(testButton());
    await card().findByText("Token test passed");

    testToken = () => new Promise<Response>((resolve) => (answer = resolve));
    fireEvent.click(testButton());
    const pressed = await card().findByRole("button", { name: /Testing token/ });
    expect(pressed.getAttribute("aria-disabled")).toBe("true");
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");
    expect(
      card().getByText("This can take a minute while TierPlan waits for the record to appear."),
    ).toBeTruthy();
    expect(card().queryByText("Token test passed")).toBeNull();
    expect(card().queryByRole("status")).toBeNull();

    answer(Response.json(tokenTestResult()));
    await card().findByText("Token test passed");
    expect(
      card().queryByText("This can take a minute while TierPlan waits for the record to appear."),
    ).toBeNull();
  });

  it("shows the generic failure when the server refuses the test", async () => {
    testToken = () => Response.json({ title: "no" }, { status: 500 });
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(testButton());
    expect(await card().findByText("Something went wrong. Try again.")).toBeTruthy();
  });
});
