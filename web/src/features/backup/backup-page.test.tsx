// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
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
let minLength: number;
// Every toast goes through sonner's toast.success or toast.error, so "no toast"
// is a spy that was never called: the Toaster itself is global and still holds
// the toasts of earlier tests.
let successToast: ReturnType<typeof vi.spyOn>;
let errorToast: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  sent = [];
  saved = [];
  minLength = 16;
  successToast = vi.spyOn(toast, "success");
  errorToast = vi.spyOn(toast, "error");
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
        return Response.json({ minPassphraseLength: minLength, fileExtension: ".bayplan" });
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

const FAILED = "Couldn't create the backup. Check the container logs, then try again.";

const download = () =>
  screen.getByRole("button", { name: /^(Download backup|Preparing backup…)$/ });

describe("Backup page layout", () => {
  it("shows the intro, then the Download card, then the collapsible Master key note", async () => {
    renderPage();
    const intro = screen.getByText(/^A backup holds users, teams, access, settings/);
    expect(intro.textContent).toBe(
      "A backup holds users, teams, access, settings, environments and the audit log, and the same for anything added in later releases. It doesn't hold the master key, sessions or pending links. Keep the backup passphrase safe: Bayplan can't recover it.",
    );
    const card = screen.getByRole("heading", { name: "Download a backup" });
    const note = screen.getByRole("button", { name: "Master key" });
    expect(intro.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(card.compareDocumentPosition(note) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps the rotation command behind the Master key trigger, as an offline command", async () => {
    renderPage();
    const trigger = screen.getByRole("button", { name: "Master key" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText(/Rotate the master key from the command line/)).toBeNull();

    fireEvent.click(trigger);
    expect(
      await screen.findByText(
        "Rotate the master key from the command line while Bayplan is stopped:",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText("docker run --rm -v <volume>:/data ghcr.io/ianglennon/bayplan key rotate"),
    ).toBeTruthy();
    expect(
      screen.getByText("The old key is kept as bayplan.key.old until you delete it."),
    ).toBeTruthy();
    expect(screen.queryByText(/docker exec/)).toBeNull();
  });
});

describe("Download card fields", () => {
  it("takes both passphrases as password inputs the browser will not autofill, each with a show and hide toggle", async () => {
    renderPage();
    for (const label of ["Passphrase", "Confirm passphrase"]) {
      const input = field(label);
      expect(input.type).toBe("password");
      expect(input.autocomplete).toBe("new-password");
    }
    const toggles = screen.getAllByRole("button", { name: "Show passphrase" });
    expect(toggles).toHaveLength(2);
    fireEvent.click(toggles[0] as HTMLElement);
    expect(field("Passphrase").type).toBe("text");
    expect(field("Confirm passphrase").type).toBe("password");
    fireEvent.click(screen.getByRole("button", { name: "Hide passphrase" }));
    expect(field("Passphrase").type).toBe("password");
  });

  it("states the minimum the server gave", async () => {
    minLength = 20;
    renderPage();
    expect(await screen.findByText("At least 20 characters.")).toBeTruthy();
  });

  it("asks for the minimum from the server and refuses a shorter passphrase without sending it", async () => {
    minLength = 20;
    renderPage();
    await screen.findByText("At least 20 characters.");
    fill("nineteen characters"); // 19 characters
    fireEvent.click(download());
    expect(await screen.findByText("Use at least 20 characters.")).toBeTruthy();
    expect(field("Passphrase").getAttribute("aria-invalid")).toBe("true");
    expect(exports()).toHaveLength(0);
  });

  it("refuses passphrases that differ, without sending anything", async () => {
    renderPage();
    fill(PASS, `${PASS}!`);
    fireEvent.click(download());
    expect(await screen.findByText("The passphrases don't match.")).toBeTruthy();
    expect(field("Confirm passphrase").getAttribute("aria-invalid")).toBe("true");
    expect(exports()).toHaveLength(0);
  });

  it("counts characters, not bytes: sixteen accented letters are enough", async () => {
    renderPage();
    const sixteen = "é".repeat(16);
    fill(sixteen);
    fireEvent.click(download());
    await waitFor(() => expect(exports()).toHaveLength(1));
    expect(exports()[0]?.body).toEqual({ passphrase: sixteen });
  });
});

describe("Download card while the backup is made", () => {
  it("says Preparing backup… with a spinner, disables the fields and sends a second click nowhere", async () => {
    let release: (r: Response) => void = () => {};
    answer = () =>
      new Promise<Response>((resolve) => {
        release = resolve;
      });
    renderPage();
    fill(PASS);
    fireEvent.click(download());

    const pending = await screen.findByRole("button", { name: "Preparing backup…" });
    expect(pending.querySelector("[role=status], svg")).toBeTruthy();
    expect(field("Passphrase").matches(":disabled")).toBe(true);
    expect(field("Confirm passphrase").matches(":disabled")).toBe(true);
    fireEvent.click(pending);
    fireEvent.submit(pending.closest("form") as HTMLFormElement);
    expect(exports()).toHaveLength(1);

    release(
      new Response("bytes", {
        headers: { "Content-Disposition": 'attachment; filename="b.bayplan"' },
      }),
    );
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(screen.getByRole("button", { name: "Download backup" })).toBeTruthy();
  });

  it("toasts Backup downloaded., clears both fields and keeps the passphrase nowhere on the page", async () => {
    renderPage();
    fill(PASS);
    fireEvent.click(download());

    expect(await screen.findByText("Backup downloaded.")).toBeTruthy();
    expect(successToast).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(field("Passphrase").value).toBe(""));
    expect(field("Confirm passphrase").value).toBe("");
    expect(document.body.innerHTML).not.toContain(PASS);
    expect(document.body.textContent).not.toContain(PASS);
    expect(field("Passphrase").matches(":disabled")).toBe(false);
    expect(screen.queryByText(FAILED)).toBeNull();
  });

  it.each([
    [409, { type: "backup_busy" }],
    [409, { type: "audit_chain_broken" }],
    [500, { title: "Internal Server Error" }],
  ])("shows the inline alert and no toast when the server answers %s", async (status, body) => {
    answer = () => Response.json(body, { status });
    renderPage();
    fill(PASS);
    fireEvent.click(download());

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(FAILED);
    expect(saved).toHaveLength(0);
    expect(successToast).not.toHaveBeenCalled();
    expect(errorToast).not.toHaveBeenCalled();
    // The person can correct and try again: the fields are live and still hold what was typed.
    expect(field("Passphrase").matches(":disabled")).toBe(false);
    expect(field("Passphrase").value).toBe(PASS);
  });

  it("shows the same alert when the server cannot be reached", async () => {
    answer = () => Promise.reject(new TypeError("Failed to fetch"));
    renderPage();
    fill(PASS);
    fireEvent.click(download());
    expect((await screen.findByRole("alert")).textContent).toBe(FAILED);
    expect(successToast).not.toHaveBeenCalled();
    expect(errorToast).not.toHaveBeenCalled();
  });

  it("clears the alert when the person tries again", async () => {
    answer = () => Response.json({}, { status: 500 });
    renderPage();
    fill(PASS);
    fireEvent.click(download());
    await screen.findByRole("alert");

    answer = () =>
      new Response("bytes", {
        headers: { "Content-Disposition": 'attachment; filename="b.bayplan"' },
      });
    fireEvent.click(download());
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(screen.queryByText(FAILED)).toBeNull();
  });

  it("points a passphrase the server calls too short at the field", async () => {
    answer = () => Response.json({ type: "passphrase_too_short" }, { status: 422 });
    minLength = 16;
    renderPage();
    fill(PASS);
    fireEvent.click(download());
    expect(await screen.findByText("Use at least 16 characters.")).toBeTruthy();
    expect(screen.queryByText(FAILED)).toBeNull();
  });
});
