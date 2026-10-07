// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { BackupPage } from "@/features/backup/backup-page";
import "@/i18n";

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
  body?: Record<string, unknown>;
}

const PASS = "correct horse battery";

let sent: Sent[];
let answer: () => Response | Promise<Response>;
let saved: { name: string; blob: Blob }[];

beforeEach(() => {
  sent = [];
  saved = [];
  setCsrfToken("csrf-1");
  answer = () =>
    new Response("backup-bytes", {
      headers: {
        "Content-Type": "application/octet-stream",
        "Content-Disposition": 'attachment; filename="bayplan-backup-20261007-1030.bayplan"',
      },
    });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method, csrf: req.headers.get("X-CSRF-Token") };
      if (req.method === "POST") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/backup/info") {
        return Response.json({ minPassphraseLength: 16, fileExtension: ".bayplan" });
      }
      if (path === "/api/backup/export") return answer();
      return Response.json({}, { status: 404 });
    }),
  );
  // jsdom has no object URLs; the click on the temporary link is what saves.
  URL.createObjectURL = vi.fn(() => "blob:test");
  URL.revokeObjectURL = vi.fn();
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    saved.push({ name: this.download, blob: new Blob() });
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
  setCsrfToken(null);
});

function renderPage() {
  render(
    <QueryClientProvider client={queryClient}>
      <BackupPage />
      <Toaster />
    </QueryClientProvider>,
  );
}

const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;
const exports = () => sent.filter((s) => s.path === "/api/backup/export");

function fill(passphrase: string, confirm = passphrase) {
  fireEvent.change(field("Passphrase"), { target: { value: passphrase } });
  fireEvent.change(field("Confirm passphrase"), { target: { value: confirm } });
}

describe("Download card", () => {
  it("posts the passphrase in the JSON body and hands the file to the browser under the server's name", async () => {
    renderPage();
    fill(PASS);
    fireEvent.click(screen.getByRole("button", { name: "Download backup" }));

    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]?.name).toBe("bayplan-backup-20261007-1030.bayplan");
    expect(exports()).toHaveLength(1);
    expect(exports()[0]?.body).toEqual({ passphrase: PASS });
    expect(exports()[0]?.csrf).toBe("csrf-1");
    expect(URL.createObjectURL).toHaveBeenCalledTimes(1);
  });

  it("never puts the passphrase in an address", async () => {
    renderPage();
    fill(PASS);
    fireEvent.click(screen.getByRole("button", { name: "Download backup" }));
    await waitFor(() => expect(saved).toHaveLength(1));
    for (const call of vi.mocked(fetch).mock.calls) {
      expect((call[0] as Request).url).not.toContain(PASS);
    }
  });
});

describe("Backup page", () => {
  it("is titled and shows the Download card", async () => {
    renderPage();
    expect(screen.getByRole("heading", { name: "Back up and restore Bayplan" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Download a backup" })).toBeTruthy();
  });
});
