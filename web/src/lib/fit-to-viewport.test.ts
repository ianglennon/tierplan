// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { afterEach, describe, expect, it } from "vitest";
import { fitHeight } from "@/lib/fit-to-viewport";

const MIN = 320;

function rect(top: number, height: number): DOMRect {
  return {
    top,
    bottom: top + height,
    height,
    left: 0,
    right: 0,
    width: 0,
    x: 0,
    y: top,
    toJSON: () => ({}),
  } as DOMRect;
}

function page(boxTop: number, boxHeight: number, afterBox: number, footer: number) {
  document.body.innerHTML = `<div><main style="padding-bottom: 24px"><div id="content"><div id="box"></div></div></main><div id="footer"></div></div>`;
  const box = document.getElementById("box") as HTMLElement;
  box.getBoundingClientRect = () => rect(boxTop, boxHeight);
  (document.getElementById("content") as HTMLElement).getBoundingClientRect = () =>
    rect(0, boxTop + boxHeight + afterBox);
  (document.getElementById("footer") as HTMLElement).getBoundingClientRect = () => rect(0, footer);
  return box;
}

describe("fitHeight", () => {
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("leaves room for what follows the box, main's padding and the footer, so the page does not scroll", () => {
    // 900 tall window, box starts at 300, 40 px of panel below it, 24 px padding, 37 px footer.
    expect(fitHeight(page(300, 500, 40, 37), 900, MIN)).toBe(900 - 300 - 40 - 24 - 37);
  });

  it("does not depend on the box's current height, so measuring again does not drift", () => {
    expect(fitHeight(page(300, 200, 40, 37), 900, MIN)).toBe(
      fitHeight(page(300, 800, 40, 37), 900, MIN),
    );
  });

  it("never goes below the minimum, so a short window scrolls the page instead", () => {
    expect(fitHeight(page(300, 500, 40, 37), 500, MIN)).toBe(MIN);
  });
});
