// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { BrowseBox, looksLikeRepository } from "@/features/registries/browse-box";
import "@/i18n";

// The "Browse a repository" card (plan 05-18, IMG-03, D-11).

afterEach(cleanup);

const field = () => screen.getByLabelText("Repository") as HTMLInputElement;
const submit = () => screen.getByRole("button", { name: "Browse tags" });

describe("BrowseBox", () => {
  it("opens with a mono field and an outline button", () => {
    render(<BrowseBox onBrowse={() => {}} />);
    expect(screen.getByText("Browse a repository")).toBeTruthy();
    expect(screen.getByText("See a repository's tags, digests and where they run.")).toBeTruthy();
    expect(field().placeholder).toBe("ghcr.io/acme/app or nginx");
    expect(field().className).toContain("font-mono");
    expect(submit().getAttribute("data-variant")).toBe("outline");
  });

  it("browses the trimmed reference", () => {
    const onBrowse = vi.fn();
    render(<BrowseBox onBrowse={onBrowse} />);
    fireEvent.change(field(), { target: { value: "  ghcr.io/acme/app  " } });
    fireEvent.click(submit());
    expect(onBrowse).toHaveBeenCalledExactlyOnceWith("ghcr.io/acme/app");
  });

  it.each(["", "   ", "Not A Ref", "ghcr.io/Acme/App", "a b", "x".repeat(600)])(
    "shows the reference error and goes nowhere for %j",
    (value) => {
      const onBrowse = vi.fn();
      render(<BrowseBox onBrowse={onBrowse} />);
      fireEvent.change(field(), { target: { value } });
      fireEvent.click(submit());
      expect(onBrowse).not.toHaveBeenCalled();
      expect(
        screen.getByText("Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."),
      ).toBeTruthy();
      expect(field().getAttribute("aria-invalid")).toBe("true");
      // Typing again clears the error.
      fireEvent.change(field(), { target: { value: "nginx" } });
      expect(screen.queryByText(/Enter an image like/)).toBeNull();
    },
  );
});

describe("looksLikeRepository", () => {
  it.each([
    "nginx",
    "nginx:1.27",
    "library/nginx",
    "ghcr.io/acme/app",
    "ghcr.io/acme/app:latest",
    "localhost:5000/team/app",
    "registry.example.org:5000/a/b/c-d_e.f",
    "ghcr.io/acme/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  ])("accepts %s", (ref) => expect(looksLikeRepository(ref)).toBe(true));

  it.each(["", "UPPER/case", "a//b", "/leading", "trailing/", "a b", "-x", "ghcr.io/acme/app:"])(
    "refuses %j",
    (ref) => expect(looksLikeRepository(ref)).toBe(false),
  );
});
