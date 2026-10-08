// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { getCsrfToken, setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { BackupPage } from "@/features/backup/backup-page";
import * as outcome from "@/features/backup/restore-outcome";
import "@/i18n";

// The poll after the restart is the clock's business, not these tests': it is
// replaced by a promise the test settles.
vi.mock("@/features/backup/restore-outcome", async (original) => ({
  ...(await original<typeof import("@/features/backup/restore-outcome")>()),
  waitForRestoreOutcome: vi.fn(),
}));

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
  body?: unknown;
  contentType?: string | null;
}

const PASS = "correct horse battery";
const SUMMARY = {
  appVersion: "v0.1.0",
  chainVerified: true,
  counts: { users: 3, teams: 2, environments: 4, auditEntries: 120 },
  created: "2026-10-07T10:30:00Z",
  instanceName: "Old lab",
  older: false,
  publicUrl: "https://old.example.com",
  publicUrlDiffers: false,
  schema: 12,
};

let sent: Sent[];
let answers: Record<string, () => Response | Promise<Response>>;

beforeEach(() => {
  sent = [];
  setCsrfToken("csrf-1");
  answers = {
    "/api/backup/info": () =>
      Response.json({ minPassphraseLength: 16, fileExtension: ".bayplan", maxUploadBytes: 1e9 }),
    "/api/instance": () =>
      Response.json({ instanceName: "Home lab", needsSetup: false, rememberDays: 30 }),
    "/api/backup/restore/upload": () =>
      Response.json(
        { uploadId: "up-1", size: 5, expiresAt: "2026-10-08T12:00:00Z" },
        { status: 201 },
      ),
    "/api/backup/restore/check": () => Response.json(SUMMARY),
    "/api/backup/restore/apply": () => Response.json({ restarting: true }, { status: 202 }),
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (typeof input === "string") {
        // The raw upload: a string address and an init with the file as the body.
        const path = new URL(input, window.location.origin).pathname;
        const headers = new Headers(init?.headers);
        sent.push({
          path,
          method: init?.method ?? "GET",
          csrf: headers.get("X-CSRF-Token"),
          body: init?.body,
          contentType: headers.get("Content-Type"),
        });
        return (answers[path] ?? (() => Response.json({}, { status: 404 })))();
      }
      const req = input as Request;
      const path = new URL(req.url).pathname;
      sent.push({
        path,
        method: req.method,
        csrf: req.headers.get("X-CSRF-Token"),
        body: req.method === "POST" ? await req.clone().json() : undefined,
      });
      return (answers[path] ?? (() => Response.json({}, { status: 404 })))();
    }),
  );
  vi.mocked(outcome.waitForRestoreOutcome).mockReset();
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
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "Restore from a backup" }));
const calls = (path: string) => sent.filter((s) => s.path === path);
const FILE = () => new File(["backup-bytes"], "lab.bayplan", { type: "application/octet-stream" });

function chooseAndCheck() {
  fireEvent.change(card().getByLabelText("Backup file"), { target: { files: [FILE()] } });
  fireEvent.change(card().getByLabelText("Passphrase"), { target: { value: PASS } });
  fireEvent.click(card().getByRole("button", { name: "Check backup" }));
}

async function reachConfirm() {
  renderPage();
  await screen.findByRole("heading", { name: "Restore from a backup" });
  chooseAndCheck();
  await card().findByText("What's in this backup");
  fireEvent.click(card().getByRole("button", { name: "Restore backup" }));
  const dialog = await screen.findByRole("alertdialog", { name: "Restore this backup?" });
  // The instance name is read from /api/instance; wait for the label to know it.
  await within(dialog).findByLabelText("Type Home lab to confirm");
  return dialog;
}

describe("Restore card (tracer): Settings > Backup to Backup restored", () => {
  it("sits between the Download card and the Master key note", async () => {
    renderPage();
    const titles = screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent);
    expect(titles.slice(0, 2)).toEqual(["Download a backup", "Restore from a backup"]);
    expect(
      card().getByText("Replace everything in this Bayplan with the contents of a backup file."),
    ).toBeTruthy();
    expect(card().getByLabelText("Backup file")).toBeTruthy();
    expect(card().getByText("A .bayplan file.")).toBeTruthy();
  });

  it("uploads nothing until Check backup is pressed, then uploads the file and checks it with the passphrase in the body", async () => {
    renderPage();
    fireEvent.change(card().getByLabelText("Backup file"), { target: { files: [FILE()] } });
    fireEvent.change(card().getByLabelText("Passphrase"), { target: { value: PASS } });
    expect(sent.filter((s) => s.path.includes("/restore/"))).toHaveLength(0);

    fireEvent.click(card().getByRole("button", { name: "Check backup" }));
    await card().findByText("What's in this backup");

    const upload = calls("/api/backup/restore/upload");
    expect(upload).toHaveLength(1);
    expect(upload[0]?.method).toBe("POST");
    expect(upload[0]?.contentType).toBe("application/octet-stream");
    expect(upload[0]?.csrf).toBe("csrf-1");
    expect((upload[0]?.body as File).name).toBe("lab.bayplan");
    expect(calls("/api/backup/restore/check")[0]?.body).toEqual({
      uploadId: "up-1",
      passphrase: PASS,
    });
    for (const s of sent) expect(s.path).not.toContain(PASS);
  });

  it("replaces the fields with the summary and offers Choose another file and Restore backup", async () => {
    renderPage();
    chooseAndCheck();
    await card().findByText("What's in this backup");
    expect(card().getByText("From Old lab")).toBeTruthy();
    expect(card().getByText("Bayplan v0.1.0")).toBeTruthy();
    expect(card().getByText(/^Made /)).toBeTruthy();
    expect(card().getByText("3 users, 2 teams, 4 environments, 120 audit entries")).toBeTruthy();
    expect(card().queryByLabelText("Backup file")).toBeNull();
    expect(card().getByRole("button", { name: "Choose another file" })).toBeTruthy();
    expect(card().getByRole("button", { name: "Restore backup" })).toBeTruthy();
  });

  it("keeps the destructive confirm aria-disabled until the instance name is typed, then sends apply with it", async () => {
    vi.mocked(outcome.waitForRestoreOutcome).mockResolvedValue("restored");
    const dialog = await reachConfirm();
    const confirm = within(dialog).getByRole("button", { name: "Restore backup" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(calls("/api/backup/restore/apply")).toHaveLength(0);

    fireEvent.change(within(dialog).getByLabelText("Type Home lab to confirm"), {
      target: { value: "Home lab" },
    });
    expect(confirm.getAttribute("aria-disabled")).toBe("false");
    fireEvent.click(confirm);
    await waitFor(() => expect(calls("/api/backup/restore/apply")).toHaveLength(1));
    expect(calls("/api/backup/restore/apply")[0]?.body).toEqual({
      uploadId: "up-1",
      passphrase: PASS,
      confirm: "Home lab",
    });
    expect(calls("/api/backup/restore/apply")[0]?.csrf).toBe("csrf-1");
  });

  it("shows the Restoring status while Bayplan restarts, then Backup restored with the cache cleared and the CSRF token dropped", async () => {
    let settle: (o: outcome.RestoreOutcome) => void = () => {};
    vi.mocked(outcome.waitForRestoreOutcome).mockImplementation(
      () => new Promise((resolve) => (settle = resolve)),
    );
    queryClient.setQueryData(["me"], { user: "old" });
    const dialog = await reachConfirm();
    fireEvent.change(within(dialog).getByLabelText("Type Home lab to confirm"), {
      target: { value: "Home lab" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore backup" }));

    const status = await screen.findByRole("status");
    expect(within(status).getByText("Restoring…")).toBeTruthy();
    expect(within(status).getByText("Don't close this page. This takes a moment.")).toBeTruthy();

    settle("restored");
    expect(await screen.findByRole("heading", { name: "Backup restored" })).toBeTruthy();
    expect(
      screen.getByText("You were signed out. Sign in with an account from the backup."),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Go to sign-in" }).getAttribute("href")).toBe("/login");
    expect(screen.getByText(/^Agents reconnect on their own/)).toBeTruthy();
    expect(queryClient.getQueryData(["me"])).toBeUndefined();
    expect(getCsrfToken()).toBeNull();
    // The old page is gone.
    expect(screen.queryByRole("heading", { name: "Download a backup" })).toBeNull();
  });
});
