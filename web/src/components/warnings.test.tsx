// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { DataNotPersistedBanner, HealthBell } from "@/components/warnings";
import { meQueryOptions } from "@/hooks/use-me";
import "@/i18n";

// Whether the page counts as "plain HTTP from another host" is decided by
// window.location, which jsdom does not let a test change.
let plainHttp = false;
vi.mock("@/components/auth-layout", () => ({ isPlainHttpRemote: () => plainHttp }));

const ADMIN = {
  user: { id: "u1", username: "admin", role: "admin" },
  csrfToken: "csrf-1",
  permissions: { global: ["settings.read", "settings.write"], environments: {} },
};
const VIEWER = { ...ADMIN, permissions: { global: [], environments: {} } };

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
}

let sent: Sent[] = [];
let me = ADMIN;
let items: Array<Record<string, string>> = [];
let ackStatus = 204;

beforeEach(() => {
  sent = [];
  me = ADMIN;
  items = [];
  ackStatus = 204;
  plainHttp = false;
  setCsrfToken("csrf-1");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/warnings") return Response.json({ items });
      if (path === "/api/warnings/key-backup/ack") {
        if (ackStatus === 204) {
          items = items.filter((w) => w.id !== "key_backup");
          return new Response(null, { status: 204 });
        }
        return Response.json({ status: ackStatus }, { status: ackStatus });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderWarnings() {
  queryClient.setQueryData(meQueryOptions.queryKey, me);
  render(
    <QueryClientProvider client={queryClient}>
      <DataNotPersistedBanner />
      <HealthBell />
      <Toaster />
    </QueryClientProvider>,
  );
}

const warningsCalls = () => sent.filter((s) => s.path === "/api/warnings");

async function openBell(name: string | RegExp = "Warnings") {
  fireEvent.click(await screen.findByRole("button", { name }));
}

describe("HealthBell", () => {
  it("says there are no warnings, with no count badge, when nothing applies", async () => {
    renderWarnings();
    await waitFor(() => expect(warningsCalls()).toHaveLength(1));
    const bell = screen.getByRole("button", { name: "Warnings" });
    expect(bell.textContent).toBe("");
    await openBell();
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });

  it("counts and lists the key backup reminder", async () => {
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    renderWarnings();
    const bell = await screen.findByRole("button", { name: "Warnings, 1 item" });
    expect(bell.textContent).toBe("1");
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("Back up your encryption key.")).toBeTruthy();
    expect(
      screen.getByText("Stored secrets can't be recovered without /data/bayplan.key."),
    ).toBeTruthy();
    expect(screen.queryByText("No warnings")).toBeNull();
  });

  it("lists the plain HTTP warning from a non-localhost http page, with its fix link", async () => {
    plainHttp = true;
    renderWarnings();
    await openBell("Warnings, 1 item");
    expect(await screen.findByText("You're using Bayplan over plain HTTP.")).toBeTruthy();
    const fix = screen.getByRole("link", {
      name: "Serve Bayplan over HTTPS or put it behind a reverse proxy",
    });
    expect(fix.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("counts both items together", async () => {
    plainHttp = true;
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    renderWarnings();
    expect(await screen.findByRole("button", { name: "Warnings, 2 items" })).toBeTruthy();
  });

  it("acknowledges the key backup once and the item disappears", async () => {
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    renderWarnings();
    await openBell("Warnings, 1 item");
    fireEvent.click(await screen.findByRole("button", { name: "I've backed it up" }));

    await waitFor(() => expect(screen.queryByText("Back up your encryption key.")).toBeNull());
    const acks = sent.filter((s) => s.path === "/api/warnings/key-backup/ack");
    expect(acks).toHaveLength(1);
    expect(acks[0]?.method).toBe("POST");
    expect(acks[0]?.csrf).toBe("csrf-1");
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });

  it("toasts a failure and keeps the reminder when the acknowledgement fails", async () => {
    items = [{ id: "key_backup", keyPath: "/data/bayplan.key" }];
    ackStatus = 500;
    renderWarnings();
    await openBell("Warnings, 1 item");
    fireEvent.click(await screen.findByRole("button", { name: "I've backed it up" }));
    expect(await screen.findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(screen.getByText("Back up your encryption key.")).toBeTruthy();
  });

  it("never asks the server for warnings without settings.read", async () => {
    me = VIEWER;
    renderWarnings();
    await screen.findByRole("button", { name: "Warnings" });
    expect(warningsCalls()).toHaveLength(0);
  });
});

describe("DataNotPersistedBanner", () => {
  it("shows the UI-SPEC copy when the data directory is not persisted, with no way to dismiss it", async () => {
    items = [{ id: "data_not_persisted", reason: "not a mount point" }];
    renderWarnings();
    const banner = await screen.findByRole("status");
    expect(banner.textContent).toBe(
      "Data isn't being persisted. /data isn't a mounted volume, so users, sessions and the audit log are lost when this container is removed. Mount a volume at /data and restart.",
    );
    expect(banner.querySelector("button")).toBeNull();
  });

  it("is absent when the data directory is persisted", async () => {
    renderWarnings();
    await waitFor(() => expect(warningsCalls()).toHaveLength(1));
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("is not part of the bell's count", async () => {
    items = [{ id: "data_not_persisted", reason: "tmpfs" }];
    renderWarnings();
    await screen.findByRole("status");
    expect(screen.getByRole("button", { name: "Warnings" })).toBeTruthy();
  });
});
