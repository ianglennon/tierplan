// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
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
  validationResult,
} from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Validation section of the Let's Encrypt card: the challenge picker, the DNS-01 detail, the
// write-only token and the save that tests a new token first (plan 15-09, CHAL-01, CHAL-02).

interface Sent {
  path: string;
  method: string;
  body?: Record<string, unknown>;
}

const TOKEN = "cf-secret-token-0123456789";
const REGISTERED_AT = "2026-11-10T08:00:00Z";

let sent: Sent[];
let status: () => Response | Promise<Response>;
let put: (body: Record<string, unknown>) => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;

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
  put = () => Response.json(validationResult());
  successToast = vi.spyOn(toast, "success");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "PUT") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/me") return Response.json(makeMe({ instanceAdmin: true }));
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/directory") {
        return Response.json({ overridden: false, profiles: [], termsOfServiceUrl: "", url: "" });
      }
      if (path === "/api/https/acme/validation" && req.method === "PUT") {
        return put(entry.body ?? {});
      }
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
const saveButton = () => card().getByRole("button", { name: /Save validation settings|Saving/ });
const type = (input: HTMLInputElement, value: string) =>
  fireEvent.change(input, { target: { value } });

describe("Validation section: the picker", () => {
  it("offers HTTP-01 and DNS-01 as a radio group named by the heading, HTTP-01 selected", async () => {
    await ready();
    const group = card().getByRole("radiogroup", { name: "Validation method" });
    const radios = within(group).getAllByRole("radio");
    expect(radios.map((r) => r.getAttribute("aria-checked"))).toEqual(["true", "false"]);
    expect(within(group).getByText("HTTP-01")).toBeTruthy();
    expect(within(group).getByText("DNS-01")).toBeTruthy();
    expect(card().getByText(/A change applies from the next order/)).toBeTruthy();
  });

  it("starts on the saved method and shows the DNS-01 detail for it", async () => {
    status = () => Response.json(withACME({ challengeType: "dns-01" }));
    await ready();
    expect(
      card()
        .getByRole("radio", { name: /DNS-01/ })
        .getAttribute("aria-checked"),
    ).toBe("true");
    expect(card().getByText("DNS provider")).toBeTruthy();
    expect(card().queryByText(/Port 80 on tierplan.example.com must reach/)).toBeNull();
  });

  it("shows the port 80 note under HTTP-01 and the setup note under DNS-01, nothing requested", async () => {
    await ready();
    expect(card().getByText(/Port 80 on tierplan.example.com must reach TierPlan/)).toBeTruthy();
    const before = sent.length;
    pickDNS();
    expect(card().getByText("DNS provider")).toBeTruthy();
    expect(card().getByText("Cloudflare")).toBeTruthy();
    expect(card().queryByText(/Port 80 on tierplan.example.com must reach TierPlan/)).toBeNull();
    expect(sent.length).toBe(before);
  });

  it("explains the Cloudflare token: both permissions in mono, the Global API Key refusal and the link", async () => {
    await ready();
    pickDNS();
    const note = card()
      .getAllByRole("note")
      .find((n) => n.textContent?.includes("Zone:DNS:Edit")) as HTMLElement;
    expect(note.textContent).toContain("Zone:Zone:Read");
    expect(note.textContent).toContain("TierPlan doesn't accept the Global API Key");
    expect(within(note).getByText("Zone:DNS:Edit").tagName).toBe("CODE");
    expect(within(note).getByText("Zone:Zone:Read").tagName).toBe("CODE");
    const link = within(note).getByRole("link", { name: "Cloudflare's API tokens page" });
    expect(link.getAttribute("href")).toBe("https://dash.cloudflare.com/profile/api-tokens");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(note.textContent).toContain("It only deletes records it created.");
  });
});

describe("Validation section: the token field", () => {
  it("is a write-only password field with no autofill, capitals or spell check, and a helper", async () => {
    await ready();
    pickDNS();
    const input = tokenInput();
    expect(input.type).toBe("password");
    expect(input.getAttribute("autocomplete")).toBe("new-password");
    expect(input.getAttribute("autocapitalize")).toBe("off");
    expect(input.getAttribute("spellcheck")).toBe("false");
    expect(input.value).toBe("");
    expect(
      card().getByText(/TierPlan keeps the token encrypted and never shows it again/),
    ).toBeTruthy();
  });

  it("shows and hides what was typed with a pressed-state button", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    const show = card().getByRole("button", { name: "Show token" });
    expect(show.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(show);
    expect(tokenInput().type).toBe("text");
    expect(card().getByRole("button", { name: "Hide token" }).getAttribute("aria-pressed")).toBe(
      "true",
    );
  });
});

describe("Validation section: saving a new token", () => {
  it("sends the method and the token, then shows only that a token is saved", async () => {
    put = () => {
      status = () =>
        Response.json(
          withACME({ challengeType: "dns-01", dns: acmeDNSStatus({ tokenSaved: true }) }),
        );
      return Response.json(validationResult());
    };
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());

    await card().findByText("Token saved");
    expect(puts()).toHaveLength(1);
    expect(puts()[0]?.body).toEqual({ challengeType: "dns-01", token: TOKEN });
    expect(card().queryByLabelText("Cloudflare API token")).toBeNull();
    expect(card().getByText("TierPlan keeps it encrypted and never shows it.")).toBeTruthy();
    expect(successToast).toHaveBeenCalledTimes(1);
    const [node] = successToast.mock.calls[0] as [{ props: { children: string } }];
    expect(node.props.children).toBe("Validation settings saved.");
    // Neither the typed token nor a mask of it is anywhere on the page.
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(document.body.innerHTML).not.toContain(TOKEN);
  });

  it("starts the replacement field empty after the save: the typed token left the form", async () => {
    put = () => {
      status = () =>
        Response.json(
          withACME({ challengeType: "dns-01", dns: acmeDNSStatus({ tokenSaved: true }) }),
        );
      return Response.json(validationResult());
    };
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());
    await card().findByText("Token saved");

    fireEvent.click(card().getByRole("button", { name: "Replace token" }));
    const field = card().getByLabelText("New Cloudflare API token") as HTMLInputElement;
    expect(field.value).toBe("");
    expect(card().getByRole("button", { name: "Keep the saved token" })).toBeTruthy();
  });

  it("shows 'Testing token...' while the server tests the token, then 'Saving...' until the status is read", async () => {
    let answer: (r: Response) => void = () => {};
    let readStatus: () => void = () => {};
    put = () => new Promise<Response>((resolve) => (answer = resolve));
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());
    expect(await card().findByRole("button", { name: /Testing token/ })).toBeTruthy();

    status = () =>
      new Promise<Response>((resolve) => {
        readStatus = () =>
          resolve(
            Response.json(
              withACME({ challengeType: "dns-01", dns: acmeDNSStatus({ tokenSaved: true }) }),
            ),
          );
      });
    answer(Response.json(validationResult()));
    expect(await card().findByRole("button", { name: /Saving\.\.\./ })).toBeTruthy();
    readStatus();
    await card().findByText("Token saved");
  });
});

describe("Validation section: a token that fails its test", () => {
  const scopeFailure = () =>
    validationResult({
      saved: false,
      tokenSaved: false,
      test: tokenTestResult({
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
    });

  it("saves nothing: shows the result, the not-saved line and the headline, with no toast", async () => {
    put = () => Response.json(scopeFailure());
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());

    await card().findByText("Token test failed");
    expect(card().getByText("Nothing was saved. The settings in use are unchanged.")).toBeTruthy();
    expect(
      card().getByText("The Cloudflare token can't manage DNS records for tierplan.example.com."),
    ).toBeTruthy();
    expect(successToast).not.toHaveBeenCalled();
    expect(card().queryByText("Token saved")).toBeNull();
  });

  it("keeps the typed token in the field so it can be fixed and tried again", async () => {
    put = () => Response.json(scopeFailure());
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());
    await card().findByText("Token test failed");
    expect(tokenInput().value).toBe(TOKEN);
  });
});
