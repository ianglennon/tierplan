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
    expect(tests()[0]?.body).toEqual({ token: TOKEN, resolvers: [] });
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
    expect(tests()[0]?.body).toEqual({ resolvers: [] });
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

const openAdvanced = () => fireEvent.click(card().getByRole("button", { name: "Advanced DNS" }));
const advancedTrigger = () => card().getByRole("button", { name: "Advanced DNS" });
const resolversInput = () => card().getByLabelText("DNS resolvers (optional)") as HTMLInputElement;
const dnsSaved = (over: Partial<HTTPSACME["dns"]> = {}) =>
  withACME({
    challengeType: "dns-01",
    dns: acmeDNSStatus({ tokenSaved: true, ...over }),
  });
const resolverProblem = (entry: string) =>
  Response.json(
    {
      type: "acme_dns_resolver_invalid",
      title: "x",
      status: 422,
      errors: [{ location: "body.resolvers", message: "acme_dns_resolver_invalid", value: entry }],
    },
    { status: 422, headers: { "Content-Type": "application/problem+json" } },
  );

describe("Advanced DNS: the resolver list", () => {
  it("starts closed when no list is saved, with the trigger the only sign of it", async () => {
    await ready();
    pickDNS();
    expect(advancedTrigger().getAttribute("aria-expanded")).toBe("false");
    expect(card().queryByLabelText("DNS resolvers (optional)")).toBeNull();
  });

  it("opens to the resolvers field, mono, with its helper and the delegation note", async () => {
    await ready();
    pickDNS();
    openAdvanced();
    expect(advancedTrigger().getAttribute("aria-expanded")).toBe("true");
    expect(resolversInput().placeholder).toBe("192.168.1.1, 10.0.0.53:5353");
    expect(resolversInput().className).toContain("font-mono");
    expect(
      card().getByText(
        "Comma-separated, each as host or host:port. When set, TierPlan asks only these while it waits for a record to appear. Leave it empty to ask the zone's own nameservers, which is right for most installs.",
      ),
    ).toBeTruthy();
    expect(card().getByText(/There is nothing to set\./)).toBeTruthy();
    expect(card().queryByText(/skip/i)).toBeNull();
  });

  it("is open by default when a list is saved, and shows it normalised", async () => {
    status = () => Response.json(dnsSaved({ resolvers: ["1.1.1.1:53", "192.168.1.1:5353"] }));
    await ready();
    expect(advancedTrigger().getAttribute("aria-expanded")).toBe("true");
    expect(resolversInput().value).toBe("1.1.1.1:53, 192.168.1.1:5353");
  });

  it("sends the comma-separated entries as a list with Test token", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    openAdvanced();
    type(resolversInput(), "1.1.1.1, 192.168.1.1:5353");
    fireEvent.click(testButton());
    await card().findByText("Token test passed");
    expect(tests()[0]?.body).toEqual({
      token: TOKEN,
      resolvers: ["1.1.1.1", "192.168.1.1:5353"],
    });
  });

  it("sends the same list with Save", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    openAdvanced();
    type(resolversInput(), " 1.1.1.1 ,192.168.1.1:5353, ");
    fireEvent.click(saveButton());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({
      challengeType: "dns-01",
      token: TOKEN,
      resolvers: ["1.1.1.1", "192.168.1.1:5353"],
    });
  });

  it("sends one entry as a list of one", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    openAdvanced();
    type(resolversInput(), "10.0.0.53");
    fireEvent.click(testButton());
    await card().findByText("Token test passed");
    expect(tests()[0]?.body).toEqual({ token: TOKEN, resolvers: ["10.0.0.53"] });
  });

  it("sends an empty list for an empty field: the zone's own nameservers", async () => {
    status = () => Response.json(dnsSaved({ resolvers: ["1.1.1.1:53"] }));
    await ready();
    type(resolversInput(), "");
    fireEvent.click(testButton());
    await card().findByText("Token test passed");
    expect(tests()[0]?.body).toEqual({ resolvers: [] });
    fireEvent.click(saveButton());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({ challengeType: "dns-01", resolvers: [] });
  });

  it("names a refused entry in the field error and opens Advanced DNS", async () => {
    put = () => resolverProblem("bad/entry");
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    openAdvanced();
    type(resolversInput(), "bad/entry");
    openAdvanced();
    expect(advancedTrigger().getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(saveButton());

    const error = await card().findByText(/isn't a valid resolver/);
    expect(error.textContent).toBe(
      "bad/entry isn't a valid resolver. Use host or host:port, separated by commas.",
    );
    expect(within(error).getByText("bad/entry").className).toContain("break-all");
    expect(advancedTrigger().getAttribute("aria-expanded")).toBe("true");
    expect(resolversInput().getAttribute("aria-invalid")).toBe("true");
  });

  it("names a refused entry from Test token too", async () => {
    testToken = () => resolverProblem("nope:x");
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(testButton());
    expect(await card().findByText(/isn't a valid resolver/)).toBeTruthy();
    expect(advancedTrigger().getAttribute("aria-expanded")).toBe("true");
  });

  it("makes an edited, unsaved list a change to save", async () => {
    status = () => Response.json(dnsSaved());
    await ready();
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");
    openAdvanced();
    type(resolversInput(), "1.1.1.1");
    expect(saveButton().getAttribute("aria-disabled")).toBe("false");
    type(resolversInput(), "");
    expect(saveButton().getAttribute("aria-disabled")).toBe("true");
  });

  it("scrolls inside the mono input and lets a long list wrap in the helper", async () => {
    await ready();
    pickDNS();
    openAdvanced();
    expect(resolversInput().className).toContain("min-w-0");
    expect(resolversInput().className).toContain("md:h-8");
    expect(resolversInput().className).toContain("h-11");
  });

  it("is aria-disabled while a test is pending", async () => {
    testToken = () => new Promise<Response>(() => {});
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    openAdvanced();
    fireEvent.click(testButton());
    await card().findByRole("button", { name: /Testing token/ });
    expect(resolversInput().getAttribute("aria-disabled")).toBe("true");
  });

  it("is aria-disabled while a save is pending", async () => {
    put = () => new Promise<Response>(() => {});
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    openAdvanced();
    fireEvent.click(saveButton());
    await card().findByRole("button", { name: /Testing token/ });
    expect(resolversInput().getAttribute("aria-disabled")).toBe("true");
  });

  it("reverts edited resolvers and closes Advanced DNS when HTTP-01 is picked (J9)", async () => {
    await ready();
    pickDNS();
    openAdvanced();
    type(resolversInput(), "1.1.1.1");
    fireEvent.click(card().getByRole("radio", { name: /HTTP-01/ }));
    pickDNS();
    expect(advancedTrigger().getAttribute("aria-expanded")).toBe("false");
    openAdvanced();
    expect(resolversInput().value).toBe("");
  });

  it("reverts to the saved list and keeps Advanced DNS open when one is saved (J9)", async () => {
    status = () => Response.json(dnsSaved({ resolvers: ["1.1.1.1:53"] }));
    await ready();
    type(resolversInput(), "9.9.9.9");
    fireEvent.click(card().getByRole("radio", { name: /HTTP-01/ }));
    pickDNS();
    expect(advancedTrigger().getAttribute("aria-expanded")).toBe("true");
    expect(resolversInput().value).toBe("1.1.1.1:53");
  });

  it("removes a test result when HTTP-01 is picked (J9)", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(testButton());
    await card().findByText("Token test passed");
    fireEvent.click(card().getByRole("radio", { name: /HTTP-01/ }));
    expect(card().queryByText("Token test passed")).toBeNull();
  });
});

describe("Test token: a result for earlier settings", () => {
  const STALE = "This result is for earlier settings. Test again.";

  it("is not marked while nothing has changed since the test", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(testButton());
    const result = (await card().findByText("Token test passed")).closest('[role="status"]');
    expect(result?.className).not.toContain("opacity-60");
    expect(result?.getAttribute("aria-live")).toBe("polite");
    expect(card().queryByText(STALE)).toBeNull();
  });

  it("greys the result with the stale note when the token changes", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(testButton());
    await card().findByText("Token test passed");
    type(tokenInput(), `${TOKEN}x`);
    const result = card().getByText("Token test passed").closest('[role="status"]');
    expect(result?.className).toContain("opacity-60");
    expect(result?.getAttribute("aria-live")).toBe("off");
    expect(card().getByText(STALE)).toBeTruthy();
  });

  it("greys the result when the resolvers change", async () => {
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(testButton());
    await card().findByText("Token test passed");
    openAdvanced();
    type(resolversInput(), "1.1.1.1");
    expect(card().getByText(STALE)).toBeTruthy();
  });

  it("stays plain after a save that stored the settings", async () => {
    put = () => {
      status = () => Response.json(dnsSaved());
      return Response.json({
        challengeType: "dns-01",
        resolvers: [],
        saved: true,
        test: tokenTestResult(),
        tokenSaved: true,
      });
    };
    await ready();
    pickDNS();
    type(tokenInput(), TOKEN);
    fireEvent.click(saveButton());
    await card().findByText("Token saved");
    expect(card().getByText("Token test passed")).toBeTruthy();
    expect(card().queryByText(STALE)).toBeNull();
  });
});
