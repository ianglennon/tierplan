// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { VersionFooter } from "@/components/version-footer";
import "@/i18n";

const SOURCE_URL = "https://github.com/ianglennon/bayplan";

function renderFooter() {
  // retry: false so a failing fetch settles into the error state at once.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <VersionFooter />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("VersionFooter", () => {
  it("shows the running version linked to its source tree", async () => {
    const treeUrl = `${SOURCE_URL}/tree/abc1234abc1234abc1234abc1234abc1234abc12`;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        Response.json({
          version: "",
          commit: "abc1234abc1234abc1234abc1234abc1234abc12",
          display: "dev (abc1234)",
          sourceUrl: SOURCE_URL,
          sourceTreeUrl: treeUrl,
        }),
      ),
    );
    renderFooter();

    const versionLink = await screen.findByRole("link", { name: "Bayplan dev (abc1234)" });
    expect(versionLink.getAttribute("href")).toBe(treeUrl);

    const sourceLink = screen.getByRole("link", { name: "Source code (opens in a new tab)" });
    expect(sourceLink.getAttribute("href")).toBe(SOURCE_URL);

    for (const link of [versionLink, sourceLink]) {
      expect(link.getAttribute("target")).toBe("_blank");
      const rel = link.getAttribute("rel") ?? "";
      expect(rel).toContain("noopener");
      expect(rel).toContain("noreferrer");
    }
  });

  it("still shows the wordmark and a source link when the API call rejects", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("network down");
      }),
    );
    renderFooter();

    const sourceLink = await screen.findByRole("link", { name: /Source code/ });
    expect(sourceLink.getAttribute("href")).toBe(SOURCE_URL);
    expect(sourceLink.getAttribute("target")).toBe("_blank");
    expect(sourceLink.getAttribute("rel") ?? "").toContain("noopener");
    expect(screen.getByText("Bayplan")).toBeTruthy();
  });

  it("falls back the same way when the API answers with an error status", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        Response.json({ title: "Internal Server Error", status: 500 }, { status: 500 }),
      ),
    );
    renderFooter();

    const sourceLink = await screen.findByRole("link", { name: /Source code/ });
    expect(sourceLink.getAttribute("href")).toBe(SOURCE_URL);
    expect(screen.getByText("Bayplan")).toBeTruthy();
  });
});
