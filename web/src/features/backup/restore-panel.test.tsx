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
    expect(upload[0]?.body).toBeInstanceOf(File);
    expect((upload[0]?.body as File | undefined)?.name).toBe("lab.bayplan");
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

// The documented message of each way a check can end (UI-SPEC "Restore card").
const UNREADABLE =
  "Couldn't open this backup. Check the passphrase, and that the file isn't damaged.";
const NOT_A_BACKUP = "That isn't a Bayplan backup file.";
const CUT_OFF =
  "The upload was cut off. If Bayplan runs behind a reverse proxy, check its upload size limit.";
const BUSY = "Another restore is being prepared. Try again in a few minutes.";

const problem = (status: number, type: string, detail = "") =>
  Response.json({ status, type, detail }, { status });

describe("Restore card errors on the check", () => {
  const cases: [string, string, () => Response, string][] = [
    ["a wrong passphrase or damage", "check", () => problem(422, "backup_unreadable"), UNREADABLE],
    ["not a backup", "check", () => problem(422, "not_a_backup"), NOT_A_BACKUP],
    [
      "a newer Bayplan, naming its version",
      "check",
      () =>
        problem(
          409,
          "backup_newer",
          "This backup was made by a newer Bayplan (v9.9.9). Upgrade this Bayplan to v9.9.9 or newer, then try again.",
        ),
      "This backup is from a newer Bayplan (v9.9.9). Upgrade this Bayplan, then try again.",
    ],
    [
      "a newer Bayplan that did not name its version",
      "check",
      () => problem(409, "backup_newer", "This backup was made by a newer Bayplan."),
      "This backup is from a newer Bayplan. Upgrade this Bayplan, then try again.",
    ],
    [
      "an upload cut off (400 upload_incomplete)",
      "upload",
      () => problem(400, "upload_incomplete"),
      CUT_OFF,
    ],
    [
      "an upload above the cap (413 upload_too_large)",
      "upload",
      () => problem(413, "upload_too_large"),
      CUT_OFF,
    ],
    [
      "a proxy's own 413 page",
      "upload",
      () => new Response("<html>413 Request Entity Too Large</html>", { status: 413 }),
      CUT_OFF,
    ],
    ["another restore in progress", "upload", () => problem(409, "restore_busy"), BUSY],
  ];

  it.each(cases)(
    "shows the documented message for %s, inline with role=alert",
    async (_name, step, make, message) => {
      answers[`/api/backup/restore/${step}`] = make;
      renderPage();
      chooseAndCheck();
      const alert = await card().findByRole("alert");
      expect(alert.textContent).toBe(message);
      // The form is still there to try again, and nothing was summarised.
      expect(card().getByLabelText("Backup file")).toBeTruthy();
      expect(card().queryByText("What's in this backup")).toBeNull();
    },
  );

  it("says Bayplan could not be reached when the connection fails, not that the backup is bad", async () => {
    answers["/api/backup/restore/upload"] = () => {
      throw new TypeError("Failed to fetch");
    };
    renderPage();
    chooseAndCheck();
    const alert = await card().findByRole("alert");
    expect(alert.textContent).not.toBe(UNREADABLE);
    expect(alert.textContent).toMatch(/reach|connect/i);
  });

  it("clears the message when the check is tried again", async () => {
    answers["/api/backup/restore/check"] = () => problem(422, "backup_unreadable");
    renderPage();
    chooseAndCheck();
    await card().findByRole("alert");
    answers["/api/backup/restore/check"] = () => Response.json(SUMMARY);
    fireEvent.click(card().getByRole("button", { name: "Check backup" }));
    await card().findByText("What's in this backup");
    expect(card().queryByRole("alert")).toBeNull();
  });
});

describe("Restore card loading states", () => {
  it("shows Checking… with a spinner while the file uploads and is checked, and locks the fields", async () => {
    let release: () => void = () => {};
    answers["/api/backup/restore/upload"] = () =>
      new Promise<Response>((resolve) => {
        release = () =>
          resolve(Response.json({ uploadId: "up-1", size: 5, expiresAt: "x" }, { status: 201 }));
      });
    renderPage();
    chooseAndCheck();
    const button = await card().findByRole("button", { name: "Checking…" });
    expect(button.querySelector('[data-slot="spinner"], svg')).not.toBeNull();
    expect((card().getByLabelText("Passphrase") as HTMLInputElement).disabled).toBe(true);
    release();
    await card().findByText("What's in this backup");
  });

  it("does not let Check backup run without a file and a passphrase", () => {
    renderPage();
    const button = card().getByRole("button", { name: "Check backup" }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    fireEvent.change(card().getByLabelText("Passphrase"), { target: { value: PASS } });
    expect(button.disabled).toBe(true);
    fireEvent.change(card().getByLabelText("Backup file"), { target: { files: [FILE()] } });
    expect(button.disabled).toBe(false);
  });

  it("holds a beforeunload prompt while the Restoring status shows, and not after", async () => {
    let settle: (o: outcome.RestoreOutcome) => void = () => {};
    vi.mocked(outcome.waitForRestoreOutcome).mockImplementation(
      () => new Promise((resolve) => (settle = resolve)),
    );
    const before = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(before);
    expect(before.defaultPrevented).toBe(false);

    const dialog = await reachConfirm();
    fireEvent.change(within(dialog).getByLabelText("Type Home lab to confirm"), {
      target: { value: "Home lab" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore backup" }));
    await screen.findByRole("status");
    const during = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(during);
    expect(during.defaultPrevented).toBe(true);

    settle("restored");
    await screen.findByRole("heading", { name: "Backup restored" });
    const after = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(after);
    expect(after.defaultPrevented).toBe(false);
  });

  it("shows a pending Restore backup inside the dialog while apply is in flight, and cannot be closed", async () => {
    let release: () => void = () => {};
    answers["/api/backup/restore/apply"] = () =>
      new Promise<Response>((resolve) => {
        release = () => resolve(Response.json({ restarting: true }, { status: 202 }));
      });
    vi.mocked(outcome.waitForRestoreOutcome).mockResolvedValue("restored");
    const dialog = await reachConfirm();
    fireEvent.change(within(dialog).getByLabelText("Type Home lab to confirm"), {
      target: { value: "Home lab" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore backup" }));
    const pending = await within(dialog).findByRole("button", { name: "Applying backup…" });
    expect(pending.getAttribute("aria-disabled")).toBe("true");
    expect(
      (within(dialog).getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    release();
    await screen.findByRole("heading", { name: "Backup restored" });
  });
});

describe("Restore card failures", () => {
  async function confirmRestore() {
    const dialog = await reachConfirm();
    fireEvent.change(within(dialog).getByLabelText("Type Home lab to confirm"), {
      target: { value: "Home lab" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore backup" }));
    return dialog;
  }

  it("says nothing was changed, with the server's reason, when apply is refused", async () => {
    answers["/api/backup/restore/apply"] = () =>
      problem(500, "internal", "The database could not be prepared.");
    const dialog = await confirmRestore();
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("The restore didn't finish and nothing was changed.");
    expect(alert.textContent).toContain("The database could not be prepared.");
    expect(screen.queryByRole("status")).toBeNull();
    // The dialog stays for another go, and the session reactions are released.
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });

  it("says Bayplan went back to the snapshot when it came back and still accepts the session", async () => {
    vi.mocked(outcome.waitForRestoreOutcome).mockResolvedValue("rolled_back");
    await confirmRestore();
    const alert = await screen.findByRole("alert");
    const dialog = screen.getByRole("alertdialog");
    expect(alert.textContent).toBe("The restore failed and Bayplan went back to the snapshot.");
    // Reopened for another go, the name has to be typed again.
    expect(
      within(dialog).getByRole("button", { name: "Restore backup" }).getAttribute("aria-disabled"),
    ).toBe("true");
    expect(screen.queryByRole("heading", { name: "Backup restored" })).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("says Bayplan has not come back when nothing answers in time, without claiming a result", async () => {
    vi.mocked(outcome.waitForRestoreOutcome).mockResolvedValue("timeout");
    await confirmRestore();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Bayplan hasn't come back yet. Reload this page in a minute. If it doesn't load, check the container logs.",
    );
    expect(screen.queryByRole("heading", { name: "Backup restored" })).toBeNull();
  });

  it("treats a dropped connection on apply as the restart and waits for the outcome", async () => {
    answers["/api/backup/restore/apply"] = () => {
      throw new TypeError("Failed to fetch");
    };
    vi.mocked(outcome.waitForRestoreOutcome).mockResolvedValue("restored");
    await confirmRestore();
    expect(await screen.findByRole("heading", { name: "Backup restored" })).toBeTruthy();
    expect(outcome.waitForRestoreOutcome).toHaveBeenCalledTimes(1);
  });
});

describe("Restore card summary", () => {
  it("shows counts of 0 as 0 and still shows the summary", async () => {
    answers["/api/backup/restore/check"] = () =>
      Response.json({
        ...SUMMARY,
        counts: { users: 0, teams: 0, environments: 0, auditEntries: 0 },
      });
    renderPage();
    chooseAndCheck();
    expect(
      await card().findByText("0 users, 0 teams, 0 environments, 0 audit entries"),
    ).toBeTruthy();
  });

  it("says a single thing in the singular", async () => {
    answers["/api/backup/restore/check"] = () =>
      Response.json({
        ...SUMMARY,
        counts: { users: 1, teams: 1, environments: 1, auditEntries: 1 },
      });
    renderPage();
    chooseAndCheck();
    expect(await card().findByText("1 user, 1 team, 1 environment, 1 audit entry")).toBeTruthy();
  });

  it("notes that an older backup is upgraded while restoring", async () => {
    answers["/api/backup/restore/check"] = () => Response.json({ ...SUMMARY, older: true });
    renderPage();
    chooseAndCheck();
    expect(
      await card().findByText(
        "This backup is from an older Bayplan and is upgraded while restoring.",
      ),
    ).toBeTruthy();
  });

  it("does not add the older note for a current backup", async () => {
    renderPage();
    chooseAndCheck();
    await card().findByText("What's in this backup");
    expect(card().queryByText(/older Bayplan/)).toBeNull();
  });

  it("shows the restored public URL, and no warning while it matches the address in use", async () => {
    renderPage();
    chooseAndCheck();
    expect(await card().findByText("Restored public URL: https://old.example.com.")).toBeTruthy();
    expect(card().queryByText(/isn't the address you're using now/)).toBeNull();
  });

  it("warns, with what to do, when the restored public URL differs from the address in use", async () => {
    answers["/api/backup/restore/check"] = () =>
      Response.json({ ...SUMMARY, publicUrlDiffers: true });
    renderPage();
    chooseAndCheck();
    expect(
      await card().findByText(
        "This isn't the address you're using now. After you sign in, check Settings → General, or set BAYPLAN_PUBLIC_URL.",
      ),
    ).toBeTruthy();
  });

  it("leaves the public URL line out when the backup had none", async () => {
    answers["/api/backup/restore/check"] = () => Response.json({ ...SUMMARY, publicUrl: "" });
    renderPage();
    chooseAndCheck();
    await card().findByText("What's in this backup");
    expect(card().queryByText(/Restored public URL/)).toBeNull();
  });
});

describe("Restore card file and passphrase", () => {
  it("shows the chosen file name in mono, truncated, with the whole name in a tooltip", async () => {
    renderPage();
    expect(card().getByText("No file chosen")).toBeTruthy();
    const long = `${"a-very-long-backup-name-".repeat(6)}.bayplan`;
    fireEvent.change(card().getByLabelText("Backup file"), {
      target: { files: [new File(["x"], long)] },
    });
    const name = card().getByText(long);
    expect(name.className).toContain("font-mono");
    expect(name.className).toContain("truncate");
    expect(card().queryByText("No file chosen")).toBeNull();
    fireEvent.focus(name);
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe(long);
  });

  it("takes the passphrase as a password input with a show and hide toggle and no autofill", () => {
    renderPage();
    const input = card().getByLabelText("Passphrase") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.autocomplete).toBe("off");
    fireEvent.click(card().getByRole("button", { name: "Show passphrase" }));
    expect(input.type).toBe("text");
    fireEvent.click(card().getByRole("button", { name: "Hide passphrase" }));
    expect(input.type).toBe("password");
  });

  it("only takes .bayplan files in the picker", () => {
    renderPage();
    expect(card().getByLabelText("Backup file").getAttribute("accept")).toBe(".bayplan");
  });

  it("clears the file and the passphrase on Choose another file", async () => {
    renderPage();
    chooseAndCheck();
    await card().findByText("What's in this backup");
    fireEvent.click(card().getByRole("button", { name: "Choose another file" }));
    expect(card().getByText("No file chosen")).toBeTruthy();
    expect((card().getByLabelText("Passphrase") as HTMLInputElement).value).toBe("");
    expect(
      (card().getByRole("button", { name: "Check backup" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});
