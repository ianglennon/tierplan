// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ACMEAccount, HTTPSACME, HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AcmeCard } from "@/features/https/acme-card";
import {
  accountMode,
  advancedMode,
  type CardState,
  deriveCardState,
} from "@/features/https/card-state";
import "@/i18n";
import { acmeDirectory, acmeLastError, httpsACME, httpsStatus } from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
  body?: Record<string, unknown>;
}

const REGISTERED_AT = "2026-11-10T08:00:00Z";

let sent: Sent[];
let status: () => Response | Promise<Response>;
let me: () => Response;
let directory: () => Response;
let post: (body: Record<string, unknown>) => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));
const viewer = () => Response.json(makeMe({ allEnvironments: "viewer" }));

function withACME(over: Partial<HTTPSACME>, top: Partial<HTTPSStatus> = {}): HTTPSStatus {
  return httpsStatus({ acme: httpsACME(over), ...top });
}

function accountOf(over: Partial<ACMEAccount> = {}): ACMEAccount {
  return {
    contact: "ops@example.com",
    registeredAt: REGISTERED_AT,
    termsOfServiceUrl: "",
    ...over,
  };
}

function problem(status_: number, type: string, errors: { location: string; message: string }[]) {
  return Response.json(
    { type, title: "x", status: status_, detail: "d", errors },
    { status: status_, headers: { "Content-Type": "application/problem+json" } },
  );
}

beforeEach(() => {
  sent = [];
  status = () => Response.json(httpsStatus());
  me = instanceAdmin;
  directory = () => Response.json(acmeDirectory());
  post = () => Response.json(accountOf(), { status: 201 });
  successToast = vi.spyOn(toast, "success");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method, csrf: req.headers.get("X-CSRF-Token") };
      if (req.method === "POST") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/me") return me();
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/directory") return directory();
      if (path === "/api/https/acme/account" && req.method === "POST") {
        return post(entry.body ?? {});
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

function renderCard() {
  render(
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

// The card, once the status and who is signed in have both arrived.
async function ready() {
  renderCard();
  await card().findByText("Names on the certificate");
  await waitFor(() => expect(sent.some((s) => s.path === "/api/me")).toBe(true));
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

const registerButton = () => card().queryByRole("button", { name: /Register account|Registering/ });
const email = () => card().getByLabelText("Contact email (optional)") as HTMLInputElement;
const terms = () => card().getByRole("checkbox");
const posts = () => sent.filter((s) => s.method === "POST");

describe("Let's Encrypt card: method and port", () => {
  it("shows the HTTP-01 method and the port 80 note with the public host and the listen port", async () => {
    await ready();
    expect(card().getByText("Validation method")).toBeTruthy();
    expect(card().getByText("HTTP-01")).toBeTruthy();
    const note = card().getByRole("note");
    expect(note.textContent).toContain("Port 80 on tierplan.example.com must reach TierPlan");
    expect(note.textContent).toContain("-p 80:7480");
    expect(within(note).getByText("-p 80:7480").tagName).toBe("CODE");
  });

  it("says 'the public host' when no public host is set", async () => {
    status = () => Response.json(withACME({ publicHost: "", names: [], listenPort: 9000 }));
    await ready();
    const note = card().getByRole("note");
    expect(note.textContent).toContain("Port 80 on the public host must reach TierPlan");
    expect(note.textContent).toContain("-p 80:9000");
  });
});

describe("Names on the certificate", () => {
  it("lists one name as a mono list, with the helper linking to Settings > General", async () => {
    await ready();
    const items = card().getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual(["tierplan.example.com"]);
    expect(within(items[0] as HTMLElement).getByText("tierplan.example.com").className).toContain(
      "font-mono",
    );
    const link = card().getByRole("link", { name: "Settings > General" });
    expect(link.getAttribute("href")).toBe("/settings/general");
    expect(card().getByText(/IP addresses and wildcards can't be used/)).toBeTruthy();
  });

  it("lists two names when the agent URL host differs", async () => {
    status = () =>
      Response.json(
        withACME({
          names: [
            { name: "tierplan.example.com", refused: false },
            { name: "agents.example.com", refused: false },
          ],
        }),
      );
    await ready();
    expect(
      card()
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual(["tierplan.example.com", "agents.example.com"]);
  });

  it("shows 'On the certificate' and 'Needed now' when the set changed", async () => {
    status = () =>
      Response.json(
        withACME(
          {
            hasState: true,
            account: accountOf(),
            namesMismatch: true,
            names: [{ name: "new.example.com", refused: false }],
            certificate: {
              issuedAt: REGISTERED_AT,
              notAfter: "2027-02-08T08:00:00Z",
              notBefore: REGISTERED_AT,
              renewed: false,
              names: ["old.example.com"],
            },
          },
          { certificate: { ...httpsStatus().certificate, source: "lets_encrypt" } },
        ),
      );
    await ready();
    const old = card().getByRole("list", { name: "On the certificate" });
    const now = card().getByRole("list", { name: "Needed now" });
    expect(within(old).getByText("old.example.com")).toBeTruthy();
    expect(within(now).getByText("new.example.com")).toBeTruthy();
  });

  it("wraps a long punycode host with break-all in mono", async () => {
    const host = `xn--${"b".repeat(60)}.xn--${"c".repeat(60)}.example.com`;
    status = () => Response.json(withACME({ names: [{ name: host, refused: false }] }));
    await ready();
    const item = card().getByText(host).closest("li");
    expect(item?.className).toContain("break-all");
    expect(card().getByText(host).className).toContain("font-mono");
  });

  it("keeps a refused name listed and marked 'Not allowed'", async () => {
    status = () => Response.json(withACME({ names: [{ name: "192.168.1.20", refused: true }] }));
    await ready();
    const items = card().getAllByRole("listitem");
    expect(items).toHaveLength(1);
    expect(items[0]?.textContent).toBe("192.168.1.20Not allowed");
  });

  it("lists both hosts when one of two is refused and marks only the refused one", async () => {
    status = () =>
      Response.json(
        withACME({
          names: [
            { name: "tierplan.example.com", refused: false },
            { name: "10.0.0.5", refused: true },
          ],
        }),
      );
    await ready();
    const items = card().getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual([
      "tierplan.example.com",
      "10.0.0.5Not allowed",
    ]);
    expect(card().getAllByText("Not allowed")).toHaveLength(1);
  });

  it("renders host names as text, never as markup", async () => {
    const markup = "<img src=x onerror=alert(1)>";
    status = () => Response.json(withACME({ names: [{ name: markup, refused: true }] }));
    await ready();
    expect(card().getByText(markup)).toBeTruthy();
    expect(card().getByText(markup).querySelector("img")).toBeNull();
  });

  it("shows skeleton rows with the status query, and no names", () => {
    status = () => new Promise<Response>(() => {});
    renderCard();
    expect(screen.getByTestId("acme-loading").children).toHaveLength(3);
    expect(card().queryByText("Names on the certificate")).toBeNull();
  });

  it("shows the unreachable alert and no separate names error when the status fails", async () => {
    status = () => Response.json({}, { status: 500 });
    renderCard();
    const alert = await card().findByRole("alert");
    expect(alert.textContent).toBe("Can't reach TierPlan. Check your connection and try again.");
    expect(card().queryByText("Names on the certificate")).toBeNull();
    expect(screen.queryByTestId("acme-loading")).toBeNull();
  });
});

describe("Account registration", () => {
  it("shows the empty-state sentence above the form when there is no account", async () => {
    await ready();
    expect(
      card().getByText(
        "Let's Encrypt needs an account for this install. TierPlan keeps its key encrypted and never shows it.",
      ),
    ).toBeTruthy();
    expect(email().type).toBe("email");
    expect(registerButton()?.textContent).toBe("Register account");
    expect(card().queryByText("Advanced")).toBeNull();
  });

  it("registers over the API, refreshes the status and shows the summary with the key note", async () => {
    post = (body) => {
      expect(body).toEqual({ email: "ops@example.com", agreeTos: true });
      status = () => Response.json(withACME({ account: accountOf() }));
      return Response.json(accountOf(), { status: 201 });
    };
    await ready();
    fireEvent.change(email(), { target: { value: "ops@example.com" } });
    fireEvent.click(terms());
    fireEvent.click(registerButton() as HTMLElement);
    await card().findByText("Registered");
    expect(card().getByText("ops@example.com")).toBeTruthy();
    expect(card().getByText("Contact:")).toBeTruthy();
    expect(
      card().getByText("TierPlan keeps the account key encrypted and never shows it."),
    ).toBeTruthy();
    expect(registerButton()).toBeNull();
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.path).toBe("/api/https/acme/account");
    expect(posts()[0]?.csrf).toBe("csrf-1");
    expect(successToast).toHaveBeenCalledTimes(1);
  });

  it("registers with no contact when the email is left out, and says so", async () => {
    post = (body) => {
      expect(body).toEqual({ agreeTos: true });
      status = () => Response.json(withACME({ account: accountOf({ contact: "" }) }));
      return Response.json(accountOf({ contact: "" }), { status: 201 });
    };
    await ready();
    fireEvent.click(terms());
    fireEvent.click(registerButton() as HTMLElement);
    await card().findByText("No contact email.");
    expect(card().queryByText("Contact:")).toBeNull();
  });

  it("flags an invalid email and unaccepted terms as field errors, without a request", async () => {
    await ready();
    fireEvent.change(email(), { target: { value: "not-an-email" } });
    fireEvent.click(registerButton() as HTMLElement);
    expect(await card().findByText("Enter a valid email address, or leave it empty.")).toBeTruthy();
    expect(card().getByText("Accept the Subscriber Agreement to register.")).toBeTruthy();
    expect(email().getAttribute("aria-invalid")).toBe("true");
    expect(email().value).toBe("not-an-email");
    expect(posts()).toHaveLength(0);
  });

  it("sets the email field error for acme_contact_invalid and clears nothing", async () => {
    post = () =>
      problem(422, "acme_contact_invalid", [
        { location: "body.email", message: "acme_contact_invalid" },
      ]);
    await ready();
    fireEvent.change(email(), { target: { value: "ops@example.com" } });
    fireEvent.click(terms());
    fireEvent.click(registerButton() as HTMLElement);
    expect(await card().findByText("Enter a valid email address, or leave it empty.")).toBeTruthy();
    expect(email().value).toBe("ops@example.com");
    expect((terms() as HTMLElement).getAttribute("aria-checked")).toBe("true");
  });

  it("sets the terms error for acme_tos_required", async () => {
    post = () =>
      problem(422, "acme_tos_required", [
        { location: "body.agreeTos", message: "acme_tos_required" },
      ]);
    await ready();
    fireEvent.click(terms());
    fireEvent.click(registerButton() as HTMLElement);
    expect(await card().findByText("Accept the Subscriber Agreement to register.")).toBeTruthy();
  });

  it("shows the classified error in the section for a CA failure, and clears nothing", async () => {
    post = () => problem(502, "acme_ca_error", [{ location: "class", message: "ca_unavailable" }]);
    await ready();
    fireEvent.change(email(), { target: { value: "ops@example.com" } });
    fireEvent.click(terms());
    fireEvent.click(registerButton() as HTMLElement);
    expect(await card().findByText("Let's Encrypt couldn't be reached.")).toBeTruthy();
    expect(card().getByText(/TierPlan tries again on its own schedule\./)).toBeTruthy();
    expect(card().queryByText(/next at/)).toBeNull();
    expect(email().value).toBe("ops@example.com");
    expect(registerButton()?.textContent).toBe("Register account");
  });

  it("shows 'Registering...' with a disabled fieldset while the request is pending", async () => {
    let release: (r: Response) => void = () => {};
    post = () => new Promise<Response>((resolve) => (release = resolve));
    await ready();
    fireEvent.click(terms());
    fireEvent.click(registerButton() as HTMLElement);
    await waitFor(() => expect(registerButton()?.textContent).toBe("Registering..."));
    expect(email().matches(":disabled")).toBe(true);
    expect(email().closest("fieldset")?.disabled).toBe(true);
    release(Response.json(accountOf(), { status: 201 }));
    await waitFor(() => expect(registerButton()?.textContent).toBe("Register account"));
  });

  it("wraps a long contact email with break-all in the summary", async () => {
    const contact = `${"x".repeat(80)}@${"y".repeat(60)}.example.com`;
    status = () => Response.json(withACME({ account: accountOf({ contact }) }));
    await ready();
    const line = (await card().findByText(contact)).closest("p");
    expect(line?.className).toContain("break-all");
  });

  it("links the terms to the CA's https address in a new tab with rel noopener noreferrer", async () => {
    await ready();
    const link = await card().findByRole("link", { name: "Subscriber Agreement" });
    expect(link.getAttribute("href")).toBe(acmeDirectory().termsOfServiceUrl);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("shows plain label text when the terms address is not https", async () => {
    directory = () => Response.json(acmeDirectory({ termsOfServiceUrl: "javascript:alert(1)" }));
    await ready();
    await waitFor(() =>
      expect(sent.some((s) => s.path === "/api/https/acme/directory")).toBe(true),
    );
    expect(card().queryByRole("link", { name: "Subscriber Agreement" })).toBeNull();
    expect(card().getByText(/I agree to the Let's Encrypt Subscriber Agreement/)).toBeTruthy();
  });
});

describe("Without settings.write", () => {
  it("disables the inputs, renders no action button and shows the read-only line", async () => {
    me = viewer;
    renderCard();
    await card().findByText("You can view these settings but not change them.");
    expect(email().matches(":disabled")).toBe(true);
    expect(terms().matches(":disabled")).toBe(true);
    expect(card().queryByRole("button")).toBeNull();
    expect(sent.some((s) => s.path === "/api/https/acme/directory")).toBe(false);
  });

  it("shows the registered summary read-only with no action button", async () => {
    me = viewer;
    status = () => Response.json(withACME({ account: accountOf() }));
    renderCard();
    await card().findByText("Registered");
    await card().findByText("You can view these settings but not change them.");
    // The Advanced disclosure only opens a section; no button acts on anything.
    expect(
      card()
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Advanced"]);
  });

  it("does not show the read-only line to an admin", async () => {
    await ready();
    expect(card().queryByText("You can view these settings but not change them.")).toBeNull();
  });
});

describe("the last-run error panel", () => {
  const HEADLINE =
    "Let's Encrypt couldn't reach http://tierplan.example.com/.well-known/acme-challenge/.";

  it("explains a failed first issue, which leaves Let's Encrypt off (state F, mode off)", async () => {
    status = () =>
      Response.json(
        withACME({
          account: accountOf(),
          hasState: true,
          mode: "off",
          failures: 1,
          // The ledger plans a back-off, but nothing retries while the mode is off.
          nextAttemptAt: new Date(Date.now() + 600_000).toISOString(),
          lastError: acmeLastError(),
        }),
      );
    await ready();
    const panel = await card().findByText(HEADLINE);
    const alert = panel.closest('[data-slot="alert"]') as HTMLElement;
    expect(alert.id).toBe("lets-encrypt-last-run");
    // Read from the ledger on load: a note, not an announcement.
    expect(alert.getAttribute("role")).toBe("note");
    expect(alert.textContent).toContain("Port 80 on tierplan.example.com must reach TierPlan");
    expect(alert.textContent).toContain("1 failed attempt in a row.");
    expect(alert.textContent).not.toContain("Next automatic attempt");
    expect(within(alert).getByRole("button", { name: "Details" })).toBeTruthy();
  });

  it("still promises the next automatic attempt while Let's Encrypt is in use", async () => {
    const next = new Date(Date.now() + 600_000).toISOString();
    status = () =>
      Response.json(
        withACME({
          account: accountOf(),
          hasState: true,
          mode: "active",
          failures: 2,
          nextAttemptAt: next,
          lastError: acmeLastError(),
        }),
      );
    await ready();
    const panel = await card().findByText(HEADLINE);
    const alert = panel.closest('[data-slot="alert"]') as HTMLElement;
    expect(alert.textContent).toContain("Next automatic attempt");
  });
});

describe("deriveCardState", () => {
  const served = { certificate: { ...httpsStatus().certificate, source: "lets_encrypt" as const } };
  const account = accountOf();
  const stored = {
    issuedAt: REGISTERED_AT,
    notAfter: "2027-02-08T08:00:00Z",
    notBefore: REGISTERED_AT,
    renewed: false,
    names: ["tierplan.example.com"],
  };
  const run = {
    jobId: "j1",
    kind: "issue",
    outcome: "running",
    startedAt: REGISTERED_AT,
    trigger: "admin",
  } as const;
  const error = { at: REGISTERED_AT, code: "ca_unavailable" } as const;

  const cases: [string, HTTPSStatus, CardState][] = [
    ["A: no account", withACME({}), "A"],
    ["B: account, no certificate", withACME({ account }), "B"],
    [
      "I: certificate files, nothing kept",
      withACME({ pausedReason: "cert_files" }, { certFiles: true }),
      "I",
    ],
    [
      "J: certificate files, state kept",
      withACME(
        { pausedReason: "cert_files", hasState: true, account, certificate: stored },
        { certFiles: true },
      ),
      "J",
    ],
    [
      "H: paused after a restore",
      withACME({ account, pausedReason: "restore", hasState: true }),
      "H",
    ],
    ["C: a run is active", withACME({ account, activeRun: run }), "C"],
    [
      "D: served and healthy",
      withACME({ account, hasState: true, certificate: stored, mode: "active" }, served),
      "D",
    ],
    [
      "K: expired and serving the fallback",
      withACME({
        account,
        hasState: true,
        certificate: stored,
        fallback: { expiredAt: REGISTERED_AT },
      }),
      "K",
    ],
    [
      "E: names changed while served",
      withACME(
        { account, hasState: true, certificate: stored, namesMismatch: true, mode: "active" },
        served,
      ),
      "E",
    ],
    [
      "G: told to wait",
      withACME(
        {
          account,
          hasState: true,
          certificate: stored,
          blocked: { reason: "told_to_wait", until: REGISTERED_AT },
        },
        served,
      ),
      "G",
    ],
    [
      "F: last run failed, still serving",
      withACME({ account, hasState: true, certificate: stored, lastError: error }, served),
      "F",
    ],
    [
      "L: stopped, certificate kept",
      withACME({ account, hasState: true, certificate: stored }),
      "L",
    ],
  ];

  it.each(cases)("returns %s", (_name, st, want) => {
    expect(deriveCardState(st)).toBe(want);
  });

  it("returns A when the build has no Let's Encrypt block", () => {
    expect(deriveCardState(httpsStatus({ acme: undefined }))).toBe("A");
  });
});

describe("accountMode and advancedMode", () => {
  it("draws the form in A, disabled in I, and the summary elsewhere", () => {
    expect(accountMode("A", true)).toBe("form");
    expect(accountMode("I", true)).toBe("form-disabled");
    expect(accountMode("B", true)).toBe("summary");
    expect(accountMode("J", true)).toBe("summary-disabled");
    expect(accountMode("A", false)).toBe("form-disabled");
    expect(accountMode("D", false)).toBe("summary-disabled");
  });

  it("hides Advanced in A and I, disables it in C, H and J or without write, and shows it otherwise", () => {
    for (const s of ["A", "I"] as const) expect(advancedMode(s, true)).toBe("hidden");
    for (const s of ["C", "H", "J"] as const) expect(advancedMode(s, true)).toBe("disabled");
    for (const s of ["B", "D", "E", "F", "G", "K", "L"] as const) {
      expect(advancedMode(s, true)).toBe("shown");
      expect(advancedMode(s, false)).toBe("disabled");
    }
  });
});
