// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { addressSettingsQuery } from "@/api/address-space";
import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AddressPoolsCard, validatePoolRows } from "@/features/address-space/address-pools-card";
import "@/i18n";
import { TestRouter } from "@/test-router";

// The Address pools card of a standalone host's settings page (D-07 as amended): an optional
// override, validated before it is sent and saved with the version it was read at.

interface Sent {
  method: string;
  path: string;
  body: unknown;
}

let sent: Sent[];
let settings: Record<string, unknown>;
let putAnswer: () => Response;
let client: QueryClient;

beforeEach(() => {
  sent = [];
  setCsrfToken("csrf-1");
  settings = { smallSubnets: { enabled: false, mask: 26 }, pools: [], version: 7, changedBy: "" };
  putAnswer = () => Response.json({ ...settings, version: 8 });
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
      if (path === "/api/env/e1/address-space/pools" && req.method === "PUT") return putAnswer();
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
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AddressPoolsCard envId="e1" envName="lab" canEdit={canEdit} />
        </TooltipProvider>
      </TestRouter>
      <Toaster />
    </QueryClientProvider>,
  );
  await screen.findByRole("button", { name: "Save address pools" });
  await act(async () => {});
}

const echo = (key: string, params?: Record<string, unknown>) =>
  `${key}${params ? JSON.stringify(params) : ""}`;
const puts = () => sent.filter((s) => s.method === "PUT");
const base = (n: number) => screen.getByLabelText(`Base for pool ${n}`) as HTMLInputElement;
const size = (n: number) => screen.getByLabelText(`Size for pool ${n}`) as HTMLInputElement;
const add = () => screen.getByRole("button", { name: "Add pool" });
const save = () => screen.getByRole("button", { name: "Save address pools" });

function type(el: HTMLInputElement, value: string) {
  fireEvent.change(el, { target: { value } });
}

describe("validatePoolRows", () => {
  it("refuses a base with host bits, and a base that is not a prefix", () => {
    const [a, b, c] = validatePoolRows(
      [
        { base: "10.20.0.1/16", size: "24" },
        { base: "nope", size: "24" },
        { base: "fd00::/48", size: "64" },
      ],
      echo,
    );
    expect(a?.base).toContain("baseInvalid");
    expect(b?.base).toContain("baseInvalid");
    expect(c?.base).toContain("baseInvalid");
  });

  it("refuses a size outside the base's own mask to 30", () => {
    const [tooBig, tooSmall, ok, blank] = validatePoolRows(
      [
        { base: "10.20.0.0/16", size: "12" },
        { base: "10.30.0.0/16", size: "31" },
        { base: "10.40.0.0/16", size: "30" },
        { base: "10.50.0.0/16", size: "" },
      ],
      echo,
    );
    expect(tooBig?.size).toBe('envsettings.addressPools.sizeInvalid{"min":16}');
    expect(tooSmall?.size).toContain("sizeInvalid");
    expect(ok).toEqual({});
    expect(blank?.size).toContain("sizeInvalid");
  });

  it("puts the overlap on the second of two overlapping rows", () => {
    const [first, second, third] = validatePoolRows(
      [
        { base: "10.20.0.0/16", size: "24" },
        { base: "10.20.4.0/22", size: "24" },
        { base: "10.30.0.0/16", size: "24" },
      ],
      echo,
    );
    expect(first).toEqual({});
    expect(second?.base).toBe('envsettings.addressPools.overlap{"base":"10.20.4.0/22"}');
    expect(third).toEqual({});
  });

  it("finds nothing wrong with valid rows", () => {
    expect(
      validatePoolRows(
        [
          { base: "10.20.0.0/16", size: "24" },
          { base: "172.30.0.0/16", size: "24" },
        ],
        echo,
      ),
    ).toEqual([{}, {}]);
  });
});

describe("AddressPoolsCard", () => {
  it("shows the amended description and, with no entered pools, the empty state", async () => {
    await mount();
    expect(document.getElementById("address-pools")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Address pools" })).toBeTruthy();
    expect(
      screen.getByText(
        "Optional. Coaming reads the address pools the daemon reports, or Docker's defaults when daemon.json sets none. Enter pools here only if Coaming should measure against different ones.",
      ),
    ).toBeTruthy();
    // The description leads to the standalone part of the pool guidance.
    expect(
      screen.getByRole("link", { name: "Generate a daemon.json snippet" }).getAttribute("href"),
    ).toBe("/env/e1/address-space/guide?part=standalone");
    expect(screen.getByText("Using the daemon's pools")).toBeTruthy();
    expect(
      screen.getByText(
        "Coaming reads the address pools the daemon reports, or Docker's defaults when it sets none. Add pools here only to measure against different ones.",
      ),
    ).toBeTruthy();
    expect(screen.queryByText("Using Docker's default pools")).toBeNull();
    expect(add()).toBeTruthy();
  });

  it("adds a row, validates it and saves the pools with the read version", async () => {
    await mount();
    fireEvent.click(add());
    type(base(1), "10.20.0.0/16");
    type(size(1), "24");
    fireEvent.click(save());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]).toEqual({
      method: "PUT",
      path: "/api/env/e1/address-space/pools",
      body: { pools: [{ base: "10.20.0.0/16", size: 24 }], version: 7 },
    });
    expect(await screen.findByText("Saved the address pools.")).toBeTruthy();
  });

  it("refuses a bad row on the row and sends nothing", async () => {
    await mount();
    fireEvent.click(add());
    type(base(1), "10.20.0.1/16");
    type(size(1), "24");
    fireEvent.click(save());
    expect(await screen.findByText("Enter a base like 10.20.0.0/16.")).toBeTruthy();
    expect(base(1).getAttribute("aria-invalid")).toBe("true");
    type(base(1), "10.20.0.0/16");
    type(size(1), "12");
    fireEvent.click(save());
    expect(await screen.findByText("Size must be between 16 and 30.")).toBeTruthy();
    expect(puts()).toEqual([]);
  });

  it("flags an overlap on the second row", async () => {
    settings = {
      ...settings,
      pools: [
        { base: "10.20.0.0/16", size: 24 },
        { base: "10.20.4.0/22", size: 24 },
      ],
    };
    await mount();
    fireEvent.click(save());
    expect(await screen.findByText("10.20.4.0/22 overlaps another pool you entered.")).toBeTruthy();
    expect(puts()).toEqual([]);
  });

  it("moves focus to Add pool after a row is removed", async () => {
    settings = {
      ...settings,
      pools: [
        { base: "10.20.0.0/16", size: 24 },
        { base: "10.30.0.0/16", size: 24 },
      ],
    };
    await mount();
    expect(base(1).value).toBe("10.20.0.0/16");
    fireEvent.click(screen.getByRole("button", { name: "Remove pool 2" }));
    expect(screen.queryByLabelText("Base for pool 2")).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(add()));
  });

  it("shows a server pool_overlap on the row it names", async () => {
    putAnswer = () =>
      Response.json(
        {
          status: 422,
          type: "pool_overlap",
          errors: [{ location: "body.pools[0].base", message: "pool_overlap", value: 0 }],
        },
        { status: 422 },
      );
    await mount();
    fireEvent.click(add());
    type(base(1), "10.20.0.0/16");
    type(size(1), "24");
    fireEvent.click(add());
    type(base(2), "10.40.0.0/16");
    type(size(2), "24");
    fireEvent.click(save());
    const line = await screen.findByText("10.20.0.0/16 overlaps another pool you entered.");
    expect(line.getAttribute("role")).toBe("alert");
    expect(base(1).getAttribute("aria-invalid")).toBe("true");
    expect(base(2).getAttribute("aria-invalid")).toBeNull();
  });

  it("shows the conflict with reload, and a save error with Try again", async () => {
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
    fireEvent.click(add());
    type(base(1), "10.20.0.0/16");
    type(size(1), "24");
    fireEvent.click(save());
    expect(
      await screen.findByText(
        "Sam changed these settings while you were editing. Reload to see them.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();
    cleanup();

    putAnswer = () => new Response("boom", { status: 500 });
    await mount();
    fireEvent.click(add());
    type(base(1), "10.20.0.0/16");
    type(size(1), "24");
    fireEvent.click(save());
    expect(await screen.findByText("Couldn't save the address pools. Try again.")).toBeTruthy();
  });

  it("is read-only below Env Admin, with the reason on Save", async () => {
    settings = { ...settings, pools: [{ base: "10.20.0.0/16", size: 24 }] };
    await mount(false);
    expect(base(1).readOnly).toBe(true);
    expect(size(1).readOnly).toBe(true);
    expect(screen.queryByRole("button", { name: "Add pool" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove pool 1" })).toBeNull();
    expect(save().getAttribute("aria-disabled")).toBe("true");
    expect(document.body.textContent).toContain("Needs Admin on lab");
    fireEvent.click(save());
    expect(puts()).toEqual([]);
  });

  function enterPool() {
    fireEvent.click(add());
    type(base(1), "10.20.0.0/16");
    type(size(1), "24");
  }

  const conflictAnswer = (by: string) => () =>
    Response.json(
      { status: 409, type: "settings_changed", errors: [{ location: "changedBy", message: by }] },
      { status: 409 },
    );

  it("sends the newer version when only Default stack subnets was saved since the edit began", async () => {
    await mount();
    enterPool();
    await act(async () => {
      client.setQueryData(addressSettingsQuery("e1").queryKey, {
        smallSubnets: { enabled: true, mask: 27 },
        pools: [],
        version: 8,
        changedBy: "Ian",
      });
      // The observers are told on the next tick.
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    // The rows being edited were not replaced by the newer read.
    expect(base(1).value).toBe("10.20.0.0/16");
    fireEvent.click(save());
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({ pools: [{ base: "10.20.0.0/16", size: 24 }], version: 8 });
    expect(await screen.findByText("Saved the address pools.")).toBeTruthy();
    expect(screen.queryByText(/changed these settings/)).toBeNull();
  });

  it("re-sends once after a 409 when the pools are unchanged", async () => {
    let calls = 0;
    putAnswer = () => {
      calls++;
      if (calls === 1) {
        settings = { ...settings, smallSubnets: { enabled: true, mask: 27 }, version: 8 };
        return conflictAnswer("Ian")();
      }
      return Response.json({ ...settings, version: 9 });
    };
    await mount();
    enterPool();
    fireEvent.click(save());
    expect(await screen.findByText("Saved the address pools.")).toBeTruthy();
    expect(puts().map((p) => p.body)).toEqual([
      { pools: [{ base: "10.20.0.0/16", size: 24 }], version: 7 },
      { pools: [{ base: "10.20.0.0/16", size: 24 }], version: 8 },
    ]);
    expect(screen.queryByText(/changed these settings/)).toBeNull();
  });

  it("still conflicts when someone else changed the pools", async () => {
    putAnswer = () => {
      settings = {
        ...settings,
        pools: [{ base: "10.99.0.0/16", size: 24 }],
        version: 8,
        changedBy: "Alice",
      };
      return conflictAnswer("Sam")();
    };
    await mount();
    enterPool();
    fireEvent.click(save());
    expect(
      await screen.findByText(
        "Alice changed these settings while you were editing. Reload to see them.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();
    expect(puts()).toHaveLength(1);
    // The pending rows stay until Reload.
    expect(base(1).value).toBe("10.20.0.0/16");
  });

  it("never re-sends twice in one Save", async () => {
    let calls = 0;
    putAnswer = () => {
      calls++;
      settings = { ...settings, version: 7 + calls, changedBy: "Ian" };
      return conflictAnswer("Ian")();
    };
    await mount();
    enterPool();
    fireEvent.click(save());
    expect(
      await screen.findByText(
        "Ian changed these settings while you were editing. Reload to see them.",
      ),
    ).toBeTruthy();
    expect(puts()).toHaveLength(2);
    await act(async () => {});
    expect(puts()).toHaveLength(2);
  });
});
