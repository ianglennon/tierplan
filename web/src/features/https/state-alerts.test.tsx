// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AcmeCard } from "@/features/https/acme-card";
import { HttpsPage } from "@/features/https/https-page";
import "@/i18n";
import {
  httpsACME,
  httpsCertFilesKeptStatus,
  httpsCertFilesStatus,
  httpsFallbackStatus,
  httpsLetsEncryptStatus,
  httpsNamesChangedStatus,
  httpsRestoredStatus,
  httpsStatus,
  httpsTestCAStatus,
} from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
}

let sent: Sent[];
let status: () => Response | Promise<Response>;
let me: () => Response;
let resume: () => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;
let errorToast: ReturnType<typeof vi.spyOn>;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));
const viewer = () => Response.json(makeMe({ allEnvironments: "viewer" }));

function problem(code: number, type: string) {
  return Response.json(
    { type, title: "x", status: code, detail: "d" },
    { status: code, headers: { "Content-Type": "application/problem+json" } },
  );
}

beforeEach(() => {
  sent = [];
  status = () => Response.json(httpsStatus());
  me = instanceAdmin;
  resume = () => new Response(null, { status: 204 });
  successToast = vi.spyOn(toast, "success");
  errorToast = vi.spyOn(toast, "error");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/me") return me();
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/directory") return Response.json({ profiles: [] });
      if (path === "/api/https/acme/resume" && req.method === "POST") return resume();
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

function renderUI(ui: React.ReactNode) {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>{ui}</TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "Let's Encrypt" }));

// The card, once the status and who is signed in have both arrived.
async function ready() {
  renderUI(<AcmeCard />);
  await card().findByText("Names on the certificate");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

// The text of every note in the card, the persistent state alerts among them.
const notes = () => card().queryAllByRole("note");
const noteWith = (title: string) =>
  notes().find((n) => within(n).queryByText(title) !== null) as HTMLElement | undefined;

describe("Paused after restore (ACME-08, D-20)", () => {
  it("shows the warning with a primary Resume button, and resuming clears it with a toast", async () => {
    status = () => Response.json(httpsRestoredStatus());
    resume = () => {
      status = () => Response.json(httpsLetsEncryptStatus());
      return new Response(null, { status: 204 });
    };
    await ready();

    const alert = noteWith("Paused after restore") as HTMLElement;
    expect(alert).toBeTruthy();
    expect(alert.textContent).toContain("This instance was restored from a backup.");
    const button = within(alert).getByRole("button", { name: "Resume Let's Encrypt" });
    fireEvent.click(button);

    await waitFor(() => expect(noteWith("Paused after restore")).toBeUndefined());
    const resumes = sent.filter((s) => s.path === "/api/https/acme/resume");
    expect(resumes).toHaveLength(1);
    expect(resumes[0]?.method).toBe("POST");
    expect(resumes[0]?.csrf).toBe("csrf-1");
    expect(successToast).toHaveBeenCalledTimes(1);
    const content = successToast.mock.calls[0]?.[0] as { props: { children: string } };
    expect(content.props.children).toBe("Let's Encrypt resumed.");
    expect(card().queryByRole("button", { name: "Resume Let's Encrypt" })).toBeNull();
  });
});
