// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { BackupSchedule } from "@/api/backup";
import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { SchedulePanel } from "@/features/backup/schedule-panel";
import "@/i18n";

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
  body?: Record<string, unknown>;
}

const PASS = "a long scheduled passphrase";
const NAME = "bayplan-backup-20261008-0300.bayplan";

let sent: Sent[];
let schedule: BackupSchedule;
let files: { name: string; size: number; created: string }[];
let putAnswer: (body: Record<string, unknown>) => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;
let errorToast: ReturnType<typeof vi.spyOn>;

const saved: BackupSchedule = {
  enabled: true,
  frequency: "daily",
  weekday: 0,
  timeUtc: "03:00",
  keep: 7,
  passphraseSaved: true,
  dir: "/data/backups",
  nextRun: "2026-10-09T03:00:00Z",
  lastRun: { at: "2026-10-08T03:00:09Z", ok: true, fileName: NAME, size: 1_572_864 },
  lastSuccess: { at: "2026-10-08T03:00:09Z", ok: true, fileName: NAME, size: 1_572_864 },
};

beforeEach(() => {
  sent = [];
  schedule = saved;
  files = [{ name: NAME, size: 1_572_864, created: "2026-10-08T03:00:00Z" }];
  successToast = vi.spyOn(toast, "success");
  errorToast = vi.spyOn(toast, "error");
  setCsrfToken("csrf-1");
  putAnswer = (body) => Response.json({ ...schedule, ...body, passphraseSaved: true });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method, csrf: req.headers.get("X-CSRF-Token") };
      if (req.method === "PUT") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/backup/info") {
        return Response.json({ minPassphraseLength: 16, fileExtension: ".bayplan" });
      }
      if (path === "/api/backup/schedule" && req.method === "GET") return Response.json(schedule);
      if (path === "/api/backup/schedule" && req.method === "PUT") {
        return putAnswer(entry.body ?? {});
      }
      if (path === "/api/backup/files") return Response.json({ items: files });
      if (path === `/api/backup/files/${NAME}`) {
        return new Response("backup-bytes", {
          headers: {
            "Content-Type": "application/octet-stream",
            "Content-Disposition": `attachment; filename="${NAME}"`,
          },
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
  URL.createObjectURL = vi.fn(() => "blob:test");
  URL.revokeObjectURL = vi.fn();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
  setCsrfToken(null);
});

function renderPanel() {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <SchedulePanel />
      </TooltipProvider>
      <Toaster />
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "Scheduled backups" }));
const puts = () => sent.filter((s) => s.method === "PUT");

describe("Scheduled backups card", () => {
  it("shows the saved schedule, where the files go and how the last run went", async () => {
    renderPanel();
    const on = await card().findByRole("switch", { name: "Back up automatically" });
    expect(on.getAttribute("aria-checked")).toBe("true");
    expect(card().getByRole("combobox", { name: "How often" }).textContent).toContain("Every day");
    expect((card().getByLabelText("Time (UTC)") as HTMLInputElement).value).toBe("03:00");
    expect((card().getByLabelText("Keep the newest") as HTMLInputElement).value).toBe("7");
    expect(card().getByText("Older backups are deleted.")).toBeTruthy();
    const location = card().getByLabelText("Location") as HTMLInputElement;
    expect(location.value).toBe("/data/backups");
    expect(location.readOnly).toBe(true);
    expect(card().getByText(/^Last backup .*, 1\.5 MiB\.$/)).toBeTruthy();
    expect(card().getByText(/^Next backup .*\.$/)).toBeTruthy();
    expect((card().getByLabelText("Passphrase") as HTMLInputElement).placeholder).toBe(
      "Saved. Leave empty to keep it.",
    );
  });

  it("saves the schedule with the passphrase in the body, says so, and empties the passphrase fields", async () => {
    renderPanel();
    await card().findByRole("switch", { name: "Back up automatically" });
    fireEvent.change(card().getByLabelText("Keep the newest"), { target: { value: "14" } });
    fireEvent.change(card().getByLabelText("Passphrase"), { target: { value: PASS } });
    fireEvent.change(card().getByLabelText("Confirm passphrase"), { target: { value: PASS } });
    fireEvent.click(card().getByRole("button", { name: "Save schedule" }));

    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.csrf).toBe("csrf-1");
    expect(puts()[0]?.body).toEqual({
      enabled: true,
      frequency: "daily",
      weekday: 0,
      timeUtc: "03:00",
      keep: 14,
      passphrase: PASS,
    });
    await waitFor(() => expect(successToast).toHaveBeenCalled());
    expect(screen.getByText("Schedule saved.")).toBeTruthy();
    await waitFor(() =>
      expect((card().getByLabelText("Passphrase") as HTMLInputElement).value).toBe(""),
    );
    expect((card().getByLabelText("Confirm passphrase") as HTMLInputElement).value).toBe("");
    for (const call of vi.mocked(fetch).mock.calls) {
      expect((call[0] as Request).url).not.toContain(PASS);
    }
  });

  it("leaves the passphrase out of the body when it is empty, so the saved one stays", async () => {
    renderPanel();
    await card().findByRole("switch", { name: "Back up automatically" });
    fireEvent.click(card().getByRole("button", { name: "Save schedule" }));
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).not.toHaveProperty("passphrase");
    expect(errorToast).not.toHaveBeenCalled();
  });
});

describe("Saved backups", () => {
  it("lists each file with its size and downloads one on request", async () => {
    const link = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      expect(this.download).toBe(NAME);
    });
    renderPanel();
    expect(await card().findByText(NAME)).toBeTruthy();
    expect(card().getByRole("heading", { name: "Saved backups" })).toBeTruthy();
    expect(card().getByText("1.5 MiB")).toBeTruthy();

    fireEvent.click(card().getByRole("button", { name: `Download ${NAME}` }));
    await waitFor(() => expect(link).toHaveBeenCalledTimes(1));
    expect(URL.createObjectURL).toHaveBeenCalledTimes(1);
  });
});
