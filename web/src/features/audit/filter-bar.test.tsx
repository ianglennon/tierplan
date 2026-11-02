// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import "@/i18n";
import { makeMe } from "@/test-me";
import { FilterBar } from "./filter-bar";
import type { AuditSearch } from "./filters";

const BULK = "018f0000-0000-7000-8000-000000000001";

let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ instanceAdmin: true }));
      if (path === "/api/audit/actors") return Response.json([]);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(search: AuditSearch) {
  const onChange = vi.fn();
  const onClear = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <FilterBar search={search} onChange={onChange} onClear={onClear} />
    </QueryClientProvider>,
  );
  return { onChange, onClear };
}

describe("FilterBar bulk chip", () => {
  it("shows no chip without a bulk filter", () => {
    mount({ range: "24h" });
    expect(screen.queryByText(/Bulk operation/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove bulk filter" })).toBeNull();
  });

  it("shows 'Bulk operation' with the first 8 characters of the ID, and removing it clears only the bulk", () => {
    const { onChange } = mount({ range: "24h", bulk: BULK });
    expect(screen.getByText("Bulk operation 018f0000")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove bulk filter" }));
    expect(onChange).toHaveBeenCalledWith({ bulk: undefined });
  });

  it("counts a bulk filter as something Clear filters can clear", () => {
    const { onClear } = mount({ range: "24h", bulk: BULK });
    const clear = screen.getByRole("button", { name: "Clear filters" }) as HTMLButtonElement;
    expect(clear.disabled).toBe(false);
    fireEvent.click(clear);
    expect(onClear).toHaveBeenCalled();
  });
});
