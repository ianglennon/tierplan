// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ITheme } from "@xterm/xterm";
import { useEffect, useState } from "react";

// The terminal's colours follow the app's light or dark theme (D-33). They are read from
// the CSS variables the theme sets, so the app's palette stays the one source: the 16
// --ansi-* colours (index.css, checked for contrast in plan 04-20), --card behind the
// text, --foreground for the text and --muted-foreground at 35% for a selection.

export type ResolvedTheme = "light" | "dark";

// The 16 ANSI colours in xterm's names, in --ansi-0 to --ansi-15 order.
const ANSI_NAMES = [
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

// What each theme falls back to when the stylesheet has not set a variable (a test, or a
// page whose CSS has not loaded): the values index.css gives them.
const FALLBACK: Record<
  ResolvedTheme,
  { card: string; foreground: string; muted: string; ansi: readonly string[] }
> = {
  light: {
    card: "#ffffff",
    foreground: "#0a0a0a",
    muted: "#666666",
    ansi: [
      "#404040",
      "#b91c1c",
      "#166534",
      "#854d0e",
      "#1d4ed8",
      "#a21caf",
      "#155e75",
      "#525252",
      "#666666",
      "#991b1b",
      "#14532d",
      "#713f12",
      "#1e40af",
      "#86198f",
      "#164e63",
      "#171717",
    ],
  },
  dark: {
    card: "#171717",
    foreground: "#fafafa",
    muted: "#a3a3a3",
    ansi: [
      "#8a8a8a",
      "#f87171",
      "#4ade80",
      "#facc15",
      "#60a5fa",
      "#e879f9",
      "#22d3ee",
      "#d4d4d4",
      "#a3a3a3",
      "#fca5a5",
      "#86efac",
      "#fde047",
      "#93c5fd",
      "#f0abfc",
      "#67e8f9",
      "#ffffff",
    ],
  },
};

// The alpha of a selection over the background.
const SELECTION_ALPHA = 0.35;

// withAlpha is color at the given opacity. A #rrggbb colour (what --muted-foreground is
// in both themes) becomes rgba(); anything else is mixed with transparent by the browser.
export function withAlpha(color: string, alpha: number): string {
  const hex = /^#([0-9a-f]{6})$/i.exec(color.trim());
  if (hex?.[1]) {
    const n = Number.parseInt(hex[1], 16);
    return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
  }
  return `color-mix(in srgb, ${color.trim()} ${Math.round(alpha * 100)}%, transparent)`;
}

// xtermTheme is the terminal's colour theme for the app theme: the variables read from the
// document's computed style, else the theme's built-in values. The CSS applies the
// variables of the theme the document is in, so call it for the theme the document shows
// (resolvedTheme()).
export function xtermTheme(theme: ResolvedTheme, root: Element = document.documentElement): ITheme {
  const style = getComputedStyle(root);
  const read = (name: string, fallback: string) => {
    const v = style.getPropertyValue(name).trim();
    return v === "" ? fallback : v;
  };
  const fb = FALLBACK[theme];
  const out: Record<string, string> = {
    background: read("--card", fb.card),
    foreground: read("--foreground", fb.foreground),
    cursor: read("--foreground", fb.foreground),
    cursorAccent: read("--card", fb.card),
    selectionBackground: withAlpha(read("--muted-foreground", fb.muted), SELECTION_ALPHA),
  };
  ANSI_NAMES.forEach((name, i) => {
    out[name] = read(`--ansi-${i}`, fb.ansi[i] ?? fb.foreground);
  });
  return out as ITheme;
}

// resolvedTheme is the theme the document is showing right now: the theme module puts a
// "dark" class on <html> for dark, whatever the saved choice (light, dark or system).
export function resolvedTheme(): ResolvedTheme {
  return document.documentElement.classList.contains("dark") ? "dark" : "light";
}

// useResolvedTheme is the theme the document shows, kept current: the choice in the theme
// menu and the operating system's change both end in the class on <html>.
export function useResolvedTheme(): ResolvedTheme {
  const [theme, setTheme] = useState<ResolvedTheme>(resolvedTheme);
  useEffect(() => {
    const observer = new MutationObserver(() => setTheme(resolvedTheme()));
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    setTheme(resolvedTheme());
    return () => observer.disconnect();
  }, []);
  return theme;
}
