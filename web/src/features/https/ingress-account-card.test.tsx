// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { IngressAccount } from "@/api/ingress-account";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { IngressAccountCard } from "@/features/https/ingress-account-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

interface Sent {
  path: string;
  method: string;
  body?: Record<string, unknown>;
}

const ACCEPTED_AT = "2026-11-10T08:00:00Z";
const ACCOUNT_PATH = "/api/ingress/certificates/account";
const RESUME_PATH = "/api/ingress/certificates/resume";

let sent: Sent[];
let me: () => Response;
let accepted: boolean;
let account: () => Response | Promise<Response>;
let put: (body: Record<string, unknown>) => Response | Promise<Response>;
let resume: () => Response | Promise<Response>;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));
const environmentAdmin = () => Response.json(makeMe({ envs: { e1: "admin" } }));

function accountOf(over: Partial<IngressAccount> = {}): IngressAccount {
  return {
    termsAccepted: false,
    directories: [
      {
        issuer: "production",
        url: "https://acme-v02.api.letsencrypt.org/directory",
        termsOfService: "https://letsencrypt.org/documents/LE-SA-v1.5.pdf",
      },
      {
        issuer: "staging",
        url: "https://acme-staging-v02.api.letsencrypt.org/directory",
        termsOfService: "",
      },
    ],
    paused: false,
    pausedReason: "",
    ...over,
  };
}

function problem(status: number, detail: string) {
  return Response.json(
    { type: "about:blank", title: "x", status, detail },
    { status, headers: { "Content-Type": "application/problem+json" } },
  );
}

beforeEach(() => {
  sent = [];
  me = instanceAdmin;
  accepted = false;
  account = () => Response.json(accountOf());
  put = () => {
    accepted = true;
    return Response.json(accountOf({ termsAccepted: true, termsAcceptedAt: ACCEPTED_AT }));
  };
  resume = () => new Response(null, { status: 204 });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "POST" || req.method === "PUT") {
        const text = await req.clone().text();
        if (text !== "") entry.body = JSON.parse(text);
      }
      sent.push(entry);
      if (path === "/api/me") return me();
      if (path === ACCOUNT_PATH && req.method === "GET") {
        return accepted
          ? Response.json(accountOf({ termsAccepted: true, termsAcceptedAt: ACCEPTED_AT }))
          : account();
      }
      if (path === ACCOUNT_PATH && req.method === "PUT") return put(entry.body ?? {});
      if (path === RESUME_PATH && req.method === "POST") return resume();
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
          <IngressAccountCard />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "Ingress certificates" }));
const reads = () => sent.filter((s) => s.path === ACCOUNT_PATH && s.method === "GET");

// The card, once who is signed in and the account have both arrived.
async function ready() {
  renderCard();
  await screen.findByRole("region", { name: "Ingress certificates" });
  await waitFor(() => expect(card().queryByTestId("ingress-account-loading")).toBeNull());
}

describe("Ingress certificates card: the account read", () => {
  it("says the terms are not accepted, with the explanation and a degraded dot", async () => {
    await ready();
    expect(card().getByText("Terms not accepted")).toBeTruthy();
    expect(
      card().getByText(
        "TierPlan orders certificates for ingress hostnames from Let's Encrypt. Before it orders the first one, an Admin accepts Let's Encrypt's terms for this install.",
      ),
    ).toBeTruthy();
    expect(
      card().getByText("No certificate is ordered until the terms are accepted."),
    ).toBeTruthy();
  });

  it("says when the terms were accepted", async () => {
    account = () => Response.json(accountOf({ termsAccepted: true, termsAcceptedAt: ACCEPTED_AT }));
    await ready();
    expect(card().getByText(/^Terms accepted\s+\S/)).toBeTruthy();
    expect(card().queryByText("Terms not accepted")).toBeNull();
  });

  it("links the terms only for a directory that gave an address, in a new tab without an opener", async () => {
    await ready();
    const link = card().getByRole("link", { name: "Read the production terms" });
    expect(link.getAttribute("href")).toBe("https://letsencrypt.org/documents/LE-SA-v1.5.pdf");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(card().queryByRole("link", { name: "Read the staging terms" })).toBeNull();
  });

  it("draws no link and no list for a server that named no terms address", async () => {
    account = () => Response.json(accountOf({ directories: null }));
    await ready();
    expect(card().queryAllByRole("link")).toHaveLength(0);
  });

  it("draws the pause after a restore as a warning", async () => {
    account = () => Response.json(accountOf({ paused: true, pausedReason: "restore" }));
    await ready();
    expect(card().getByText("Certificate work is paused after a restore.")).toBeTruthy();
  });

  it("names any other reason for a pause", async () => {
    account = () => Response.json(accountOf({ paused: true, pausedReason: "maintenance" }));
    await ready();
    expect(card().getByText("Certificate work is paused: maintenance.")).toBeTruthy();
  });

  it("draws a loading skeleton until the account arrives", async () => {
    account = () => new Promise<Response>(() => {});
    renderCard();
    await screen.findByRole("region", { name: "Ingress certificates" });
    expect(card().getByTestId("ingress-account-loading")).toBeTruthy();
  });

  it("draws the failure and Retry reads the account again", async () => {
    let calls = 0;
    account = () => {
      calls += 1;
      return calls === 1 ? problem(500, "boom") : Response.json(accountOf());
    };
    await ready();
    expect(card().getByText("Couldn't read the certificate account.")).toBeTruthy();
    fireEvent.click(card().getByRole("button", { name: "Retry reading the certificate account" }));
    await card().findByText("Terms not accepted");
    expect(reads()).toHaveLength(2);
    expect(card().queryByText("Couldn't read the certificate account.")).toBeNull();
  });
});

describe("Ingress certificates card: who sees it", () => {
  it("is not drawn for an environment Admin", async () => {
    me = environmentAdmin;
    renderCard();
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
    expect(screen.queryByRole("region", { name: "Ingress certificates" })).toBeNull();
    expect(sent.some((s) => s.path === ACCOUNT_PATH)).toBe(false);
  });
});

const dialog = () => within(screen.getByRole("alertdialog"));
const writes = () => sent.filter((s) => s.method === "PUT" || s.method === "POST");

describe("Ingress certificates card: accept the terms", () => {
  it("asks first, then sends agreeTos true and shows the terms as accepted", async () => {
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Accept terms" }));
    expect(dialog().getByText("Accept Let's Encrypt's terms?")).toBeTruthy();
    expect(
      dialog().getByText("You agree to the terms of the directories above for this install."),
    ).toBeTruthy();
    expect(writes()).toHaveLength(0);
    fireEvent.click(dialog().getByRole("button", { name: "Accept terms" }));
    // The open dialog hides the page from the accessibility tree, so the card is read by text here.
    await screen.findByText(/^Terms accepted\s+\S/);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(writes()).toEqual([{ path: ACCOUNT_PATH, method: "PUT", body: { agreeTos: true } }]);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(card().queryByRole("button", { name: "Accept terms" })).toBeNull();
  });

  it("closes with 'Leave terms unaccepted' and sends nothing", async () => {
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Accept terms" }));
    fireEvent.click(dialog().getByRole("button", { name: "Leave terms unaccepted" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(writes()).toHaveLength(0);
    expect(card().getByText("Terms not accepted")).toBeTruthy();
  });

  it("is not offered once the terms are accepted", async () => {
    account = () => Response.json(accountOf({ termsAccepted: true, termsAcceptedAt: ACCEPTED_AT }));
    await ready();
    expect(card().queryByRole("button", { name: "Accept terms" })).toBeNull();
  });

  it("shows the server's reason in the dialog, never as a toast", async () => {
    const errorToast = vi.spyOn(toast, "error");
    put = () => problem(403, "Only an instance admin can accept the terms.");
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Accept terms" }));
    fireEvent.click(dialog().getByRole("button", { name: "Accept terms" }));
    const alert = await dialog().findByRole("alert");
    expect(alert.textContent).toContain("Only an instance admin can accept the terms.");
    expect(errorToast).not.toHaveBeenCalled();
    expect(screen.getByText("Terms not accepted")).toBeTruthy();
  });

  it("is drawn disabled with the reason when settings.write is missing, and sends nothing", async () => {
    me = () =>
      Response.json(
        makeMe({
          instanceAdmin: true,
          permissions: { global: ["settings.read"], environments: {} },
        }),
      );
    await ready();
    const button = card().getByRole("button", { name: "Accept terms" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(writes()).toHaveLength(0);
  });
});

describe("Ingress certificates card: resume certificate work", () => {
  beforeEach(() => {
    account = () => Response.json(accountOf({ paused: true, pausedReason: "restore" }));
  });

  it("asks first, then posts to resume and reads the account again", async () => {
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Resume certificate work" }));
    expect(dialog().getByText("Resume certificate work?")).toBeTruthy();
    expect(
      dialog().getByText(
        "TierPlan starts ordering and renewing certificates again. Check that DNS and the tokens still point at this install first.",
      ),
    ).toBeTruthy();
    expect(writes()).toHaveLength(0);
    const before = reads().length;
    account = () => Response.json(accountOf());
    fireEvent.click(dialog().getByRole("button", { name: "Resume certificate work" }));
    await waitFor(() => expect(writes()).toEqual([{ path: RESUME_PATH, method: "POST" }]));
    await waitFor(() => expect(reads().length).toBeGreaterThan(before));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(card().queryByText("Certificate work is paused after a restore.")).toBeNull();
  });

  it("closes with 'Keep work paused' and sends nothing", async () => {
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Resume certificate work" }));
    fireEvent.click(dialog().getByRole("button", { name: "Keep work paused" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(writes()).toHaveLength(0);
    expect(card().getByText("Certificate work is paused after a restore.")).toBeTruthy();
  });

  it("shows the server's reason in the dialog when the resume is refused", async () => {
    resume = () => problem(409, "Certificate work is not paused.");
    await ready();
    fireEvent.click(card().getByRole("button", { name: "Resume certificate work" }));
    fireEvent.click(dialog().getByRole("button", { name: "Resume certificate work" }));
    const alert = await dialog().findByRole("alert");
    expect(alert.textContent).toContain("Certificate work is not paused.");
  });

  it("is not offered while certificate work is running", async () => {
    account = () => Response.json(accountOf());
    await ready();
    expect(card().queryByRole("button", { name: "Resume certificate work" })).toBeNull();
  });
});
