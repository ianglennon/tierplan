// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { PopoutFrame } from "@/components/popout-frame";
import "@/i18n";

// The chrome-less layout of the pop-out windows (plan 04-21, D-22, PLAT-05).

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderFrame() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({
        version: "",
        commit: "abc1234abc1234abc1234abc1234abc1234abc12",
        display: "dev (abc1234)",
        sourceUrl: "https://github.com/ianglennon/coaming",
        sourceTreeUrl: "https://github.com/ianglennon/coaming/tree/abc1234",
      }),
    ),
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <PopoutFrame title="web-1" state={<span>Running</span>} environment="Homelab">
        <p>the viewer</p>
      </PopoutFrame>
    </QueryClientProvider>,
  );
}

describe("PopoutFrame", () => {
  it("has a 48px bar with the name in mono as the heading, the state and the environment", () => {
    renderFrame();
    const bar = screen.getByRole("banner");
    expect(bar.className).toContain("h-12");
    const heading = screen.getByRole("heading", { level: 1, name: "web-1" });
    expect(heading.className).toContain("font-mono");
    expect(bar.contains(heading)).toBe(true);
    expect(bar.textContent).toContain("Running");
    const env = screen.getByText("Homelab");
    expect(bar.contains(env)).toBe(true);
    expect(env.className).toContain("text-muted-foreground");
  });

  it("holds the page in the main region, with no sidebar, and ends in the version footer", async () => {
    renderFrame();
    expect(screen.getByRole("main").textContent).toBe("the viewer");
    expect(screen.queryByRole("navigation")).toBeNull();
    const link = await screen.findByRole("link", { name: /dev \(abc1234\)/ });
    expect(link.getAttribute("href")).toBe("https://github.com/ianglennon/coaming/tree/abc1234");
    expect(screen.getByRole("contentinfo")).toBeTruthy();
  });

  it("truncates a long name instead of growing the bar", () => {
    renderFrame();
    expect(screen.getByRole("heading", { level: 1 }).className).toContain("truncate");
  });
});
