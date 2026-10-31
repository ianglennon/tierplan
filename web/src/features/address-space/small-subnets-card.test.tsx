// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { roomFor, SmallSubnetsCard } from "@/features/address-space/small-subnets-card";
import "@/i18n";

// The Default stack subnets card of an environment's settings page (NET-06, D-21): the opt-in, its
// size and the capacity note, saved with the version it was read at.

interface Sent {
  method: string;
  path: string;
  body: unknown;
}

let sent: Sent[];
let settings: Record<string, unknown>;
let putAnswer: () => Response;
let pools: Record<string, unknown>[];

const pool = (extra: Record<string, unknown> = {}) => ({
  cidr: "10.0.0.0/8",
  mask: 24,
  capacity: 65536,
  used: 36,
  percent: 0,
  state: "normal",
  source: "swarm",
  estimated: false,
  hostRouteBlocks: 0,
  largestFree: "10.0.0.0/9",
  largestFreeSubnets: 32768,
  ...extra,
});

beforeEach(() => {
  sent = [];
  setCsrfToken("csrf-1");
  pools = [pool()];
  settings = {
    smallSubnets: { enabled: false, mask: 26 },
    pools: [],
    version: 4,
    changedBy: "",
  };
  putAnswer = () => Response.json({ ...settings, smallSubnets: { enabled: true, mask: 26 } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "PUT"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      if (path === "/api/env/e1/address-space/settings") return Response.json(settings);
      if (path === "/api/env/e1/address-space/small-subnets" && req.method === "PUT") {
        return putAnswer();
      }
      if (path === "/api/env/e1/address-space") {
        return Response.json({ envType: "swarm", measured: true, pools, overlays: [], kept: [] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  setCsrfToken(null);
});

async function mount(canEdit = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <SmallSubnetsCard envId="e1" envName="prod" canEdit={canEdit} />
      </TooltipProvider>
      <Toaster />
    </QueryClientProvider>,
  );
  await screen.findByRole("switch", { name: "Use smaller subnets for new stack networks" });
  await act(async () => {});
}

const toggle = () =>
  screen.getByRole("switch", { name: "Use smaller subnets for new stack networks" });
const size = () => screen.getByRole("combobox", { name: "Subnet size" });
const save = () => screen.getByRole("button", { name: "Save subnet settings" });
const puts = () => sent.filter((s) => s.method === "PUT");

describe("roomFor", () => {
  it("cuts each free block of the pool into blocks of the size", () => {
    // 65536 - 36 free /24 blocks, each holding four /26 blocks.
    expect(roomFor([pool() as never], 26)).toBe((65536 - 36) * 4);
    expect(roomFor([pool() as never], 24)).toBe(65536 - 36);
  });

  it("says nothing without a measured pool, or for a size larger than the pool's own", () => {
    expect(roomFor([], 26)).toBeUndefined();
    expect(roomFor(null, 26)).toBeUndefined();
    expect(roomFor([pool({ state: "unknown" }) as never], 26)).toBeUndefined();
    expect(roomFor([pool({ mask: 27 }) as never], 26)).toBeUndefined();
  });
});

describe("SmallSubnetsCard", () => {
  it("shows its title, description, and off by default at /26 with the size disabled", async () => {
    await mount();
    expect(screen.getByRole("heading", { name: "Default stack subnets" })).toBeTruthy();
    expect(document.getElementById("default-subnets")).toBeTruthy();
    expect(
      screen.getByText(/Existing networks and your compose files are never changed\./),
    ).toBeTruthy();
    expect(toggle().getAttribute("aria-checked")).toBe("false");
    expect(size().textContent).toContain("/26 · 61 addresses");
    expect(size().getAttribute("aria-disabled")).toBe("true");
  });

  it("words the capacity note with the room left in the pool", async () => {
    await mount();
    expect(
      await screen.findByText(
        "A /26 holds 61 addresses. The pool has room for about 262,000 more networks of this size.",
      ),
    ).toBeTruthy();
  });

  it("drops the second sentence while the pool is unknown", async () => {
    pools = [];
    await mount();
    expect(screen.getByText("A /26 holds 61 addresses.")).toBeTruthy();
    expect(screen.queryByText(/room for about/)).toBeNull();
  });

  it("saves the setting with the read version", async () => {
    await mount();
    fireEvent.click(toggle());
    expect(size().getAttribute("aria-disabled")).toBeNull();
    fireEvent.keyDown(size(), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "/27 · 29 addresses" }));
    fireEvent.click(save());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]).toEqual({
      method: "PUT",
      path: "/api/env/e1/address-space/small-subnets",
      body: { enabled: true, mask: 27, version: 4 },
    });
    expect(await screen.findByText("Saved the subnet settings.")).toBeTruthy();
  });

  it("shows the conflict with reload, and reload reads the settings again", async () => {
    putAnswer = () =>
      Response.json(
        {
          status: 409,
          type: "settings_changed",
          errors: [{ location: "changedBy", message: "Sam" }],
        },
        { status: 409 },
      );
    await mount();
    fireEvent.click(toggle());
    fireEvent.click(save());
    const line = await screen.findByText(
      "Sam changed these settings while you were editing. Reload to see them.",
    );
    expect(line.closest("[role=alert]")).toBeTruthy();
    settings = { ...settings, smallSubnets: { enabled: true, mask: 28 }, version: 5 };
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(toggle().getAttribute("aria-checked")).toBe("true"));
    expect(size().textContent).toContain("/28 · 13 addresses");
    expect(
      screen.queryByText("Sam changed these settings while you were editing. Reload to see them."),
    ).toBeNull();
  });

  it("shows a save error inline", async () => {
    putAnswer = () => new Response("boom", { status: 500 });
    await mount();
    fireEvent.click(toggle());
    fireEvent.click(save());
    const line = await screen.findByText("Couldn't save the subnet settings. Try again.");
    expect(line.getAttribute("role")).toBe("alert");
  });

  it("is read-only below Env Admin, with the reason on Save", async () => {
    await mount(false);
    expect(toggle().hasAttribute("disabled")).toBe(true);
    expect(save().getAttribute("aria-disabled")).toBe("true");
    expect(save().getAttribute("aria-describedby")).toBeTruthy();
    expect(document.body.textContent).toContain("Needs Admin on prod");
    fireEvent.click(save());
    expect(puts()).toEqual([]);
  });
});
