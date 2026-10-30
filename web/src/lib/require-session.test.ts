// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { isRedirect } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { queryClient } from "@/api/query-client";
import { requireSession } from "@/lib/require-session";
import { makeMe } from "@/test-me";

// The guard shared by the app layout and the pop-out windows (plan 04-21, T-04-74).

function stub(opts: { needsSetup?: boolean; me?: "in" | "out" | "broken" }) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Coaming", needsSetup: opts.needsSetup === true });
      }
      if (path === "/api/me") {
        if (opts.me === "out") return Response.json({ status: 401 }, { status: 401 });
        if (opts.me === "broken") return Response.json({ status: 500 }, { status: 500 });
        return Response.json(makeMe({ csrfToken: "c" }));
      }
      return Response.json({}, { status: 404 });
    }),
  );
}

async function rejection(p: Promise<void>): Promise<unknown> {
  try {
    await p;
  } catch (err) {
    return err;
  }
  return undefined;
}

beforeEach(() => queryClient.clear());
afterEach(() => {
  vi.unstubAllGlobals();
  queryClient.clear();
});

describe("requireSession", () => {
  it("resolves for a signed-in visitor", async () => {
    stub({ me: "in" });
    await expect(requireSession({ location: { href: "/popout/e1/x" } })).resolves.toBeUndefined();
  });

  it("sends a visitor without a session to sign-in with the page remembered in next", async () => {
    stub({ me: "out" });
    const err = await rejection(
      requireSession({ location: { href: "/popout/e1/containers/c1/logs?q=error" } }),
    );
    expect(isRedirect(err)).toBe(true);
    const options = (err as { options: { to: string; search: { next?: string } } }).options;
    expect(options.to).toBe("/login");
    expect(options.search.next).toBe("/popout/e1/containers/c1/logs?q=error");
  });

  it("remembers nothing for the home page", async () => {
    stub({ me: "out" });
    const err = await rejection(requireSession({ location: { href: "/" } }));
    expect((err as { options: { search: { next?: string } } }).options.search.next).toBeUndefined();
  });

  it("sends a fresh install to the setup wizard before it asks who is signed in", async () => {
    stub({ needsSetup: true, me: "out" });
    const err = await rejection(requireSession({ location: { href: "/anything" } }));
    expect(isRedirect(err)).toBe(true);
    expect((err as { options: { to: string } }).options.to).toBe("/setup");
  });

  it("lets any other failure through to the error page", async () => {
    stub({ me: "broken" });
    const err = await rejection(requireSession({ location: { href: "/x" } }));
    expect(isRedirect(err)).toBe(false);
    expect(err).toBeInstanceOf(Error);
  });
});
