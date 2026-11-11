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
import { type CardState, validationMode } from "@/features/https/card-state";
import "@/i18n";
import {
  acmeDNSStatus,
  httpsACME,
  httpsCertFilesStatus,
  httpsRestoredStatus,
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
let me: () => Response;
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
  me = () => Response.json(makeMe({ instanceAdmin: true }));
  put = () => Response.json(validationResult());
  successToast = vi.spyOn(toast, "success");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "PUT") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/me") return me();
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

const pickHTTP = () => fireEvent.click(card().getByRole("radio", { name: /HTTP-01/ }));
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
    expect(puts()[0]?.body).toEqual({ challengeType: "dns-01", token: TOKEN, resolvers: [] });
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

// A saved DNS-01 setup with a token, and an HTTP-01 one that still keeps a token for later.
const dnsSaved = () =>
  withACME({ challengeType: "dns-01", dns: acmeDNSStatus({ tokenSaved: true }) });
const httpWithToken = () => withACME({ dns: acmeDNSStatus({ tokenSaved: true }) });

const scopeFailed = () =>
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
          ],
          error: { code: "dns_token_scope", host: "tierplan.example.com" },
          info: { resolvers: null, seconds: 0, state: "not_checked" },
        }),
      ],
    }),
  });

const isAriaDisabled = (button: HTMLElement) => button.getAttribute("aria-disabled") === "true";

describe("Validation section: leaving DNS-01 discards the DNS-01 draft (J9)", () => {
  it("clears the typed token: DNS-01 again shows an empty field", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    pickHTTP();
    pickDNS();
    expect(tokenInput().value).toBe("");
  });

  it("cancels replacing: DNS-01 again shows the Token saved row, not the field", async () => {
    status = () => Response.json(httpWithToken());
    await ready();
    pickDNS();
    fireEvent.click(card().getByRole("button", { name: "Replace token" }));
    expect(card().getByLabelText("New Cloudflare API token")).toBeTruthy();
    pickHTTP();
    pickDNS();
    expect(card().getByText("Token saved")).toBeTruthy();
    expect(card().queryByLabelText("New Cloudflare API token")).toBeNull();
  });

  it("removes the result of a failed save", async () => {
    put = () => Response.json(scopeFailed());
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());
    await card().findByText("Token test failed");
    pickHTTP();
    expect(card().queryByText("Token test failed")).toBeNull();
    expect(card().queryByText("Nothing was saved. The settings in use are unchanged.")).toBeNull();
  });

  it("draws no result block before any save", async () => {
    await ready();
    pickDNS();
    expect(card().queryByText(/Token test (passed|failed)/)).toBeNull();
  });

  it("keeps a saved token when going back to DNS-01: Token saved shows with no typing", async () => {
    status = () => Response.json(httpWithToken());
    await ready();
    expect(card().getByText("Token saved")).toBeTruthy();
    expect(card().getByText(/Kept for DNS-01/)).toBeTruthy();
    pickDNS();
    expect(card().getByText("Token saved")).toBeTruthy();
    expect(card().getByRole("button", { name: "Replace token" })).toBeTruthy();
    expect(card().getByText(/It only deletes records it created/)).toBeTruthy();
  });
});

describe("Validation section: what Save sends", () => {
  it("sends the method alone under an HTTP-01 draft", async () => {
    status = () => Response.json(dnsSaved());
    put = () => validationResultResponse({ challengeType: "http-01", test: undefined });
    await ready();
    pickHTTP();
    fireEvent.click(saveButton());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({ challengeType: "http-01" });
  });

  it("sends no token and runs no test when DNS-01 is picked with a token already saved", async () => {
    status = () => Response.json(httpWithToken());
    put = () => validationResultResponse({ test: undefined });
    await ready();
    pickDNS();
    fireEvent.click(saveButton());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({ challengeType: "dns-01", resolvers: [] });
    await waitFor(() => expect(successToast).toHaveBeenCalledTimes(1));
    expect(card().queryByText(/Token test (passed|failed)/)).toBeNull();
  });

  it("sends the token trimmed", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), `  ${TOKEN}\n `);
    fireEvent.click(saveButton());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({ challengeType: "dns-01", token: TOKEN, resolvers: [] });
  });

  it("sends the new token when replacing a saved one", async () => {
    status = () => Response.json(dnsSaved());
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Replace token" }));
    type(card().getByLabelText("New Cloudflare API token") as HTMLInputElement, TOKEN);
    fireEvent.click(saveButton());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({ challengeType: "dns-01", token: TOKEN, resolvers: [] });
  });

  it("returns to the saved token with 'Keep the saved token', dropping what was typed", async () => {
    status = () => Response.json(dnsSaved());
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Replace token" }));
    type(card().getByLabelText("New Cloudflare API token") as HTMLInputElement, TOKEN);
    fireEvent.click(card().getByRole("button", { name: "Keep the saved token" }));
    expect(card().getByText("Token saved")).toBeTruthy();
    expect(isAriaDisabled(saveButton())).toBe(true);
  });
});

function validationResultResponse(over: Parameters<typeof validationResult>[0] = {}) {
  return Response.json(validationResult(over));
}

describe("Validation section: Save is aria-disabled until something changed", () => {
  it("is aria-disabled when nothing changed, and pressing it sends nothing", async () => {
    await ready();
    expect(isAriaDisabled(saveButton())).toBe(true);
    fireEvent.click(saveButton());
    await waitFor(() => expect(sent.length).toBeGreaterThan(0));
    expect(puts()).toHaveLength(0);
  });

  it("under an HTTP-01 draft is dirty only when the method differs from the saved one", async () => {
    status = () => Response.json(dnsSaved());
    await ready();
    expect(isAriaDisabled(saveButton())).toBe(true);
    pickHTTP();
    expect(isAriaDisabled(saveButton())).toBe(false);
    pickDNS();
    expect(isAriaDisabled(saveButton())).toBe(true);
  });

  it("under a DNS-01 draft is dirty when the method differs", async () => {
    await ready();
    pickDNS();
    expect(isAriaDisabled(saveButton())).toBe(false);
  });

  it("under a saved DNS-01 is dirty when a token is typed, and not when it is cleared again", async () => {
    status = () => Response.json(dnsSaved());
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Replace token" }));
    expect(isAriaDisabled(saveButton())).toBe(false);
    const field = card().getByLabelText("New Cloudflare API token") as HTMLInputElement;
    type(field, TOKEN);
    expect(isAriaDisabled(saveButton())).toBe(false);
    fireEvent.click(card().getByRole("button", { name: "Keep the saved token" }));
    expect(isAriaDisabled(saveButton())).toBe(true);
  });

  it("is aria-disabled again once the save has been stored", async () => {
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
    expect(isAriaDisabled(saveButton())).toBe(true);
  });
});

describe("Validation section: a token is required for DNS-01", () => {
  it("shows 'Enter a Cloudflare API token.' with aria-invalid and sends nothing", async () => {
    await ready();
    pickDNS();
    fireEvent.click(saveButton());
    expect(await card().findByText("Enter a Cloudflare API token.")).toBeTruthy();
    expect(tokenInput().getAttribute("aria-invalid")).toBe("true");
    expect(puts()).toHaveLength(0);
  });

  it("takes spaces alone as no token", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), "   ");
    fireEvent.click(saveButton());
    expect(await card().findByText("Enter a Cloudflare API token.")).toBeTruthy();
    expect(puts()).toHaveLength(0);
  });

  it("asks for a new token when 'Replace token' was pressed and none is typed", async () => {
    status = () => Response.json(dnsSaved());
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Replace token" }));
    fireEvent.click(saveButton());
    expect(await card().findByText("Enter a Cloudflare API token.")).toBeTruthy();
    expect(puts()).toHaveLength(0);
  });

  it("shows the field error when the server says the token is required, the page being out of date", async () => {
    // The token was removed elsewhere: the page still believes one is saved.
    put = () => {
      status = () => Response.json(withACME({ dns: acmeDNSStatus({ tokenSaved: false }) }));
      return Response.json(
        {
          type: "acme_dns_token_required",
          title: "x",
          status: 422,
          errors: [{ location: "body.token", message: "acme_dns_token_required" }],
        },
        { status: 422, headers: { "Content-Type": "application/problem+json" } },
      );
    };
    status = () => Response.json(httpWithToken());
    await ready();
    pickDNS();
    fireEvent.click(saveButton());
    expect(await card().findByText("Enter a Cloudflare API token.")).toBeTruthy();
    expect(tokenInput().getAttribute("aria-invalid")).toBe("true");
  });
});

describe("Validation section: failures and the pending state", () => {
  it("shows 'Something went wrong. Try again.' when the save fails with no classified reason", async () => {
    put = () => Response.json({}, { status: 500 });
    await ready();
    pickHTTP();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());
    expect(await card().findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(tokenInput().value).toBe(TOKEN);
    expect(successToast).not.toHaveBeenCalled();
  });

  it("freezes the radios, the field and the button while the save is pending", async () => {
    put = () => new Promise<Response>(() => {});
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());
    const busy = await card().findByRole("button", { name: /Testing token/ });
    expect(busy.matches(":disabled")).toBe(true);
    expect(busy.querySelector('[role="presentation"], svg')).toBeTruthy();
    expect(tokenInput().matches(":disabled")).toBe(true);
    for (const radio of card().getAllByRole("radio")) expect(radio.matches(":disabled")).toBe(true);
  });

  it("draws no radios while the status is loading: the card's skeleton stands in", () => {
    status = () => new Promise<Response>(() => {});
    render(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <TestRouter>
            <AcmeCard />
          </TestRouter>
        </TooltipProvider>
      </QueryClientProvider>,
    );
    expect(screen.getByTestId("acme-loading")).toBeTruthy();
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.queryByRole("radio")).toBeNull();
  });
});

describe("Validation section: long text", () => {
  it("lets the token input scroll inside itself at the touch height", async () => {
    await ready();
    pickDNS();
    const group = tokenInput().closest('[data-slot="input-group"]') as HTMLElement;
    expect(group.className).toContain("h-11");
    expect(group.className).toContain("md:h-8");
    expect(tokenInput().className).toContain("min-w-0");
    type(tokenInput(), "t".repeat(400));
    expect(tokenInput().value).toHaveLength(400);
  });

  it("wraps the radio descriptions and keeps each radio row at the touch height", async () => {
    await ready();
    const description = card().getByText(/Let's Encrypt checks over plain HTTP on port 80/);
    expect(description.className).toContain("break-words");
    const label = card().getByText("DNS-01", { selector: "label" });
    expect(label.className).toContain("max-md:min-h-11");
  });

  it("renders the buttons without the filled default variant", async () => {
    await ready();
    pickDNS();
    expect(saveButton().className).not.toContain("bg-primary");
  });
});

describe("validationMode", () => {
  const editable: CardState[] = ["A", "B", "D", "E", "F", "G", "K", "L"];
  const disabled: CardState[] = ["C", "H", "I", "J"];

  it.each(editable)("is editable in state %s with settings.write", (state) => {
    expect(validationMode(state, true)).toBe("editable");
  });

  it.each(disabled)("is disabled in state %s", (state) => {
    expect(validationMode(state, true)).toBe("disabled");
  });

  it.each([...editable, ...disabled])("is disabled in state %s without settings.write", (state) => {
    expect(validationMode(state, false)).toBe("disabled");
  });
});

describe("Validation section: disabled and read-only", () => {
  it("disables the radios and the token field and draws no action button in state H (paused after a restore)", async () => {
    status = () =>
      Response.json({
        ...httpsRestoredStatus({ challengeType: "dns-01", dns: acmeDNSStatus() }),
      });
    await ready();
    for (const radio of card().getAllByRole("radio")) expect(radio.matches(":disabled")).toBe(true);
    expect(tokenInput().matches(":disabled")).toBe(true);
    expect(card().queryByRole("button", { name: /Save validation settings/ })).toBeNull();
  });

  it("shows the saved token with no Replace button in state J", async () => {
    status = () =>
      Response.json(
        httpsCertFilesStatus({
          hasState: true,
          challengeType: "dns-01",
          dns: acmeDNSStatus({ tokenSaved: true }),
          account: { contact: "", registeredAt: REGISTERED_AT, termsOfServiceUrl: "" },
        }),
      );
    await ready();
    expect(card().getByText("Token saved")).toBeTruthy();
    expect(card().queryByRole("button", { name: "Replace token" })).toBeNull();
    expect(card().queryByRole("button", { name: /Save validation settings/ })).toBeNull();
  });

  it("without settings.write disables the inputs, draws no action button and shows the read-only line", async () => {
    status = () => Response.json(dnsSaved());
    me = () => Response.json(makeMe({ allEnvironments: "viewer" }));
    await ready();
    for (const radio of card().getAllByRole("radio")) expect(radio.matches(":disabled")).toBe(true);
    expect(
      card().queryByRole("button", { name: /Save validation settings|Replace token/ }),
    ).toBeNull();
    expect(card().getByText("You can view these settings but not change them.")).toBeTruthy();
  });
});

describe("Validation section: the DNS-01 suggestion (D-02, J2)", () => {
  const SUGGEST = { host: "tierplan.example.com", address: "192.168.1.20" };
  const TITLE = "DNS-01 may suit this install better";
  const alert = () => card().getByText(TITLE).closest('[role="note"]') as HTMLElement;

  it("is not drawn when the server suggests nothing", async () => {
    await ready();
    expect(card().queryByText(TITLE)).toBeNull();
  });

  it("is not drawn once DNS-01 is the draft", async () => {
    status = () => Response.json(withACME({ suggestDns01: SUGGEST }));
    await ready();
    expect(card().getByText(TITLE)).toBeTruthy();
    pickDNS();
    expect(card().queryByText(TITLE)).toBeNull();
  });

  it("is not drawn when DNS-01 is the saved method", async () => {
    status = () => Response.json(withACME({ suggestDns01: SUGGEST, challengeType: "dns-01" }));
    await ready();
    pickHTTP();
    expect(card().queryByText(TITLE)).toBeNull();
  });

  it("names the host and the address, with a Use DNS-01 action", async () => {
    status = () => Response.json(withACME({ suggestDns01: SUGGEST }));
    await ready();
    expect(alert().textContent).toContain(
      "tierplan.example.com points at a private address (192.168.1.20) as this server sees it.",
    );
    expect(alert().textContent).toContain("DNS-01 doesn't need Let's Encrypt to reach TierPlan.");
    expect(within(alert()).getByRole("button", { name: "Use DNS-01" })).toBeTruthy();
  });

  it("wraps a long host in mono code that breaks anywhere", async () => {
    const host = `${"a".repeat(60)}.internal.example.com`;
    status = () => Response.json(withACME({ suggestDns01: { host, address: "10.0.0.5" } }));
    await ready();
    const code = within(alert()).getByText(host);
    expect(code.tagName).toBe("CODE");
    expect(code.className).toContain("break-all");
    expect(code.className).toContain("font-mono");
  });

  it("selects DNS-01 in the draft only and focuses the token input, requesting nothing", async () => {
    status = () => Response.json(withACME({ suggestDns01: SUGGEST }));
    await ready();
    const before = sent.length;
    fireEvent.click(within(alert()).getByRole("button", { name: "Use DNS-01" }));
    await waitFor(() => expect(document.activeElement).toBe(tokenInput()));
    expect(
      card()
        .getByRole("radio", { name: /DNS-01/ })
        .getAttribute("aria-checked"),
    ).toBe("true");
    expect(sent).toHaveLength(before);
    expect(puts()).toHaveLength(0);
  });

  it("focuses Replace token when a token is already saved", async () => {
    status = () =>
      Response.json(withACME({ suggestDns01: SUGGEST, dns: acmeDNSStatus({ tokenSaved: true }) }));
    await ready();
    fireEvent.click(within(alert()).getByRole("button", { name: "Use DNS-01" }));
    await waitFor(() =>
      expect(document.activeElement).toBe(card().getByRole("button", { name: "Replace token" })),
    );
  });

  it("is not drawn in a disabled section", async () => {
    status = () =>
      Response.json(withACME({ suggestDns01: SUGGEST, pausedReason: "restore", hasState: true }));
    await ready();
    expect(card().queryByRole("button", { name: "Use DNS-01" })).toBeNull();
  });
});
