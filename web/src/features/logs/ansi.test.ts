// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import {
  ANSI_PALETTE,
  ansiToSpans,
  blend,
  contrastRatio,
  LOG_SURFACE,
  stripAnsi,
  type Theme,
} from "@/features/logs/ansi";

// Colour in logs (plan 04-20, D-16, T-04-68): escape sequences become styled text, the
// palette is readable in both themes, and nothing is ever HTML or a link.

import css from "@/index.css?raw";

const ESC = "\u001b";
const themes: Theme[] = ["light", "dark"];

describe("ansiToSpans", () => {
  it("turns a colour code into a span with the palette colour, and the rest into plain text", () => {
    expect(ansiToSpans(`${ESC}[31mred${ESC}[0m plain`)).toEqual([
      { text: "red", fg: "ansi-1" },
      { text: " plain" },
    ]);
  });

  it("is one plain span for a line with no escape codes, whatever else it holds", () => {
    expect(ansiToSpans("<b>not bold</b> https://example.test")).toEqual([
      { text: "<b>not bold</b> https://example.test" },
    ]);
    expect(ansiToSpans("")).toEqual([{ text: "" }]);
  });

  it("maps the eight normal and eight bright colours, for text and background", () => {
    expect(ansiToSpans(`${ESC}[37mw${ESC}[0m`)[0]?.fg).toBe("ansi-7");
    expect(ansiToSpans(`${ESC}[91mbr${ESC}[0m`)[0]?.fg).toBe("ansi-9");
    expect(ansiToSpans(`${ESC}[97mbw${ESC}[0m`)[0]?.fg).toBe("ansi-15");
    expect(ansiToSpans(`${ESC}[44mb${ESC}[0m`, "dark")[0]?.bg).toBe("ansi-4");
    expect(ansiToSpans(`${ESC}[38;5;9mx${ESC}[0m`)[0]?.fg).toBe("ansi-9");
  });

  it("renders bold at 600, dim in the muted colour and underline as underline", () => {
    const [bold] = ansiToSpans(`${ESC}[1mb${ESC}[0m`);
    expect(bold).toEqual({ text: "b", bold: true });
    const [dim] = ansiToSpans(`${ESC}[2md${ESC}[0m`);
    expect(dim).toEqual({ text: "d", dim: true });
    const [ul] = ansiToSpans(`${ESC}[4mu${ESC}[0m`);
    expect(ul).toEqual({ text: "u", underline: true });
    expect(ansiToSpans(`${ESC}[1;32mboth${ESC}[0m`)).toEqual([
      { text: "both", fg: "ansi-2", bold: true },
    ]);
  });

  it("ignores blinking, italics and the sequences that move a cursor", () => {
    expect(ansiToSpans(`${ESC}[5mblink${ESC}[0m`)).toEqual([{ text: "blink" }]);
    expect(ansiToSpans(`${ESC}[3mitalic${ESC}[0m`)).toEqual([{ text: "italic" }]);
    expect(ansiToSpans(`${ESC}[2Kclear ${ESC}[1Aup${ESC}[?25l`)).toEqual([{ text: "clear up" }]);
  });

  it("joins neighbours that end up in the same style", () => {
    expect(ansiToSpans(`${ESC}[31ma${ESC}[31mb${ESC}[0m`)).toEqual([{ text: "ab", fg: "ansi-1" }]);
  });

  it("keeps a 256-colour or truecolor value that stands out from the surface", () => {
    expect(ansiToSpans(`${ESC}[38;5;208mo${ESC}[0m`, "dark")[0]?.fg).toBe("#FF8700");
    expect(ansiToSpans(`${ESC}[38;2;255;80;80mt${ESC}[0m`, "dark")[0]?.fg).toBe("#FF5050");
    expect(ansiToSpans(`${ESC}[38;2;20;30;120mt${ESC}[0m`, "light")[0]?.fg).toBe("#141E78");
  });

  it("falls back to the foreground for a colour under 3:1 against the surface", () => {
    // Dark on dark, and pale on pale.
    expect(ansiToSpans(`${ESC}[38;2;16;16;16mdark${ESC}[0m`, "dark")).toEqual([{ text: "dark" }]);
    expect(ansiToSpans(`${ESC}[38;5;232mgrey${ESC}[0m`, "dark")).toEqual([{ text: "grey" }]);
    expect(ansiToSpans(`${ESC}[38;2;245;245;245mpale${ESC}[0m`, "light")).toEqual([
      { text: "pale" },
    ]);
    // The same colour is fine on the other surface.
    expect(ansiToSpans(`${ESC}[38;2;245;245;245mpale${ESC}[0m`, "dark")[0]?.fg).toBe("#F5F5F5");
  });

  it("drops a background that does not stand out, and fixes text that cannot be read on one that does", () => {
    expect(ansiToSpans(`${ESC}[48;2;18;18;18mx${ESC}[0m`, "dark")).toEqual([{ text: "x" }]);
    // Bright white text on a bright yellow background is unreadable: the page's text wins.
    const [s] = ansiToSpans(`${ESC}[97;103mx${ESC}[0m`, "light");
    expect(s?.bg).toBe("ansi-11");
    expect(["foreground", "background"]).toContain(s?.fg);
  });

  it("never makes a link or HTML of what a program prints", () => {
    const spans = ansiToSpans(
      `${ESC}]8;;https://evil.test${"\u0007"}click${ESC}]8;;${"\u0007"} <img src=x onerror=1>`,
    );
    expect(spans.map((s) => s.text).join("")).toBe("click <img src=x onerror=1>");
    expect(JSON.stringify(spans)).not.toContain("evil.test");
    expect(JSON.stringify(spans)).not.toContain("href");
  });

  it("leaves no escape byte in the text, however the sequence is cut", () => {
    for (const bad of [
      `${ESC}`,
      `a${ESC}`,
      `${ESC}[`,
      `${ESC}[31`,
      `${ESC}]8;;x`,
      `${ESC}[999999999m x`,
    ]) {
      const text = ansiToSpans(bad)
        .map((s) => s.text)
        .join("");
      expect(text).not.toContain(ESC);
    }
  });
});

describe("stripAnsi", () => {
  it("is the text of the spans, and what search reads", () => {
    for (const line of [
      `${ESC}[1;31mfail${ESC}[0m: ${ESC}[2mwhy${ESC}[0m`,
      `${ESC}]0;title${"\u0007"}x`,
      "plain",
    ]) {
      expect(stripAnsi(line)).toBe(
        ansiToSpans(line)
          .map((s) => s.text)
          .join(""),
      );
    }
    expect(stripAnsi(`${ESC}[31mred${ESC}[0m`)).toBe("red");
  });
});

describe("the palette", () => {
  // The log surface, and the stderr tint laid over it: the status colour at 8%.
  const stderr: Record<Theme, string> = { light: "#DC2626", dark: "#F87171" };

  for (const theme of themes) {
    it(`keeps all 16 ${theme} colours at 4.5:1 or more on the surface and on the stderr tint`, () => {
      const surface = LOG_SURFACE[theme];
      const tint = blend(stderr[theme], surface, 0.08);
      expect(ANSI_PALETTE[theme]).toHaveLength(16);
      ANSI_PALETTE[theme].forEach((colour, i) => {
        expect(
          contrastRatio(colour, surface),
          `${theme} ansi-${i} on the surface`,
        ).toBeGreaterThanOrEqual(4.5);
        expect(
          contrastRatio(colour, tint),
          `${theme} ansi-${i} on the stderr tint`,
        ).toBeGreaterThanOrEqual(4.5);
      });
    });
  }

  it("is the palette index.css defines, for both themes", () => {
    const blocks = {
      light: /:root\s*\{[^}]*\}/s.exec(css)?.[0] ?? "",
      dark: /\.dark\s*\{[^}]*\}/s.exec(css)?.[0] ?? "",
    };
    for (const theme of themes) {
      for (let i = 0; i < 16; i++) {
        const m = new RegExp(`--ansi-${i}:\\s*(#[0-9A-Fa-f]{6})`).exec(blocks[theme]);
        expect(m?.[1]?.toUpperCase(), `${theme} --ansi-${i}`).toBe(ANSI_PALETTE[theme][i]);
      }
    }
  });

  it("measures contrast the way the guidelines do", () => {
    expect(contrastRatio("#000000", "#FFFFFF")).toBeCloseTo(21, 1);
    expect(contrastRatio("#777777", "#777777")).toBeCloseTo(1, 5);
  });
});
