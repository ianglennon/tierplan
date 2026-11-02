// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { DigestChip } from "@/features/stacks/digest-chip";
import "@/i18n";

const DIGEST = `sha256:${"ab12".repeat(16)}`;

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(variant?: "pinned" | "new" | "local", digest = DIGEST) {
  render(
    <TooltipProvider>
      <DigestChip digest={digest} variant={variant} />
    </TooltipProvider>,
  );
}

describe("DigestChip", () => {
  it("shows the short digest, the algorithm and twelve characters", () => {
    mount();
    expect(screen.getByRole("button").textContent).toBe(DIGEST.slice(0, "sha256:".length + 12));
  });

  it("carries the full digest in its accessible name, so nothing is hover-only", () => {
    mount();
    expect(screen.getByRole("button", { name: `Pinned to ${DIGEST}` })).toBeTruthy();
  });

  it("words the accessible name for a digest just looked up and for a local image", () => {
    mount("new");
    expect(screen.getByRole("button", { name: `Will be pinned to ${DIGEST}` })).toBeTruthy();
    cleanup();
    mount("local");
    expect(screen.getByRole("button", { name: `Local image ${DIGEST}` })).toBeTruthy();
  });

  it("copies the full digest on click and says so", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    mount();
    fireEvent.click(screen.getByRole("button"));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(DIGEST));
    expect(await screen.findByText("Copied")).toBeTruthy();
  });

  it("shows the full digest in the tooltip when focused", async () => {
    mount();
    screen.getByRole("button").focus();
    expect((await screen.findAllByText(DIGEST)).length).toBeGreaterThan(0);
  });
});
