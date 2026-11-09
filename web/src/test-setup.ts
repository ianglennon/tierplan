// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { configure } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach } from "vitest";

// findBy and waitFor give up after 1 s by default, which a loaded CI runner (three or four
// times slower than a quiet one) can miss for a render that is merely slow. A deadline only
// decides how long a broken test takes to fail (docs/testing.md), so this one is generous: a
// test does not wait this long when the element is there, and it sits under the 60 s test
// timeout so an element that never comes fails with its own message.
configure({ asyncUtilTimeout: 30000 });

// Sonner keeps its toasts in one store per module, not per <Toaster>, and replays every toast
// still active to each <Toaster> that mounts. A toast is dismissed only by its own timer (4 s or
// 8 s) in a <Toaster> that is still mounted, so a test that ends sooner leaves its toast active,
// and the next test in the file that mounts a <Toaster> shows it again. A findByText for the same
// words then matched one element or two depending on whether its first look came before or after
// the test's own toast rendered, which a loaded runner decides. Dismissing them all after every
// test means each test starts with no toasts.
afterEach(() => {
  toast.dismiss();
});

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

// jsdom has no scrolling: its window.scrollTo only logs "Not implemented", and the router
// calls it on every navigation, so this one does nothing quietly.
window.scrollTo = () => {};

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
