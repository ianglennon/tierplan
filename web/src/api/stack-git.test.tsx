// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { useCheckStackGit } from "@/api/stack-git";
import { ENV_WARNINGS_KEY } from "@/api/warnings";

let client: QueryClient;
let invalidated: unknown[][];

const wrapper = ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={client}>{children}</QueryClientProvider>
);

beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  invalidated = [];
  const original = client.invalidateQueries.bind(client);
  vi.spyOn(client, "invalidateQueries").mockImplementation((filters, options) => {
    invalidated.push([...((filters?.queryKey as unknown[] | undefined) ?? [])]);
    return original(filters, options);
  });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (req.method === "POST" && path === "/api/env/e1/stacks/s1/git/check") {
        return Response.json({ checking: true }, { status: 202 });
      }
      if (path === "/api/env/e1/stacks/s1") {
        return Response.json({ id: "s1", name: "web", git: { checking: false } });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

describe("useCheckStackGit", () => {
  it("reads the bell again when the check is accepted and when it has ended (IN-E07)", async () => {
    const { result } = renderHook(() => useCheckStackGit("e1", "s1"), { wrapper });
    act(() => result.current.check());
    // The check may start and end between two looks, so the test waits for where it ends, not for it to be running.
    const bells = () =>
      invalidated.filter((k) => JSON.stringify(k) === JSON.stringify([...ENV_WARNINGS_KEY])).length;
    // Once as the request settled, and once more when the stack's summary said the check was over.
    await waitFor(() => {
      expect(bells()).toBeGreaterThanOrEqual(2);
      expect(result.current.running).toBe(false);
    });
    expect(fetch).toHaveBeenCalled();
  });
});
