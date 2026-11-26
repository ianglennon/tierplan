// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CertificatesTab } from "@/features/ingress/certificates-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

interface Sent {
  path: string;
  method: string;
  body?: Json;
}

let sent: Sent[];
let items: Json[];
let answer: (req: Sent) => Response | undefined;
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

function problem(status: number, type: string, detail: string): Response {
  return Response.json({ status, type, title: type, detail }, { status });
}

beforeEach(() => {
  sent = [];
  items = [cert()];
  answer = () => undefined;
  setCsrfToken("csrf");
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "POST")
        entry.body = await req
          .clone()
          .json()
          .catch(() => undefined);
      sent.push(entry);
      if (path === "/api/me") return Response.json(me);
      const custom = req.method === "POST" ? answer(entry) : undefined;
      if (custom) return custom;
      if (path === `/api/env/${ENV}/ingress/certificates` && req.method === "POST") {
        return Response.json(cert({ id: "c9", hostname: String(entry.body?.hostname) }), {
          status: 201,
        });
      }
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

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <CertificatesTab envId={ENV} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const gets = () =>
  sent.filter((s) => s.method === "GET" && s.path === `/api/env/${ENV}/ingress/certificates`);
const posts = () => sent.filter((s) => s.method === "POST");

async function openRequest() {
  const button = await screen.findByRole("button", { name: "Request certificate" });
  fireEvent.click(button);
  return screen.findByRole("dialog");
}

describe("requesting a certificate ahead of a claim", () => {
  it("posts the hostname on Enter, closes with a toast and reads the list again", async () => {
    mount();
    await openRequest();
    const before = gets().length;
    const input = screen.getByRole("textbox", { name: "Hostname" });
    fireEvent.change(input, { target: { value: "new.apps.example.com" } });
    fireEvent.submit(input.closest("form") as HTMLFormElement);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]).toMatchObject({
      path: `/api/env/${ENV}/ingress/certificates`,
      body: { hostname: "new.apps.example.com" },
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(
      await screen.findByText("Requested a certificate for new.apps.example.com."),
    ).toBeTruthy();
    await waitFor(() => expect(gets().length).toBeGreaterThan(before));
  });

  it("keeps the primary disabled until a hostname is typed", async () => {
    mount();
    const dialog = await openRequest();
    const submit = dialog.querySelector("button[type=submit]") as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(screen.getByRole("textbox", { name: "Hostname" }), {
      target: { value: "x.apps.example.com" },
    });
    expect(submit.disabled).toBe(false);
    expect(screen.getByText(/Under a domain registered on the Domains tab/)).toBeTruthy();
  });

  it("keeps the dialog open with the server's detail when it refuses", async () => {
    answer = () =>
      problem(422, "domain_not_allowed", "bad.other.org is not under a registered domain.");
    mount();
    await openRequest();
    const input = screen.getByRole("textbox", { name: "Hostname" });
    fireEvent.change(input, { target: { value: "bad.other.org" } });
    fireEvent.submit(input.closest("form") as HTMLFormElement);
    const alert = await screen.findByText("bad.other.org is not under a registered domain.");
    expect(alert.closest("[role=alert]")).not.toBeNull();
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect((input as HTMLInputElement).value).toBe("bad.other.org");
    expect(screen.queryByText(/Requested a certificate for/)).toBeNull();
  });

  it("starts empty again after Discard request", async () => {
    mount();
    await openRequest();
    fireEvent.change(screen.getByRole("textbox", { name: "Hostname" }), {
      target: { value: "half.apps.example.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Discard request" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await openRequest();
    expect((screen.getByRole("textbox", { name: "Hostname" }) as HTMLInputElement).value).toBe("");
    expect(posts()).toHaveLength(0);
  });

  it("shows a Viewer the toolbar button disabled with its reason", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    const button = await screen.findByRole("button", { name: "Request certificate" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getAllByText("Needs Admin").length).toBeGreaterThan(0);
  });
});

const KEY = "-----BEGIN PRIVATE KEY-----\nSECRETKEYBYTES0123456789\n-----END PRIVATE KEY-----";
const CHAIN = "-----BEGIN CERTIFICATE-----\nCHAINBYTES\n-----END CERTIFICATE-----";

async function openMenu(host: string) {
  const button = await screen.findByRole("button", { name: `Actions for ${host}` });
  fireEvent.keyDown(button, { key: "Enter" });
  return screen.findByRole("menu");
}

const itemNames = (menu: HTMLElement) =>
  within(menu)
    .getAllByRole("menuitem")
    .map((i) => i.textContent);

async function openUpload(host = "app.example.com") {
  const menu = await openMenu(host);
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Upload certificate…" }));
  return screen.findByRole("dialog");
}

function fillUpload() {
  fireEvent.change(screen.getByLabelText("Certificate chain (PEM)"), { target: { value: CHAIN } });
  fireEvent.change(screen.getByLabelText("Private key (PEM)"), { target: { value: KEY } });
}

describe("the row menu", () => {
  it("offers Retry now only for failed and renewal_failed rows", async () => {
    items = [
      cert({
        id: "a",
        hostname: "a.example.com",
        state: "failed",
        stateText: "Failed",
        ready: false,
      }),
      cert({
        id: "b",
        hostname: "b.example.com",
        state: "renewal_failed",
        stateText: "Renewal failed",
      }),
      cert({ id: "c", hostname: "c.example.com" }),
    ];
    mount();
    expect(itemNames(await openMenu("a.example.com"))).toContain("Retry now");
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    expect(itemNames(await openMenu("b.example.com"))).toContain("Retry now");
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    expect(itemNames(await openMenu("c.example.com"))).not.toContain("Retry now");
  });

  it("posts the retry and says so in a toast", async () => {
    items = [
      cert({
        id: "a",
        hostname: "a.example.com",
        state: "failed",
        stateText: "Failed",
        ready: false,
      }),
    ];
    answer = (req) =>
      req.path.endsWith("/retry") ? new Response(null, { status: 202 }) : undefined;
    mount();
    fireEvent.click(
      within(await openMenu("a.example.com")).getByRole("menuitem", { name: "Retry now" }),
    );
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe(`/api/env/${ENV}/ingress/certificates/a/retry`);
    expect(await screen.findByText("Retrying a.example.com.")).toBeTruthy();
  });

  it("puts the server's detail in a destructive toast when the retry is refused", async () => {
    items = [
      cert({
        id: "a",
        hostname: "a.example.com",
        state: "failed",
        stateText: "Failed",
        ready: false,
      }),
    ];
    answer = () =>
      problem(409, "ingress_cert_blocked", "The certificate is blocked by a rate limit.");
    mount();
    fireEvent.click(
      within(await openMenu("a.example.com")).getByRole("menuitem", { name: "Retry now" }),
    );
    expect(await screen.findByText("The certificate is blocked by a rate limit.")).toBeTruthy();
  });

  it("offers Switch back only for an uploaded row", async () => {
    items = [
      cert({
        id: "u",
        hostname: "u.example.com",
        source: "upload",
        issuer: undefined,
        state: "uploaded",
        stateText: "Uploaded",
      }),
      cert({ id: "c", hostname: "c.example.com" }),
    ];
    mount();
    expect(itemNames(await openMenu("u.example.com"))).toContain("Switch back to Let's Encrypt…");
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    expect(itemNames(await openMenu("c.example.com"))).not.toContain(
      "Switch back to Let's Encrypt…",
    );
  });

  it("draws every write item disabled with the reason for a Viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    items = [
      cert({
        id: "u",
        hostname: "u.example.com",
        source: "upload",
        issuer: undefined,
        state: "uploaded",
        stateText: "Uploaded",
      }),
      cert({
        id: "a",
        hostname: "a.example.com",
        state: "failed",
        stateText: "Failed",
        ready: false,
      }),
    ];
    mount();
    await screen.findByRole("button", { name: "Actions for u.example.com" });
    await waitFor(() => expect(gets().length).toBeGreaterThan(0));
    const upload = within(await openMenu("u.example.com")).getByRole("menuitem", {
      name: "Upload certificate…",
    });
    await waitFor(() => expect(upload.getAttribute("aria-disabled")).toBe("true"));
    const swap = within(screen.getByRole("menu")).getByRole("menuitem", {
      name: "Switch back to Let's Encrypt…",
    });
    expect(swap.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(upload);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(posts()).toHaveLength(0);
  });
});

describe("uploading a certificate", () => {
  it("needs both fields, then posts the chain and key for the row's host", async () => {
    answer = (req) =>
      req.path.endsWith("/upload") ? Response.json(cert({ id: "n" }), { status: 201 }) : undefined;
    mount();
    const dialog = await openUpload();
    expect(within(dialog).getByText("Upload a certificate for app.example.com")).toBeTruthy();
    expect(
      within(dialog).getByText("TierPlan stores the key sealed and never shows it again."),
    ).toBeTruthy();
    const submit = within(dialog).getByRole("button", {
      name: "Upload certificate",
    }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Certificate chain (PEM)"), {
      target: { value: CHAIN },
    });
    expect(submit.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Private key (PEM)"), { target: { value: KEY } });
    expect(submit.disabled).toBe(false);
    fireEvent.click(submit);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]).toMatchObject({
      path: `/api/env/${ENV}/ingress/certificates/upload`,
      body: { hostname: "app.example.com", certificatePem: CHAIN, privateKeyPem: KEY },
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("keeps the text and shows the detail when the upload is refused", async () => {
    answer = () => problem(422, "ingress_cert_expired", "The certificate has expired.");
    mount();
    await openUpload();
    fillUpload();
    fireEvent.click(screen.getByRole("button", { name: "Upload certificate" }));
    expect(await screen.findByText("The certificate has expired.")).toBeTruthy();
    expect((screen.getByLabelText("Private key (PEM)") as HTMLTextAreaElement).value).toBe(KEY);
    expect((screen.getByLabelText("Certificate chain (PEM)") as HTMLTextAreaElement).value).toBe(
      CHAIN,
    );
  });

  it("clears both fields after Discard upload and after a success, and never shows the key anywhere", async () => {
    answer = (req) =>
      req.path.endsWith("/upload") ? Response.json(cert({ id: "n" }), { status: 201 }) : undefined;
    mount();
    await openUpload();
    fillUpload();
    fireEvent.click(screen.getByRole("button", { name: "Discard upload" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(posts()).toHaveLength(0);
    await openUpload();
    expect((screen.getByLabelText("Private key (PEM)") as HTMLTextAreaElement).value).toBe("");
    expect((screen.getByLabelText("Certificate chain (PEM)") as HTMLTextAreaElement).value).toBe(
      "",
    );
    fillUpload();
    fireEvent.click(screen.getByRole("button", { name: "Upload certificate" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText("Uploaded a certificate for app.example.com.")).toBeTruthy();
    // No toast, live region or alert holds the key text, and the closed form kept nothing of it.
    const live = document.querySelectorAll("[aria-live], [role=status], [role=alert], [role=log]");
    for (const el of Array.from(live)) expect(el.textContent).not.toContain("SECRETKEYBYTES");
    expect(document.body.textContent).not.toContain("SECRETKEYBYTES");
    await openUpload();
    expect((screen.getByLabelText("Private key (PEM)") as HTMLTextAreaElement).value).toBe("");
  });
});

describe("switching an uploaded hostname back to Let's Encrypt", () => {
  const uploaded = () => [
    cert({
      id: "u",
      hostname: "u.example.com",
      source: "upload",
      issuer: undefined,
      state: "uploaded",
      stateText: "Uploaded",
    }),
  ];

  async function openSwitch() {
    const menu = await openMenu("u.example.com");
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Switch back to Let's Encrypt…" }));
    return screen.findByRole("alertdialog");
  }

  it("sends nothing when the uploaded certificate is kept", async () => {
    items = uploaded();
    mount();
    const dialog = await openSwitch();
    expect(within(dialog).getByText("Switch u.example.com back to Let's Encrypt?")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep uploaded certificate" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(posts()).toHaveLength(0);
  });

  it("posts to the acme path on confirm and reads the list again", async () => {
    items = uploaded();
    answer = (req) => (req.path.endsWith("/acme") ? Response.json(cert({ id: "q" })) : undefined);
    mount();
    const dialog = await openSwitch();
    const before = gets().length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Switch to Let's Encrypt" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe(`/api/env/${ENV}/ingress/certificates/u/acme`);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() => expect(gets().length).toBeGreaterThan(before));
  });

  it("keeps the dialog open with the server's detail on a refusal", async () => {
    items = uploaded();
    answer = () => problem(409, "ingress_domain_not_found", "Register the domain again first.");
    mount();
    const dialog = await openSwitch();
    fireEvent.click(within(dialog).getByRole("button", { name: "Switch to Let's Encrypt" }));
    expect(await screen.findByText("Register the domain again first.")).toBeTruthy();
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });
});
