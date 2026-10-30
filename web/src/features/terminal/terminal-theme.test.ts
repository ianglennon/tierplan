// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { renderHook, waitFor } from "@testing-library/react";
import { act } from "react";
import { afterEach, describe, expect, it } from "vitest";

import {
  resolvedTheme,
  useResolvedTheme,
  withAlpha,
  xtermTheme,
} from "@/features/terminal/terminal-theme";

// The terminal's colours (plan 04-24, D-33): the app's own palette, light or dark.

const NAMES = [
  "black",
  "red",
  "green",
  "yellow",
  "blue",
  "magenta",
  "cyan",
  "white",
  "brightBlack",
  "brightRed",
  "brightGreen",
  "brightYellow",
  "brightBlue",
  "brightMagenta",
  "brightCyan",
  "brightWhite",
] as const;

afterEach(() => {
  document.documentElement.removeAttribute("style");
  document.documentElement.classList.remove("dark");
});

function setVars(vars: Record<string, string>) {
  for (const [k, v] of Object.entries(vars)) document.documentElement.style.setProperty(k, v);
}

describe("xtermTheme", () => {
  it("maps the 16 --ansi values, the card as background, the foreground and the muted foreground at 35% as selection", () => {
    const vars: Record<string, string> = {
      "--card": "#101010",
      "--foreground": "#f0f0f0",
      "--muted-foreground": "#a0b0c0",
    };
    NAMES.forEach((_, i) => {
      vars[`--ansi-${i}`] = `#0000${i.toString(16).padStart(2, "0")}`;
    });
    setVars(vars);
    const theme = xtermTheme("dark");
    NAMES.forEach((name, i) => {
      expect(theme[name], name).toBe(`#0000${i.toString(16).padStart(2, "0")}`);
    });
    expect(theme.background).toBe("#101010");
    expect(theme.foreground).toBe("#f0f0f0");
    expect(theme.cursor).toBe("#f0f0f0");
    expect(theme.selectionBackground).toBe("rgba(160, 176, 192, 0.35)");
  });

  it("gives different colours for light and dark when the stylesheet has set nothing", () => {
    const light = xtermTheme("light");
    const dark = xtermTheme("dark");
    expect(light.background).toBe("#ffffff");
    expect(dark.background).not.toBe(light.background);
    expect(light.foreground).not.toBe(dark.foreground);
    for (const name of NAMES) {
      expect(light[name], `light ${name}`).toMatch(/^#[0-9a-f]{6}$/);
      expect(dark[name], `dark ${name}`).toMatch(/^#[0-9a-f]{6}$/);
    }
    // Light text on dark ground and the reverse, never the same on both.
    expect(light.red).not.toBe(dark.red);
  });

  it("takes a variable that is set over the built-in value, one at a time", () => {
    setVars({ "--ansi-1": "#123456" });
    expect(xtermTheme("light").red).toBe("#123456");
    expect(xtermTheme("light").green).toBe("#166534");
  });
});

describe("withAlpha", () => {
  it("turns #rrggbb into rgba and mixes anything else with transparent", () => {
    expect(withAlpha("#000000", 0.35)).toBe("rgba(0, 0, 0, 0.35)");
    expect(withAlpha("oklch(0.5 0 0)", 0.35)).toBe(
      "color-mix(in srgb, oklch(0.5 0 0) 35%, transparent)",
    );
  });
});

describe("the theme the document shows", () => {
  it("is dark with the dark class on <html>, and light without it", () => {
    expect(resolvedTheme()).toBe("light");
    document.documentElement.classList.add("dark");
    expect(resolvedTheme()).toBe("dark");
  });

  it("follows a change of the class, live", async () => {
    const { result } = renderHook(() => useResolvedTheme());
    expect(result.current).toBe("light");
    act(() => document.documentElement.classList.add("dark"));
    await waitFor(() => expect(result.current).toBe("dark"));
    act(() => document.documentElement.classList.remove("dark"));
    await waitFor(() => expect(result.current).toBe("light"));
  });
});
