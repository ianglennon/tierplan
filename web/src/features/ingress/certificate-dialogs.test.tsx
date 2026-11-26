// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
