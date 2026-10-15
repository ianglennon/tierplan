// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { configure } from "@testing-library/react";

// findBy and waitFor give up after 1 s by default, which a loaded CI runner (three or
// four times slower than a quiet one) can miss for a render that is merely slow. 4 s
// is the wait the load-error tests already chose one by one.
configure({ asyncUtilTimeout: 4000 });

// jsdom has no ResizeObserver (Radix checkboxes and menus measure with it) and
// no matchMedia (the sidebar and the theme switch read it). These stand-ins
// report nothing and never match, which is all the tests need.
class NoopResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

if (typeof globalThis.ResizeObserver === "undefined") {
  globalThis.ResizeObserver = NoopResizeObserver;
}

if (typeof window.matchMedia === "undefined") {
  window.matchMedia = (query: string): MediaQueryList =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as MediaQueryList;
}

// jsdom does not lay anything out, so it has no scrollIntoView either; cmdk
// calls it to keep the highlighted item in view.
if (typeof Element.prototype.scrollIntoView === "undefined") {
  Element.prototype.scrollIntoView = () => {};
}

// jsdom has no layout, so a Range cannot measure itself, which CodeMirror does after every
// change. These report an empty box, which is all it needs to run.
if (typeof Range !== "undefined" && typeof Range.prototype.getClientRects === "undefined") {
  Range.prototype.getClientRects = () =>
    ({
      length: 0,
      item: () => null,
      [Symbol.iterator]: [][Symbol.iterator],
    }) as unknown as DOMRectList;
}
if (typeof Range !== "undefined" && typeof Range.prototype.getBoundingClientRect === "undefined") {
  Range.prototype.getBoundingClientRect = () =>
    ({
      x: 0,
      y: 0,
      width: 0,
      height: 0,
      top: 0,
      left: 0,
      right: 0,
      bottom: 0,
      toJSON: () => ({}),
    }) as DOMRect;
}
