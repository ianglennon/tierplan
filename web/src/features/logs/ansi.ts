// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import Anser from "anser";

// Colour in logs (D-16): escape sequences are read into spans of plain text with a
// style, and drawn as React text. Nothing here makes HTML, and nothing makes a link
// (T-04-68). anser reads the escape sequences; the colours are ours.

export type Theme = "light" | "dark";

// One run of text in one style. fg and bg are "ansi-0" to "ansi-15" (the palette of the
// theme, a CSS variable), "foreground" or "background" (the page's own), or a "#rrggbb"
// a program asked for and whose contrast is good enough. A style that is not set is the
// default one.
export interface AnsiSpan {
  text: string;
  fg?: string;
  bg?: string;
  bold?: true;
  dim?: true;
  underline?: true;
}

// The 16 colours of each theme (UI-SPEC "Color"). index.css holds the same values as
// custom properties --ansi-0 to --ansi-15; a test keeps the two together.
export const ANSI_PALETTE: Record<Theme, readonly string[]> = {
  light: [
    "#404040",
    "#B91C1C",
    "#166534",
    "#854D0E",
    "#1D4ED8",
    "#A21CAF",
    "#155E75",
    "#525252",
    "#666666",
    "#991B1B",
    "#14532D",
    "#713F12",
    "#1E40AF",
    "#86198F",
    "#164E63",
    "#171717",
  ],
  dark: [
    "#8A8A8A",
    "#F87171",
    "#4ADE80",
    "#FACC15",
    "#60A5FA",
    "#E879F9",
    "#22D3EE",
    "#D4D4D4",
    "#A3A3A3",
    "#FCA5A5",
    "#86EFAC",
    "#FDE047",
    "#93C5FD",
    "#F0ABFC",
    "#67E8F9",
    "#FFFFFF",
  ],
};

// What the log is drawn on, and the page's own text and background, per theme.
export const LOG_SURFACE: Record<Theme, string> = { light: "#FAFAFA", dark: "#171717" };
const FOREGROUND: Record<Theme, string> = { light: "#0A0A0A", dark: "#FAFAFA" };
const BACKGROUND: Record<Theme, string> = { light: "#FFFFFF", dark: "#0A0A0A" };

// A colour a program asked for (256 colours, truecolor) is shown only when it stands
// out from the surface by at least this much; otherwise the default text colour is used,
// so dark-on-dark output stays readable.
export const MIN_CONTRAST = 3;

function hexToRgb(hex: string): [number, number, number] {
  const n = Number.parseInt(hex.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

function rgbToHex(r: number, g: number, b: number): string {
  const c = (v: number) =>
    Math.max(0, Math.min(255, Math.round(v)))
      .toString(16)
      .padStart(2, "0");
  return `#${c(r)}${c(g)}${c(b)}`.toUpperCase();
}

function luminance(hex: string): number {
  const [r, g, b] = hexToRgb(hex).map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  }) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

// The WCAG contrast ratio of two #rrggbb colours, from 1 to 21.
export function contrastRatio(a: string, b: string): number {
  const la = luminance(a);
  const lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

// The colour of `over` laid at `alpha` over `under`, as the page does for the stderr tint.
export function blend(over: string, under: string, alpha: number): string {
  const [or, og, ob] = hexToRgb(over);
  const [ur, ug, ub] = hexToRgb(under);
  return rgbToHex(
    or * alpha + ur * (1 - alpha),
    og * alpha + ug * (1 - alpha),
    ob * alpha + ub * (1 - alpha),
  );
}

const NAMES = ["black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"];

// anser's class names, with use_classes, for the 16 colours: ansi-red, ansi-bright-red.
function paletteIndex(cls: string): number | undefined {
  const m = /^ansi-(bright-)?(black|red|green|yellow|blue|magenta|cyan|white)$/.exec(cls);
  if (!m) return undefined;
  return NAMES.indexOf(m[2] as string) + (m[1] ? 8 : 0);
}

// The colour of a 256-colour index above 15: the 6x6x6 cube and the greys.
function paletteRgb(n: number): string {
  if (n >= 232) {
    const v = 8 + 10 * (n - 232);
    return rgbToHex(v, v, v);
  }
  const i = n - 16;
  const level = (v: number) => (v === 0 ? 0 : 55 + 40 * v);
  return rgbToHex(level(Math.floor(i / 36)), level(Math.floor(i / 6) % 6), level(i % 6));
}

// Reads anser's description of a colour into ours: a palette name, a hex colour, or
// nothing for a colour that is not set or not valid.
function colour(cls: string | null, truecolor: string | null): string | undefined {
  if (!cls) return undefined;
  const idx = paletteIndex(cls);
  if (idx !== undefined) return `ansi-${idx}`;
  const pal = /^ansi-palette-(\d{1,3})$/.exec(cls);
  if (pal) {
    const n = Number(pal[1]);
    return n >= 16 && n <= 255 ? paletteRgb(n) : undefined;
  }
  if (cls === "ansi-truecolor" && truecolor) {
    const rgb = truecolor.split(",").map((v) => Number.parseInt(v.trim(), 10));
    if (rgb.length === 3 && rgb.every((v) => v >= 0 && v <= 255)) {
      return rgbToHex(rgb[0] as number, rgb[1] as number, rgb[2] as number);
    }
  }
  return undefined;
}

// The #rrggbb of a colour of ours, for working out contrast.
function hexOf(c: string, theme: Theme): string {
  if (c === "foreground") return FOREGROUND[theme];
  if (c === "background") return BACKGROUND[theme];
  const idx = /^ansi-(\d+)$/.exec(c);
  return idx ? (ANSI_PALETTE[theme][Number(idx[1])] as string) : c;
}

// Operating system commands (a window title, a link) are not colour: they are removed
// whole, so a program cannot put a link in a log (T-04-68).
// biome-ignore lint/suspicious/noControlCharactersInRegex: the escape byte is the point
const OSC = /\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)?/g;
// biome-ignore lint/suspicious/noControlCharactersInRegex: the escape byte is the point
const STRAY_ESC = /\u001b/g;

// ansiToSpans reads the colour codes of a line into spans of text. The text of the spans
// together is exactly what a person reads and what search looks in (stripAnsi). Bold
// is weight 600, dim is the muted colour, underline is underline, and blink, italics and
// the rest are left out. A 256-colour or truecolor value, or a background, that does not
// stand out from the surface by 3:1 is dropped; text whose colour is too close to its
// own background is drawn in the page's text or background colour, whichever shows.
export function ansiToSpans(text: string, theme: Theme = "light"): AnsiSpan[] {
  if (!text.includes("\u001b")) return [{ text }];
  const surface = LOG_SURFACE[theme];
  const out: AnsiSpan[] = [];
  const parsed = Anser.ansiToJson(text.replace(OSC, ""), {
    json: true,
    remove_empty: true,
    use_classes: true,
  });
  for (const e of parsed) {
    const content = e.content.replace(STRAY_ESC, "");
    if (content === "") continue;
    const span: AnsiSpan = { text: content };
    let fg = colour(e.fg, e.fg_truecolor);
    let bg = colour(e.bg, e.bg_truecolor);
    if (fg && !fg.startsWith("ansi-") && contrastRatio(fg, surface) < MIN_CONTRAST) fg = undefined;
    if (bg && !bg.startsWith("ansi-") && contrastRatio(bg, surface) < MIN_CONTRAST) bg = undefined;
    if (bg) {
      const behind = hexOf(bg, theme);
      const front = fg ? hexOf(fg, theme) : FOREGROUND[theme];
      if (contrastRatio(front, behind) < MIN_CONTRAST) {
        fg =
          contrastRatio(FOREGROUND[theme], behind) >= contrastRatio(BACKGROUND[theme], behind)
            ? "foreground"
            : "background";
      }
    }
    if (fg) span.fg = fg;
    if (bg) span.bg = bg;
    if (e.decorations.includes("bold")) span.bold = true;
    if (e.decorations.includes("dim")) span.dim = true;
    if (e.decorations.includes("underline")) span.underline = true;
    const last = out[out.length - 1];
    if (last && sameStyle(last, span)) last.text += span.text;
    else out.push(span);
  }
  return out.length > 0 ? out : [{ text: "" }];
}

function sameStyle(a: AnsiSpan, b: AnsiSpan): boolean {
  return (
    a.fg === b.fg &&
    a.bg === b.bg &&
    a.bold === b.bold &&
    a.dim === b.dim &&
    a.underline === b.underline
  );
}

// stripAnsi is the text of a line without its colour codes: the text of ansiToSpans.
export function stripAnsi(text: string): string {
  if (!text.includes("\u001b")) return text;
  return ansiToSpans(text)
    .map((s) => s.text)
    .join("");
}
